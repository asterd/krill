package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/krill/krill/config"
)

func TestControlPlaneScheduleCRUDAndTrigger(t *testing.T) {
	var (
		mu        sync.Mutex
		triggered []Trigger
	)
	engine, err := NewEngine(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:              "nightly",
			CronExpr:        "* * * * *",
			Target:          "wf-nightly",
			PayloadTemplate: "run",
			Enabled:         true,
		}},
	}, func(_ context.Context, trigger Trigger) error {
		mu.Lock()
		triggered = append(triggered, trigger)
		mu.Unlock()
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := engine.PauseSchedule("nightly"); err != nil {
		t.Fatal(err)
	}
	status, ok := engine.ScheduleStatus("nightly")
	if !ok || status.Config.Enabled {
		t.Fatalf("expected paused schedule, got %+v found=%v", status, ok)
	}

	if _, err := engine.ResumeSchedule("nightly"); err != nil {
		t.Fatal(err)
	}
	status, ok = engine.ScheduleStatus("nightly")
	if !ok || !status.Config.Enabled {
		t.Fatalf("expected resumed schedule, got %+v found=%v", status, ok)
	}

	if _, err := engine.CreateOrUpdateSchedule(config.ScheduleConfig{
		ID:              "nightly",
		CronExpr:        "*/5 * * * *",
		Target:          "wf-nightly",
		PayloadTemplate: "run-2",
		Enabled:         true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := engine.TriggerNow(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(triggered)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	count := len(triggered)
	mu.Unlock()
	if count == 0 {
		t.Fatal("expected control-plane trigger execution")
	}

	history := engine.ScheduleHistory("nightly")
	if len(history) < 2 {
		t.Fatalf("expected audit history, got %+v", history)
	}

	if _, err := engine.CancelSchedule("nightly"); err != nil {
		t.Fatal(err)
	}
	status, ok = engine.ScheduleStatus("nightly")
	if !ok || status.Config.Enabled {
		t.Fatalf("expected cancelled schedule to be disabled, got %+v", status)
	}
}
