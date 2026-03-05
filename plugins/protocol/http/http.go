// Package http — HTTP/REST + SSE protocol plugin.
//
// Reply routing: subscribes to bus.ReplyKey("http") and filters by ClientID.
// This means the bus publishes replies to "__reply__:http" and this plugin
// delivers them to the correct waiting HTTP client via SSE.
package httpproto

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/core"
	"github.com/krill/krill/internal/ingress"
	"github.com/krill/krill/internal/telemetry"
)

func init() {
	core.Global().RegisterProtocol("http", func(cfg map[string]interface{}) (core.Protocol, error) {
		return New(cfg)
	})
}

// Plugin is the HTTP protocol plugin.
type Plugin struct {
	addr   string
	apiKey string
	srv    *http.Server
	b      bus.Bus
	log    *slog.Logger

	// waiters: requestID -> channel that receives the correlated reply envelope.
	mu      sync.RWMutex
	waiters map[string]chan *bus.Envelope
	norm    *ingress.Normalizer
}

func New(cfg map[string]interface{}) (*Plugin, error) {
	addr, _ := cfg["addr"].(string)
	if addr == "" {
		addr = ":8080"
	}
	return &Plugin{
		addr:    addr,
		apiKey:  strVal(cfg, "api_key"),
		waiters: make(map[string]chan *bus.Envelope),
		norm:    ingress.NewNormalizer(boolVal(cfg, "_strict_v2_validation") || boolVal(cfg, "strict_v2_validation")),
	}, nil
}

func (p *Plugin) Name() string { return "http" }

func (p *Plugin) Start(ctx context.Context, b bus.Bus, log *slog.Logger) error {
	p.b = b
	p.log = log

	// Subscribe to reply channel and fan out to waiting HTTP clients
	go p.relayReplies(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat", p.auth(p.handleChat))
	mux.HandleFunc("/v1/health", handleHealth)

	p.srv = &http.Server{
		Addr:              p.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
	}
	go func() {
		if err := p.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http listen error", "err", err)
		}
	}()
	log.Info("http plugin listening", "addr", p.addr)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	return p.srv.Shutdown(ctx)
}

// relayReplies reads from the reply channel and unblocks waiting HTTP handlers.
func (p *Plugin) relayReplies(ctx context.Context) {
	ch := p.b.Subscribe(bus.ReplyKey("http"))
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-ch:
			if !ok {
				return
			}
			if env.Role != bus.RoleAssistant {
				continue
			}
			// Deliver to the specific waiting client
			p.mu.RLock()
			waiter, ok := p.waiters[env.Meta["request_id"]]
			p.mu.RUnlock()
			if ok {
				select {
				case waiter <- env:
				default:
				}
			}
		}
	}
}

func (p *Plugin) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := uuid.NewString()
	traceID := traceIDFromRequest(r)
	rootSpan := telemetry.StartSpan(p.log, traceID, "", "http.chat.request",
		"request_id", requestID,
		"path", r.URL.Path,
		"method", r.Method,
	)
	defer func() {
		rootSpan.End(nil, "request_id", requestID)
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ClientID string `json:"client_id"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if req.ClientID == "" {
		req.ClientID = uuid.NewString()
	}
	if req.ThreadID == "" {
		req.ThreadID = req.ClientID
	}
	p.log.Info("http chat received",
		"trace_id", rootSpan.TraceID(),
		"span_id", rootSpan.SpanID(),
		"request_id", requestID,
		"client_id", req.ClientID,
		"thread_id", req.ThreadID,
		"message_bytes", len(req.Message),
	)

	// Register a waiter BEFORE publishing (avoid race)
	replyCh := make(chan *bus.Envelope, 1)
	p.mu.Lock()
	p.waiters[requestID] = replyCh
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, requestID)
		p.mu.Unlock()
	}()

	// Publish inbound message to orchestrator
	env := &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       req.ClientID,
		ThreadID:       req.ThreadID,
		Role:           bus.RoleUser,
		Text:           req.Message,
		SourceProtocol: "http",
		Meta: map[string]string{
			"trace_id":      rootSpan.TraceID(),
			"request_id":    requestID,
			"ingress_span":  rootSpan.SpanID(),
			"source_plugin": "http",
		},
		CreatedAt: time.Now(),
	}
	if err := p.norm.PublishInbound(r.Context(), p.b, env); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p.log.Info("http chat published inbound",
		"trace_id", rootSpan.TraceID(),
		"request_id", requestID,
		"client_id", req.ClientID,
	)

	// Wait for reply with timeout
	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()

	// SSE streaming
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	select {
	case <-r.Context().Done():
		return
	case <-timeout.C:
		p.log.Warn("http chat timeout",
			"trace_id", rootSpan.TraceID(),
			"request_id", requestID,
			"client_id", req.ClientID,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		fmt.Fprintf(w, "data: {\"error\":\"timeout\"}\n\n")
		return
	case reply := <-replyCh:
		data, _ := json.Marshal(map[string]interface{}{
			"client_id":         reply.ClientID,
			"text":              reply.Text,
			"agent":             reply.Meta["agent"],
			"request_id":        reply.Meta["request_id"],
			"trace_id":          reply.Meta["trace_id"],
			"tokens_prompt":     reply.Meta["tokens_prompt"],
			"tokens_completion": reply.Meta["tokens_completion"],
			"tokens_total":      reply.Meta["tokens_total"],
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		p.log.Info("http chat response sent",
			"trace_id", rootSpan.TraceID(),
			"request_id", requestID,
			"client_id", reply.ClientID,
			"agent", reply.Meta["agent"],
			"duration_ms", time.Since(start).Milliseconds(),
			"tokens_prompt", reply.Meta["tokens_prompt"],
			"tokens_completion", reply.Meta["tokens_completion"],
			"tokens_total", reply.Meta["tokens_total"],
		)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "krill"})
}

func (p *Plugin) auth(next http.HandlerFunc) http.HandlerFunc {
	if p.apiKey == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+p.apiKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func strVal(m map[string]interface{}, k string) string {
	v, _ := m[k].(string)
	return v
}

func boolVal(m map[string]interface{}, k string) bool {
	v, ok := m[k]
	if !ok {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	default:
		return false
	}
}

func traceIDFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Trace-ID")); v != "" {
		return v
	}
	if tp := strings.TrimSpace(r.Header.Get("traceparent")); tp != "" {
		parts := strings.Split(tp, "-")
		if len(parts) >= 4 && len(parts[1]) == 32 {
			return parts[1]
		}
	}
	return telemetry.NewTraceID()
}
