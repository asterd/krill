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
	"github.com/krill/krill/internal/skill"
)

func BenchmarkManagerMutationPath(b *testing.B) {
	globalMu.Lock()
	global = nil
	globalMu.Unlock()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Root{
		Core:         config.CoreConfig{BusBuffer: 16, MaxClients: 4, MemoryBackend: "ram"},
		ControlPlane: config.ControlPlaneConfig{Path: filepath.Join(b.TempDir(), "control-plane.json"), AuditRetention: 5000},
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
	reg, _ := skill.NewRegistry(nil, cfg.Core, logger)
	orch, _ := orchestrator.New(cfg, bus.NewLocal(16), reg, memory.NewRAM(), nil, logger, nil)
	sched, _ := scheduler.NewEngine(cfg.Scheduler, func(context.Context, scheduler.Trigger) error { return nil }, logger)
	manager := Install(Runtime{Config: cfg, Orchestrator: orch, Scheduler: sched})
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		manager.UpdatePlannerConfig("bench", "operator", orchestrator.PlannerConfigSnapshot{ProgressiveMode: "enforce"}, false)
	}
	b.StopTimer()
	_ = manager.Flush()
}
