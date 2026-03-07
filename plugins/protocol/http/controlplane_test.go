package httpproto

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/controlplane"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/orchestrator"
	"github.com/krill/krill/internal/scheduler"
	"github.com/krill/krill/internal/session"
	"github.com/krill/krill/internal/skill"
)

func TestControlPlaneAuthzAndPlannerEndpoint(t *testing.T) {
	manager := installHTTPControlPlaneRuntime(t)
	p, err := New(map[string]interface{}{
		"control_plane": map[string]interface{}{
			"enabled": true,
			"tokens": map[string]interface{}{
				"viewer":   "viewer-token",
				"operator": "operator-token",
				"admin":    "admin-token",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/control/diagnostics", nil)
	w := httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/control/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected viewer diagnostics 200, got %d body=%s", w.Code, w.Body.String())
	}

	body, _ := json.Marshal(map[string]any{"dry_run": true, "progressive_mode": "enforce"})
	req = httptest.NewRequest(http.MethodPut, "/v1/control/planner/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer operator-token")
	req.Header.Set("X-Control-Plane-Actor", "alice")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected planner update 200, got %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/control/secrets", bytes.NewBufferString(`{"name":"KRILL_ROTATE","value":"x"}`))
	req.Header.Set("Authorization", "Bearer operator-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected operator secret rotate forbidden, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/control/secrets", bytes.NewBufferString(`{"name":"KRILL_ROTATE","value":"x"}`))
	req.Header.Set("Authorization", "Bearer admin-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected admin secret rotate 200, got %d body=%s", w.Code, w.Body.String())
	}

	if len(manager.AuditEvents()) == 0 {
		t.Fatal("expected control-plane audit events")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/control/capabilities", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected capabilities 200, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/control/capabilities", bytes.NewBufferString(`{"name":"notify:user","type":"notification_action","dry_run":true}`))
	req.Header.Set("Authorization", "Bearer operator-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected capability apply 200, got %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/control/schedules", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected schedules 200, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/control/schedules/nightly/pause", nil)
	req.Header.Set("Authorization", "Bearer operator-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected pause 200, got %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/control/schedules/nightly/history", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected schedule history 200, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/control/sessions", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected sessions 200, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/control/audit", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected audit 200, got %d", w.Code)
	}

	rollout, _ := manager.UpdatePlannerConfig("alice", "operator", orchestrator.PlannerConfigSnapshot{ProgressiveMode: "monitor"}, true)
	req = httptest.NewRequest(http.MethodGet, "/v1/control/rollouts", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected rollouts 200, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/control/rollouts/"+rollout.ID+"/rollback", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	w = httptest.NewRecorder()
	p.handleControlPlane(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusBadRequest {
		t.Fatalf("expected rollback handled, got %d body=%s", w.Code, w.Body.String())
	}
}

func installHTTPControlPlaneRuntime(t *testing.T) *controlplane.Manager {
	t.Helper()
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
	if _, err := sessions.Open(session.OpenRequest{ClientID: "c1", ThreadID: "t1"}, session.Provenance{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	return controlplane.Install(controlplane.Runtime{
		Config:       cfg,
		Orchestrator: orch,
		Scheduler:    sched,
		Sessions:     sessions,
	})
}
