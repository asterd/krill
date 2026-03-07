package orchestrator

import (
	"testing"
	"time"

	"github.com/krill/krill/config"
)

func TestControlPlaneSnapshotsAndMutations(t *testing.T) {
	orch, _, _, cancel := newPlannerOrch(t, nil, nil)
	defer cancel()

	planner := orch.PlannerConfigSnapshot()
	if planner.ProgressiveMode != "enforce" {
		t.Fatalf("unexpected planner snapshot: %+v", planner)
	}
	updated := orch.UpdatePlannerConfig(PlannerConfigSnapshot{
		ProgressiveMode:       "monitor",
		DefaultSandboxProfile: "extended",
	})
	if updated.ProgressiveMode != "monitor" || updated.DefaultSandboxProfile != "extended" {
		t.Fatalf("unexpected planner update result: %+v", updated)
	}

	capability := orch.UpsertCapability(config.CapabilityConfig{
		Name:           "notify:user",
		Type:           "notification_action",
		ReleaseChannel: "stable",
		RuntimeProfile: "default",
		SandboxProfile: "strict",
		Fallbacks:      []string{"echo"},
	})
	if capability.Name != "notify:user" {
		t.Fatalf("unexpected capability snapshot: %+v", capability)
	}
	if len(orch.CapabilitySnapshots()) == 0 {
		t.Fatal("expected capability snapshots")
	}
	if !orch.DeleteCapability("notify:user") {
		t.Fatal("expected capability delete to succeed")
	}

	now := time.Now().UTC()
	orch.planMu.Lock()
	orch.planRuns["req-1"] = plan{
		ID:         "plan-1",
		RequestID:  "req-1",
		WorkflowID: "wf-1",
		Status:     "completed",
		Steps: []planStep{{
			ID:                 "step-1",
			Goal:               "deploy",
			SelectedCapability: "deploy:asset",
			Status:             "completed",
			Output:             "ok",
		}},
		Artifacts: []artifact{{Name: "deploy:asset", Type: "deployment", Content: "ok", Hash: "hash"}},
		Audit:     []planAuditEvent{{StepID: "step-1", Capability: "deploy:asset", Status: "completed", OccurredAt: now}},
		Escalations: []escalation{{
			Kind:   "approval",
			Reason: "approval_required",
		}},
		Attestation: attestation{InputHash: "in", ArtifactHash: "art", RuntimeProfile: "default", SandboxProfile: "strict"},
	}
	orch.planMu.Unlock()
	orch.wfMu.Lock()
	orch.wfStates["req-1"] = WorkflowState{
		WorkflowID: "wf-1",
		Hop:        2,
		TokensUsed: 42,
		HandoffChain: []HandoffEvent{{
			OriginAgent: "router",
			TargetAgent: "specialist",
			Reason:      "handoff",
			OccurredAt:  now,
		}},
	}
	orch.wfMu.Unlock()

	snapshot, ok := orch.PlanSnapshotByRequest("req-1")
	if !ok || snapshot.ID != "plan-1" || len(snapshot.Artifacts) != 1 {
		t.Fatalf("unexpected plan snapshot: %+v found=%v", snapshot, ok)
	}
	if got := orch.PlanSnapshots(); len(got) == 0 {
		t.Fatal("expected stored plan snapshots")
	}
	if state, ok := orch.WorkflowSnapshotByRequest("req-1"); !ok || state.WorkflowID != "wf-1" {
		t.Fatalf("unexpected workflow snapshot: %+v found=%v", state, ok)
	}
}
