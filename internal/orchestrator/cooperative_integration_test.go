package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/ingress"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/skill"
)

func TestIntegration_CooperativeWorkflowEndToEnd(t *testing.T) {
	orch, b, ctx, cancel := newCoopTestOrch(t, cooperativeTestConfig(2, 5000), false)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	reply := publishAndWaitReply(t, ctx, b, map[string]string{"workflow_id": "wf-coop"})
	if reply.Meta["workflow_id"] != "wf-coop" {
		t.Fatalf("unexpected workflow_id: %q", reply.Meta["workflow_id"])
	}
	if reply.Meta["trace_id"] != "trace-1" {
		t.Fatalf("trace continuity broken, got %q", reply.Meta["trace_id"])
	}
	if !strings.Contains(reply.Text, "synth-agent") {
		t.Fatalf("expected synthesizer output, got: %q", reply.Text)
	}
	if reply.Meta["handoff_chain"] == "" {
		t.Fatal("missing handoff_chain metadata")
	}
}

func TestIntegration_CooperativeWorkflowPolicyViolationBlocked(t *testing.T) {
	orch, b, ctx, cancel := newCoopTestOrch(t, cooperativeTestConfig(1, 5000), false)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	reply := publishAndWaitReply(t, ctx, b, map[string]string{"workflow_id": "wf-coop"})
	if reply.Meta["status"] != "blocked" {
		t.Fatalf("expected blocked status, got %q", reply.Meta["status"])
	}
	if !strings.Contains(reply.Text, "cooperative workflow blocked") {
		t.Fatalf("expected blocked text, got %q", reply.Text)
	}
}

func TestIntegration_DeclarativeOrgSchemaWithTwoSpecialists(t *testing.T) {
	cfg := cooperativeTestConfig(4, 5000)
	cfg.OrgSchemas[0].Roles = []config.OrgRoleConfig{
		{Name: "router", Kind: "router", Agent: "router-agent"},
		{Name: "spec_a", Kind: "specialist", Agent: "specialist-agent"},
		{Name: "spec_b", Kind: "specialist", Agent: "specialist-b-agent"},
		{Name: "synth", Kind: "synthesizer", Agent: "synth-agent"},
	}
	cfg.OrgSchemas[0].HandoffRules = []config.OrgHandoffRuleConfig{
		{From: "router", To: []string{"spec_a", "spec_b"}},
		{From: "spec_a", To: []string{"spec_b", "synth"}},
		{From: "spec_a", To: []string{"synth"}},
		{From: "spec_b", To: []string{"synth"}},
	}
	orch, b, ctx, cancel := newCoopTestOrch(t, cfg, false)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	reply := publishAndWaitReply(t, ctx, b, map[string]string{"workflow_id": "wf-coop"})
	var chain []HandoffEvent
	if err := json.Unmarshal([]byte(reply.Meta["handoff_chain"]), &chain); err != nil {
		t.Fatalf("invalid handoff chain json: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("expected 3 handoffs (router->specA->specB->synth), got %d", len(chain))
	}
	if !strings.Contains(reply.Text, "synth-agent") {
		t.Fatalf("expected synthesizer output, got: %q", reply.Text)
	}
}

func TestIntegration_CooperativeFailureInjectionLLMError(t *testing.T) {
	orch, b, ctx, cancel := newCoopTestOrch(t, cooperativeTestConfig(4, 5000), true)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	reply := publishAndWaitReply(t, ctx, b, map[string]string{"workflow_id": "wf-coop"})
	if reply.Meta["status"] != "blocked" {
		t.Fatalf("expected blocked status on llm failure, got %q", reply.Meta["status"])
	}
}

func TestIntegration_NonRegressionSingleAgentModeUnchanged(t *testing.T) {
	cfg := cooperativeTestConfig(4, 5000)
	cfg.Workflows = []config.WorkflowConfig{{ID: "wf-single", OrchestrationMode: "single"}}
	orch, b, ctx, cancel := newCoopTestOrch(t, cfg, false)
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	reply := publishAndWaitReply(t, ctx, b, map[string]string{"workflow_id": "wf-single"})
	if reply.Meta["agent"] != "router-agent" {
		t.Fatalf("expected single-agent router-agent reply, got %q", reply.Meta["agent"])
	}
	if reply.Meta["handoff_chain"] != "" {
		t.Fatalf("single-agent mode should not emit handoff chain: %+v", reply.Meta)
	}
}

func newCoopTestOrch(t *testing.T, cfg *config.Root, failSpecialist bool) (*Orch, bus.Bus, context.Context, context.CancelFunc) {
	t.Helper()
	restore := llm.SetClientFactoryForTests(func() *http.Client {
		return &http.Client{Transport: coopTransport{failSpecialist: failSpecialist}, Timeout: 3 * time.Second}
	})
	t.Cleanup(restore)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := bus.NewLocal(64)
	mem := memory.NewRAM()
	skillReg, err := skill.NewRegistry(nil, cfg.Core, logger)
	if err != nil {
		t.Fatal(err)
	}
	llmPool, err := llm.NewPool(cfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	orch, err := New(cfg, b, skillReg, mem, llmPool, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return orch, b, ctx, cancel
}

func cooperativeTestConfig(maxHops int, timeoutMs int) *config.Root {
	return &config.Root{
		Core: config.CoreConfig{BusBuffer: 64, MaxClients: 8, SandboxType: "exec", SkillTimeoutMs: 1000, MemoryBackend: "ram", MemoryWindow: 32},
		LLM: config.LLMPool{
			Default:  "mock",
			Backends: []config.LLMConfig{{Name: "mock", BaseURL: "http://mock.local", APIKey: "k", Model: "m", MaxTokens: 64}},
		},
		Agents: []config.AgentConfig{
			{Name: "router-agent", LLM: "mock", MaxTurns: 2, SystemPrompt: "router"},
			{Name: "specialist-agent", LLM: "mock", MaxTurns: 2, SystemPrompt: "specialist"},
			{Name: "specialist-b-agent", LLM: "mock", MaxTurns: 2, SystemPrompt: "specialist b"},
			{Name: "synth-agent", LLM: "mock", MaxTurns: 2, SystemPrompt: "synth"},
		},
		OrgSchemas: []config.OrgSchemaConfig{{
			SchemaID: "schema-1",
			Version:  "v1",
			Roles: []config.OrgRoleConfig{
				{Name: "router", Kind: "router", Agent: "router-agent"},
				{Name: "specialist", Kind: "specialist", Agent: "specialist-agent"},
				{Name: "synth", Kind: "synthesizer", Agent: "synth-agent"},
			},
			HandoffRules: []config.OrgHandoffRuleConfig{
				{From: "router", To: []string{"specialist"}},
				{From: "specialist", To: []string{"synth"}},
			},
		}},
		Workflows: []config.WorkflowConfig{{
			ID:                "wf-coop",
			OrchestrationMode: "cooperative",
			OrgSchema:         "schema-1",
			Policy: config.WorkflowPolicyConfig{
				MaxHops:       maxHops,
				StepTimeoutMs: timeoutMs,
				TokenBudget:   32,
			},
		}},
	}
}

type coopTransport struct {
	failSpecialist bool
}

func (t coopTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	body, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(body, &payload)
	content := ""
	if len(payload.Messages) > 0 {
		content = payload.Messages[len(payload.Messages)-1].Content
	}
	systemPrompt := ""
	for _, m := range payload.Messages {
		if m.Role == "system" {
			systemPrompt = strings.ToLower(m.Content)
			break
		}
	}
	if t.failSpecialist && strings.Contains(systemPrompt, "specialist") && !strings.Contains(systemPrompt, "specialist b") {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"injected"}`)),
		}, nil
	}

	agentName := "agent"
	switch {
	case strings.Contains(systemPrompt, "router"):
		agentName = "router-agent"
	case strings.Contains(systemPrompt, "specialist b"):
		agentName = "specialist-b-agent"
	case strings.Contains(systemPrompt, "specialist"):
		agentName = "specialist-agent"
	case strings.Contains(systemPrompt, "synth"):
		agentName = "synth-agent"
	case strings.Contains(content, "user prompt"):
		agentName = "router-agent"
	}
	respBody, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": agentName + " output"}}},
		"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 2, "total_tokens": 4},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}, nil
}

func publishAndWaitReply(t *testing.T, ctx context.Context, b bus.Bus, meta map[string]string) *bus.Envelope {
	t.Helper()
	replyCh := b.Subscribe(bus.ReplyKey("http"))
	norm := ingress.NewNormalizer(false)
	envMeta := map[string]string{"trace_id": "trace-1", "request_id": "req-1"}
	for k, v := range meta {
		envMeta[k] = v
	}
	err := norm.PublishInbound(ctx, b, &bus.Envelope{
		ID:             "env-1",
		ClientID:       "http:c1",
		ThreadID:       "http:c1",
		Role:           bus.RoleUser,
		Text:           "user prompt",
		SourceProtocol: "http",
		Meta:           envMeta,
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-replyCh:
		return reply
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting reply")
		return nil
	}
}
