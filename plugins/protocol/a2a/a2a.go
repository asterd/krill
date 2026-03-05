package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/core"
	"github.com/krill/krill/internal/schema"
	"github.com/krill/krill/internal/telemetry"
)

func init() {
	core.Global().RegisterProtocol("a2a", func(cfg map[string]interface{}) (core.Protocol, error) {
		return New(cfg)
	})
}

type Plugin struct {
	addr string
	path string
	srv  *http.Server
	b    bus.Bus
	log  *slog.Logger
}

func New(cfg map[string]interface{}) (*Plugin, error) {
	p := strVal(cfg, "path")
	if p == "" {
		p = "/a2a/v1/envelope"
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("a2a path must start with '/'")
	}
	addr := strVal(cfg, "addr")
	if addr == "" {
		addr = ":8091"
	}
	return &Plugin{addr: addr, path: p}, nil
}

func (p *Plugin) Name() string { return "a2a" }

func (p *Plugin) Start(ctx context.Context, b bus.Bus, log *slog.Logger) error {
	p.b = b
	p.log = log
	go p.relayReplies(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc(p.path, p.handleEnvelope)
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "krill-a2a"})
	})

	p.srv = &http.Server{
		Addr:              p.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		if err := p.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("a2a listen error", "err", err)
		}
	}()
	log.Info("a2a plugin listening", "addr", p.addr, "path", p.path)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	if p.srv == nil {
		return nil
	}
	return p.srv.Shutdown(ctx)
}

func (p *Plugin) relayReplies(ctx context.Context) {
	replyCh := p.b.Subscribe(bus.ReplyKey("a2a"))
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
			p.log.Info("a2a reply", "client", env.ClientID, "workflow_id", env.Meta["workflow_id"], "bytes", len(env.Text))
		}
	}
}

func (p *Plugin) handleEnvelope(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	v2, err := schema.NormalizeJSON(data, schema.NormalizeOptions{StrictV2Validation: true})
	if err != nil {
		http.Error(w, "invalid a2a envelope", http.StatusBadRequest)
		return
	}
	if v2.Meta == nil {
		v2.Meta = map[string]string{}
	}
	if v2.Meta["trace_id"] == "" {
		v2.Meta["trace_id"] = extractTraceID(r)
	}
	if v2.Meta["request_id"] == "" {
		v2.Meta["request_id"] = uuid.NewString()
	}
	if v2.Meta["origin_agent"] == "" {
		v2.Meta["origin_agent"] = strHeader(r, "X-A2A-Origin-Agent")
	}
	if v2.Meta["target_agent"] == "" {
		v2.Meta["target_agent"] = strHeader(r, "X-A2A-Target-Agent")
	}
	if v2.Meta["handoff_reason"] == "" {
		v2.Meta["handoff_reason"] = strHeader(r, "X-A2A-Handoff-Reason")
	}

	env := schema.V2ToBus(v2)
	env.SourceProtocol = "a2a"
	if env.Meta == nil {
		env.Meta = map[string]string{}
	}
	env.Meta["source_plugin"] = "a2a"
	span := telemetry.StartSpan(p.log, env.Meta["trace_id"], env.Meta["ingress_span"], "a2a.ingress",
		"client", env.ClientID,
		"workflow_id", v2.WorkflowID,
	)
	env.Meta["ingress_span"] = span.SpanID()
	err = p.b.Publish(r.Context(), bus.InboundKey, env)
	span.End(err)
	if err != nil {
		http.Error(w, "publish failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "accepted",
		"id":          env.ID,
		"client_id":   env.ClientID,
		"workflow_id": v2.WorkflowID,
	})
}

func extractTraceID(r *http.Request) string {
	if tp := strings.TrimSpace(r.Header.Get("traceparent")); tp != "" {
		parts := strings.Split(tp, "-")
		if len(parts) >= 4 && len(parts[1]) == 32 {
			return parts[1]
		}
	}
	return telemetry.NewTraceID()
}

func strVal(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	v, _ := m[k].(string)
	return strings.TrimSpace(v)
}

func strHeader(r *http.Request, key string) string {
	return strings.TrimSpace(r.Header.Get(key))
}
