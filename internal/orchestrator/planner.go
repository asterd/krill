package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/telemetry"
)

type capabilityType string

const (
	capabilitySkill        capabilityType = "skill"
	capabilityMCP          capabilityType = "mcp"
	capabilityCode         capabilityType = "code"
	capabilityBackend      capabilityType = "backend_action"
	capabilityAgent        capabilityType = "agent_action"
	capabilityDeployment   capabilityType = "deployment_action"
	capabilityNotification capabilityType = "notification_action"
)

type capability struct {
	Name                 string
	Type                 capabilityType
	Ref                  string
	Description          string
	ReleaseChannel       string
	RuntimeProfile       string
	SandboxProfile       string
	Protocols            map[string]struct{}
	Workflows            map[string]struct{}
	AllowedAgents        map[string]struct{}
	RequiredSecrets      []string
	RequiredApprovals    int
	AllowNetwork         bool
	AllowFilesystemWrite bool
	MaxTimeoutMs         int
	CostWeight           int
	LatencyWeight        int
	TrustScore           int
	RiskScore            int
	MaxConcurrency       int
	Fallbacks            []string
}

type capabilityFeedback struct {
	Successes   int
	Failures    int
	Violations  int
	AvgLatency  time.Duration
	LastLatency time.Duration
}

type capabilityGraph struct {
	mu       sync.Mutex
	items    map[string]capability
	feedback map[string]capabilityFeedback
	running  map[string]int
}

type intent struct {
	Goal                  string
	RequestedCapabilities []string
	CapabilityInput       string
	RuntimeProfile        string
	SandboxProfile        string
	ReleaseChannel        string
	RequiresPlanner       bool
	RequiresApproval      bool
	RequestedNetwork      bool
	RequestedPaths        []string
	RequestedTimeoutMs    int
	NeedCooperative       bool
	Scheduled             bool
	MissingSecrets        []string
	SecurityReason        string
}

type plan struct {
	ID           string
	RequestID    string
	WorkflowID   string
	Status       string
	Rationale    string
	ReplayKey    string
	NeedApproval bool
	Steps        []planStep
	Artifacts    []artifact
	Audit        []planAuditEvent
	Escalations  []escalation
	Attestation  attestation
}

type planStep struct {
	ID                    string
	Goal                  string
	CandidateCapabilities []string
	SelectedCapability    string
	CapabilityInput       string
	Fallbacks             []string
	Status                string
	Rationale             string
	Output                string
	TimeoutMs             int
	RoleName              string
	RoleKind              string
	Agent                 string
	Responsibilities      []string
}

type artifact struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Hash    string `json:"hash"`
}

type attestation struct {
	InputHash      string `json:"input_hash"`
	ArtifactHash   string `json:"artifact_hash"`
	RuntimeProfile string `json:"runtime_profile"`
	SandboxProfile string `json:"sandbox_profile"`
}

type escalation struct {
	Kind    string   `json:"kind"`
	Reason  string   `json:"reason"`
	Missing []string `json:"missing,omitempty"`
}

type planAuditEvent struct {
	StepID     string    `json:"step_id"`
	Capability string    `json:"capability"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

type policyDecision struct {
	Allow          bool
	Reason         string
	ApprovalNeeded bool
	MissingSecrets []string
	Monitored      bool
}

type plannerRouteDecision struct {
	Mode                  string   `json:"mode"`
	Rationale             string   `json:"rationale"`
	RequestedCapabilities []string `json:"requested_capabilities"`
	CapabilityInput       string   `json:"capability_input"`
	RuntimeProfile        string   `json:"runtime_profile"`
	SandboxProfile        string   `json:"sandbox_profile"`
	ReleaseChannel        string   `json:"release_channel"`
	RequestedNetwork      bool     `json:"requested_network"`
	RequestedPaths        []string `json:"requested_paths"`
	RequestedTimeoutMs    int      `json:"requested_timeout_ms"`
	RequiresApproval      bool     `json:"requires_approval"`
	MissingSecrets        []string `json:"missing_secrets"`
}

type plannerSpec struct {
	Rationale string            `json:"rationale"`
	Steps     []plannerSpecStep `json:"steps"`
}

type plannerSpecStep struct {
	Goal                  string   `json:"goal"`
	CandidateCapabilities []string `json:"candidate_capabilities"`
	CapabilityInput       string   `json:"capability_input"`
	Rationale             string   `json:"rationale"`
	TimeoutMs             int      `json:"timeout_ms"`
}

func newCapabilityGraph(cfg *config.Root, skillNames []string) *capabilityGraph {
	graph := &capabilityGraph{
		items:    make(map[string]capability),
		feedback: make(map[string]capabilityFeedback),
		running:  make(map[string]int),
	}
	add := func(capCfg config.CapabilityConfig) {
		name := strings.TrimSpace(capCfg.Name)
		if name == "" {
			return
		}
		graph.items[name] = capability{
			Name:                 name,
			Type:                 capabilityType(defaultString(capCfg.Type, "skill")),
			Ref:                  defaultString(capCfg.Ref, name),
			Description:          capCfg.Description,
			ReleaseChannel:       defaultString(capCfg.ReleaseChannel, "stable"),
			RuntimeProfile:       defaultString(capCfg.RuntimeProfile, defaultString(cfg.Planner.DefaultRuntimeProfile, "default")),
			SandboxProfile:       defaultString(capCfg.SandboxProfile, defaultString(cfg.Planner.DefaultSandboxProfile, "balanced")),
			Protocols:            stringSet(capCfg.Protocols),
			Workflows:            stringSet(capCfg.Workflows),
			AllowedAgents:        stringSet(capCfg.AllowedAgents),
			RequiredSecrets:      append([]string(nil), capCfg.RequiredSecrets...),
			RequiredApprovals:    capCfg.RequiredApprovals,
			AllowNetwork:         capCfg.AllowNetwork,
			AllowFilesystemWrite: capCfg.AllowFilesystemWrite,
			MaxTimeoutMs:         capCfg.MaxTimeoutMs,
			CostWeight:           capCfg.CostWeight,
			LatencyWeight:        capCfg.LatencyWeight,
			TrustScore:           defaultInt(capCfg.TrustScore, 60),
			RiskScore:            capCfg.RiskScore,
			MaxConcurrency:       capCfg.MaxConcurrency,
			Fallbacks:            append([]string(nil), capCfg.Fallbacks...),
		}
	}
	for _, capCfg := range cfg.Capabilities {
		add(capCfg)
	}
	for _, name := range skillNames {
		if _, ok := graph.items[name]; ok {
			continue
		}
		capType := capabilitySkill
		sandboxProfile := defaultString(cfg.Planner.DefaultSandboxProfile, "balanced")
		runtimeProfile := defaultString(cfg.Planner.DefaultRuntimeProfile, "default")
		trustScore := 70
		riskScore := 20
		if name == "code_exec" {
			capType = capabilityCode
			sandboxProfile = "balanced"
			runtimeProfile = "opencode"
			trustScore = 65
			riskScore = 55
		}
		graph.items[name] = capability{
			Name:           name,
			Type:           capType,
			Ref:            name,
			ReleaseChannel: "stable",
			RuntimeProfile: runtimeProfile,
			SandboxProfile: sandboxProfile,
			TrustScore:     trustScore,
			RiskScore:      riskScore,
			MaxConcurrency: 2,
			MaxTimeoutMs:   30000,
		}
	}
	for _, agentCfg := range cfg.Agents {
		name := "agent:" + strings.TrimSpace(agentCfg.Name)
		if _, ok := graph.items[name]; ok {
			continue
		}
		graph.items[name] = capability{
			Name:           name,
			Type:           capabilityAgent,
			Ref:            strings.TrimSpace(agentCfg.Name),
			ReleaseChannel: "stable",
			RuntimeProfile: "default",
			SandboxProfile: defaultString(cfg.Planner.DefaultSandboxProfile, "balanced"),
			AllowedAgents:  stringSet([]string{agentCfg.Name}),
			TrustScore:     80,
			MaxConcurrency: 4,
		}
	}
	for _, built := range []config.CapabilityConfig{
		{Name: "backend:query", Type: string(capabilityBackend), ReleaseChannel: "stable", SandboxProfile: "strict", TrustScore: 75, RiskScore: 35},
		{Name: "backend:api", Type: string(capabilityBackend), ReleaseChannel: "stable", SandboxProfile: "balanced", TrustScore: 70, RiskScore: 45, AllowNetwork: true},
		{Name: "backend:operation", Type: string(capabilityBackend), ReleaseChannel: "stable", SandboxProfile: "balanced", TrustScore: 68, RiskScore: 55},
		{Name: "deploy:asset", Type: string(capabilityDeployment), ReleaseChannel: "candidate", SandboxProfile: "extended", RuntimeProfile: "opencode", TrustScore: 60, RiskScore: 70, AllowNetwork: true, AllowFilesystemWrite: true},
		{Name: "notify:user", Type: string(capabilityNotification), ReleaseChannel: "stable", SandboxProfile: "strict", TrustScore: 90, RiskScore: 15},
	} {
		if _, ok := graph.items[built.Name]; !ok {
			add(built)
		}
	}
	return graph
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out[trimmed] = struct{}{}
	}
	return out
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func (g *capabilityGraph) get(name string) (capability, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	capCfg, ok := g.items[name]
	return capCfg, ok
}

func (g *capabilityGraph) feedbackSnapshot(name string) capabilityFeedback {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.feedback[name]
}

func (g *capabilityGraph) runningCount(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running[name]
}

func (g *capabilityGraph) selectCapability(candidates []string, env *bus.Envelope, wf config.WorkflowConfig, step planStep, planCfg *plan, in intent, agentName string, mode string) (capability, []string, policyDecision) {
	type candidateScore struct {
		capCfg capability
		score  int
		dec    policyDecision
	}
	var ranked []candidateScore
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		capCfg, ok := g.get(candidate)
		if !ok {
			continue
		}
		dec := evaluateCapabilityPolicy(capCfg, env, wf, in, agentName, mode, g.runningCount(capCfg.Name))
		score := capCfg.TrustScore*100 - capCfg.RiskScore*10 - capCfg.CostWeight*10 - capCfg.LatencyWeight
		if capCfg.ReleaseChannel == "stable" {
			score += 2000
		}
		if capCfg.ReleaseChannel == "candidate" {
			score += 1000
		}
		if capCfg.ReleaseChannel == "deprecated" {
			score -= 5000
		}
		if fb := g.feedbackSnapshot(capCfg.Name); fb.Successes > 0 {
			score += fb.Successes * 3
		}
		if fb := g.feedbackSnapshot(capCfg.Name); fb.Failures > 0 {
			score -= fb.Failures * 10
		}
		if fb := g.feedbackSnapshot(capCfg.Name); fb.Violations > 0 {
			score -= fb.Violations * 25
		}
		if in.ReleaseChannel != "" && capCfg.ReleaseChannel == in.ReleaseChannel {
			score += 5000
		}
		if in.RuntimeProfile != "" && capCfg.RuntimeProfile == in.RuntimeProfile {
			score += 500
		}
		ranked = append(ranked, candidateScore{capCfg: capCfg, score: score, dec: dec})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].dec.Allow != ranked[j].dec.Allow {
			return ranked[i].dec.Allow
		}
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].capCfg.Name < ranked[j].capCfg.Name
	})
	if len(ranked) == 0 {
		return capability{}, nil, policyDecision{Allow: false, Reason: "no_compatible_capability"}
	}
	chosen := ranked[0]
	fallbacks := make([]string, 0, len(ranked)-1)
	for _, item := range ranked[1:] {
		fallbacks = append(fallbacks, item.capCfg.Name)
	}
	return chosen.capCfg, fallbacks, chosen.dec
}

func evaluateCapabilityPolicy(capCfg capability, env *bus.Envelope, wf config.WorkflowConfig, in intent, agentName, mode string, running int) policyDecision {
	decision := policyDecision{Allow: true}
	match := func(set map[string]struct{}, value string) bool {
		if len(set) == 0 || strings.TrimSpace(value) == "" {
			return true
		}
		_, ok := set[value]
		return ok
	}
	if !match(capCfg.Protocols, env.SourceProtocol) {
		decision.Allow = false
		decision.Reason = "protocol_not_allowed"
	}
	if decision.Allow && !match(capCfg.Workflows, wf.ID) {
		decision.Allow = false
		decision.Reason = "workflow_not_allowed"
	}
	if decision.Allow && !match(capCfg.AllowedAgents, agentName) {
		decision.Allow = false
		decision.Reason = "agent_not_allowed"
	}
	if decision.Allow && in.ReleaseChannel != "" && capCfg.ReleaseChannel != in.ReleaseChannel {
		decision.Allow = false
		decision.Reason = "release_channel_not_pinned"
	}
	if decision.Allow && in.RuntimeProfile != "" && capCfg.RuntimeProfile != "" && capCfg.RuntimeProfile != in.RuntimeProfile {
		decision.Allow = false
		decision.Reason = "runtime_profile_mismatch"
	}
	if decision.Allow && in.SandboxProfile != "" && capCfg.SandboxProfile != "" && capCfg.SandboxProfile != in.SandboxProfile {
		decision.Allow = false
		decision.Reason = "sandbox_profile_mismatch"
	}
	if decision.Allow && in.RequestedNetwork && !capCfg.AllowNetwork {
		decision.Allow = false
		decision.Reason = "network_not_allowed"
	}
	if decision.Allow && len(in.RequestedPaths) > 0 && !capCfg.AllowFilesystemWrite && capCfg.Type != capabilitySkill && capCfg.Type != capabilityAgent && capCfg.Type != capabilityNotification {
		decision.Allow = false
		decision.Reason = "filesystem_write_not_allowed"
	}
	if decision.Allow && len(in.RequestedPaths) > 0 && hasUnsafePath(in.RequestedPaths) {
		decision.Allow = false
		decision.Reason = "path_traversal_blocked"
	}
	if decision.Allow && capCfg.MaxTimeoutMs > 0 && in.RequestedTimeoutMs > capCfg.MaxTimeoutMs {
		decision.Allow = false
		decision.Reason = "timeout_exceeds_capability_budget"
	}
	if decision.Allow && capCfg.MaxConcurrency > 0 && running >= capCfg.MaxConcurrency {
		decision.Allow = false
		decision.Reason = "capability_quota_exceeded"
	}
	if decision.Allow {
		var missing []string
		for _, secret := range capCfg.RequiredSecrets {
			if strings.TrimSpace(secret) == "" {
				continue
			}
			if _, ok := os.LookupEnv(secret); !ok {
				missing = append(missing, secret)
			}
		}
		for _, secret := range in.MissingSecrets {
			if strings.TrimSpace(secret) != "" {
				missing = append(missing, secret)
			}
		}
		if len(missing) > 0 {
			decision.Allow = false
			decision.Reason = "missing_secret"
			decision.MissingSecrets = uniqueStrings(missing)
		}
	}
	if decision.Allow && (capCfg.RequiredApprovals > 0 || in.RequiresApproval) {
		decision.Allow = false
		decision.Reason = "approval_required"
		decision.ApprovalNeeded = true
	}
	if !decision.Allow && strings.EqualFold(mode, "monitor") {
		decision.Monitored = true
		decision.Allow = true
	}
	return decision
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var out []string
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func hasUnsafePath(paths []string) bool {
	for _, path := range paths {
		clean := strings.TrimSpace(path)
		if clean == "" {
			continue
		}
		if strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") {
			return true
		}
	}
	return false
}

func (g *capabilityGraph) begin(name string) {
	g.mu.Lock()
	g.running[name]++
	g.mu.Unlock()
}

func (g *capabilityGraph) end(name string, err error, latency time.Duration, violation bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[name] > 0 {
		g.running[name]--
	}
	fb := g.feedback[name]
	if err == nil {
		fb.Successes++
	} else {
		fb.Failures++
	}
	if violation {
		fb.Violations++
	}
	fb.LastLatency = latency
	if fb.AvgLatency == 0 {
		fb.AvgLatency = latency
	} else {
		totalRuns := fb.Successes + fb.Failures
		fb.AvgLatency = time.Duration((int64(fb.AvgLatency)*(int64(totalRuns)-1) + int64(latency)) / int64(totalRuns))
	}
	g.feedback[name] = fb
}

func (o *Orch) resolveIntent(env *bus.Envelope, wf config.WorkflowConfig) intent {
	result := intent{
		Goal:           strings.TrimSpace(env.Text),
		RuntimeProfile: defaultString(metaValue(env, "runtime_profile"), defaultString(o.cfg.Planner.DefaultRuntimeProfile, "default")),
		SandboxProfile: defaultString(metaValue(env, "sandbox_profile"), defaultString(o.cfg.Planner.DefaultSandboxProfile, "balanced")),
		ReleaseChannel: strings.TrimSpace(metaValue(env, "release_channel")),
		Scheduled:      strings.EqualFold(env.SourceProtocol, "scheduler"),
	}
	if raw := strings.TrimSpace(metaValue(env, "target_capability")); raw != "" {
		result.RequestedCapabilities = append(result.RequestedCapabilities, raw)
	}
	if raw := strings.TrimSpace(metaValue(env, "capabilities")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				result.RequestedCapabilities = append(result.RequestedCapabilities, trimmed)
			}
		}
	}
	if len(result.RequestedCapabilities) == 0 {
		if workflowID := strings.TrimSpace(metaValue(env, "workflow_id")); workflowID != "" {
			if _, ok := o.capabilities.get(workflowID); ok {
				result.RequestedCapabilities = append(result.RequestedCapabilities, workflowID)
			}
		}
	}
	result.CapabilityInput = strings.TrimSpace(metaValue(env, "capability_input"))
	if strings.EqualFold(metaValue(env, "planner"), "true") || strings.EqualFold(metaValue(env, "planner_path"), "true") {
		result.RequiresPlanner = true
	}
	if strings.EqualFold(metaValue(env, "requires_approval"), "true") {
		result.RequiresApproval = true
	}
	if strings.EqualFold(metaValue(env, "network"), "true") {
		result.RequestedNetwork = true
	}
	if raw := strings.TrimSpace(metaValue(env, "paths")); raw != "" {
		result.RequestedPaths = strings.Split(raw, ",")
	}
	if raw := strings.TrimSpace(metaValue(env, "missing_secret")); raw != "" {
		result.MissingSecrets = append(result.MissingSecrets, strings.Split(raw, ",")...)
	}
	if raw := strings.TrimSpace(metaValue(env, "timeout_ms")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			result.RequestedTimeoutMs = value
		}
	}
	result.NeedCooperative = isCooperativeWorkflow(wf)
	if result.Scheduled || result.NeedCooperative || len(result.RequestedCapabilities) > 0 {
		result.RequiresPlanner = true
	}
	if result.RequestedNetwork {
		result.RequiresPlanner = true
		result.SecurityReason = "requested_network"
	}
	if len(result.RequestedPaths) > 0 {
		result.RequiresPlanner = true
		result.SecurityReason = "requested_filesystem_access"
	}
	if !result.RequiresPlanner && !result.Scheduled && !result.NeedCooperative {
		if decided, ok := o.inferPlannerRoute(env, wf, result); ok {
			result.RequiresPlanner = strings.EqualFold(decided.Mode, "planned")
			if len(decided.RequestedCapabilities) > 0 {
				result.RequestedCapabilities = uniqueStrings(decided.RequestedCapabilities)
			}
			if strings.TrimSpace(decided.CapabilityInput) != "" {
				result.CapabilityInput = strings.TrimSpace(decided.CapabilityInput)
			}
			result.RuntimeProfile = defaultString(decided.RuntimeProfile, result.RuntimeProfile)
			result.SandboxProfile = defaultString(decided.SandboxProfile, result.SandboxProfile)
			if strings.TrimSpace(decided.ReleaseChannel) != "" {
				result.ReleaseChannel = strings.TrimSpace(decided.ReleaseChannel)
			}
			result.RequestedNetwork = result.RequestedNetwork || decided.RequestedNetwork
			if len(decided.RequestedPaths) > 0 {
				result.RequestedPaths = append([]string(nil), decided.RequestedPaths...)
			}
			if decided.RequestedTimeoutMs > 0 {
				result.RequestedTimeoutMs = decided.RequestedTimeoutMs
			}
			result.RequiresApproval = result.RequiresApproval || decided.RequiresApproval
			if len(decided.MissingSecrets) > 0 {
				result.MissingSecrets = uniqueStrings(append(result.MissingSecrets, decided.MissingSecrets...))
			}
		}
	}
	return result
}

func metaValue(env *bus.Envelope, key string) string {
	if env == nil || env.Meta == nil {
		return ""
	}
	return env.Meta[key]
}

func (o *Orch) shouldUsePlanner(env *bus.Envelope, wf config.WorkflowConfig, in intent) bool {
	if in.RequiresPlanner {
		return true
	}
	if wf.ID != "" && isCooperativeWorkflow(wf) {
		return true
	}
	return false
}

func (o *Orch) inferPlannerRoute(env *bus.Envelope, wf config.WorkflowConfig, current intent) (plannerRouteDecision, bool) {
	if o.llms == nil {
		return plannerRouteDecision{}, false
	}
	agentCfg := o.plannerAgentConfig(env)
	backend, err := o.llms.Get(agentCfg.LLM)
	if err != nil {
		return plannerRouteDecision{}, false
	}
	summary := o.capabilityCatalogSummary()
	systemPrompt := strings.TrimSpace(`You are Krill's planning router.
Decide whether a request should stay on the direct conversational loop or use the explicit planner.
Return strict JSON only with keys:
- mode: "direct" or "planned"
- rationale
- requested_capabilities: array of capability names from the catalog when planner is needed
- capability_input: JSON string to pass to the selected capability, empty string if none
- runtime_profile
- sandbox_profile
- release_channel
- requested_network
- requested_paths
- requested_timeout_ms
- requires_approval
- missing_secrets

Use "direct" for ordinary conversational/tool-use requests that the existing loop can safely handle.
Use "planned" for multi-step, policy-sensitive, operational, scheduled, deployment, backend, or governed capability requests.
Infer semantics independent of the request language.`)
	userPrompt := fmt.Sprintf("Workflow: %s\nRequest: %s\nCapability catalog:\n%s\nCurrent defaults: runtime_profile=%s sandbox_profile=%s\nReturn JSON only.",
		defaultString(wf.ID, "default"), env.Text, summary, current.RuntimeProfile, current.SandboxProfile)
	resp, err := backend.Complete(context.Background(), llm.Request{
		ModelName:    agentCfg.LLM,
		SystemPrompt: systemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: userPrompt}},
		MaxTokens:    800,
	})
	if err != nil {
		return plannerRouteDecision{}, false
	}
	var decision plannerRouteDecision
	if err := decodePlannerJSON(resp.Content, &decision); err != nil {
		return plannerRouteDecision{}, false
	}
	return decision, true
}

func (o *Orch) capabilityCatalogSummary() string {
	o.capabilities.mu.Lock()
	defer o.capabilities.mu.Unlock()
	names := make([]string, 0, len(o.capabilities.items))
	for name := range o.capabilities.items {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		capCfg := o.capabilities.items[name]
		line := fmt.Sprintf("- %s | type=%s | runtime=%s | sandbox=%s | release=%s", capCfg.Name, capCfg.Type, capCfg.RuntimeProfile, capCfg.SandboxProfile, capCfg.ReleaseChannel)
		if capCfg.Description != "" {
			line += " | " + capCfg.Description
		}
		if toolDef, ok := o.skillReg.GetDef(capCfg.Ref); ok {
			line += " | input_schema=" + compactJSON(toolDef.Function.Parameters)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

func decodePlannerJSON(content string, out any) error {
	trimmed := strings.TrimSpace(content)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		trimmed = trimmed[start : end+1]
	}
	return json.Unmarshal([]byte(trimmed), out)
}

func (o *Orch) buildPlan(env *bus.Envelope, wf config.WorkflowConfig, in intent) (plan, error) {
	requestID := defaultString(metaValue(env, "request_id"), env.ID)
	p := plan{
		ID:         uuid.NewString(),
		RequestID:  requestID,
		WorkflowID: wf.ID,
		Status:     "planned",
		Rationale:  "planner path required for policy-sensitive, scheduled, or multi-step execution",
		ReplayKey:  requestID,
	}
	if wf.ID != "" && isCooperativeWorkflow(wf) {
		orgSchema, err := o.findOrgSchema(wf.OrgSchema)
		if err != nil {
			return plan{}, err
		}
		compiled, err := compileOrgSchema(*orgSchema)
		if err != nil {
			return plan{}, err
		}
		for idx, role := range compiled.RoleOrder {
			step := planStep{
				ID:                    fmt.Sprintf("step-%d", idx+1),
				Goal:                  strings.TrimSpace(env.Text),
				CandidateCapabilities: append([]string{"agent:" + role.Agent}, o.capabilityFallbacks("agent:"+role.Agent)...),
				Status:                "pending",
				Rationale:             "cooperative role execution",
				RoleName:              role.Name,
				RoleKind:              role.Kind,
				Agent:                 role.Agent,
				Responsibilities:      append([]string(nil), role.Responsibilities...),
				TimeoutMs:             wf.Policy.StepTimeoutMs,
			}
			p.Steps = append(p.Steps, step)
		}
		return p, nil
	}
	if spec, ok := o.buildPlanWithLLM(env, wf, in); ok && len(spec.Steps) > 0 {
		p.Rationale = defaultString(spec.Rationale, p.Rationale)
		for idx, planned := range spec.Steps {
			candidates := uniqueStrings(planned.CandidateCapabilities)
			if len(candidates) == 0 {
				continue
			}
			step := planStep{
				ID:                    fmt.Sprintf("step-%d", idx+1),
				Goal:                  defaultString(planned.Goal, strings.TrimSpace(env.Text)),
				CandidateCapabilities: append([]string(nil), candidates...),
				CapabilityInput:       strings.TrimSpace(planned.CapabilityInput),
				Status:                "pending",
				Rationale:             defaultString(planned.Rationale, "llm-generated plan step"),
				TimeoutMs:             planned.TimeoutMs,
			}
			p.Steps = append(p.Steps, step)
		}
		if len(p.Steps) > 0 {
			return p, nil
		}
	}
	requested := in.RequestedCapabilities
	if len(requested) == 0 {
		requested = []string{"backend:operation"}
	}
	for idx, name := range requested {
		step := planStep{
			ID:                    fmt.Sprintf("step-%d", idx+1),
			Goal:                  strings.TrimSpace(env.Text),
			CandidateCapabilities: append([]string{name}, o.capabilityFallbacks(name)...),
			CapabilityInput:       in.CapabilityInput,
			Status:                "pending",
			Rationale:             "policy-governed capability execution",
			TimeoutMs:             in.RequestedTimeoutMs,
		}
		p.Steps = append(p.Steps, step)
	}
	return p, nil
}

func (o *Orch) buildPlanWithLLM(env *bus.Envelope, wf config.WorkflowConfig, in intent) (plannerSpec, bool) {
	if o.llms == nil {
		return plannerSpec{}, false
	}
	agentCfg := o.plannerAgentConfig(env)
	backend, err := o.llms.Get(agentCfg.LLM)
	if err != nil {
		return plannerSpec{}, false
	}
	systemPrompt := strings.TrimSpace(`You are Krill's planning engine.
Build an explicit execution plan using the provided capability catalog.
Return strict JSON only with keys:
- rationale
- steps: array of objects with goal, candidate_capabilities, capability_input, rationale, timeout_ms

Rules:
- candidate_capabilities must use exact names from the catalog
- capability_input must be a JSON string matching the selected tool/capability input shape
- prefer a single deterministic step when one governed capability is sufficient
- avoid inventing capability names
- infer tool parameters from the user request semantics, even if the tool name is not mentioned`)
	userPrompt := fmt.Sprintf("Workflow: %s\nRequest: %s\nIntent requested capabilities: %s\nCatalog:\n%s\nReturn JSON only.",
		defaultString(wf.ID, "default"), env.Text, strings.Join(in.RequestedCapabilities, ","), o.capabilityCatalogSummary())
	resp, err := backend.Complete(context.Background(), llm.Request{
		ModelName:    agentCfg.LLM,
		SystemPrompt: systemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: userPrompt}},
		MaxTokens:    1400,
	})
	if err != nil {
		return plannerSpec{}, false
	}
	var spec plannerSpec
	if err := decodePlannerJSON(resp.Content, &spec); err != nil {
		return plannerSpec{}, false
	}
	return spec, true
}

func (o *Orch) plannerAgentConfig(env *bus.Envelope) config.AgentConfig {
	agents := o.cfg.Agents
	if len(agents) == 0 {
		return defaultAgent(o.cfg.LLM.Default)
	}
	if env != nil && env.Meta != nil {
		target := strings.TrimSpace(env.Meta["target_agent"])
		if target != "" {
			for _, a := range agents {
				if strings.TrimSpace(a.Name) == target {
					return a
				}
			}
		}
	}
	for _, a := range agents {
		if a.MatchProtocol != "" && env != nil && a.MatchProtocol == env.SourceProtocol {
			return a
		}
	}
	for _, a := range agents {
		if a.MatchProtocol == "" {
			return a
		}
	}
	return agents[0]
}

func (o *Orch) capabilityFallbacks(name string) []string {
	capCfg, ok := o.capabilities.get(name)
	if !ok {
		return nil
	}
	return append([]string(nil), capCfg.Fallbacks...)
}

func (o *Orch) executePlan(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig, in intent, p *plan) error {
	traceID := defaultString(metaValue(env, "trace_id"), telemetry.NewTraceID())
	requestID := defaultString(metaValue(env, "request_id"), env.ID)
	p.Attestation.InputHash = hashStrings(env.Text, in.CapabilityInput)
	p.Attestation.RuntimeProfile = in.RuntimeProfile
	p.Attestation.SandboxProfile = in.SandboxProfile
	var state WorkflowState
	policy := policyFromWorkflow(wf, nil)
	input := env.Text
	output := env.Text
	parentSpanID := metaValue(env, "consume_span")
	var totals tokenTotals
	for idx := range p.Steps {
		step := &p.Steps[idx]
		selected, fallbacks, decision := o.capabilities.selectCapability(step.CandidateCapabilities, env, wf, *step, p, in, step.Agent, o.cfg.Planner.ProgressiveMode)
		step.SelectedCapability = selected.Name
		step.Fallbacks = fallbacks
		if strings.TrimSpace(step.CapabilityInput) == "" {
			step.CapabilityInput = in.CapabilityInput
		}
		if err := o.validateCapabilityInput(selected, step.CapabilityInput, input); err != nil {
			p.Status = "blocked"
			step.Status = "blocked"
			p.Audit = append(p.Audit, planAuditEvent{StepID: step.ID, Capability: selected.Name, Status: "blocked", Reason: err.Error(), OccurredAt: time.Now().UTC()})
			return o.publishPlanBlocked(ctx, env, wf, traceID, requestID, p, policyDecision{Reason: err.Error()})
		}
		if !decision.Allow {
			p.Status = "blocked"
			step.Status = "blocked"
			p.Audit = append(p.Audit, planAuditEvent{StepID: step.ID, Capability: selected.Name, Status: "blocked", Reason: decision.Reason, OccurredAt: time.Now().UTC()})
			if decision.ApprovalNeeded {
				p.NeedApproval = true
				p.Escalations = append(p.Escalations, escalation{Kind: "approval", Reason: decision.Reason})
			}
			if len(decision.MissingSecrets) > 0 {
				p.Escalations = append(p.Escalations, escalation{Kind: "secret", Reason: decision.Reason, Missing: decision.MissingSecrets})
			}
			o.capabilities.end(selected.Name, fmt.Errorf("%s", decision.Reason), 0, true)
			return o.publishPlanBlocked(ctx, env, wf, traceID, requestID, p, decision)
		}
		if idx > 0 && isCooperativeWorkflow(wf) {
			prev := p.Steps[idx-1]
			cmd := HandoffCommand{
				OriginAgent: prev.Agent,
				TargetAgent: step.Agent,
				Reason:      "planned_cooperative_handoff",
				Hop:         state.Hop + 1,
			}
			handoffDecision := policy.Evaluate(cmd, state)
			if !handoffDecision.Allow {
				p.Status = "blocked"
				step.Status = "blocked"
				p.Audit = append(p.Audit, planAuditEvent{StepID: step.ID, Capability: selected.Name, Status: "blocked", Reason: handoffDecision.Reason, OccurredAt: time.Now().UTC()})
				o.auditPolicyDeny(traceID, parentSpanID, wf.ID, cmd, handoffDecision)
				o.capabilities.end(selected.Name, fmt.Errorf("%s", handoffDecision.Reason), 0, true)
				return o.publishPlanBlocked(ctx, env, wf, traceID, requestID, p, policyDecision{Reason: handoffDecision.Reason})
			}
			state.Hop++
			state.HandoffChain = append(state.HandoffChain, HandoffEvent{
				OriginAgent: prev.Agent,
				TargetAgent: step.Agent,
				Reason:      cmd.Reason,
				OccurredAt:  time.Now().UTC(),
			})
		}
		o.capabilities.begin(selected.Name)
		start := time.Now()
		stepCtx := telemetry.WithTrace(ctx, traceID, parentSpanID, requestID)
		stepOutput, artifacts, usage, spanID, execErr := o.executeCapability(stepCtx, env, wf, selected, *step, input)
		latency := time.Since(start)
		o.capabilities.end(selected.Name, execErr, latency, decision.Monitored)
		parentSpanID = spanID
		if execErr != nil {
			p.Status = "failed"
			step.Status = "failed"
			p.Audit = append(p.Audit, planAuditEvent{StepID: step.ID, Capability: selected.Name, Status: "failed", Reason: execErr.Error(), OccurredAt: time.Now().UTC()})
			return o.publishPlanBlocked(ctx, env, wf, traceID, requestID, p, policyDecision{Reason: execErr.Error()})
		}
		step.Status = "completed"
		step.Output = stepOutput
		p.Audit = append(p.Audit, planAuditEvent{StepID: step.ID, Capability: selected.Name, Status: "completed", OccurredAt: time.Now().UTC()})
		p.Artifacts = append(p.Artifacts, artifacts...)
		output = stepOutput
		input = stepOutput
		totals.prompt += usage.PromptTokens
		totals.completion += usage.CompletionTokens
		totals.total += usage.TotalTokens
		if isCooperativeWorkflow(wf) {
			state.TokensUsed += usage.TotalTokens
			cmd := HandoffCommand{EstimatedTokens: usage.TotalTokens, Elapsed: latency}
			postDecision := policy.Evaluate(cmd, state)
			if !postDecision.Allow {
				p.Status = "blocked"
				step.Status = "blocked"
				p.Audit = append(p.Audit, planAuditEvent{StepID: step.ID, Capability: selected.Name, Status: "blocked", Reason: postDecision.Reason, OccurredAt: time.Now().UTC()})
				o.auditPolicyDeny(traceID, parentSpanID, wf.ID, cmd, postDecision)
				return o.publishPlanBlocked(ctx, env, wf, traceID, requestID, p, policyDecision{Reason: postDecision.Reason})
			}
		}
	}
	p.Status = "completed"
	p.Attestation.ArtifactHash = hashArtifacts(p.Artifacts)
	o.storePlan(*p)
	if isCooperativeWorkflow(wf) {
		o.storeWorkflowState(env.ID, state)
	}
	return o.publishPlanReply(ctx, env, wf, output, totals, state, traceID, requestID, *p)
}

func (o *Orch) executeCapability(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig, capCfg capability, step planStep, input string) (string, []artifact, llm.Usage, string, error) {
	switch capCfg.Type {
	case capabilitySkill, capabilityCode:
		argsJSON := strings.TrimSpace(step.CapabilityInput)
		if argsJSON == "" {
			argsJSON = strings.TrimSpace(metaValue(env, "capability_input"))
		}
		if argsJSON == "" {
			payload := map[string]any{"input": input}
			encoded, _ := json.Marshal(payload)
			argsJSON = string(encoded)
		}
		result, err := o.skillReg.Execute(ctx, capCfg.Ref, argsJSON)
		resultArtifact := artifact{Name: capCfg.Name, Type: "result", Content: result, Hash: hashStrings(result)}
		return result, []artifact{resultArtifact}, llm.Usage{}, parentSpan(ctx), err
	case capabilityBackend, capabilityDeployment, capabilityNotification:
		payload := map[string]any{
			"capability": capCfg.Name,
			"type":       capCfg.Type,
			"workflow":   wf.ID,
			"goal":       step.Goal,
			"input":      input,
		}
		rawArgs := strings.TrimSpace(step.CapabilityInput)
		if rawArgs == "" {
			rawArgs = strings.TrimSpace(metaValue(env, "capability_input"))
		}
		if rawArgs != "" {
			var decoded any
			if err := json.Unmarshal([]byte(rawArgs), &decoded); err == nil {
				payload["args"] = decoded
			}
		}
		if capCfg.Type == capabilityDeployment {
			payload["endpoint"] = "pending://deployment"
		}
		data, _ := json.Marshal(payload)
		artType := "operation"
		if capCfg.Type == capabilityDeployment {
			artType = "deployment"
		}
		if capCfg.Type == capabilityNotification {
			artType = "notification"
		}
		output := string(data)
		return output, []artifact{{Name: capCfg.Name, Type: artType, Content: output, Hash: hashStrings(output)}}, llm.Usage{}, parentSpan(ctx), nil
	case capabilityAgent:
		role := config.OrgRoleConfig{
			Name:             step.RoleName,
			Kind:             step.RoleKind,
			Agent:            step.Agent,
			Responsibilities: append([]string(nil), step.Responsibilities...),
		}
		traceID, parentSpanID, requestID := telemetry.TraceFromContext(ctx)
		if traceID == "" {
			traceID = defaultString(metaValue(env, "trace_id"), telemetry.NewTraceID())
		}
		output, usage, spanID, err := o.runCooperativeStep(ctx, traceID, parentSpanID, requestID, wf, role, input)
		resultArtifact := artifact{Name: capCfg.Name, Type: "agent_output", Content: output, Hash: hashStrings(output)}
		return output, []artifact{resultArtifact}, usage, spanID, err
	default:
		return "", nil, llm.Usage{}, parentSpan(ctx), fmt.Errorf("unsupported capability type %s", capCfg.Type)
	}
}

func parentSpan(ctx context.Context) string {
	_, parentSpanID, _ := telemetry.TraceFromContext(ctx)
	return parentSpanID
}

func (o *Orch) validateCapabilityInput(capCfg capability, raw string, input string) error {
	if capCfg.Type == capabilityAgent {
		return nil
	}
	if capCfg.Type != capabilitySkill && capCfg.Type != capabilityCode {
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return fmt.Errorf("capability_input_invalid_json")
		}
		return nil
	}
	toolDef, ok := o.skillReg.GetDef(capCfg.Ref)
	if !ok {
		return nil
	}
	var schema struct {
		Required []string               `json:"required"`
		Props    map[string]interface{} `json:"properties"`
	}
	if err := json.Unmarshal(toolDef.Function.Parameters, &schema); err != nil {
		return nil
	}
	if strings.TrimSpace(raw) == "" {
		if capCfg.Ref == "code_exec" || len(schema.Required) > 0 {
			return fmt.Errorf("capability_input_missing")
		}
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return fmt.Errorf("capability_input_invalid_json")
	}
	for _, field := range schema.Required {
		if _, ok := payload[field]; !ok {
			return fmt.Errorf("capability_input_missing_required:%s", field)
		}
	}
	if capCfg.Ref == "code_exec" {
		if code, _ := payload["code"].(string); strings.TrimSpace(code) == "" {
			return fmt.Errorf("capability_input_missing_required:code")
		}
		if language, _ := payload["language"].(string); strings.TrimSpace(language) == "" {
			return fmt.Errorf("capability_input_missing_required:language")
		}
	}
	_ = input
	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func hashStrings(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "|")))
	return hex.EncodeToString(sum[:])
}

func hashArtifacts(artifacts []artifact) string {
	if len(artifacts) == 0 {
		return ""
	}
	values := make([]string, 0, len(artifacts))
	for _, art := range artifacts {
		values = append(values, art.Hash)
	}
	return hashStrings(values...)
}

func (o *Orch) publishPlanReply(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig, text string, totals tokenTotals, state WorkflowState, traceID, requestID string, p plan) error {
	chainJSON, _ := json.Marshal(state.HandoffChain)
	planJSON, _ := json.Marshal(p)
	agentName := "planner"
	if isCooperativeWorkflow(wf) {
		agentName = "synthesizer"
	}
	reply := &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       env.ClientID,
		ThreadID:       env.ThreadID,
		Role:           bus.RoleAssistant,
		Text:           text,
		SourceProtocol: env.SourceProtocol,
		Meta: map[string]string{
			"agent":                 agentName,
			"trace_id":              traceID,
			"request_id":            requestID,
			"workflow_id":           wf.ID,
			"plan_id":               p.ID,
			"plan_status":           p.Status,
			"handoff_chain":         string(chainJSON),
			"plan":                  string(planJSON),
			"attestation_input":     p.Attestation.InputHash,
			"attestation_artifacts": p.Attestation.ArtifactHash,
			"tokens_prompt":         strconv.Itoa(totals.prompt),
			"tokens_completion":     strconv.Itoa(totals.completion),
			"tokens_total":          strconv.Itoa(totals.total),
		},
		CreatedAt: time.Now().UTC(),
	}
	return o.b.Publish(ctx, bus.ReplyKey(env.SourceProtocol), reply)
}

func (o *Orch) publishPlanBlocked(ctx context.Context, env *bus.Envelope, wf config.WorkflowConfig, traceID, requestID string, p *plan, decision policyDecision) error {
	o.storePlan(*p)
	planJSON, _ := json.Marshal(p)
	text := "planned execution blocked: " + defaultString(decision.Reason, "policy_denied")
	if isCooperativeWorkflow(wf) {
		text = "cooperative workflow blocked: " + defaultString(decision.Reason, "policy_denied")
	}
	reply := &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       env.ClientID,
		ThreadID:       env.ThreadID,
		Role:           bus.RoleAssistant,
		Text:           text,
		SourceProtocol: env.SourceProtocol,
		Meta: map[string]string{
			"agent":       "planner",
			"trace_id":    traceID,
			"request_id":  requestID,
			"workflow_id": wf.ID,
			"plan_id":     p.ID,
			"plan_status": p.Status,
			"status":      "blocked",
			"reason":      defaultString(decision.Reason, "policy_denied"),
			"plan":        string(planJSON),
		},
		CreatedAt: time.Now().UTC(),
	}
	return o.b.Publish(ctx, bus.ReplyKey(env.SourceProtocol), reply)
}

func (o *Orch) storePlan(p plan) {
	o.planMu.Lock()
	o.planRuns[p.RequestID] = p
	o.planMu.Unlock()
}

func (o *Orch) PlanByRequest(requestID string) (plan, bool) {
	o.planMu.Lock()
	defer o.planMu.Unlock()
	p, ok := o.planRuns[requestID]
	return p, ok
}
