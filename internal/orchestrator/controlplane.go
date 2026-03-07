package orchestrator

import (
	"strings"
	"time"

	"github.com/krill/krill/config"
)

type CapabilitySnapshot struct {
	Name                 string             `json:"name"`
	Type                 string             `json:"type"`
	Ref                  string             `json:"ref"`
	Description          string             `json:"description,omitempty"`
	ReleaseChannel       string             `json:"release_channel"`
	RuntimeProfile       string             `json:"runtime_profile"`
	SandboxProfile       string             `json:"sandbox_profile"`
	Protocols            []string           `json:"protocols,omitempty"`
	Workflows            []string           `json:"workflows,omitempty"`
	AllowedAgents        []string           `json:"allowed_agents,omitempty"`
	RequiredSecrets      []string           `json:"required_secrets,omitempty"`
	RequiredApprovals    int                `json:"required_approvals,omitempty"`
	AllowNetwork         bool               `json:"allow_network"`
	AllowFilesystemWrite bool               `json:"allow_filesystem_write"`
	MaxTimeoutMs         int                `json:"max_timeout_ms,omitempty"`
	CostWeight           int                `json:"cost_weight,omitempty"`
	LatencyWeight        int                `json:"latency_weight,omitempty"`
	TrustScore           int                `json:"trust_score"`
	RiskScore            int                `json:"risk_score"`
	MaxConcurrency       int                `json:"max_concurrency,omitempty"`
	Fallbacks            []string           `json:"fallbacks,omitempty"`
	Feedback             CapabilityFeedback `json:"feedback"`
	Running              int                `json:"running"`
}

type CapabilityFeedback struct {
	Successes   int           `json:"successes"`
	Failures    int           `json:"failures"`
	Violations  int           `json:"violations"`
	AvgLatency  time.Duration `json:"avg_latency"`
	LastLatency time.Duration `json:"last_latency"`
}

type PlannerConfigSnapshot struct {
	ProgressiveMode       string `json:"progressive_mode"`
	DefaultSandboxProfile string `json:"default_sandbox_profile"`
	DefaultRuntimeProfile string `json:"default_runtime_profile"`
}

type PlanAuditSnapshot struct {
	StepID     string    `json:"step_id"`
	Capability string    `json:"capability"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

type EscalationSnapshot struct {
	Kind    string   `json:"kind"`
	Reason  string   `json:"reason"`
	Missing []string `json:"missing,omitempty"`
}

type ArtifactSnapshot struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Hash    string `json:"hash"`
}

type PlanStepSnapshot struct {
	ID                    string   `json:"id"`
	Goal                  string   `json:"goal"`
	CandidateCapabilities []string `json:"candidate_capabilities,omitempty"`
	SelectedCapability    string   `json:"selected_capability,omitempty"`
	CapabilityInput       string   `json:"capability_input,omitempty"`
	Fallbacks             []string `json:"fallbacks,omitempty"`
	Status                string   `json:"status"`
	Rationale             string   `json:"rationale,omitempty"`
	Output                string   `json:"output,omitempty"`
	TimeoutMs             int      `json:"timeout_ms,omitempty"`
	RoleName              string   `json:"role_name,omitempty"`
	RoleKind              string   `json:"role_kind,omitempty"`
	Agent                 string   `json:"agent,omitempty"`
	Responsibilities      []string `json:"responsibilities,omitempty"`
}

type AttestationSnapshot struct {
	InputHash      string `json:"input_hash"`
	ArtifactHash   string `json:"artifact_hash"`
	RuntimeProfile string `json:"runtime_profile"`
	SandboxProfile string `json:"sandbox_profile"`
}

type PlanSnapshot struct {
	ID           string               `json:"id"`
	RequestID    string               `json:"request_id"`
	WorkflowID   string               `json:"workflow_id,omitempty"`
	Status       string               `json:"status"`
	Rationale    string               `json:"rationale,omitempty"`
	ReplayKey    string               `json:"replay_key,omitempty"`
	NeedApproval bool                 `json:"need_approval"`
	Steps        []PlanStepSnapshot   `json:"steps,omitempty"`
	Artifacts    []ArtifactSnapshot   `json:"artifacts,omitempty"`
	Audit        []PlanAuditSnapshot  `json:"audit,omitempty"`
	Escalations  []EscalationSnapshot `json:"escalations,omitempty"`
	Attestation  AttestationSnapshot  `json:"attestation"`
}

func (o *Orch) PlannerConfigSnapshot() PlannerConfigSnapshot {
	o.planMu.Lock()
	defer o.planMu.Unlock()
	return PlannerConfigSnapshot{
		ProgressiveMode:       defaultString(o.cfg.Planner.ProgressiveMode, "monitor"),
		DefaultSandboxProfile: defaultString(o.cfg.Planner.DefaultSandboxProfile, "balanced"),
		DefaultRuntimeProfile: defaultString(o.cfg.Planner.DefaultRuntimeProfile, "default"),
	}
}

func (o *Orch) UpdatePlannerConfig(cfg PlannerConfigSnapshot) PlannerConfigSnapshot {
	o.planMu.Lock()
	if v := strings.TrimSpace(cfg.ProgressiveMode); v != "" {
		o.cfg.Planner.ProgressiveMode = v
	}
	if v := strings.TrimSpace(cfg.DefaultSandboxProfile); v != "" {
		o.cfg.Planner.DefaultSandboxProfile = v
	}
	if v := strings.TrimSpace(cfg.DefaultRuntimeProfile); v != "" {
		o.cfg.Planner.DefaultRuntimeProfile = v
	}
	snapshot := PlannerConfigSnapshot{
		ProgressiveMode:       defaultString(o.cfg.Planner.ProgressiveMode, "monitor"),
		DefaultSandboxProfile: defaultString(o.cfg.Planner.DefaultSandboxProfile, "balanced"),
		DefaultRuntimeProfile: defaultString(o.cfg.Planner.DefaultRuntimeProfile, "default"),
	}
	o.planMu.Unlock()
	return snapshot
}

func (o *Orch) CapabilitySnapshots() []CapabilitySnapshot {
	return o.capabilities.snapshots()
}

func (o *Orch) UpsertCapability(cfg config.CapabilityConfig) CapabilitySnapshot {
	o.capabilities.upsert(cfg, o.cfg)
	return o.CapabilitySnapshot(cfg.Name)
}

func (o *Orch) DeleteCapability(name string) bool {
	return o.capabilities.delete(name)
}

func (o *Orch) CapabilitySnapshot(name string) CapabilitySnapshot {
	for _, snap := range o.CapabilitySnapshots() {
		if snap.Name == name {
			return snap
		}
	}
	return CapabilitySnapshot{}
}

func (o *Orch) PlanSnapshotByRequest(requestID string) (PlanSnapshot, bool) {
	o.planMu.Lock()
	defer o.planMu.Unlock()
	p, ok := o.planRuns[requestID]
	if !ok {
		return PlanSnapshot{}, false
	}
	return snapshotPlan(p), true
}

func (o *Orch) PlanSnapshots() []PlanSnapshot {
	o.planMu.Lock()
	defer o.planMu.Unlock()
	out := make([]PlanSnapshot, 0, len(o.planRuns))
	for _, p := range o.planRuns {
		out = append(out, snapshotPlan(p))
	}
	return out
}

func (o *Orch) WorkflowSnapshotByRequest(requestID string) (WorkflowState, bool) {
	o.wfMu.Lock()
	defer o.wfMu.Unlock()
	state, ok := o.wfStates[requestID]
	return state, ok
}

func snapshotPlan(p plan) PlanSnapshot {
	out := PlanSnapshot{
		ID:           p.ID,
		RequestID:    p.RequestID,
		WorkflowID:   p.WorkflowID,
		Status:       p.Status,
		Rationale:    p.Rationale,
		ReplayKey:    p.ReplayKey,
		NeedApproval: p.NeedApproval,
		Attestation: AttestationSnapshot{
			InputHash:      p.Attestation.InputHash,
			ArtifactHash:   p.Attestation.ArtifactHash,
			RuntimeProfile: p.Attestation.RuntimeProfile,
			SandboxProfile: p.Attestation.SandboxProfile,
		},
	}
	for _, step := range p.Steps {
		out.Steps = append(out.Steps, PlanStepSnapshot{
			ID:                    step.ID,
			Goal:                  step.Goal,
			CandidateCapabilities: append([]string(nil), step.CandidateCapabilities...),
			SelectedCapability:    step.SelectedCapability,
			CapabilityInput:       step.CapabilityInput,
			Fallbacks:             append([]string(nil), step.Fallbacks...),
			Status:                step.Status,
			Rationale:             step.Rationale,
			Output:                step.Output,
			TimeoutMs:             step.TimeoutMs,
			RoleName:              step.RoleName,
			RoleKind:              step.RoleKind,
			Agent:                 step.Agent,
			Responsibilities:      append([]string(nil), step.Responsibilities...),
		})
	}
	for _, art := range p.Artifacts {
		out.Artifacts = append(out.Artifacts, ArtifactSnapshot(art))
	}
	for _, evt := range p.Audit {
		out.Audit = append(out.Audit, PlanAuditSnapshot(evt))
	}
	for _, esc := range p.Escalations {
		out.Escalations = append(out.Escalations, EscalationSnapshot(esc))
	}
	return out
}

func (g *capabilityGraph) upsert(capCfg config.CapabilityConfig, root *config.Root) {
	g.mu.Lock()
	defer g.mu.Unlock()
	name := strings.TrimSpace(capCfg.Name)
	if name == "" {
		return
	}
	g.items[name] = capability{
		Name:                 name,
		Type:                 capabilityType(defaultString(capCfg.Type, "skill")),
		Ref:                  defaultString(capCfg.Ref, name),
		Description:          capCfg.Description,
		ReleaseChannel:       defaultString(capCfg.ReleaseChannel, "stable"),
		RuntimeProfile:       defaultString(capCfg.RuntimeProfile, defaultString(root.Planner.DefaultRuntimeProfile, "default")),
		SandboxProfile:       defaultString(capCfg.SandboxProfile, defaultString(root.Planner.DefaultSandboxProfile, "balanced")),
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

func (g *capabilityGraph) delete(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	name = strings.TrimSpace(name)
	if _, ok := g.items[name]; !ok {
		return false
	}
	delete(g.items, name)
	delete(g.feedback, name)
	delete(g.running, name)
	return true
}

func (g *capabilityGraph) snapshots() []CapabilitySnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]CapabilitySnapshot, 0, len(g.items))
	for _, capCfg := range g.items {
		fb := g.feedback[capCfg.Name]
		out = append(out, CapabilitySnapshot{
			Name:                 capCfg.Name,
			Type:                 string(capCfg.Type),
			Ref:                  capCfg.Ref,
			Description:          capCfg.Description,
			ReleaseChannel:       capCfg.ReleaseChannel,
			RuntimeProfile:       capCfg.RuntimeProfile,
			SandboxProfile:       capCfg.SandboxProfile,
			Protocols:            setKeys(capCfg.Protocols),
			Workflows:            setKeys(capCfg.Workflows),
			AllowedAgents:        setKeys(capCfg.AllowedAgents),
			RequiredSecrets:      append([]string(nil), capCfg.RequiredSecrets...),
			RequiredApprovals:    capCfg.RequiredApprovals,
			AllowNetwork:         capCfg.AllowNetwork,
			AllowFilesystemWrite: capCfg.AllowFilesystemWrite,
			MaxTimeoutMs:         capCfg.MaxTimeoutMs,
			CostWeight:           capCfg.CostWeight,
			LatencyWeight:        capCfg.LatencyWeight,
			TrustScore:           capCfg.TrustScore,
			RiskScore:            capCfg.RiskScore,
			MaxConcurrency:       capCfg.MaxConcurrency,
			Fallbacks:            append([]string(nil), capCfg.Fallbacks...),
			Feedback: CapabilityFeedback{
				Successes:   fb.Successes,
				Failures:    fb.Failures,
				Violations:  fb.Violations,
				AvgLatency:  fb.AvgLatency,
				LastLatency: fb.LastLatency,
			},
			Running: g.running[capCfg.Name],
		})
	}
	return out
}

func setKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	return out
}
