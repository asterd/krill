package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/telemetry"
)

type compiledOrgSchema struct {
	ID                string
	RoleOrder         []config.OrgRoleConfig
	AllowedAgentPairs map[string]map[string]struct{}
}

type tokenTotals struct {
	prompt     int
	completion int
	total      int
}

func (o *Orch) workflowFor(env *bus.Envelope) (config.WorkflowConfig, bool) {
	if env == nil {
		return config.WorkflowConfig{}, false
	}
	workflowID := "default"
	if env.Meta != nil {
		if wf := strings.TrimSpace(env.Meta["workflow_id"]); wf != "" {
			workflowID = wf
		}
	}
	for _, wf := range o.cfg.Workflows {
		if strings.TrimSpace(wf.ID) == workflowID {
			wf.OrchestrationMode = strings.ToLower(strings.TrimSpace(wf.OrchestrationMode))
			if wf.OrchestrationMode == "" {
				wf.OrchestrationMode = "single"
			}
			return wf, true
		}
	}
	return config.WorkflowConfig{}, false
}

func (o *Orch) routeCooperative(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig) {
	traceID := ""
	parentSpan := ""
	if env.Meta != nil {
		traceID = env.Meta["trace_id"]
		parentSpan = env.Meta["consume_span"]
	}
	routeSpan := telemetry.StartSpan(nil, traceID, parentSpan, "orchestrator.route_cooperative",
		"workflow_id", wf.ID,
		"client", env.ClientID,
	)
	defer routeSpan.End(nil, "workflow_id", wf.ID, "client", env.ClientID)

	select {
	case o.sem <- struct{}{}:
	default:
		o.log.Warn("max_clients reached, dropping cooperative workflow",
			"client", env.ClientID,
			"workflow_id", wf.ID,
		)
		return
	}

	go func() {
		defer func() {
			<-o.sem
		}()
		if err := o.executeCooperative(ctx, env, wf, routeSpan.SpanID()); err != nil {
			o.log.Warn("cooperative workflow failed", "workflow_id", wf.ID, "err", err)
			o.publishCooperativeFailure(ctx, env, wf, err)
		}
	}()
}

func (o *Orch) executeCooperative(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig, parentSpanID string) error {
	orgSchema, err := o.findOrgSchema(wf.OrgSchema)
	if err != nil {
		return err
	}
	compiled, err := compileOrgSchema(*orgSchema)
	if err != nil {
		return err
	}

	traceID := env.Meta["trace_id"]
	if traceID == "" {
		traceID = telemetry.NewTraceID()
	}
	requestID := env.Meta["request_id"]
	if requestID == "" {
		requestID = env.ID
	}

	policy := policyFromWorkflow(wf, compiled.AllowedAgentPairs)
	state := WorkflowState{WorkflowID: wf.ID}
	input := env.Text
	output := env.Text
	currentParent := parentSpanID
	totals := tokenTotals{}

	for idx, role := range compiled.RoleOrder {
		if idx > 0 {
			prev := compiled.RoleOrder[idx-1]
			cmd := HandoffCommand{
				OriginAgent: prev.Agent,
				TargetAgent: role.Agent,
				Reason:      "orgschema_pipeline",
				Hop:         state.Hop + 1,
			}
			decision := policy.Evaluate(cmd, state)
			if !decision.Allow {
				o.auditPolicyDeny(traceID, currentParent, wf.ID, cmd, decision)
				return fmt.Errorf("handoff blocked: %s", decision.Reason)
			}
			state.Hop++
			event := HandoffEvent{
				OriginAgent: prev.Agent,
				TargetAgent: role.Agent,
				Reason:      cmd.Reason,
				OccurredAt:  time.Now().UTC(),
			}
			state.HandoffChain = append(state.HandoffChain, event)
			_ = o.mem.Append(env.ClientID, env.ThreadID, llm.Message{Role: "system", Content: fmt.Sprintf("handoff %s -> %s (%s)", event.OriginAgent, event.TargetAgent, event.Reason)})
			telemetry.IncCounter(telemetry.MetricAgentHandoffTotal, 1, map[string]string{"workflow": wf.ID})
		}

		stepTimeout := policy.StepTimeout
		stepCtx := ctx
		cancel := func() {}
		if stepTimeout > 0 {
			stepCtx, cancel = context.WithTimeout(ctx, stepTimeout)
		}
		stepStart := time.Now()
		stepOutput, usage, stepSpanID, stepErr := o.runCooperativeStep(stepCtx, traceID, currentParent, requestID, wf, role, input)
		cancel()
		elapsed := time.Since(stepStart)
		if stepErr != nil {
			return fmt.Errorf("agent %s failed: %w", role.Agent, stepErr)
		}
		currentParent = stepSpanID
		cmd := HandoffCommand{EstimatedTokens: usage.TotalTokens, Elapsed: elapsed}
		decision := policy.Evaluate(cmd, state)
		if !decision.Allow {
			o.auditPolicyDeny(traceID, currentParent, wf.ID, cmd, decision)
			return fmt.Errorf("workflow policy blocked after %s: %s", role.Agent, decision.Reason)
		}

		state.TokensUsed += usage.TotalTokens
		totals.prompt += usage.PromptTokens
		totals.completion += usage.CompletionTokens
		totals.total += usage.TotalTokens
		output = stepOutput
		input = stepOutput
	}

	o.storeWorkflowState(env.ID, state)
	return o.publishCooperativeReply(ctx, env, wf, output, totals, state, traceID, requestID)
}

func (o *Orch) runCooperativeStep(
	ctx context.Context,
	traceID, parentSpanID, requestID string,
	wf config.WorkflowConfig,
	role config.OrgRoleConfig,
	input string,
) (string, llm.Usage, string, error) {
	span := telemetry.StartSpan(nil, traceID, parentSpanID, "orchestrator.cooperative.step",
		"workflow_id", wf.ID,
		"role", role.Kind,
		"role_name", role.Name,
		"agent", role.Agent,
	)
	defer span.End(nil,
		"workflow_id", wf.ID,
		"role", role.Kind,
		"role_name", role.Name,
		"agent", role.Agent,
	)

	agentCfg, err := o.findAgentConfig(role.Agent)
	if err != nil {
		return "", llm.Usage{}, span.SpanID(), err
	}
	backend, err := o.llms.Get(agentCfg.LLM)
	if err != nil {
		return "", llm.Usage{}, span.SpanID(), err
	}

	systemPrompt := strings.TrimSpace(agentCfg.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = "You are an agent in a cooperative workflow."
	}
	if len(role.Responsibilities) > 0 {
		systemPrompt += "\n\nResponsibilities:\n- " + strings.Join(role.Responsibilities, "\n- ")
	}
	requestCtx := telemetry.WithTrace(ctx, traceID, span.SpanID(), requestID)
	resp, err := backend.Complete(requestCtx, llm.Request{
		ModelName:    agentCfg.LLM,
		SystemPrompt: systemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: input}},
		MaxTokens:    512,
	})
	if err != nil {
		return "", llm.Usage{}, span.SpanID(), err
	}
	if strings.TrimSpace(resp.Content) == "" {
		return "", llm.Usage{}, span.SpanID(), fmt.Errorf("empty cooperative response from agent %s", role.Agent)
	}
	_ = o.mem.Append(role.Agent, wf.ID, llm.Message{Role: "assistant", Content: resp.Content})
	return resp.Content, resp.Usage, span.SpanID(), nil
}

func (o *Orch) findAgentConfig(name string) (config.AgentConfig, error) {
	wanted := strings.TrimSpace(name)
	for _, a := range o.cfg.Agents {
		if strings.TrimSpace(a.Name) == wanted {
			return a, nil
		}
	}
	return config.AgentConfig{}, fmt.Errorf("agent %q not found", name)
}

func (o *Orch) findOrgSchema(schemaID string) (*config.OrgSchemaConfig, error) {
	wanted := strings.TrimSpace(schemaID)
	for _, s := range o.cfg.OrgSchemas {
		if strings.TrimSpace(s.SchemaID) == wanted {
			copySchema := s
			return &copySchema, nil
		}
	}
	return nil, fmt.Errorf("org schema %q not found", schemaID)
}

func compileOrgSchema(s config.OrgSchemaConfig) (compiledOrgSchema, error) {
	if len(s.Roles) == 0 {
		return compiledOrgSchema{}, fmt.Errorf("org schema %q has no roles", s.SchemaID)
	}
	roleByName := make(map[string]config.OrgRoleConfig, len(s.Roles))
	var router config.OrgRoleConfig
	var synth config.OrgRoleConfig
	specialists := make([]config.OrgRoleConfig, 0)
	for _, role := range s.Roles {
		roleByName[role.Name] = role
		switch strings.ToLower(strings.TrimSpace(role.Kind)) {
		case "router":
			router = role
		case "specialist":
			specialists = append(specialists, role)
		case "synthesizer":
			synth = role
		}
	}
	if router.Name == "" || synth.Name == "" || len(specialists) == 0 {
		return compiledOrgSchema{}, fmt.Errorf("org schema %q must include router/specialist/synthesizer roles", s.SchemaID)
	}

	allowedPairs := make(map[string]map[string]struct{})
	if len(s.HandoffRules) == 0 {
		for _, sp := range specialists {
			allowPair(allowedPairs, router.Agent, sp.Agent)
			allowPair(allowedPairs, sp.Agent, synth.Agent)
		}
	} else {
		for _, rule := range s.HandoffRules {
			fromRole, ok := roleByName[rule.From]
			if !ok {
				return compiledOrgSchema{}, fmt.Errorf("org schema %q invalid handoff from role %q", s.SchemaID, rule.From)
			}
			for _, to := range rule.To {
				toRole, ok := roleByName[to]
				if !ok {
					return compiledOrgSchema{}, fmt.Errorf("org schema %q invalid handoff target role %q", s.SchemaID, to)
				}
				allowPair(allowedPairs, fromRole.Agent, toRole.Agent)
			}
		}
	}

	for _, sp := range specialists {
		if !pairAllowed(allowedPairs, router.Agent, sp.Agent) {
			return compiledOrgSchema{}, fmt.Errorf("org schema %q missing handoff router->%s", s.SchemaID, sp.Name)
		}
		if !pairAllowed(allowedPairs, sp.Agent, synth.Agent) {
			return compiledOrgSchema{}, fmt.Errorf("org schema %q missing handoff %s->synthesizer", s.SchemaID, sp.Name)
		}
	}

	order := make([]config.OrgRoleConfig, 0, len(specialists)+2)
	order = append(order, router)
	order = append(order, specialists...)
	order = append(order, synth)
	return compiledOrgSchema{ID: s.SchemaID, RoleOrder: order, AllowedAgentPairs: allowedPairs}, nil
}

func allowPair(m map[string]map[string]struct{}, from, to string) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return
	}
	if _, ok := m[from]; !ok {
		m[from] = make(map[string]struct{})
	}
	m[from][to] = struct{}{}
}

func pairAllowed(m map[string]map[string]struct{}, from, to string) bool {
	outs, ok := m[strings.TrimSpace(from)]
	if !ok {
		return false
	}
	_, ok = outs[strings.TrimSpace(to)]
	return ok
}

func (o *Orch) publishCooperativeReply(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig, text string, totals tokenTotals, state WorkflowState, traceID, requestID string) error {
	chainJSON, _ := json.Marshal(state.HandoffChain)
	reply := &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       env.ClientID,
		ThreadID:       env.ThreadID,
		Role:           bus.RoleAssistant,
		Text:           text,
		SourceProtocol: env.SourceProtocol,
		Meta: map[string]string{
			"agent":             "synthesizer",
			"trace_id":          traceID,
			"request_id":        requestID,
			"workflow_id":       wf.ID,
			"handoff_chain":     string(chainJSON),
			"tokens_prompt":     strconv.Itoa(totals.prompt),
			"tokens_completion": strconv.Itoa(totals.completion),
			"tokens_total":      strconv.Itoa(totals.total),
		},
		CreatedAt: time.Now().UTC(),
	}
	return o.b.Publish(ctx, bus.ReplyKey(env.SourceProtocol), reply)
}

func (o *Orch) publishCooperativeFailure(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig, err error) {
	reply := &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       env.ClientID,
		ThreadID:       env.ThreadID,
		Role:           bus.RoleAssistant,
		Text:           "cooperative workflow blocked: " + err.Error(),
		SourceProtocol: env.SourceProtocol,
		Meta: map[string]string{
			"agent":       "synthesizer",
			"trace_id":    env.Meta["trace_id"],
			"request_id":  env.Meta["request_id"],
			"workflow_id": wf.ID,
			"status":      "blocked",
		},
		CreatedAt: time.Now().UTC(),
	}
	if pubErr := o.b.Publish(ctx, bus.ReplyKey(env.SourceProtocol), reply); pubErr != nil {
		o.log.Warn("failed publishing cooperative failure reply", "workflow_id", wf.ID, "err", pubErr)
	}
}

func (o *Orch) auditPolicyDeny(traceID, parentSpanID, workflowID string, cmd HandoffCommand, decision PolicyDecision) {
	span := telemetry.StartSpan(nil, traceID, parentSpanID, "orchestrator.policy.deny",
		"workflow_id", workflowID,
		"origin_agent", cmd.OriginAgent,
		"target_agent", cmd.TargetAgent,
		"reason", decision.Reason,
	)
	span.End(nil,
		"workflow_id", workflowID,
		"origin_agent", cmd.OriginAgent,
		"target_agent", cmd.TargetAgent,
		"reason", decision.Reason,
	)
	o.log.Warn("handoff policy denied",
		"workflow_id", workflowID,
		"origin_agent", cmd.OriginAgent,
		"target_agent", cmd.TargetAgent,
		"reason", decision.Reason,
	)
}

func (o *Orch) storeWorkflowState(requestID string, state WorkflowState) {
	o.wfMu.Lock()
	o.wfStates[requestID] = state
	o.wfMu.Unlock()
}
