package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/orchestrator"
	"github.com/krill/krill/internal/scheduler"
	"github.com/krill/krill/internal/session"
)

type Runtime struct {
	Config       *config.Root
	Orchestrator *orchestrator.Orch
	Scheduler    *scheduler.Engine
	Sessions     *session.Service
}

type AuditEvent struct {
	Seq        int64          `json:"seq"`
	ID         string         `json:"id"`
	Actor      string         `json:"actor"`
	Role       string         `json:"role"`
	Action     string         `json:"action"`
	Resource   string         `json:"resource"`
	OccurredAt time.Time      `json:"occurred_at"`
	Changes    map[string]any `json:"changes,omitempty"`
	PrevHash   string         `json:"prev_hash,omitempty"`
	Hash       string         `json:"hash"`
}

type SecretRevision struct {
	Name      string    `json:"name"`
	Revision  int       `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ApprovalRecord struct {
	RequestID string    `json:"request_id"`
	Status    string    `json:"status"`
	Approver  string    `json:"approver,omitempty"`
	Comment   string    `json:"comment,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type RolloutRecord struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Target    string         `json:"target"`
	Status    string         `json:"status"`
	DryRun    bool           `json:"dry_run"`
	CreatedAt time.Time      `json:"created_at"`
	AppliedAt time.Time      `json:"applied_at,omitempty"`
	Previous  map[string]any `json:"previous,omitempty"`
	Current   map[string]any `json:"current,omitempty"`
}

type Manager struct {
	mu sync.Mutex

	runtime  Runtime
	audit    []AuditEvent
	lastHash string
	seq      int64

	secrets   map[string]SecretRevision
	approvals map[string]ApprovalRecord
	rollouts  map[string]RolloutRecord

	plannerOverride     *orchestrator.PlannerConfigSnapshot
	capabilityOverrides map[string]config.CapabilityConfig
	scheduleOverrides   map[string]config.ScheduleConfig

	persistPath       string
	loadedPersistPath string
	auditRetention    int
	persistQueue      chan struct{}
	persistWG         sync.WaitGroup
}

var (
	globalMu sync.RWMutex
	global   *Manager
)

type persistedState struct {
	SchemaVersion       string                              `json:"schema_version"`
	PlannerOverride     *orchestrator.PlannerConfigSnapshot `json:"planner_override,omitempty"`
	CapabilityOverrides map[string]config.CapabilityConfig  `json:"capability_overrides,omitempty"`
	ScheduleOverrides   map[string]config.ScheduleConfig    `json:"schedule_overrides,omitempty"`
	Secrets             map[string]SecretRevision           `json:"secrets,omitempty"`
	Approvals           map[string]ApprovalRecord           `json:"approvals,omitempty"`
	Rollouts            map[string]RolloutRecord            `json:"rollouts,omitempty"`
	Audit               []AuditEvent                        `json:"audit,omitempty"`
	LastHash            string                              `json:"last_hash,omitempty"`
	Seq                 int64                               `json:"seq"`
}

func Install(rt Runtime) *Manager {
	globalMu.Lock()
	defer globalMu.Unlock()
	if global == nil {
		global = &Manager{
			secrets:             map[string]SecretRevision{},
			approvals:           map[string]ApprovalRecord{},
			rollouts:            map[string]RolloutRecord{},
			capabilityOverrides: map[string]config.CapabilityConfig{},
			scheduleOverrides:   map[string]config.ScheduleConfig{},
			persistQueue:        make(chan struct{}, 1),
		}
		go global.runPersistenceWorker()
	}
	global.runtime = rt
	global.configurePersistence(rt.Config)
	global.applyPersistedRuntime()
	return global
}

func Current() *Manager {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}

func (m *Manager) Flush() error {
	if m == nil {
		return nil
	}
	m.persistWG.Wait()
	return nil
}

func (m *Manager) configurePersistence(cfg *config.Root) {
	if cfg == nil {
		return
	}
	path := strings.TrimSpace(cfg.ControlPlane.Path)
	if path == "" {
		path = "./.krill/control-plane.json"
	}
	m.mu.Lock()
	m.persistPath = path
	m.auditRetention = cfg.ControlPlane.AuditRetention
	needsLoad := m.loadedPersistPath != path
	m.mu.Unlock()
	if needsLoad {
		m.loadPersisted(path)
	}
}

func (m *Manager) applyPersistedRuntime() {
	m.mu.Lock()
	planner := m.plannerOverride
	capabilities := make([]config.CapabilityConfig, 0, len(m.capabilityOverrides))
	for _, cfg := range m.capabilityOverrides {
		capabilities = append(capabilities, cfg)
	}
	schedules := make([]config.ScheduleConfig, 0, len(m.scheduleOverrides))
	for _, cfg := range m.scheduleOverrides {
		schedules = append(schedules, cfg)
	}
	m.mu.Unlock()

	if planner != nil && m.runtime.Orchestrator != nil {
		m.runtime.Orchestrator.UpdatePlannerConfig(*planner)
	}
	if m.runtime.Orchestrator != nil {
		for _, cfg := range capabilities {
			m.runtime.Orchestrator.UpsertCapability(cfg)
		}
	}
	if m.runtime.Scheduler != nil {
		for _, cfg := range schedules {
			_, _ = m.runtime.Scheduler.CreateOrUpdateSchedule(cfg)
		}
	}
}

func (m *Manager) runPersistenceWorker() {
	for range m.persistQueue {
		_ = m.persistSnapshot()
		m.persistWG.Done()
	}
}

func (m *Manager) enqueuePersist() {
	if m == nil {
		return
	}
	m.persistWG.Add(1)
	select {
	case m.persistQueue <- struct{}{}:
	default:
		m.persistWG.Done()
	}
}

func (m *Manager) persistSnapshot() error {
	m.mu.Lock()
	path := m.persistPath
	snapshot := persistedState{
		SchemaVersion:       "v1",
		PlannerOverride:     m.plannerOverride,
		CapabilityOverrides: cloneCapabilityMap(m.capabilityOverrides),
		ScheduleOverrides:   cloneScheduleMap(m.scheduleOverrides),
		Secrets:             cloneSecretMap(m.secrets),
		Approvals:           cloneApprovalMap(m.approvals),
		Rollouts:            cloneRolloutMap(m.rollouts),
		Audit:               append([]AuditEvent(nil), m.audit...),
		LastHash:            m.lastHash,
		Seq:                 m.seq,
	}
	m.mu.Unlock()

	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) loadPersisted(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			m.mu.Lock()
			m.loadedPersistPath = path
			m.mu.Unlock()
		}
		return
	}
	var snapshot persistedState
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plannerOverride = snapshot.PlannerOverride
	m.capabilityOverrides = cloneCapabilityMap(snapshot.CapabilityOverrides)
	m.scheduleOverrides = cloneScheduleMap(snapshot.ScheduleOverrides)
	m.secrets = cloneSecretMap(snapshot.Secrets)
	m.approvals = cloneApprovalMap(snapshot.Approvals)
	m.rollouts = cloneRolloutMap(snapshot.Rollouts)
	m.audit = append([]AuditEvent(nil), snapshot.Audit...)
	m.lastHash = snapshot.LastHash
	m.seq = snapshot.Seq
	m.loadedPersistPath = path
}

func (m *Manager) Diagnostics() map[string]any {
	result := map[string]any{
		"status": "ok",
	}
	if m == nil {
		result["status"] = "unavailable"
		return result
	}
	if m.runtime.Config != nil {
		if m.runtime.Orchestrator != nil {
			result["planner"] = m.runtime.Orchestrator.PlannerConfigSnapshot()
		}
		result["protocols"] = len(m.runtime.Config.Protocols)
	}
	if m.runtime.Orchestrator != nil {
		result["capabilities"] = len(m.runtime.Orchestrator.CapabilitySnapshots())
		result["plans"] = len(m.runtime.Orchestrator.PlanSnapshots())
	}
	if m.runtime.Scheduler != nil {
		result["schedules"] = m.runtime.Scheduler.ScheduleStatuses()
	}
	if m.runtime.Sessions != nil {
		result["sessions"] = len(m.runtime.Sessions.List())
	}
	result["audit_ok"] = m.VerifyAudit()
	return result
}

func (m *Manager) PlannerConfig() orchestrator.PlannerConfigSnapshot {
	if m == nil || m.runtime.Orchestrator == nil {
		return orchestrator.PlannerConfigSnapshot{}
	}
	return m.runtime.Orchestrator.PlannerConfigSnapshot()
}

func (m *Manager) UpdatePlannerConfig(actor, role string, cfg orchestrator.PlannerConfigSnapshot, dryRun bool) (RolloutRecord, orchestrator.PlannerConfigSnapshot) {
	current := m.PlannerConfig()
	next := current
	if strings.TrimSpace(cfg.ProgressiveMode) != "" {
		next.ProgressiveMode = cfg.ProgressiveMode
	}
	if strings.TrimSpace(cfg.DefaultSandboxProfile) != "" {
		next.DefaultSandboxProfile = cfg.DefaultSandboxProfile
	}
	if strings.TrimSpace(cfg.DefaultRuntimeProfile) != "" {
		next.DefaultRuntimeProfile = cfg.DefaultRuntimeProfile
	}
	record := m.newRollout("planner", "planner/default", dryRun, map[string]any{"previous": current}, map[string]any{"current": next})
	if !dryRun && m.runtime.Orchestrator != nil {
		next = m.runtime.Orchestrator.UpdatePlannerConfig(next)
		record.Status = "applied"
		record.AppliedAt = time.Now().UTC()
		m.mu.Lock()
		snap := next
		m.plannerOverride = &snap
		m.mu.Unlock()
		m.recordAudit(actor, role, "planner.update", "planner/default", map[string]any{"previous": current, "current": next})
	}
	m.storeRollout(record)
	return record, next
}

func (m *Manager) Capabilities() []orchestrator.CapabilitySnapshot {
	if m == nil || m.runtime.Orchestrator == nil {
		return nil
	}
	return m.runtime.Orchestrator.CapabilitySnapshots()
}

func (m *Manager) ApplyCapability(actor, role string, cfg config.CapabilityConfig, dryRun bool) (RolloutRecord, orchestrator.CapabilitySnapshot) {
	var previous any
	if m.runtime.Orchestrator != nil {
		previous = m.runtime.Orchestrator.CapabilitySnapshot(cfg.Name)
	}
	next := capabilityPreview(cfg)
	record := m.newRollout("capability", cfg.Name, dryRun, map[string]any{"previous": previous}, map[string]any{"current": next})
	if !dryRun && m.runtime.Orchestrator != nil {
		next = m.runtime.Orchestrator.UpsertCapability(cfg)
		record.Status = "applied"
		record.AppliedAt = time.Now().UTC()
		m.mu.Lock()
		m.capabilityOverrides[cfg.Name] = cfg
		m.mu.Unlock()
		m.recordAudit(actor, role, "capability.apply", cfg.Name, map[string]any{"previous": previous, "current": next})
	}
	m.storeRollout(record)
	return record, next
}

func capabilityPreview(cfg config.CapabilityConfig) orchestrator.CapabilitySnapshot {
	return orchestrator.CapabilitySnapshot{
		Name:                 cfg.Name,
		Type:                 cfg.Type,
		Ref:                  cfg.Ref,
		Description:          cfg.Description,
		ReleaseChannel:       cfg.ReleaseChannel,
		RuntimeProfile:       cfg.RuntimeProfile,
		SandboxProfile:       cfg.SandboxProfile,
		Protocols:            append([]string(nil), cfg.Protocols...),
		Workflows:            append([]string(nil), cfg.Workflows...),
		AllowedAgents:        append([]string(nil), cfg.AllowedAgents...),
		RequiredSecrets:      append([]string(nil), cfg.RequiredSecrets...),
		RequiredApprovals:    cfg.RequiredApprovals,
		AllowNetwork:         cfg.AllowNetwork,
		AllowFilesystemWrite: cfg.AllowFilesystemWrite,
		MaxTimeoutMs:         cfg.MaxTimeoutMs,
		CostWeight:           cfg.CostWeight,
		LatencyWeight:        cfg.LatencyWeight,
		TrustScore:           cfg.TrustScore,
		RiskScore:            cfg.RiskScore,
		MaxConcurrency:       cfg.MaxConcurrency,
		Fallbacks:            append([]string(nil), cfg.Fallbacks...),
	}
}

func (m *Manager) Rollback(actor, role, id string) (RolloutRecord, error) {
	m.mu.Lock()
	record, ok := m.rollouts[id]
	m.mu.Unlock()
	if !ok {
		return RolloutRecord{}, fmt.Errorf("rollout %q not found", id)
	}
	switch record.Kind {
	case "planner":
		prev, ok := record.Previous["previous"]
		if !ok || prev == nil {
			return RolloutRecord{}, fmt.Errorf("planner rollback payload missing")
		}
		var cfg orchestrator.PlannerConfigSnapshot
		data, _ := json.Marshal(prev)
		_ = json.Unmarshal(data, &cfg)
		if m.runtime.Orchestrator != nil {
			m.runtime.Orchestrator.UpdatePlannerConfig(cfg)
		}
		m.mu.Lock()
		snap := cfg
		m.plannerOverride = &snap
		m.mu.Unlock()
	case "schedule":
		prev, ok := record.Previous["previous"]
		if !ok || prev == nil {
			return RolloutRecord{}, fmt.Errorf("schedule rollback payload missing")
		}
		var sc config.ScheduleConfig
		data, _ := json.Marshal(prev)
		_ = json.Unmarshal(data, &sc)
		if m.runtime.Scheduler != nil {
			if _, err := m.runtime.Scheduler.CreateOrUpdateSchedule(sc); err != nil {
				return RolloutRecord{}, err
			}
		}
		m.mu.Lock()
		m.scheduleOverrides[sc.ID] = sc
		m.mu.Unlock()
	case "capability":
		prev, ok := record.Previous["previous"]
		if !ok || prev == nil {
			return RolloutRecord{}, fmt.Errorf("capability rollback payload missing")
		}
		var cfg config.CapabilityConfig
		data, _ := json.Marshal(prev)
		_ = json.Unmarshal(data, &cfg)
		if m.runtime.Orchestrator != nil {
			m.runtime.Orchestrator.UpsertCapability(cfg)
		}
		m.mu.Lock()
		m.capabilityOverrides[cfg.Name] = cfg
		m.mu.Unlock()
	default:
		return RolloutRecord{}, fmt.Errorf("rollback not supported for %s", record.Kind)
	}
	record.Status = "rolled_back"
	record.AppliedAt = time.Now().UTC()
	m.storeRollout(record)
	m.recordAudit(actor, role, "rollout.rollback", record.Target, map[string]any{"rollout_id": record.ID})
	return record, nil
}

func (m *Manager) ApplySchedule(actor, role string, sc config.ScheduleConfig, dryRun bool) (RolloutRecord, scheduler.ScheduleStatus, error) {
	var previous any
	if m.runtime.Scheduler != nil {
		if current, ok := m.runtime.Scheduler.ScheduleStatus(sc.ID); ok {
			previous = current.Config
		}
	}
	record := m.newRollout("schedule", sc.ID, dryRun, map[string]any{"previous": previous}, map[string]any{"current": sc})
	var status scheduler.ScheduleStatus
	var err error
	if !dryRun && m.runtime.Scheduler != nil {
		status, err = m.runtime.Scheduler.CreateOrUpdateSchedule(sc)
		if err != nil {
			return RolloutRecord{}, scheduler.ScheduleStatus{}, err
		}
		record.Status = "applied"
		record.AppliedAt = time.Now().UTC()
		m.mu.Lock()
		m.scheduleOverrides[sc.ID] = sc
		m.mu.Unlock()
		m.recordAudit(actor, role, "schedule.apply", sc.ID, map[string]any{"current": sc})
	}
	m.storeRollout(record)
	return record, status, nil
}

func (m *Manager) ScheduleStatuses() []scheduler.ScheduleStatus {
	if m == nil || m.runtime.Scheduler == nil {
		return nil
	}
	return m.runtime.Scheduler.ScheduleStatuses()
}

func (m *Manager) ScheduleStatus(id string) (scheduler.ScheduleStatus, bool) {
	if m == nil || m.runtime.Scheduler == nil {
		return scheduler.ScheduleStatus{}, false
	}
	return m.runtime.Scheduler.ScheduleStatus(id)
}

func (m *Manager) PauseSchedule(actor, role, id string) (scheduler.ScheduleStatus, error) {
	if m == nil || m.runtime.Scheduler == nil {
		return scheduler.ScheduleStatus{}, fmt.Errorf("scheduler unavailable")
	}
	status, err := m.runtime.Scheduler.PauseSchedule(id)
	if err == nil {
		m.mu.Lock()
		m.scheduleOverrides[id] = status.Config
		m.mu.Unlock()
		m.recordAudit(actor, role, "schedule.pause", id, nil)
	}
	return status, err
}

func (m *Manager) ResumeSchedule(actor, role, id string) (scheduler.ScheduleStatus, error) {
	if m == nil || m.runtime.Scheduler == nil {
		return scheduler.ScheduleStatus{}, fmt.Errorf("scheduler unavailable")
	}
	status, err := m.runtime.Scheduler.ResumeSchedule(id)
	if err == nil {
		m.mu.Lock()
		m.scheduleOverrides[id] = status.Config
		m.mu.Unlock()
		m.recordAudit(actor, role, "schedule.resume", id, nil)
	}
	return status, err
}

func (m *Manager) CancelSchedule(actor, role, id string) (scheduler.ScheduleStatus, error) {
	if m == nil || m.runtime.Scheduler == nil {
		return scheduler.ScheduleStatus{}, fmt.Errorf("scheduler unavailable")
	}
	status, err := m.runtime.Scheduler.CancelSchedule(id)
	if err == nil {
		m.mu.Lock()
		m.scheduleOverrides[id] = status.Config
		m.mu.Unlock()
		m.recordAudit(actor, role, "schedule.cancel", id, nil)
	}
	return status, err
}

func (m *Manager) TriggerSchedule(actor, role, id string) error {
	if m == nil || m.runtime.Scheduler == nil {
		return fmt.Errorf("scheduler unavailable")
	}
	err := m.runtime.Scheduler.TriggerNow(context.Background(), id)
	if err == nil {
		m.recordAudit(actor, role, "schedule.trigger", id, nil)
	}
	return err
}

func (m *Manager) ScheduleHistory(id string) []scheduler.AuditRecord {
	if m == nil || m.runtime.Scheduler == nil {
		return nil
	}
	return m.runtime.Scheduler.ScheduleHistory(id)
}

func (m *Manager) Plans() []orchestrator.PlanSnapshot {
	if m == nil || m.runtime.Orchestrator == nil {
		return nil
	}
	return m.runtime.Orchestrator.PlanSnapshots()
}

func (m *Manager) Plan(requestID string) (orchestrator.PlanSnapshot, bool) {
	if m == nil || m.runtime.Orchestrator == nil {
		return orchestrator.PlanSnapshot{}, false
	}
	return m.runtime.Orchestrator.PlanSnapshotByRequest(requestID)
}

func (m *Manager) Workflow(requestID string) (orchestrator.WorkflowState, bool) {
	if m == nil || m.runtime.Orchestrator == nil {
		return orchestrator.WorkflowState{}, false
	}
	return m.runtime.Orchestrator.WorkflowSnapshotByRequest(requestID)
}

func (m *Manager) Sessions() []session.Session {
	if m == nil || m.runtime.Sessions == nil {
		return nil
	}
	return m.runtime.Sessions.List()
}

func (m *Manager) Session(sessionID string) (session.Session, bool) {
	if m == nil || m.runtime.Sessions == nil {
		return session.Session{}, false
	}
	return m.runtime.Sessions.Snapshot(sessionID)
}

func (m *Manager) RotateSecret(actor, role, name, value string) SecretRevision {
	_ = os.Setenv(name, value)
	m.mu.Lock()
	defer m.mu.Unlock()
	rev := m.secrets[name]
	rev.Name = name
	rev.Revision++
	rev.UpdatedAt = time.Now().UTC()
	m.secrets[name] = rev
	m.recordAuditLocked(actor, role, "secret.rotate", name, map[string]any{"revision": rev.Revision})
	m.enqueuePersist()
	return rev
}

func (m *Manager) SecretRevisions() []SecretRevision {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SecretRevision, 0, len(m.secrets))
	for _, rev := range m.secrets {
		out = append(out, rev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) SetApproval(actor, role, requestID, status, comment string) ApprovalRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := ApprovalRecord{
		RequestID: requestID,
		Status:    status,
		Approver:  actor,
		Comment:   comment,
		UpdatedAt: time.Now().UTC(),
	}
	m.approvals[requestID] = rec
	m.recordAuditLocked(actor, role, "approval."+status, requestID, map[string]any{"comment": comment})
	m.enqueuePersist()
	return rec
}

func (m *Manager) Approval(requestID string) ApprovalRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.approvals[requestID]
}

func (m *Manager) Rollouts() []RolloutRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RolloutRecord, 0, len(m.rollouts))
	for _, rec := range m.rollouts {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Manager) AuditEvents() []AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditEvent(nil), m.audit...)
}

func (m *Manager) VerifyAudit() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := ""
	for _, evt := range m.audit {
		payload := map[string]any{
			"seq": evt.Seq, "id": evt.ID, "actor": evt.Actor, "role": evt.Role,
			"action": evt.Action, "resource": evt.Resource, "occurred_at": evt.OccurredAt,
			"changes": evt.Changes, "prev_hash": evt.PrevHash,
		}
		raw, _ := json.Marshal(payload)
		sum := sha256.Sum256(append([]byte(prev), raw...))
		if evt.PrevHash != prev || evt.Hash != hex.EncodeToString(sum[:]) {
			return false
		}
		prev = evt.Hash
	}
	return true
}

func (m *Manager) newRollout(kind, target string, dryRun bool, previous, current map[string]any) RolloutRecord {
	status := "dry_run"
	if !dryRun {
		status = "staged"
	}
	return RolloutRecord{
		ID:        uuid.NewString(),
		Kind:      kind,
		Target:    target,
		Status:    status,
		DryRun:    dryRun,
		CreatedAt: time.Now().UTC(),
		Previous:  previous,
		Current:   current,
	}
}

func (m *Manager) storeRollout(record RolloutRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollouts[record.ID] = record
	m.enqueuePersist()
}

func (m *Manager) recordAudit(actor, role, action, resource string, changes map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordAuditLocked(actor, role, action, resource, changes)
	m.enqueuePersist()
}

func (m *Manager) recordAuditLocked(actor, role, action, resource string, changes map[string]any) {
	m.seq++
	evt := AuditEvent{
		Seq:        m.seq,
		ID:         uuid.NewString(),
		Actor:      actor,
		Role:       role,
		Action:     action,
		Resource:   resource,
		OccurredAt: time.Now().UTC(),
		Changes:    changes,
		PrevHash:   m.lastHash,
	}
	payload := map[string]any{
		"seq": evt.Seq, "id": evt.ID, "actor": evt.Actor, "role": evt.Role,
		"action": evt.Action, "resource": evt.Resource, "occurred_at": evt.OccurredAt,
		"changes": evt.Changes, "prev_hash": evt.PrevHash,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(append([]byte(evt.PrevHash), raw...))
	evt.Hash = hex.EncodeToString(sum[:])
	m.lastHash = evt.Hash
	m.audit = append(m.audit, evt)
}

func cloneCapabilityMap(in map[string]config.CapabilityConfig) map[string]config.CapabilityConfig {
	if len(in) == 0 {
		return map[string]config.CapabilityConfig{}
	}
	out := make(map[string]config.CapabilityConfig, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneScheduleMap(in map[string]config.ScheduleConfig) map[string]config.ScheduleConfig {
	if len(in) == 0 {
		return map[string]config.ScheduleConfig{}
	}
	out := make(map[string]config.ScheduleConfig, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSecretMap(in map[string]SecretRevision) map[string]SecretRevision {
	if len(in) == 0 {
		return map[string]SecretRevision{}
	}
	out := make(map[string]SecretRevision, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneApprovalMap(in map[string]ApprovalRecord) map[string]ApprovalRecord {
	if len(in) == 0 {
		return map[string]ApprovalRecord{}
	}
	out := make(map[string]ApprovalRecord, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneRolloutMap(in map[string]RolloutRecord) map[string]RolloutRecord {
	if len(in) == 0 {
		return map[string]RolloutRecord{}
	}
	out := make(map[string]RolloutRecord, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
