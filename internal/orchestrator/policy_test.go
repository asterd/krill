package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/krill/krill/config"
)

func TestHandoffPolicyEvaluator(t *testing.T) {
	policy := HandoffPolicy{
		MaxHops:     2,
		StepTimeout: 200 * time.Millisecond,
		TokenBudget: 10,
		AllowedPairs: map[string]map[string]struct{}{
			"router": {"specialist": {}},
		},
	}
	state := WorkflowState{TokensUsed: 4}

	if decision := policy.Evaluate(HandoffCommand{OriginAgent: "router", TargetAgent: "specialist", Hop: 1}, state); !decision.Allow {
		t.Fatalf("expected allowed handoff, got %s", decision.Reason)
	}
	if decision := policy.Evaluate(HandoffCommand{OriginAgent: "router", TargetAgent: "other", Hop: 1}, state); decision.Allow || decision.Reason != "pair_not_allowed" {
		t.Fatalf("expected pair_not_allowed, got %+v", decision)
	}
	if decision := policy.Evaluate(HandoffCommand{OriginAgent: "router", TargetAgent: "specialist", Hop: 3}, state); decision.Allow || !strings.Contains(decision.Reason, "max_hops_exceeded") {
		t.Fatalf("expected max_hops_exceeded, got %+v", decision)
	}
	if decision := policy.Evaluate(HandoffCommand{Elapsed: 500 * time.Millisecond}, state); decision.Allow || decision.Reason != "step_timeout_exceeded" {
		t.Fatalf("expected timeout denial, got %+v", decision)
	}
	if decision := policy.Evaluate(HandoffCommand{EstimatedTokens: 7}, state); decision.Allow || decision.Reason != "token_budget_exceeded" {
		t.Fatalf("expected budget denial, got %+v", decision)
	}
}

func TestCompileOrgSchema(t *testing.T) {
	schema := config.OrgSchemaConfig{
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
	}
	compiled, err := compileOrgSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.RoleOrder) != 3 {
		t.Fatalf("expected 3 roles in pipeline, got %d", len(compiled.RoleOrder))
	}
	if !pairAllowed(compiled.AllowedAgentPairs, "router-agent", "specialist-agent") {
		t.Fatalf("expected allowed router->specialist pair")
	}
}

func TestPolicyHelpers(t *testing.T) {
	if from, to, ok := splitPair("router->specialist"); !ok || from != "router" || to != "specialist" {
		t.Fatalf("unexpected splitPair result: %q %q %v", from, to, ok)
	}
	if _, _, ok := splitPair("broken"); ok {
		t.Fatal("expected invalid pair format")
	}

	policy := policyFromWorkflow(config.WorkflowConfig{
		OrchestrationMode: "cooperative",
		Policy: config.WorkflowPolicyConfig{
			MaxHops:       3,
			StepTimeoutMs: 1200,
			TokenBudget:   500,
			AllowedPairs:  []string{"a->b"},
		},
	}, map[string]map[string]struct{}{"x": {"y": {}}})
	if policy.MaxHops != 3 || policy.StepTimeout != 1200*time.Millisecond || policy.TokenBudget != 500 {
		t.Fatalf("unexpected policy conversion: %+v", policy)
	}
	if !pairAllowed(policy.AllowedPairs, "a", "b") || !pairAllowed(policy.AllowedPairs, "x", "y") {
		t.Fatalf("expected merged allowed pairs: %+v", policy.AllowedPairs)
	}
	if !isCooperativeWorkflow(config.WorkflowConfig{OrchestrationMode: "cooperative"}) {
		t.Fatal("expected cooperative mode")
	}
	if isCooperativeWorkflow(config.WorkflowConfig{OrchestrationMode: "single"}) {
		t.Fatal("single mode should not be cooperative")
	}
}
