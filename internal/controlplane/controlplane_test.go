package controlplane

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/orchestrator"
	"github.com/krill/krill/internal/scheduler"
	"github.com/krill/krill/internal/session"
	"github.com/krill/krill/internal/skill"
)

func TestManagerAppliesRolloutsAndAuditTamperCheck(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Root{
		Core: config.CoreConfig{BusBuffer: 16, MaxClients: 4, MemoryBackend: "ram"},
		Planner: config.PlannerConfig{
			ProgressiveMode:       "monitor",
			DefaultSandboxProfile: "balanced",
			DefaultRuntimeProfile: "default",
		},
		Scheduler: config.SchedulerConfig{
			Schedules: []config.ScheduleConfig{{
				ID:              "nightly",
				CronExpr:        "* * * * *",
				Target:          "wf-nightly",
				PayloadTemplate: "run",
				Enabled:         true,
			}},
		},
	}
	reg, err := skill.NewRegistry(nil, cfg.Core, logger)
	if err != nil {
		t.Fatal(err)
	}
	orch, err := orchestrator.New(cfg, bus.NewLocal(16), reg, memory.NewRAM(), nil, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.NewEngine(cfg.Scheduler, func(context.Context, scheduler.Trigger) error { return nil }, logger)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(config.SessionConfig{
		Enabled:                  true,
		Path:                     filepath.Join(t.TempDir(), "sessions.json"),
		RetentionMaxMessages:     10,
		SummarizationThreshold:   0,
		SummarizationKeepRecent:  0,
		DefaultMergeConflictMode: "last-write-wins",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Shutdown() })

	manager := Install(Runtime{Config: cfg, Orchestrator: orch, Scheduler: sched, Sessions: sessions})
	manager.mu.Lock()
	manager.audit = nil
	manager.lastHash = ""
	manager.seq = 0
	manager.rollouts = map[string]RolloutRecord{}
	manager.secrets = map[string]SecretRevision{}
	manager.approvals = map[string]ApprovalRecord{}
	manager.mu.Unlock()

	if diag := manager.Diagnostics(); diag["status"] != "ok" {
		t.Fatalf("unexpected diagnostics: %+v", diag)
	}

	rollout, planner := manager.UpdatePlannerConfig("alice", "operator", orchestrator.PlannerConfigSnapshot{
		ProgressiveMode: "enforce",
	}, false)
	if rollout.Status != "applied" || planner.ProgressiveMode != "enforce" {
		t.Fatalf("unexpected planner rollout result: %+v planner=%+v", rollout, planner)
	}

	capRollout, capability := manager.ApplyCapability("alice", "operator", config.CapabilityConfig{
		Name:              "deploy:asset",
		Type:              "deployment_action",
		ReleaseChannel:    "candidate",
		RuntimeProfile:    "opencode",
		SandboxProfile:    "extended",
		RequiredApprovals: 1,
	}, false)
	if capRollout.Status != "applied" || capability.Name != "deploy:asset" {
		t.Fatalf("unexpected capability rollout: %+v capability=%+v", capRollout, capability)
	}

	if _, status, err := manager.ApplySchedule("alice", "operator", config.ScheduleConfig{
		ID:              "nightly",
		CronExpr:        "*/5 * * * *",
		Target:          "wf-nightly",
		PayloadTemplate: "run-2",
		Enabled:         true,
	}, false); err != nil || status.Config.CronExpr != "*/5 * * * *" {
		t.Fatalf("unexpected schedule apply: status=%+v err=%v", status, err)
	}

	secret := manager.RotateSecret("alice", "admin", "KRILL_ROTATED_SECRET", "value")
	if secret.Revision != 1 {
		t.Fatalf("expected secret revision 1, got %+v", secret)
	}
	approval := manager.SetApproval("alice", "operator", "req-1", "approved", "go")
	if approval.Status != "approved" {
		t.Fatalf("unexpected approval: %+v", approval)
	}

	if len(manager.Capabilities()) == 0 {
		t.Fatal("expected capability snapshots")
	}
	if _, ok := manager.ScheduleStatus("nightly"); !ok {
		t.Fatal("expected schedule status")
	}
	if len(manager.ScheduleStatuses()) == 0 {
		t.Fatal("expected schedule statuses")
	}
	if err := manager.TriggerSchedule("alice", "operator", "nightly"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PauseSchedule("alice", "operator", "nightly"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ResumeSchedule("alice", "operator", "nightly"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CancelSchedule("alice", "operator", "nightly"); err != nil {
		t.Fatal(err)
	}
	if len(manager.ScheduleHistory("nightly")) == 0 {
		t.Fatal("expected schedule history")
	}
	if len(manager.SecretRevisions()) == 0 {
		t.Fatal("expected secret revisions")
	}
	if manager.Approval("req-1").Status != "approved" {
		t.Fatal("expected approval lookup")
	}
	if len(manager.Rollouts()) < 3 {
		t.Fatal("expected rollout history")
	}
	if len(manager.AuditEvents()) == 0 {
		t.Fatal("expected audit events")
	}

	if _, err := manager.Rollback("alice", "admin", capRollout.ID); err != nil {
		t.Fatalf("expected capability rollback, got %v", err)
	}
	if _, err := manager.Rollback("alice", "admin", rollout.ID); err != nil {
		t.Fatalf("expected planner rollback, got %v", err)
	}

	openSession, err := sessions.Open(session.OpenRequest{ClientID: "c1", ThreadID: "t1"}, session.Provenance{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.Sessions()) == 0 {
		t.Fatal("expected session list")
	}
	if _, ok := manager.Session(openSession.ID); !ok {
		t.Fatal("expected session snapshot")
	}

	if !manager.VerifyAudit() {
		t.Fatal("expected intact audit chain")
	}
	manager.mu.Lock()
	manager.audit[0].Resource = "tampered"
	manager.mu.Unlock()
	if manager.VerifyAudit() {
		t.Fatal("expected tamper detection failure")
	}
}

func TestManagerPersistsAndReplaysAcrossInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-plane.json")
	manager, cleanup := newPersistentManager(t, path)
	defer cleanup()

	if _, planner := manager.UpdatePlannerConfig("alice", "operator", orchestrator.PlannerConfigSnapshot{
		ProgressiveMode: "enforce",
	}, false); planner.ProgressiveMode != "enforce" {
		t.Fatalf("unexpected planner update: %+v", planner)
	}
	if _, _, err := manager.ApplySchedule("alice", "operator", config.ScheduleConfig{
		ID:              "nightly",
		CronExpr:        "*/5 * * * *",
		Target:          "wf-nightly",
		PayloadTemplate: "run-2",
		Enabled:         true,
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, capability := manager.ApplyCapability("alice", "operator", config.CapabilityConfig{
		Name:           "notify:user",
		Type:           "notification_action",
		ReleaseChannel: "stable",
		RuntimeProfile: "default",
		SandboxProfile: "strict",
	}, false); capability.Name != "notify:user" {
		t.Fatalf("unexpected capability apply: %+v", capability)
	}
	manager.SetApproval("alice", "operator", "req-1", "approved", "ok")
	if err := manager.Flush(); err != nil {
		t.Fatal(err)
	}

	globalMu.Lock()
	global = nil
	globalMu.Unlock()

	replayed, cleanupReplay := newPersistentManager(t, path)
	defer cleanupReplay()
	if replayed.PlannerConfig().ProgressiveMode != "enforce" {
		t.Fatalf("expected planner replay, got %+v", replayed.PlannerConfig())
	}
	if _, ok := replayed.ScheduleStatus("nightly"); !ok {
		t.Fatal("expected schedule replay")
	}
	foundCapability := false
	for _, capability := range replayed.Capabilities() {
		if capability.Name == "notify:user" {
			foundCapability = true
			break
		}
	}
	if !foundCapability {
		t.Fatal("expected capability replay")
	}
	if replayed.Approval("req-1").Status != "approved" {
		t.Fatal("expected approval replay")
	}
	if len(replayed.AuditEvents()) == 0 {
		t.Fatal("expected audit replay")
	}
}

func newPersistentManager(t *testing.T, path string) (*Manager, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Root{
		Core:         config.CoreConfig{BusBuffer: 16, MaxClients: 4, MemoryBackend: "ram"},
		ControlPlane: config.ControlPlaneConfig{Path: path, AuditRetention: 5000},
		Planner: config.PlannerConfig{
			ProgressiveMode:       "monitor",
			DefaultSandboxProfile: "balanced",
			DefaultRuntimeProfile: "default",
		},
		Scheduler: config.SchedulerConfig{
			Schedules: []config.ScheduleConfig{{
				ID:              "nightly",
				CronExpr:        "* * * * *",
				Target:          "wf-nightly",
				PayloadTemplate: "run",
				Enabled:         true,
			}},
		},
	}
	reg, err := skill.NewRegistry(nil, cfg.Core, logger)
	if err != nil {
		t.Fatal(err)
	}
	orch, err := orchestrator.New(cfg, bus.NewLocal(16), reg, memory.NewRAM(), nil, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.NewEngine(cfg.Scheduler, func(context.Context, scheduler.Trigger) error { return nil }, logger)
	if err != nil {
		t.Fatal(err)
	}
	manager := Install(Runtime{Config: cfg, Orchestrator: orch, Scheduler: sched})
	return manager, func() {
		_ = manager.Flush()
	}
}
