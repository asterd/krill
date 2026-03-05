// Package webhook — generic inbound webhook plugin (Slack, Discord, WhatsApp, etc.)
// Outbound replies: for webhooks (fire-and-forget inbound), the reply
// goes to a configurable reply_url or is discarded. Most webhooks are async.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/krill/krill/internal/ingress"
	"github.com/krill/krill/internal/telemetry"
)

func init() {
	core.Global().RegisterProtocol("webhook", func(cfg map[string]interface{}) (core.Protocol, error) {
		return New(cfg)
	})
}

type Plugin struct {
	addr        string
	path        string
	secret      string
	verifyMode  string
	clientField string
	textField   string
	srv         *http.Server
	b           bus.Bus
	log         *slog.Logger
	norm        *ingress.Normalizer
}

func New(cfg map[string]interface{}) (*Plugin, error) {
	return &Plugin{
		addr:        str(cfg, "addr", ":8081"),
		path:        str(cfg, "path", "/webhook"),
		secret:      str(cfg, "secret", ""),
		verifyMode:  str(cfg, "verify_mode", "none"),
		clientField: str(cfg, "client_field", "user.id"),
		textField:   str(cfg, "text_field", "text"),
		norm:        ingress.NewNormalizer(boolVal(cfg, "_strict_v2_validation") || boolVal(cfg, "strict_v2_validation")),
	}, nil
}

func (p *Plugin) Name() string { return "webhook" }

func (p *Plugin) Start(ctx context.Context, b bus.Bus, log *slog.Logger) error {
	p.b = b
	p.log = log
	go p.relay(ctx) // start reply relay (for webhooks that expect a response body)

	mux := http.NewServeMux()
	mux.HandleFunc(p.path, p.handle)
	p.srv = &http.Server{
		Addr:              p.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		if err := p.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("webhook listen error", "err", err)
		}
	}()
	log.Info("webhook listening", "addr", p.addr, "path", p.path)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error { return p.srv.Shutdown(ctx) }

func (p *Plugin) relay(ctx context.Context) {
	ch := p.b.Subscribe(bus.ReplyKey("webhook"))
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
			// For async webhooks, log the reply (or forward to a callback URL in Meta)
			p.log.Info("webhook reply", "client", env.ClientID, "text_len", len(env.Text))
		}
	}
}

func (p *Plugin) handle(w http.ResponseWriter, r *http.Request) {
	traceID := telemetry.NewTraceID()
	span := telemetry.StartSpan(p.log, traceID, "", "webhook.receive", "path", p.path)
	defer span.End(nil, "path", p.path)

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	if p.secret != "" && p.verifyMode == "hmac" {
		if !verifyHMAC(body, p.secret, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Discord ping challenge
	if p.verifyMode == "discord" {
		if t, _ := payload["type"].(float64); t == 1 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]int{"type": 1})
			return
		}
	}
	// Slack URL verification challenge
	if cb, _ := payload["type"].(string); cb == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"challenge": payload["challenge"]})
		return
	}

	clientID := dotGet(payload, p.clientField)
	if clientID == "" {
		clientID = "wh:" + uuid.NewString()
	}
	text := dotGet(payload, p.textField)
	if text == "" {
		text = string(body)
	}

	env := &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       clientID,
		ThreadID:       clientID,
		Role:           bus.RoleUser,
		Text:           text,
		SourceProtocol: "webhook",
		Meta: map[string]string{
			"raw":          string(body),
			"trace_id":     traceID,
			"request_id":   uuid.NewString(),
			"ingress_span": span.SpanID(),
		},
		CreatedAt: time.Now(),
	}
	p.norm.PublishInbound(r.Context(), p.b, env) //nolint:errcheck
	w.WriteHeader(http.StatusAccepted)
}

func dotGet(obj map[string]interface{}, path string) string {
	parts := strings.SplitN(path, ".", 2)
	val, ok := obj[parts[0]]
	if !ok {
		return ""
	}
	if len(parts) == 1 {
		return fmt.Sprint(val)
	}
	if nested, ok := val.(map[string]interface{}); ok {
		return dotGet(nested, parts[1])
	}
	return ""
}

func verifyHMAC(body []byte, secret, sig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(sig))
}

func str(m map[string]interface{}, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
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
