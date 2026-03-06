package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/scheduler"
	"github.com/krill/krill/internal/skill"
	builtins "github.com/krill/krill/plugins/skill/builtins"
)

type plannerExec struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
}

func (e *plannerExec) Execute(_ context.Context, argsJSON string) (string, error) {
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	if e.release != nil {
		<-e.release
	}
	return `{"ok":true,"args":` + argsJSON + `}`, nil
}

type plannerLLMTransport struct{}

func (plannerLLMTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &payload)
	last := payload.Messages[len(payload.Messages)-1].Content
	content := `{"mode":"direct","rationale":"simple conversational request","requested_capabilities":[],"capability_input":"","runtime_profile":"default","sandbox_profile":"balanced","release_channel":"","requested_network":false,"requested_paths":[],"requested_timeout_ms":0,"requires_approval":false,"missing_secrets":[]}`
	if strings.Contains(last, "Request: Scrivi del codice Python che stampi i primi 10 numeri di Fibonacci") {
		content = `{"mode":"planned","rationale":"deterministic computation should use governed code execution","requested_capabilities":["code_exec"],"capability_input":"{\"language\":\"python\",\"code\":\"print([0, 1, 1, 2, 3, 5, 8, 13, 21, 34])\",\"timeout_ms\":1000}","runtime_profile":"opencode","sandbox_profile":"balanced","release_channel":"stable","requested_network":false,"requested_paths":[],"requested_timeout_ms":1000,"requires_approval":false,"missing_secrets":[]}`
	}
	if strings.Contains(last, "Request: Create a plan") {
		content = `{"rationale":"single governed code execution step","steps":[{"goal":"compute fibonacci numbers deterministically","candidate_capabilities":["code_exec"],"capability_input":"{\"language\":\"python\",\"code\":\"print([0, 1, 1, 2, 3, 5, 8, 13, 21, 34])\",\"timeout_ms\":1000}","rationale":"run deterministic python snippet","timeout_ms":1000}]}`
	}
	respBody, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": content}}},
		"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 3, "total_tokens": 6},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}, nil
}

func TestCapabilitySelectionDeterministicAndFallbackOrdering(t *testing.T) {
	cfg := &config.Root{
		Planner: config.PlannerConfig{ProgressiveMode: "enforce", DefaultSandboxProfile: "balanced", DefaultRuntimeProfile: "default"},
		Capabilities: []config.CapabilityConfig{
			{Name: "candidate-api", Type: "backend_action", ReleaseChannel: "candidate", TrustScore: 70, RiskScore: 20},
			{Name: "stable-api", Type: "backend_action", ReleaseChannel: "stable", TrustScore: 70, RiskScore: 20},
			{Name: "deprecated-api", Type: "backend_action", ReleaseChannel: "deprecated", TrustScore: 99, RiskScore: 20},
		},
	}
	graph := newCapabilityGraph(cfg, nil)
	selected, fallbacks, decision := graph.selectCapability(
		[]string{"deprecated-api", "candidate-api", "stable-api"},
		&bus.Envelope{SourceProtocol: "http"},
		config.WorkflowConfig{},
		planStep{ID: "step-1"},
		&plan{ID: "plan-1"},
		intent{RuntimeProfile: "default", SandboxProfile: "balanced"},
		"",
		"enforce",
	)
	if !decision.Allow {
		t.Fatalf("expected allowed decision, got %+v", decision)
	}
	if selected.Name != "stable-api" {
		t.Fatalf("expected stable-api to win deterministic ranking, got %q", selected.Name)
	}
	if len(fallbacks) != 2 || fallbacks[0] != "candidate-api" || fallbacks[1] != "deprecated-api" {
		t.Fatalf("unexpected fallback ordering: %+v", fallbacks)
	}
}

func TestEvaluateCapabilityPolicySecurityDenials(t *testing.T) {
	decision := evaluateCapabilityPolicy(capability{
		Name:           "backend:query",
		Type:           capabilityBackend,
		ReleaseChannel: "stable",
		RuntimeProfile: "default",
		SandboxProfile: "strict",
	}, &bus.Envelope{SourceProtocol: "http"}, config.WorkflowConfig{}, intent{
		RuntimeProfile:     "default",
		SandboxProfile:     "strict",
		RequestedNetwork:   true,
		RequestedPaths:     []string{"../secret.txt"},
		RequestedTimeoutMs: 100,
	}, "", "enforce", 0)
	if decision.Allow || decision.Reason != "network_not_allowed" {
		t.Fatalf("expected network denial, got %+v", decision)
	}

	missingSecret := evaluateCapabilityPolicy(capability{
		Name:            "deploy:asset",
		Type:            capabilityDeployment,
		ReleaseChannel:  "candidate",
		RuntimeProfile:  "opencode",
		SandboxProfile:  "extended",
		RequiredSecrets: []string{"KRILL_TEST_SECRET_DOES_NOT_EXIST"},
	}, &bus.Envelope{SourceProtocol: "http"}, config.WorkflowConfig{}, intent{
		RuntimeProfile: "opencode",
		SandboxProfile: "extended",
	}, "", "enforce", 0)
	if missingSecret.Allow || missingSecret.Reason != "missing_secret" {
		t.Fatalf("expected missing secret denial, got %+v", missingSecret)
	}
}

func TestEvaluateCapabilityPolicyAdditionalBranches(t *testing.T) {
	baseCap := capability{
		Name:                 "notify:user",
		Type:                 capabilityNotification,
		ReleaseChannel:       "stable",
		RuntimeProfile:       "default",
		SandboxProfile:       "strict",
		Protocols:            stringSet([]string{"http"}),
		Workflows:            stringSet([]string{"wf-a"}),
		AllowedAgents:        stringSet([]string{"agent-a"}),
		MaxTimeoutMs:         50,
		MaxConcurrency:       1,
		RequiredApprovals:    1,
		AllowFilesystemWrite: false,
	}
	cases := []struct {
		name    string
		env     *bus.Envelope
		wf      config.WorkflowConfig
		intent  intent
		agent   string
		mode    string
		running int
		reason  string
		allowed bool
	}{
		{name: "protocol", env: &bus.Envelope{SourceProtocol: "telegram"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict"}, agent: "agent-a", mode: "enforce", reason: "protocol_not_allowed"},
		{name: "workflow", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-b"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict"}, agent: "agent-a", mode: "enforce", reason: "workflow_not_allowed"},
		{name: "agent", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict"}, agent: "agent-b", mode: "enforce", reason: "agent_not_allowed"},
		{name: "release", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict", ReleaseChannel: "candidate"}, agent: "agent-a", mode: "enforce", reason: "release_channel_not_pinned"},
		{name: "runtime", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "opencode", SandboxProfile: "strict"}, agent: "agent-a", mode: "enforce", reason: "runtime_profile_mismatch"},
		{name: "sandbox", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "balanced"}, agent: "agent-a", mode: "enforce", reason: "sandbox_profile_mismatch"},
		{name: "timeout", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict", RequestedTimeoutMs: 100}, agent: "agent-a", mode: "enforce", reason: "timeout_exceeds_capability_budget"},
		{name: "quota", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict"}, agent: "agent-a", mode: "enforce", running: 1, reason: "capability_quota_exceeded"},
		{name: "approval", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict"}, agent: "agent-a", mode: "enforce", reason: "approval_required"},
		{name: "monitor", env: &bus.Envelope{SourceProtocol: "http"}, wf: config.WorkflowConfig{ID: "wf-a"}, intent: intent{RuntimeProfile: "default", SandboxProfile: "strict"}, agent: "agent-b", mode: "monitor", allowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := evaluateCapabilityPolicy(baseCap, tc.env, tc.wf, tc.intent, tc.agent, tc.mode, tc.running)
			if tc.allowed {
				if !decision.Allow || !decision.Monitored {
					t.Fatalf("expected monitored allow, got %+v", decision)
				}
				return
			}
			if decision.Allow || decision.Reason != tc.reason {
				t.Fatalf("expected %s denial, got %+v", tc.reason, decision)
			}
		})
	}

	filesystemCap := capability{
		Name:                 "backend:op",
		Type:                 capabilityBackend,
		ReleaseChannel:       "stable",
		RuntimeProfile:       "default",
		SandboxProfile:       "strict",
		AllowFilesystemWrite: false,
	}
	filesystemDecision := evaluateCapabilityPolicy(filesystemCap, &bus.Envelope{SourceProtocol: "http"}, config.WorkflowConfig{}, intent{
		RuntimeProfile: "default",
		SandboxProfile: "strict",
		RequestedPaths: []string{"result.txt"},
	}, "", "enforce", 0)
	if filesystemDecision.Allow || filesystemDecision.Reason != "filesystem_write_not_allowed" {
		t.Fatalf("expected filesystem denial, got %+v", filesystemDecision)
	}

	pathDecision := evaluateCapabilityPolicy(capability{
		Name:                 "deploy:asset",
		Type:                 capabilityDeployment,
		ReleaseChannel:       "stable",
		RuntimeProfile:       "default",
		SandboxProfile:       "extended",
		AllowFilesystemWrite: true,
	}, &bus.Envelope{SourceProtocol: "http"}, config.WorkflowConfig{}, intent{
		RuntimeProfile: "default",
		SandboxProfile: "extended",
		RequestedPaths: []string{"../escape"},
	}, "", "enforce", 0)
	if pathDecision.Allow || pathDecision.Reason != "path_traversal_blocked" {
		t.Fatalf("expected path traversal denial, got %+v", pathDecision)
	}
}

func TestResolveIntentAndHelpers(t *testing.T) {
	orch, _, _, cancel := newPlannerOrch(t, nil, nil)
	defer cancel()
	wf := config.WorkflowConfig{ID: "wf-coop", OrchestrationMode: "cooperative"}
	in := orch.resolveIntent(&bus.Envelope{
		Text:           "deploy and notify",
		SourceProtocol: "scheduler",
		Meta: map[string]string{
			"workflow_id":       "echo",
			"capabilities":      "backend:api,notify:user",
			"runtime_profile":   "opencode",
			"sandbox_profile":   "extended",
			"release_channel":   "candidate",
			"network":           "true",
			"paths":             "tmp/output.txt",
			"missing_secret":    "ONE,TWO",
			"timeout_ms":        "1200",
			"requires_approval": "true",
		},
	}, wf)
	if !in.RequiresPlanner || !in.NeedCooperative || !in.Scheduled || !in.RequiresApproval || !in.RequestedNetwork {
		t.Fatalf("unexpected intent flags: %+v", in)
	}
	if len(in.RequestedCapabilities) != 2 || in.RuntimeProfile != "opencode" || in.SandboxProfile != "extended" || in.ReleaseChannel != "candidate" {
		t.Fatalf("unexpected intent payload: %+v", in)
	}
	if !hasUnsafePath([]string{"../bad"}) || hasUnsafePath([]string{"safe/path"}) {
		t.Fatal("unexpected unsafe path helper behavior")
	}
	if metaValue(&bus.Envelope{}, "missing") != "" {
		t.Fatal("expected empty meta value for missing map")
	}
	if !orch.shouldUsePlanner(&bus.Envelope{}, wf, in) {
		t.Fatal("expected planner path")
	}
}

func TestResolveIntentConversationalCodeRequestStaysOnDirectLoop(t *testing.T) {
	orch, _, _, cancel := newPlannerOrch(t, nil, nil)
	defer cancel()
	env := &bus.Envelope{
		Text:           "Use code_exec to run python code that prints fibonacci numbers",
		SourceProtocol: "http",
		Meta:           map[string]string{},
	}
	in := orch.resolveIntent(env, config.WorkflowConfig{ID: "default", OrchestrationMode: "single"})
	if in.RequiresPlanner {
		t.Fatalf("expected conversational code request to stay off planner path, got %+v", in)
	}
	if orch.shouldUsePlanner(env, config.WorkflowConfig{ID: "default", OrchestrationMode: "single"}, in) {
		t.Fatal("expected direct loop path for conversational code request")
	}
}

func TestCapabilityFallbacksAndExecuteSyntheticCapabilities(t *testing.T) {
	capabilities := []config.CapabilityConfig{
		{Name: "deploy:asset", Type: "deployment_action", Fallbacks: []string{"notify:user"}, RuntimeProfile: "opencode", SandboxProfile: "extended"},
		{Name: "notify:user", Type: "notification_action", RuntimeProfile: "default", SandboxProfile: "strict"},
	}
	orch, _, _, cancel := newPlannerOrch(t, nil, capabilities)
	defer cancel()
	fallbacks := orch.capabilityFallbacks("deploy:asset")
	if len(fallbacks) != 1 || fallbacks[0] != "notify:user" {
		t.Fatalf("unexpected fallbacks: %+v", fallbacks)
	}
	capCfg, ok := orch.capabilities.get("deploy:asset")
	if !ok {
		t.Fatal("expected synthetic capability")
	}
	output, artifacts, _, _, err := orch.executeCapability(context.Background(), &bus.Envelope{
		SourceProtocol: "http",
		Meta:           map[string]string{"capability_input": `{"app":"store"}`},
	}, config.WorkflowConfig{ID: "wf-deploy"}, capCfg, planStep{Goal: "deploy app"}, "input")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "pending://deployment") || len(artifacts) != 1 {
		t.Fatalf("unexpected synthetic execution output=%q artifacts=%+v", output, artifacts)
	}
	if hashArtifacts(nil) != "" || parentSpan(context.Background()) != "" {
		t.Fatal("expected empty helper outputs")
	}
	if defaultString("", "fallback") != "fallback" || defaultInt(0, 7) != 7 {
		t.Fatal("default helpers not behaving as expected")
	}
}

func TestRoutePlannedBuildFailurePublishesBlockedReply(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Root{
		Core:    config.CoreConfig{BusBuffer: 16, MaxClients: 2, MemoryBackend: "ram"},
		Planner: config.PlannerConfig{ProgressiveMode: "enforce", DefaultSandboxProfile: "balanced", DefaultRuntimeProfile: "default"},
		Workflows: []config.WorkflowConfig{{
			ID: "wf-coop", OrchestrationMode: "cooperative", OrgSchema: "missing-schema",
		}},
	}
	reg, err := skill.NewRegistry(nil, cfg.Core, logger)
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewLocal(8)
	orch, err := New(cfg, b, reg, memory.NewRAM(), nil, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	replyCh := b.Subscribe(bus.ReplyKey("http"))
	orch.routePlanned(ctx, &bus.Envelope{
		ID:             "env-1",
		ClientID:       "c",
		ThreadID:       "t",
		Role:           bus.RoleUser,
		Text:           "run cooperative",
		SourceProtocol: "http",
		Meta:           map[string]string{"request_id": "req-1", "trace_id": "trace-1"},
	}, cfg.Workflows[0], intent{RequiresPlanner: true, NeedCooperative: true})
	select {
	case reply := <-replyCh:
		if reply.Meta["status"] != "blocked" {
			t.Fatalf("expected blocked reply, got %+v", reply.Meta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected blocked planner reply")
	}
}

func TestPlannedSkillExecutionCollectsArtifactsAndAttestation(t *testing.T) {
	orch, b, ctx, cancel := newPlannerOrch(t, nil, nil)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reply := publishPlannerRequest(t, ctx, b, "echo", `{"message":"ciao"}`, nil)
	if reply.Meta["plan_status"] != "completed" {
		t.Fatalf("expected completed plan, got %+v", reply.Meta)
	}
	if reply.Meta["attestation_input"] == "" || reply.Meta["attestation_artifacts"] == "" {
		t.Fatalf("expected attestation hashes, got %+v", reply.Meta)
	}
	requestID := reply.Meta["request_id"]
	stored, ok := orch.PlanByRequest(requestID)
	if !ok {
		t.Fatalf("expected stored plan for request %q", requestID)
	}
	if len(stored.Artifacts) == 0 || stored.Artifacts[0].Hash == "" {
		t.Fatalf("expected collected artifacts, got %+v", stored.Artifacts)
	}
}

func TestSchedulerTriggeredPlanExecution(t *testing.T) {
	orch, b, ctx, cancel := newPlannerOrch(t, nil, nil)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	replyCh := b.Subscribe(bus.ReplyKey("scheduler"))
	if err := scheduler.BusExecutor(b)(ctx, scheduler.Trigger{
		ScheduleID:  "nightly",
		Target:      "echo",
		Payload:     "scheduled work",
		RunAt:       time.Now().UTC(),
		Attempt:     1,
		ClientID:    "sched-client",
		ThreadID:    "sched-thread",
		SessionMode: "persistent",
		Tenant:      "tenant-a",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-replyCh:
		if reply.Meta["plan_id"] == "" || reply.Meta["plan_status"] != "completed" {
			t.Fatalf("expected scheduler plan reply, got %+v", reply.Meta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected scheduler reply")
	}
}

func TestOpenCodeProfileExecutesExpectedWorkflow(t *testing.T) {
	orch, b, ctx, cancel := newPlannerOrch(t, func(r *skill.Registry) {
		builtins.Register(r, config.CoreConfig{})
	}, nil)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	args := `{"language":"bash","code":"echo open-code","timeout_ms":1000}`
	reply := publishPlannerRequest(t, ctx, b, "code_exec", args, map[string]string{
		"runtime_profile": "opencode",
		"sandbox_profile": "balanced",
	})
	if reply.Meta["plan_status"] != "completed" {
		t.Fatalf("expected completed opencode plan, got %+v", reply.Meta)
	}
	if !strings.Contains(reply.Text, "open-code") {
		t.Fatalf("expected code_exec output, got %q", reply.Text)
	}
}

func TestApprovalAndMissingSecretEscalation(t *testing.T) {
	capabilities := []config.CapabilityConfig{
		{
			Name:              "deploy:asset",
			Type:              "deployment_action",
			ReleaseChannel:    "candidate",
			RuntimeProfile:    "opencode",
			SandboxProfile:    "extended",
			RequiredApprovals: 1,
			RequiredSecrets:   []string{"KRILL_TEST_SECRET_DOES_NOT_EXIST"},
		},
	}
	orch, b, ctx, cancel := newPlannerOrch(t, nil, capabilities)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reply := publishPlannerRequest(t, ctx, b, "deploy:asset", `{"app":"shop"}`, map[string]string{
		"runtime_profile": "opencode",
		"sandbox_profile": "extended",
	})
	if reply.Meta["status"] != "blocked" {
		t.Fatalf("expected blocked reply, got %+v", reply.Meta)
	}
	if !strings.Contains(reply.Text, "blocked") {
		t.Fatalf("expected blocked text, got %q", reply.Text)
	}
	stored, ok := orch.PlanByRequest(reply.Meta["request_id"])
	if !ok || len(stored.Escalations) == 0 {
		t.Fatalf("expected stored escalations, got %+v ok=%v", stored.Escalations, ok)
	}
}

func TestConcurrentPlanExecutionHonorsCapabilityQuota(t *testing.T) {
	blocking := &plannerExec{started: make(chan struct{}, 2), release: make(chan struct{})}
	capabilities := []config.CapabilityConfig{
		{Name: "echo", Type: "skill", MaxConcurrency: 1, ReleaseChannel: "stable", SandboxProfile: "balanced", RuntimeProfile: "default"},
	}
	orch, b, ctx, cancel := newPlannerOrch(t, func(r *skill.Registry) {
		r.RegisterBuiltin("echo", "echo", blocking, nil)
	}, capabilities)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	replyCh := b.Subscribe(bus.ReplyKey("http"))
	if err := b.Publish(ctx, bus.InboundKey, plannerEnvelope("echo", `{}`, nil)); err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	if err := b.Publish(ctx, bus.InboundKey, plannerEnvelope("echo", `{}`, map[string]string{"request_id": "req-2"})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(blocking.release)

	var replies []*bus.Envelope
	deadline := time.After(2 * time.Second)
	for len(replies) < 2 {
		select {
		case reply := <-replyCh:
			replies = append(replies, reply)
		case <-deadline:
			t.Fatalf("expected two replies, got %d", len(replies))
		}
	}
	foundBlocked := false
	for _, reply := range replies {
		if reply.Meta["status"] == "blocked" && reply.Meta["reason"] == "capability_quota_exceeded" {
			foundBlocked = true
		}
	}
	if !foundBlocked {
		var metas []map[string]string
		for _, reply := range replies {
			metas = append(metas, reply.Meta)
		}
		t.Fatalf("expected one blocked quota reply, got %+v", metas)
	}
}

func newPlannerOrch(t *testing.T, register func(*skill.Registry), capabilities []config.CapabilityConfig) (*Orch, bus.Bus, context.Context, context.CancelFunc) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Root{
		Core: config.CoreConfig{BusBuffer: 64, MaxClients: 8, MemoryBackend: "ram", MemoryWindow: 16},
		Planner: config.PlannerConfig{
			ProgressiveMode:       "enforce",
			DefaultSandboxProfile: "balanced",
			DefaultRuntimeProfile: "default",
		},
		Capabilities: capabilities,
		Agents:       []config.AgentConfig{{Name: "default", LLM: "mock", MaxTurns: 2}},
	}
	b := bus.NewLocal(64)
	reg, err := skill.NewRegistry(nil, cfg.Core, logger)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterBuiltin("echo", "echo", &plannerExec{}, nil)
	if register != nil {
		register(reg)
	}
	orch, err := New(cfg, b, reg, memory.NewRAM(), nil, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	return orch, b, runCtx, cancel
}

func newPlannerOrchWithLLM(t *testing.T, register func(*skill.Registry), capabilities []config.CapabilityConfig) (*Orch, bus.Bus, context.Context, context.CancelFunc) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Root{
		Core: config.CoreConfig{BusBuffer: 64, MaxClients: 8, MemoryBackend: "ram", MemoryWindow: 16},
		Planner: config.PlannerConfig{
			ProgressiveMode:       "enforce",
			DefaultSandboxProfile: "balanced",
			DefaultRuntimeProfile: "default",
		},
		Capabilities: capabilities,
		Agents:       []config.AgentConfig{{Name: "default", LLM: "mock", MaxTurns: 2}},
		LLM: config.LLMPool{
			Default:  "mock",
			Backends: []config.LLMConfig{{Name: "mock", BaseURL: "http://planner.mock", APIKey: "test", Model: "mock-model", MaxTokens: 512}},
		},
	}
	b := bus.NewLocal(64)
	reg, err := skill.NewRegistry(nil, cfg.Core, logger)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterBuiltin("echo", "echo", &plannerExec{}, nil)
	if register != nil {
		register(reg)
	}
	pool, err := llm.NewPool(cfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	orch, err := New(cfg, b, reg, memory.NewRAM(), pool, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	return orch, b, runCtx, cancel
}

func publishPlannerRequest(t *testing.T, ctx context.Context, b bus.Bus, capabilityName, capabilityArgs string, meta map[string]string) *bus.Envelope {
	t.Helper()
	replyCh := b.Subscribe(bus.ReplyKey("http"))
	if err := b.Publish(ctx, bus.InboundKey, plannerEnvelope(capabilityName, capabilityArgs, meta)); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-replyCh:
		return reply
	case <-time.After(2 * time.Second):
		t.Fatal("expected planner reply")
		return nil
	}
}

func plannerEnvelope(capabilityName, capabilityArgs string, meta map[string]string) *bus.Envelope {
	envMeta := map[string]string{
		"trace_id":          "trace-" + capabilityName,
		"request_id":        uuid.NewString(),
		"target_capability": capabilityName,
		"capability_input":  capabilityArgs,
	}
	for key, value := range meta {
		envMeta[key] = value
	}
	return &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       "planner-client",
		ThreadID:       "planner-thread",
		Role:           bus.RoleUser,
		Text:           "run " + capabilityName,
		SourceProtocol: "http",
		Meta:           envMeta,
		CreatedAt:      time.Now().UTC(),
	}
}

func TestStoredPlanJSONRoundTrip(t *testing.T) {
	orch, b, ctx, cancel := newPlannerOrch(t, nil, nil)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reply := publishPlannerRequest(t, ctx, b, "echo", `{"message":"json"}`, nil)
	var stored plan
	if err := json.Unmarshal([]byte(reply.Meta["plan"]), &stored); err != nil {
		t.Fatalf("expected valid plan json, got %v", err)
	}
	if stored.ID == "" || len(stored.Steps) == 0 {
		t.Fatalf("expected populated plan, got %+v", stored)
	}
}

func TestResolveIntentLLMClassifierSupportsMultilingualPlannedRequest(t *testing.T) {
	restore := llm.SetClientFactoryForTests(func() *http.Client {
		return &http.Client{Transport: plannerLLMTransport{}, Timeout: 3 * time.Second}
	})
	defer restore()
	orch, _, _, cancel := newPlannerOrchWithLLM(t, nil, nil)
	defer cancel()
	in := orch.resolveIntent(&bus.Envelope{
		Text:           "Scrivi del codice Python che stampi i primi 10 numeri di Fibonacci",
		SourceProtocol: "http",
		Meta:           map[string]string{},
	}, config.WorkflowConfig{ID: "default", OrchestrationMode: "single"})
	if !in.RequiresPlanner {
		t.Fatalf("expected LLM classifier to choose planner path, got %+v", in)
	}
	if len(in.RequestedCapabilities) != 1 || in.RequestedCapabilities[0] != "code_exec" {
		t.Fatalf("unexpected inferred capabilities: %+v", in.RequestedCapabilities)
	}
	if !strings.Contains(in.CapabilityInput, `"language":"python"`) {
		t.Fatalf("expected structured capability input, got %q", in.CapabilityInput)
	}
}

func TestBuildPlanWithLLMProducesStructuredArgs(t *testing.T) {
	restore := llm.SetClientFactoryForTests(func() *http.Client {
		return &http.Client{Transport: plannerLLMTransport{}, Timeout: 3 * time.Second}
	})
	defer restore()
	orch, _, _, cancel := newPlannerOrchWithLLM(t, func(r *skill.Registry) {
		builtins.Register(r, config.CoreConfig{})
	}, nil)
	defer cancel()
	env := &bus.Envelope{
		ID:             "env-plan",
		ClientID:       "c1",
		ThreadID:       "t1",
		Role:           bus.RoleUser,
		Text:           "Create a plan",
		SourceProtocol: "http",
		Meta:           map[string]string{"request_id": "req-plan", "trace_id": "trace-plan"},
	}
	p, err := orch.buildPlan(env, config.WorkflowConfig{ID: "default"}, intent{
		Goal:            env.Text,
		RequiresPlanner: true,
		RuntimeProfile:  "opencode",
		SandboxProfile:  "balanced",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 1 || p.Steps[0].CandidateCapabilities[0] != "code_exec" {
		t.Fatalf("unexpected plan steps: %+v", p.Steps)
	}
	if !strings.Contains(p.Steps[0].CapabilityInput, `"language":"python"`) {
		t.Fatalf("expected structured args in plan step, got %q", p.Steps[0].CapabilityInput)
	}
}
