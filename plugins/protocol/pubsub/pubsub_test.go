package pubsub

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/plugincfg"
	ipubsub "github.com/krill/krill/internal/pubsub"
	"github.com/krill/krill/internal/schema"
)

type mockAdapter struct {
	connectErr   error
	subscribeErr error
	pubErr       error
	msgCh        chan *ipubsub.Message
	publishN     int
}

func (m *mockAdapter) Connect(context.Context) error { return m.connectErr }
func (m *mockAdapter) Subscribe(context.Context, string, string) (<-chan *ipubsub.Message, error) {
	if m.subscribeErr != nil {
		return nil, m.subscribeErr
	}
	if m.msgCh == nil {
		m.msgCh = make(chan *ipubsub.Message, 8)
	}
	return m.msgCh, nil
}
func (m *mockAdapter) Publish(context.Context, string, *ipubsub.Message) error {
	m.publishN++
	return m.pubErr
}
func (m *mockAdapter) Ack(*ipubsub.Message) error                       { return nil }
func (m *mockAdapter) Nack(*ipubsub.Message, ipubsub.RetryPolicy) error { return nil }
func (m *mockAdapter) Close() error                                     { return nil }

func TestPlugin_DedupAndReplyRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapter := ipubsub.NewNATSAdapter(ipubsub.Config{BrokerType: "nats", Endpoint: "pubsub-test-dedup"})
	p := NewWithAdapter(Config{
		BrokerType: "nats",
		TopicIn:    "in",
		TopicOut:   "out",
		TopicDLQ:   "dlq",
		Group:      "g1",
		Retry:      ipubsub.RetryPolicy{MaxRetries: 1, Backoff: time.Millisecond},
		AckTimeout: 200 * time.Millisecond,
		DedupTTL:   5 * time.Second,
	}, adapter)

	b := bus.NewLocal(32)
	if err := p.Start(ctx, b, slog.Default()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop(context.Background()) }()

	inboundCh := b.Subscribe(bus.InboundKey)
	outCh, err := adapter.Subscribe(ctx, "out", "")
	if err != nil {
		t.Fatal(err)
	}

	payload := mustV2Payload(t, schema.EnvelopeV2{
		SchemaVersion:  schema.VersionV2,
		ID:             "m1",
		ClientID:       "c1",
		ThreadID:       "t1",
		Tenant:         "default",
		WorkflowID:     "default",
		SourceProtocol: "pubsub",
		Role:           "user",
		Text:           "hello",
		Meta:           map[string]string{"dedup_key": "dupk"},
		CreatedAt:      time.Now().UTC(),
	})

	inMsg := &ipubsub.Message{ID: "m1", Payload: payload}
	if err := adapter.Publish(ctx, "in", inMsg); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Publish(ctx, "in", inMsg); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-inboundCh:
		if got.Text != "hello" {
			t.Fatalf("unexpected text %q", got.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("expected inbound message")
	}

	select {
	case <-inboundCh:
		t.Fatal("duplicate message should have been dropped")
	case <-time.After(120 * time.Millisecond):
	}

	reply := &bus.Envelope{
		ID:             "r1",
		ClientID:       "c1",
		ThreadID:       "t1",
		Role:           bus.RoleAssistant,
		Text:           "done",
		SourceProtocol: "pubsub",
		Meta:           map[string]string{},
		CreatedAt:      time.Now().UTC(),
	}
	if err := b.Publish(ctx, bus.ReplyKey("pubsub"), reply); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-outCh:
		var out schema.EnvelopeV2
		if err := json.Unmarshal(got.Payload, &out); err != nil {
			t.Fatal(err)
		}
		if out.Text != "done" || out.Role != "assistant" {
			t.Fatalf("unexpected outbound payload: %+v", out)
		}
	case <-time.After(time.Second):
		t.Fatal("expected outbound reply")
	}
}

func TestPlugin_StartErrors(t *testing.T) {
	b := bus.NewLocal(8)
	p := NewWithAdapter(Config{TopicIn: "in", TopicOut: "out"}, &mockAdapter{connectErr: context.Canceled})
	if err := p.Start(context.Background(), b, slog.Default()); err == nil {
		t.Fatal("expected connect error")
	}

	p2 := NewWithAdapter(Config{TopicIn: "in", TopicOut: "out"}, &mockAdapter{subscribeErr: context.DeadlineExceeded})
	if err := p2.Start(context.Background(), b, slog.Default()); err == nil {
		t.Fatal("expected subscribe error")
	}
}

func TestPlugin_FailureInjection_RetryAndDLQ(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint := "pubsub-test-failure"
	adapter := ipubsub.NewNATSAdapter(ipubsub.Config{BrokerType: "nats", Endpoint: endpoint})
	broker := ipubsub.NewInMemoryBroker()
	_ = broker

	p := NewWithAdapter(Config{
		BrokerType: "nats",
		TopicIn:    "fin",
		TopicOut:   "fout",
		TopicDLQ:   "fdlq",
		Group:      "g1",
		Retry:      ipubsub.RetryPolicy{MaxRetries: 1, Backoff: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond, DLQTopic: "fdlq"},
		AckTimeout: 20 * time.Millisecond,
		DedupTTL:   time.Second,
	}, adapter)
	p.processFn = func(context.Context, *bus.Envelope) error {
		time.Sleep(40 * time.Millisecond)
		return nil
	}

	b := bus.NewLocal(8)
	if err := p.Start(ctx, b, slog.Default()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop(context.Background()) }()

	dlqCh, err := adapter.Subscribe(ctx, "fdlq", "")
	if err != nil {
		t.Fatal(err)
	}

	v2 := schema.DefaultV2(schema.EnvelopeV2{
		ID:             "ft-1",
		ClientID:       "c1",
		ThreadID:       "t1",
		Tenant:         "default",
		WorkflowID:     "default",
		SourceProtocol: "pubsub",
		Role:           "user",
		Text:           "timeout",
		CreatedAt:      time.Now().UTC(),
	})
	payload := mustV2Payload(t, v2)
	if err := adapter.Publish(ctx, "fin", &ipubsub.Message{ID: "ft-1", Payload: payload}); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-dlqCh:
		if got.ID != "ft-1" {
			t.Fatalf("expected dlq message ft-1, got %s", got.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected message in dlq after retries exhausted")
	}
}

func TestIntegration_PubsubIngressToOrchestratorReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapter := ipubsub.NewNATSAdapter(ipubsub.Config{BrokerType: "nats", Endpoint: "pubsub-int-e2e"})
	p := NewWithAdapter(Config{
		BrokerType: "nats",
		TopicIn:    "krill.in",
		TopicOut:   "krill.out",
		TopicDLQ:   "krill.dlq",
		Group:      "krill",
		Retry:      ipubsub.RetryPolicy{MaxRetries: 2, Backoff: 10 * time.Millisecond},
		AckTimeout: time.Second,
		DedupTTL:   time.Minute,
	}, adapter)
	b := bus.NewLocal(64)
	if err := p.Start(ctx, b, slog.Default()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop(context.Background()) }()

	// Minimal orchestrator stub: inbound user -> assistant reply.
	go func() {
		in := b.Subscribe(bus.InboundKey)
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-in:
				reply := &bus.Envelope{
					ID:             "reply-" + env.ID,
					ClientID:       env.ClientID,
					ThreadID:       env.ThreadID,
					Role:           bus.RoleAssistant,
					Text:           "processed:" + env.Text,
					SourceProtocol: "pubsub",
					Meta:           map[string]string{},
					CreatedAt:      time.Now().UTC(),
				}
				_ = b.Publish(ctx, bus.ReplyKey("pubsub"), reply)
			}
		}
	}()

	outCh, err := adapter.Subscribe(ctx, "krill.out", "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	inPayload := mustV2Payload(t, schema.DefaultV2(schema.EnvelopeV2{
		ID:             "int-1",
		ClientID:       "client-int",
		ThreadID:       "thread-int",
		Tenant:         "default",
		WorkflowID:     "default",
		SourceProtocol: "pubsub",
		Role:           "user",
		Text:           "ping",
		CreatedAt:      time.Now().UTC(),
	}))
	if err := adapter.Publish(ctx, "krill.in", &ipubsub.Message{ID: "int-1", Payload: inPayload}); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-outCh:
		var v2 schema.EnvelopeV2
		if err := json.Unmarshal(msg.Payload, &v2); err != nil {
			t.Fatal(err)
		}
		if v2.Role != "assistant" {
			t.Fatalf("expected assistant role, got %s", v2.Role)
		}
		if v2.Text == "" {
			t.Fatal("expected non-empty assistant text")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timeout waiting pubsub e2e reply")
	}
}

func TestIntegration_RestartRecoveryWithPersistentMemory(t *testing.T) {
	memPath := filepath.Join(t.TempDir(), "memory.json")

	runOnce := func(messageID string) {
		t.Helper()
		store, err := memory.NewFile(memPath)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		adapter := ipubsub.NewNATSAdapter(ipubsub.Config{BrokerType: "nats", Endpoint: "pubsub-int-restart"})
		p := NewWithAdapter(Config{
			BrokerType: "nats",
			TopicIn:    "restart.in",
			TopicOut:   "restart.out",
			TopicDLQ:   "restart.dlq",
			Group:      "krill",
			Retry:      ipubsub.RetryPolicy{MaxRetries: 2, Backoff: 10 * time.Millisecond},
			AckTimeout: 400 * time.Millisecond,
			DedupTTL:   time.Minute,
		}, adapter)
		b := bus.NewLocal(64)
		if err := p.Start(ctx, b, slog.Default()); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Stop(context.Background()) }()

		go func() {
			in := b.Subscribe(bus.InboundKey)
			for {
				select {
				case <-ctx.Done():
					return
				case env := <-in:
					store.Append(env.ClientID, env.ThreadID, llm.Message{Role: "user", Content: env.Text})
					_ = b.Publish(ctx, bus.ReplyKey("pubsub"), &bus.Envelope{
						ID:             "reply-" + env.ID,
						ClientID:       env.ClientID,
						ThreadID:       env.ThreadID,
						Role:           bus.RoleAssistant,
						Text:           "ok",
						SourceProtocol: "pubsub",
						Meta:           map[string]string{},
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}()

		outCh, err := adapter.Subscribe(ctx, "restart.out", "")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
		payload := mustV2Payload(t, schema.DefaultV2(schema.EnvelopeV2{
			ID:             messageID,
			ClientID:       "c-restart",
			ThreadID:       "t-restart",
			Tenant:         "default",
			WorkflowID:     "default",
			SourceProtocol: "pubsub",
			Role:           "user",
			Text:           "remember me",
			CreatedAt:      time.Now().UTC(),
		}))
		if err := adapter.Publish(ctx, "restart.in", &ipubsub.Message{ID: messageID, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-outCh:
		case <-time.After(4 * time.Second):
			t.Fatal("timeout waiting output on restart integration run")
		}
	}

	runOnce("restart-1")

	store, err := memory.NewFile(memPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(store.Get("c-restart", "t-restart", 100)); got < 1 {
		t.Fatalf("expected persisted history >=1 message after first run, got %d", got)
	}

	runOnce("restart-2")
	store2, err := memory.NewFile(memPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(store2.Get("c-restart", "t-restart", 100)); got < 2 {
		t.Fatalf("expected history to survive restart and grow, got %d", got)
	}
}

func mustV2Payload(t *testing.T, v2 schema.EnvelopeV2) []byte {
	t.Helper()
	payload, err := json.Marshal(v2)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestParseConfigAndHelpers(t *testing.T) {
	cfg := map[string]interface{}{
		"broker":                "redis_streams",
		"endpoint":              "redis://x",
		"topic_in":              "tin",
		"topic_out":             "tout",
		"topic_dlq":             "tdlq",
		"group":                 "g1",
		"dedup_ttl_ms":          "1000",
		"ack_timeout_ms":        float64(1500),
		"retry_max":             int64(4),
		"retry_backoff_ms":      10,
		"retry_backoff_max_ms":  20,
		"strict_v2_validation":  "true",
		"_strict_v2_validation": false,
	}
	got := parseConfig(cfg)
	if got.BrokerType != "redis_streams" || got.Endpoint != "redis://x" {
		t.Fatalf("unexpected parsed broker config: %+v", got)
	}
	if got.Retry.MaxRetries != 4 || got.Retry.Backoff <= 0 || got.Retry.MaxBackoff <= 0 {
		t.Fatalf("unexpected retry config: %+v", got.Retry)
	}
	if !got.StrictSchema {
		t.Fatal("expected strict schema true")
	}
	if plugincfg.StringDefault(nil, "x", "d") != "d" || plugincfg.IntDefault(nil, "x", 9) != 9 || plugincfg.Bool(nil, "x") {
		t.Fatal("helper defaults mismatch")
	}
}

func TestNewAndName_Defaults(t *testing.T) {
	p, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "pubsub" {
		t.Fatalf("unexpected name: %s", p.Name())
	}
	if p.cfg.BrokerType == "" || p.cfg.TopicIn == "" || p.cfg.TopicOut == "" {
		t.Fatalf("expected defaults to be set: %+v", p.cfg)
	}
}

func TestDecodeInboundErrorsAndNewValidation(t *testing.T) {
	adapter := ipubsub.NewNATSAdapter(ipubsub.Config{BrokerType: "nats", Endpoint: "decode"})
	p := NewWithAdapter(Config{
		BrokerType: "nats", TopicIn: "in", TopicOut: "out", TopicDLQ: "dlq", Group: "g", AckTimeout: time.Second,
	}, adapter)
	if _, err := p.decodeInbound(&ipubsub.Message{ID: "bad", Payload: []byte("{invalid")}); err == nil {
		t.Fatal("expected decode error")
	}
	p2, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatalf("expected defaults-based config, got err: %v", err)
	}
	if p2.cfg.TopicIn == "" || p2.cfg.TopicOut == "" {
		t.Fatal("expected default topic values")
	}
}

func TestHandleFailure_NoRetryNoDLQ(t *testing.T) {
	adapter := &mockAdapter{}
	p := NewWithAdapter(Config{
		TopicIn:    "in",
		TopicOut:   "out",
		TopicDLQ:   "",
		Retry:      ipubsub.RetryPolicy{MaxRetries: 0},
		AckTimeout: time.Second,
	}, adapter)
	p.log = slog.Default()
	p.handleFailure(context.Background(), &ipubsub.Message{ID: "x"}, context.Canceled)
}

func TestProcessWithTimeout_ErrorsAndCancel(t *testing.T) {
	adapter := &mockAdapter{}
	p := NewWithAdapter(Config{
		TopicIn:    "in",
		TopicOut:   "out",
		Retry:      ipubsub.RetryPolicy{MaxRetries: 1, Backoff: time.Millisecond},
		AckTimeout: 20 * time.Millisecond,
	}, adapter)
	p.b = bus.NewLocal(8)
	p.log = slog.Default()

	if err := p.processWithTimeout(context.Background(), nil); err == nil {
		t.Fatal("expected nil message error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := &ipubsub.Message{ID: "x", Payload: mustV2Payload(t, schema.DefaultV2(schema.EnvelopeV2{
		ID:             "x",
		ClientID:       "c",
		ThreadID:       "t",
		Tenant:         "default",
		WorkflowID:     "default",
		SourceProtocol: "pubsub",
		Role:           "user",
		Text:           "hi",
		CreatedAt:      time.Now().UTC(),
	}))}
	if err := p.processWithTimeout(ctx, msg); err == nil {
		t.Fatal("expected canceled context error")
	}
}

func TestHandleFailure_PublishErrorsAndPublishRepliesBranches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapter := &mockAdapter{pubErr: context.Canceled}
	p := NewWithAdapter(Config{
		TopicIn:    "in",
		TopicOut:   "out",
		TopicDLQ:   "dlq",
		Retry:      ipubsub.RetryPolicy{MaxRetries: 1, Backoff: time.Millisecond, DLQTopic: "dlq"},
		AckTimeout: 50 * time.Millisecond,
	}, adapter)
	b := bus.NewLocal(16)
	p.b = b
	p.log = slog.Default()

	// Retry path with republish error.
	p.handleFailure(ctx, &ipubsub.Message{ID: "m1"}, context.Canceled)
	// Exhausted retries path with dlq publish error.
	p.handleFailure(ctx, &ipubsub.Message{ID: "m2", Attempts: 2}, context.Canceled)

	go p.publishReplies(ctx)
	_ = b.Publish(ctx, bus.ReplyKey("pubsub"), &bus.Envelope{Role: bus.RoleUser}) // ignored
	_ = b.Publish(ctx, bus.ReplyKey("pubsub"), nil)                               // ignored nil
	b.Unsubscribe(bus.ReplyKey("pubsub"))                                         // close channel branch
}

func TestHandleFailure_ContextCancelledSkipsRepublish(t *testing.T) {
	adapter := &mockAdapter{}
	p := NewWithAdapter(Config{
		TopicIn:    "in",
		TopicOut:   "out",
		Retry:      ipubsub.RetryPolicy{MaxRetries: 1, Backoff: 50 * time.Millisecond},
		AckTimeout: 50 * time.Millisecond,
	}, adapter)
	p.log = slog.Default()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.handleFailure(ctx, &ipubsub.Message{ID: "cancelled"}, context.Canceled)
	if adapter.publishN != 0 {
		t.Fatalf("expected cancelled retry to skip republish, got %d publishes", adapter.publishN)
	}
}
