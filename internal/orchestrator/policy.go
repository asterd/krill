package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"github.com/krill/krill/config"
)

type HandoffPolicy struct {
	MaxHops      int
	AllowedPairs map[string]map[string]struct{}
	StepTimeout  time.Duration
	TokenBudget  int
}

type HandoffCommand struct {
	OriginAgent     string
	TargetAgent     string
	Reason          string
	Hop             int
	EstimatedTokens int
	Elapsed         time.Duration
}

type HandoffEvent struct {
	OriginAgent string    `json:"origin_agent"`
	TargetAgent string    `json:"target_agent"`
	Reason      string    `json:"reason"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type WorkflowState struct {
	WorkflowID   string         `json:"workflow_id"`
	Hop          int            `json:"hop"`
	TokensUsed   int            `json:"tokens_used"`
	HandoffChain []HandoffEvent `json:"handoff_chain"`
}

type PolicyDecision struct {
	Allow  bool
	Reason string
}

func (p HandoffPolicy) Evaluate(cmd HandoffCommand, state WorkflowState) PolicyDecision {
	if p.MaxHops > 0 && cmd.Hop > p.MaxHops {
		return PolicyDecision{Allow: false, Reason: fmt.Sprintf("max_hops_exceeded(%d)", p.MaxHops)}
	}
	if len(p.AllowedPairs) > 0 && cmd.OriginAgent != "" && cmd.TargetAgent != "" {
		if !pairAllowed(p.AllowedPairs, cmd.OriginAgent, cmd.TargetAgent) {
			return PolicyDecision{Allow: false, Reason: "pair_not_allowed"}
		}
	}
	if p.StepTimeout > 0 && cmd.Elapsed > p.StepTimeout {
		return PolicyDecision{Allow: false, Reason: "step_timeout_exceeded"}
	}
	if p.TokenBudget > 0 && cmd.EstimatedTokens > 0 && state.TokensUsed+cmd.EstimatedTokens > p.TokenBudget {
		return PolicyDecision{Allow: false, Reason: "token_budget_exceeded"}
	}
	return PolicyDecision{Allow: true}
}

func policyFromWorkflow(wf config.WorkflowConfig, defaults map[string]map[string]struct{}) HandoffPolicy {
	pairs := make(map[string]map[string]struct{}, len(defaults))
	for from, tos := range defaults {
		pairs[from] = make(map[string]struct{}, len(tos))
		for to := range tos {
			pairs[from][to] = struct{}{}
		}
	}
	for _, pair := range wf.Policy.AllowedPairs {
		from, to, ok := splitPair(pair)
		if !ok {
			continue
		}
		allowPair(pairs, from, to)
	}
	return HandoffPolicy{
		MaxHops:      wf.Policy.MaxHops,
		AllowedPairs: pairs,
		StepTimeout:  time.Duration(wf.Policy.StepTimeoutMs) * time.Millisecond,
		TokenBudget:  wf.Policy.TokenBudget,
	}
}

func splitPair(v string) (string, string, bool) {
	parts := strings.Split(v, "->")
	if len(parts) != 2 {
		return "", "", false
	}
	from := strings.TrimSpace(parts[0])
	to := strings.TrimSpace(parts[1])
	if from == "" || to == "" {
		return "", "", false
	}
	return from, to, true
}

func isCooperativeWorkflow(wf config.WorkflowConfig) bool {
	mode := strings.ToLower(strings.TrimSpace(wf.OrchestrationMode))
	if mode == "" {
		mode = "single"
	}
	return mode == "cooperative"
}
