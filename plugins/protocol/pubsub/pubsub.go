package pubsub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/core"
	"github.com/krill/krill/internal/plugincfg"
	ipubsub "github.com/krill/krill/internal/pubsub"
	"github.com/krill/krill/internal/schema"
	"github.com/krill/krill/internal/telemetry"
)

func init() {
	core.Global().RegisterProtocol("pubsub", func(cfg map[string]interface{}) (core.Protocol, error) {
		return New(cfg)
	})
}

// Plugin is the generic PubSub ingress/egress protocol plugin.
type Plugin struct {
	cfg       Config
	adapter   ipubsub.Adapter
	log       *slog.Logger
	b         bus.Bus
	dedup     *ipubsub.DedupStore
	processFn func(context.Context, *bus.Envelope) error
}

// Config holds PubSub plugin runtime settings.
type Config struct {
	BrokerType   string
	Endpoint     string
	TopicIn      string
	TopicOut     string
	TopicDLQ     string
	Group        string
	Retry        ipubsub.RetryPolicy
	DedupTTL     time.Duration
	AckTimeout   time.Duration
	StrictSchema bool
}

// New constructs the PubSub plugin from raw config.
func New(cfg map[string]interface{}) (*Plugin, error) {
	parsed := parseConfig(cfg)
	if parsed.TopicIn == "" {
		return nil, fmt.Errorf("pubsub config.topic_in is required")
	}
	if parsed.TopicOut == "" {
		return nil, fmt.Errorf("pubsub config.topic_out is required")
	}
	if parsed.BrokerType == "" {
		parsed.BrokerType = "nats"
	}
	if parsed.AckTimeout <= 0 {
		parsed.AckTimeout = 5 * time.Second
	}
	if parsed.DedupTTL <= 0 {
		parsed.DedupTTL = 5 * time.Minute
	}
	if parsed.Retry.Backoff <= 0 {
		parsed.Retry.Backoff = 100 * time.Millisecond
	}
	if parsed.Retry.MaxBackoff <= 0 {
		parsed.Retry.MaxBackoff = 2 * time.Second
	}
	if parsed.Retry.MaxRetries <= 0 {
		parsed.Retry.MaxRetries = 3
	}
	if parsed.TopicDLQ != "" {
		parsed.Retry.DLQTopic = parsed.TopicDLQ
	}
	p := &Plugin{
		cfg:     parsed,
		adapter: ipubsub.NewAdapter(ipubsub.Config{BrokerType: parsed.BrokerType, Endpoint: parsed.Endpoint}),
		dedup:   ipubsub.NewDedupStore(parsed.DedupTTL),
	}
	p.processFn = p.publishInbound
	return p, nil
}

// NewWithAdapter constructs the PubSub plugin with an injected adapter for tests.
func NewWithAdapter(cfg Config, adapter ipubsub.Adapter) *Plugin {
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = 5 * time.Second
	}
	if cfg.DedupTTL <= 0 {
		cfg.DedupTTL = 5 * time.Minute
	}
	if cfg.Retry.MaxRetries <= 0 {
		cfg.Retry.MaxRetries = 3
	}
	if cfg.Retry.Backoff <= 0 {
		cfg.Retry.Backoff = 100 * time.Millisecond
	}
	if cfg.Retry.MaxBackoff <= 0 {
		cfg.Retry.MaxBackoff = 2 * time.Second
	}
	if cfg.Retry.DLQTopic == "" {
		cfg.Retry.DLQTopic = cfg.TopicDLQ
	}
	p := &Plugin{
		cfg:     cfg,
		adapter: adapter,
		dedup:   ipubsub.NewDedupStore(cfg.DedupTTL),
	}
	p.processFn = p.publishInbound
	return p
}

func (p *Plugin) Name() string { return "pubsub" }

func (p *Plugin) Start(ctx context.Context, b bus.Bus, log *slog.Logger) error {
	p.b = b
	p.log = log
	if err := p.adapter.Connect(ctx); err != nil {
		return fmt.Errorf("connect adapter: %w", err)
	}
	inCh, err := p.adapter.Subscribe(ctx, p.cfg.TopicIn, p.cfg.Group)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	go p.consume(ctx, inCh)
	go p.publishReplies(ctx)
	p.log.Info("pubsub plugin started",
		"broker", p.cfg.BrokerType,
		"topic_in", p.cfg.TopicIn,
		"topic_out", p.cfg.TopicOut,
		"topic_dlq", p.cfg.TopicDLQ,
	)
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	return p.adapter.Close()
}

func (p *Plugin) consume(ctx context.Context, inCh <-chan *ipubsub.Message) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-inCh:
			if !ok {
				return
			}
			if err := p.processWithTimeout(ctx, msg); err != nil {
				p.handleFailure(ctx, msg, err)
				continue
			}
			if err := p.adapter.Ack(msg); err != nil {
				p.log.Warn("pubsub ack failed", "id", msg.ID, "err", err)
			}
		}
	}
}

func (p *Plugin) processWithTimeout(ctx context.Context, msg *ipubsub.Message) error {
	if msg == nil {
		return fmt.Errorf("nil message")
	}
	env, err := p.decodeInbound(msg)
	if err != nil {
		return err
	}
	span := telemetry.StartSpan(p.log, env.Meta["trace_id"], env.Meta["ingress_span"], "pubsub.receive",
		"topic", msg.Topic,
		"id", msg.ID,
	)
	duplicate := false
	defer func() { span.End(err, "duplicate", duplicate) }()

	dedupKey := p.dedupKey(env)
	if p.isDuplicate(dedupKey, env.ID) {
		duplicate = true
		return nil
	}
	procCtx, cancel := context.WithTimeout(ctx, p.cfg.AckTimeout)
	defer cancel()

	procErr := make(chan error, 1)
	go func() {
		procErr <- p.processFn(procCtx, env)
	}()

	select {
	case err = <-procErr:
		if err == nil {
			p.dedup.Mark(dedupKey, time.Now())
		}
		return err
	case <-procCtx.Done():
		if procCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("ack timeout after %s", p.cfg.AckTimeout)
		}
		return procCtx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Plugin) publishInbound(ctx context.Context, env *bus.Envelope) error {
	return p.b.Publish(ctx, bus.InboundKey, env)
}

func (p *Plugin) decodeInbound(msg *ipubsub.Message) (*bus.Envelope, error) {
	v2, err := schema.NormalizeJSON(msg.Payload, schema.NormalizeOptions{StrictV2Validation: p.cfg.StrictSchema})
	if err != nil {
		return nil, fmt.Errorf("decode v2 payload: %w", err)
	}
	env := schema.V2ToBus(v2)
	if env.SourceProtocol == "" {
		env.SourceProtocol = "pubsub"
	}
	if env.Meta == nil {
		env.Meta = map[string]string{}
	}
	if env.Meta["trace_id"] == "" {
		env.Meta["trace_id"] = telemetry.NewTraceID()
	}
	if env.Meta["request_id"] == "" {
		env.Meta["request_id"] = msg.ID
	}
	return env, nil
}

func (p *Plugin) dedupKey(env *bus.Envelope) string {
	if env == nil {
		return ""
	}
	key := env.Meta["dedup_key"]
	if strings.TrimSpace(key) == "" {
		key = env.ID
	}
	return key
}

func (p *Plugin) isDuplicate(key, envID string) bool {
	dup := p.dedup.SeenBefore(key, time.Now())
	if dup {
		p.log.Info("pubsub duplicate dropped", "dedup_key", key, "id", envID)
	}
	return dup
}

func (p *Plugin) handleFailure(ctx context.Context, msg *ipubsub.Message, cause error) {
	_ = p.adapter.Nack(msg, p.cfg.Retry)
	if ipubsub.ShouldRetry(p.cfg.Retry, msg.Attempts) {
		backoff := ipubsub.NextBackoff(p.cfg.Retry, msg.Attempts)
		p.log.Warn("pubsub nack retry", "id", msg.ID, "attempt", msg.Attempts, "backoff", backoff, "err", cause)
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := p.adapter.Publish(ctx, p.cfg.TopicIn, msg); err != nil {
			p.log.Warn("pubsub republish failed", "id", msg.ID, "err", err)
		}
		return
	}

	dlqTopic := p.cfg.TopicDLQ
	if dlqTopic == "" {
		dlqTopic = p.cfg.Retry.DLQTopic
	}
	if dlqTopic != "" {
		if err := p.adapter.Publish(ctx, dlqTopic, msg); err != nil {
			p.log.Warn("pubsub dlq publish failed", "topic", dlqTopic, "id", msg.ID, "err", err)
		} else {
			p.log.Warn("pubsub sent to dlq", "topic", dlqTopic, "id", msg.ID, "attempts", msg.Attempts)
		}
	}
	_ = p.adapter.Ack(msg)
}

func (p *Plugin) publishReplies(ctx context.Context) {
	replyCh := p.b.Subscribe(bus.ReplyKey("pubsub"))
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-replyCh:
			if !ok {
				return
			}
			if env == nil || env.Role != bus.RoleAssistant {
				continue
			}
			v2 := schema.DefaultV2(schema.BusToV2(env))
			if v2.SourceProtocol == "" {
				v2.SourceProtocol = "pubsub"
			}
			payload, err := json.Marshal(v2)
			if err != nil {
				p.log.Warn("pubsub marshal reply failed", "id", env.ID, "err", err)
				continue
			}
			msg := &ipubsub.Message{ID: env.ID, Topic: p.cfg.TopicOut, Payload: payload, Headers: map[string]string{"schema_version": schema.VersionV2}}
			if err := p.adapter.Publish(ctx, p.cfg.TopicOut, msg); err != nil {
				p.log.Warn("pubsub publish reply failed", "id", env.ID, "topic", p.cfg.TopicOut, "err", err)
			}
		}
	}
}

func parseConfig(cfg map[string]interface{}) Config {
	return Config{
		BrokerType:   plugincfg.StringDefault(cfg, "broker", "nats"),
		Endpoint:     plugincfg.StringDefault(cfg, "endpoint", "local"),
		TopicIn:      plugincfg.StringDefault(cfg, "topic_in", "krill.in"),
		TopicOut:     plugincfg.StringDefault(cfg, "topic_out", "krill.out"),
		TopicDLQ:     plugincfg.StringDefault(cfg, "topic_dlq", "krill.dlq"),
		Group:        plugincfg.StringDefault(cfg, "group", "krill"),
		DedupTTL:     time.Duration(plugincfg.IntDefault(cfg, "dedup_ttl_ms", 300000)) * time.Millisecond,
		AckTimeout:   time.Duration(plugincfg.IntDefault(cfg, "ack_timeout_ms", 5000)) * time.Millisecond,
		StrictSchema: plugincfg.Bool(cfg, "_strict_v2_validation") || plugincfg.Bool(cfg, "strict_v2_validation"),
		Retry: ipubsub.RetryPolicy{
			MaxRetries: plugincfg.IntDefault(cfg, "retry_max", 3),
			Backoff:    time.Duration(plugincfg.IntDefault(cfg, "retry_backoff_ms", 100)) * time.Millisecond,
			MaxBackoff: time.Duration(plugincfg.IntDefault(cfg, "retry_backoff_max_ms", 2000)) * time.Millisecond,
			DLQTopic:   plugincfg.StringDefault(cfg, "topic_dlq", "krill.dlq"),
		},
	}
}
