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

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/controlplane"
	"github.com/krill/krill/internal/core"
	"github.com/krill/krill/internal/ingress"
	"github.com/krill/krill/internal/orchestrator"
	"github.com/krill/krill/internal/plugincfg"
	"github.com/krill/krill/internal/scheduler"
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
	cp     controlPlaneAuth

	// waiters: requestID -> channel that receives the correlated reply envelope.
	mu      sync.RWMutex
	waiters map[string]chan *bus.Envelope
	norm    *ingress.Normalizer
}

type controlPlaneAuth struct {
	Enabled bool
	Tokens  map[string]string
}

// New constructs the HTTP protocol plugin from raw config.
func New(cfg map[string]interface{}) (*Plugin, error) {
	addr := plugincfg.String(cfg, "addr")
	if addr == "" {
		addr = ":8080"
	}
	return &Plugin{
		addr:    addr,
		apiKey:  plugincfg.String(cfg, "api_key"),
		cp:      parseControlPlaneAuth(cfg),
		waiters: make(map[string]chan *bus.Envelope),
		norm:    ingress.NewNormalizer(plugincfg.Bool(cfg, "_strict_v2_validation") || plugincfg.Bool(cfg, "strict_v2_validation")),
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
	mux.HandleFunc("/v1/control/", p.handleControlPlane)

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

func parseControlPlaneAuth(cfg map[string]interface{}) controlPlaneAuth {
	raw, ok := cfg["control_plane"].(map[string]interface{})
	if !ok {
		return controlPlaneAuth{}
	}
	auth := controlPlaneAuth{
		Enabled: plugincfg.Bool(raw, "enabled"),
		Tokens:  map[string]string{},
	}
	if tokens, ok := raw["tokens"].(map[string]interface{}); ok {
		for role, value := range tokens {
			if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
				auth.Tokens[strings.ToLower(strings.TrimSpace(role))] = s
			}
		}
	}
	return auth
}

func (p *Plugin) handleControlPlane(w http.ResponseWriter, r *http.Request) {
	manager := controlplane.Current()
	if manager == nil || !p.cp.Enabled {
		http.NotFound(w, r)
		return
	}
	role, actor, ok := p.authorizeControlPlane(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/control")
	if path == "" || path == "/" {
		writeJSON(w, http.StatusOK, manager.Diagnostics())
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "control_plane": true})
	case r.Method == http.MethodGet && path == "/readiness":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "audit_ok": manager.VerifyAudit()})
	case r.Method == http.MethodGet && path == "/diagnostics":
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		writeJSON(w, http.StatusOK, manager.Diagnostics())
	case strings.HasPrefix(path, "/planner/config"):
		p.handlePlannerConfig(w, r, manager, role, actor)
	case strings.HasPrefix(path, "/capabilities"):
		p.handleCapabilities(w, r, manager, role, actor, path)
	case strings.HasPrefix(path, "/schedules"):
		p.handleSchedules(w, r, manager, role, actor, path)
	case strings.HasPrefix(path, "/plans/"):
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		requestID := strings.TrimPrefix(path, "/plans/")
		plan, found := manager.Plan(requestID)
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, plan)
	case strings.HasPrefix(path, "/workflows/"):
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		requestID := strings.TrimPrefix(path, "/workflows/")
		state, found := manager.Workflow(requestID)
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, state)
	case strings.HasPrefix(path, "/artifacts/"):
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		requestID := strings.TrimPrefix(path, "/artifacts/")
		plan, found := manager.Plan(requestID)
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"request_id": requestID, "artifacts": plan.Artifacts, "attestation": plan.Attestation})
	case strings.HasPrefix(path, "/external-actions/"):
		p.handleExternalActions(w, r, manager, role, actor, path)
	case strings.HasPrefix(path, "/sessions"):
		p.handleSessions(w, r, manager, role, path)
	case strings.HasPrefix(path, "/secrets"):
		p.handleSecrets(w, r, manager, role, actor)
	case strings.HasPrefix(path, "/audit"):
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"audit": manager.AuditEvents(), "tamper_ok": manager.VerifyAudit()})
	case strings.HasPrefix(path, "/rollouts"):
		p.handleRollouts(w, r, manager, role, actor, path)
	default:
		http.NotFound(w, r)
	}
}

func (p *Plugin) handlePlannerConfig(w http.ResponseWriter, r *http.Request, manager *controlplane.Manager, role, actor string) {
	switch r.Method {
	case http.MethodGet:
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		writeJSON(w, http.StatusOK, manager.PlannerConfig())
	case http.MethodPut:
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		var req struct {
			DryRun bool `json:"dry_run"`
			orchestrator.PlannerConfigSnapshot
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		rollout, current := manager.UpdatePlannerConfig(actor, role, req.PlannerConfigSnapshot, req.DryRun)
		writeJSON(w, http.StatusOK, map[string]any{"rollout": rollout, "planner": current})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *Plugin) handleCapabilities(w http.ResponseWriter, r *http.Request, manager *controlplane.Manager, role, actor, path string) {
	switch {
	case r.Method == http.MethodGet && path == "/capabilities":
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		writeJSON(w, http.StatusOK, manager.Capabilities())
	case r.Method == http.MethodPost && path == "/capabilities":
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		var req struct {
			DryRun bool `json:"dry_run"`
			config.CapabilityConfig
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		rollout, capability := manager.ApplyCapability(actor, role, req.CapabilityConfig, req.DryRun)
		writeJSON(w, http.StatusOK, map[string]any{"rollout": rollout, "capability": capability})
	default:
		http.NotFound(w, r)
	}
}

func (p *Plugin) handleSchedules(w http.ResponseWriter, r *http.Request, manager *controlplane.Manager, role, actor, path string) {
	switch {
	case r.Method == http.MethodGet && path == "/schedules":
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		writeJSON(w, http.StatusOK, manager.ScheduleStatuses())
	case r.Method == http.MethodPost && path == "/schedules":
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		var req struct {
			DryRun bool `json:"dry_run"`
			config.ScheduleConfig
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		rollout, status, err := manager.ApplySchedule(actor, role, req.ScheduleConfig, req.DryRun)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rollout": rollout, "schedule": status})
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/history"):
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/schedules/"), "/history")
		writeJSON(w, http.StatusOK, manager.ScheduleHistory(id))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/pause"):
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/schedules/"), "/pause")
		status, err := manager.PauseSchedule(actor, role, id)
		handleScheduleMutation(w, status, err)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/resume"):
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/schedules/"), "/resume")
		status, err := manager.ResumeSchedule(actor, role, id)
		handleScheduleMutation(w, status, err)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel"):
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/schedules/"), "/cancel")
		status, err := manager.CancelSchedule(actor, role, id)
		handleScheduleMutation(w, status, err)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/trigger"):
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/schedules/"), "/trigger")
		if err := manager.TriggerSchedule(actor, role, id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "triggered"})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/schedules/"):
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		id := strings.TrimPrefix(path, "/schedules/")
		status, found := manager.ScheduleStatus(id)
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, status)
	default:
		http.NotFound(w, r)
	}
}

func handleScheduleMutation(w http.ResponseWriter, status scheduler.ScheduleStatus, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (p *Plugin) handleExternalActions(w http.ResponseWriter, r *http.Request, manager *controlplane.Manager, role, actor, path string) {
	requestID := strings.TrimPrefix(path, "/external-actions/")
	switch r.Method {
	case http.MethodGet:
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		plan, found := manager.Plan(requestID)
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"request_id":    requestID,
			"need_approval": plan.NeedApproval,
			"escalations":   plan.Escalations,
			"approval":      manager.Approval(requestID),
		})
	case http.MethodPost:
		if !requireControlPlaneRole(w, role, "operator") {
			return
		}
		var req struct {
			Status  string `json:"status"`
			Comment string `json:"comment"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		writeJSON(w, http.StatusOK, manager.SetApproval(actor, role, requestID, req.Status, req.Comment))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *Plugin) handleSessions(w http.ResponseWriter, r *http.Request, manager *controlplane.Manager, role, path string) {
	if !requireControlPlaneRole(w, role, "viewer") {
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/sessions":
		writeJSON(w, http.StatusOK, manager.Sessions())
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/sessions/"):
		id := strings.TrimPrefix(path, "/sessions/")
		session, found := manager.Session(id)
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, session)
	default:
		http.NotFound(w, r)
	}
}

func (p *Plugin) handleSecrets(w http.ResponseWriter, r *http.Request, manager *controlplane.Manager, role, actor string) {
	switch r.Method {
	case http.MethodGet:
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		writeJSON(w, http.StatusOK, manager.SecretRevisions())
	case http.MethodPost:
		if !requireControlPlaneRole(w, role, "admin") {
			return
		}
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		writeJSON(w, http.StatusOK, manager.RotateSecret(actor, role, req.Name, req.Value))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *Plugin) handleRollouts(w http.ResponseWriter, r *http.Request, manager *controlplane.Manager, role, actor, path string) {
	switch {
	case r.Method == http.MethodGet && path == "/rollouts":
		if !requireControlPlaneRole(w, role, "viewer") {
			return
		}
		writeJSON(w, http.StatusOK, manager.Rollouts())
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/rollback"):
		if !requireControlPlaneRole(w, role, "admin") {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/rollouts/"), "/rollback")
		record, err := manager.Rollback(actor, role, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, record)
	default:
		http.NotFound(w, r)
	}
}

func (p *Plugin) authorizeControlPlane(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	for role, expected := range p.cp.Tokens {
		if token != "" && token == expected {
			actor := r.Header.Get("X-Control-Plane-Actor")
			if actor == "" {
				actor = role
			}
			return role, actor, true
		}
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return "", "", false
}

func requireControlPlaneRole(w http.ResponseWriter, actual, minimum string) bool {
	order := map[string]int{"viewer": 1, "operator": 2, "admin": 3}
	if order[actual] < order[minimum] {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
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
