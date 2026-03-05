package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
)

type fakeClock struct{ now time.Time }

func (f fakeClock) Now() time.Time { return f.now }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCronParseAndNextRunComputation(t *testing.T) {
	cronExpr, err := Parse("*/15 9 * * 1")
	if err != nil {
		t.Fatal(err)
	}
	got := cronExpr.Next(time.Date(2026, 3, 2, 9, 1, 0, 0, time.UTC))
	want := time.Date(2026, 3, 2, 9, 15, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("expected %s, got %s", want, got)
	}
	if _, err := Parse("* *"); err == nil {
		t.Fatal("expected invalid cron field count")
	}
	if _, err := Parse("61 * * * *"); err == nil {
		t.Fatal("expected invalid minute range")
	}
	if _, err := Parse("* 24 * * *"); err == nil {
		t.Fatal("expected invalid hour range")
	}
	if _, err := Parse("* * 0 * *"); err == nil {
		t.Fatal("expected invalid day-of-month range")
	}
	if _, err := Parse("* * * 13 *"); err == nil {
		t.Fatal("expected invalid month range")
	}
	if _, err := Parse("* * * * 7"); err == nil {
		t.Fatal("expected invalid day-of-week range")
	}
}

func TestConcurrencyPolicyForbidSkipsOverlappingRuns(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	engine, err := NewEngineWithClock(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:                "forbid",
			CronExpr:          "* * * * *",
			Target:            "wf",
			Enabled:           true,
			ConcurrencyPolicy: "forbid",
		}},
	}, func(ctx context.Context, trigger Trigger) error {
		started <- struct{}{}
		<-release
		return nil
	}, testLogger(), fakeClock{now: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	engine.Tick(context.Background(), time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	<-started
	engine.Tick(context.Background(), time.Date(2026, 3, 2, 10, 1, 0, 0, time.UTC))
	close(release)
	time.Sleep(30 * time.Millisecond)

	audit := engine.Audit()
	if len(audit) < 2 {
		t.Fatalf("expected audit entries, got %+v", audit)
	}
	foundSkip := false
	for _, rec := range audit {
		if rec.Status == "skipped" && rec.Reason == "concurrency_forbid" {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Fatalf("expected concurrency skip audit, got %+v", audit)
	}
}

func TestConcurrencyPolicyReplaceCancelsRunningAttempt(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	started := make(chan struct{}, 2)
	engine, err := NewEngineWithClock(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:                "replace",
			CronExpr:          "* * * * *",
			Target:            "wf",
			Enabled:           true,
			ConcurrencyPolicy: "replace",
		}},
	}, func(ctx context.Context, trigger Trigger) error {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return nil
		}
	}, testLogger(), fakeClock{now: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	engine.Tick(context.Background(), time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	<-started
	engine.Tick(context.Background(), time.Date(2026, 3, 2, 10, 1, 0, 0, time.UTC))
	<-cancelled
}

func TestRetryPolicyRecordsFailureInjectionAndSuccess(t *testing.T) {
	var attempts atomic.Int32
	engine, err := NewEngineWithClock(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:             "retry",
			CronExpr:       "* * * * *",
			Target:         "wf",
			Enabled:        true,
			RetryLimit:     1,
			RetryBackoffMs: 1,
		}},
	}, func(ctx context.Context, trigger Trigger) error {
		if attempts.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}, testLogger(), fakeClock{now: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	engine.Tick(context.Background(), time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	time.Sleep(40 * time.Millisecond)
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
	audit := engine.Audit()
	if len(audit) < 2 || audit[0].Status != "retry" || audit[len(audit)-1].Status != "success" {
		t.Fatalf("unexpected retry audit trail: %+v", audit)
	}
}

func TestScheduledWorkflowTriggerPublishesInboundAndAudit(t *testing.T) {
	localBus := bus.NewLocal(4)
	engine, err := NewEngineWithClock(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:              "publish",
			CronExpr:        "* * * * *",
			Target:          "workflow-nightly",
			PayloadTemplate: "run nightly",
			Enabled:         true,
			ClientID:        "scheduler-client",
			ThreadID:        "scheduler-thread",
			SessionMode:     "persistent",
			Tenant:          "tenant-nightly",
		}},
	}, BusExecutor(localBus), testLogger(), fakeClock{now: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	engine.Tick(context.Background(), time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	select {
	case env := <-localBus.Subscribe(bus.InboundKey):
		if env.Text != "run nightly" {
			t.Fatalf("unexpected payload: %+v", env)
		}
		if env.Meta["workflow_id"] != "workflow-nightly" {
			t.Fatalf("expected workflow target metadata, got %+v", env.Meta)
		}
	case <-time.After(time.Second):
		t.Fatal("expected inbound schedule publish")
	}
	audit := engine.Audit()
	if len(audit) != 1 || audit[0].Status != "success" {
		t.Fatalf("expected successful audit, got %+v", audit)
	}
}

func TestNewEngineStartAndMissedRunSkip(t *testing.T) {
	localBus := bus.NewLocal(2)
	engine, err := NewEngine(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:              "start",
			CronExpr:        "* * * * *",
			Target:          "wf",
			PayloadTemplate: "payload",
			Enabled:         true,
		}},
	}, BusExecutor(localBus), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	engine.Start(ctx, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	cancel()
	if len(engine.entries) != 1 {
		t.Fatalf("expected scheduler entry, got %d", len(engine.entries))
	}

	skipEngine, err := NewEngineWithClock(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:              "skip",
			CronExpr:        "* * * * *",
			Target:          "wf",
			Enabled:         true,
			MissedRunPolicy: "skip",
		}},
	}, func(ctx context.Context, trigger Trigger) error { return nil }, testLogger(), fakeClock{now: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	skipEngine.Tick(context.Background(), time.Date(2026, 3, 2, 10, 2, 0, 0, time.UTC))
	audit := skipEngine.Audit()
	if len(audit) != 1 || audit[0].Status != "skipped" {
		t.Fatalf("expected missed run skip audit, got %+v", audit)
	}
	if got := (realClock{}).Now(); got.IsZero() {
		t.Fatal("expected real clock now")
	}
	if err := BusExecutor(nil)(context.Background(), Trigger{}); err == nil {
		t.Fatal("expected nil bus executor error")
	}
}

func TestNewEngineTimezoneSupport(t *testing.T) {
	engine, err := NewEngineWithClock(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{
			ID:       "tz",
			CronExpr: "0 9 * * *",
			Target:   "wf",
			Enabled:  true,
			Timezone: "Europe/Rome",
		}},
	}, func(ctx context.Context, trigger Trigger) error { return nil }, testLogger(), fakeClock{now: time.Date(2026, 3, 2, 7, 30, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	runAt := engine.entries[0].nextRun
	if runAt.Hour() != 8 || runAt.Minute() != 0 {
		t.Fatalf("expected 09:00 Europe/Rome to become 08:00 UTC, got %s", runAt)
	}
	if _, err := NewEngineWithClock(config.SchedulerConfig{
		Schedules: []config.ScheduleConfig{{ID: "bad", CronExpr: "* * * * *", Target: "wf", Enabled: true, Timezone: "Mars/Base"}},
	}, func(ctx context.Context, trigger Trigger) error { return nil }, testLogger(), fakeClock{now: time.Now().UTC()}); err == nil {
		t.Fatal("expected invalid timezone error")
	}
}
