package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/telemetry"
)

// Trigger is the execution payload emitted by the scheduler engine.
type Trigger struct {
	ScheduleID  string
	Target      string
	Payload     string
	RunAt       time.Time
	Attempt     int
	Tenant      string
	ClientID    string
	ThreadID    string
	SessionMode string
}

// AuditRecord captures the observable outcome of a scheduled run attempt.
type AuditRecord struct {
	ScheduleID string    `json:"schedule_id"`
	RunAt      time.Time `json:"run_at"`
	Attempt    int       `json:"attempt"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
}

// Executor runs a scheduler trigger.
type Executor func(context.Context, Trigger) error

// Clock abstracts time for deterministic scheduler tests.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Engine evaluates schedules and dispatches matching triggers.
type Engine struct {
	log      *slog.Logger
	clock    Clock
	executor Executor

	mu      sync.Mutex
	entries []*entry
	audit   []AuditRecord
}

type entry struct {
	cfg     config.ScheduleConfig
	expr    Cron
	loc     *time.Location
	nextRun time.Time
	running int
	cancel  context.CancelFunc
}

// NewEngine builds a scheduler engine using the real wall clock.
func NewEngine(cfg config.SchedulerConfig, exec Executor, log *slog.Logger) (*Engine, error) {
	return NewEngineWithClock(cfg, exec, log, realClock{})
}

// NewEngineWithClock builds a scheduler engine with an injected clock.
func NewEngineWithClock(cfg config.SchedulerConfig, exec Executor, log *slog.Logger, clock Clock) (*Engine, error) {
	if exec == nil {
		return nil, fmt.Errorf("executor is required")
	}
	if clock == nil {
		clock = realClock{}
	}
	e := &Engine{log: log, clock: clock, executor: exec}
	now := clock.Now()
	for _, sc := range cfg.Schedules {
		cronExpr, err := Parse(sc.CronExpr)
		if err != nil {
			return nil, fmt.Errorf("schedule %s: %w", sc.ID, err)
		}
		loc, err := time.LoadLocation(defaultString(sc.Timezone, "UTC"))
		if err != nil {
			return nil, fmt.Errorf("schedule %s timezone: %w", sc.ID, err)
		}
		e.entries = append(e.entries, &entry{
			cfg:     sc,
			expr:    cronExpr,
			loc:     loc,
			nextRun: nextRunAt(cronExpr, loc, now.Add(-time.Minute)),
		})
	}
	return e, nil
}

func (e *Engine) Start(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Second
	}
	ticker := time.NewTicker(tick)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.Tick(ctx, e.clock.Now())
			}
		}
	}()
}

func (e *Engine) Tick(ctx context.Context, now time.Time) {
	e.mu.Lock()
	entries := append([]*entry(nil), e.entries...)
	e.mu.Unlock()
	for _, ent := range entries {
		if !ent.cfg.Enabled {
			continue
		}
		e.tickEntry(ctx, ent, now.UTC())
	}
}

func (e *Engine) tickEntry(ctx context.Context, ent *entry, now time.Time) {
	e.mu.Lock()
	runAt := ent.nextRun
	if runAt.IsZero() {
		runAt = nextRunAt(ent.expr, ent.loc, now.Add(-time.Minute))
		ent.nextRun = runAt
	}
	if runAt.After(now) {
		e.mu.Unlock()
		return
	}
	for !ent.nextRun.After(now) {
		ent.nextRun = nextRunAt(ent.expr, ent.loc, ent.nextRun)
	}
	e.mu.Unlock()

	missed := now.Sub(runAt) >= time.Minute
	if missed && !strings.EqualFold(ent.cfg.MissedRunPolicy, "run_once") {
		e.record(AuditRecord{ScheduleID: ent.cfg.ID, RunAt: runAt, Status: "skipped", Reason: "missed_run_policy"})
		return
	}
	e.launch(ctx, ent, runAt)
}

func (e *Engine) launch(parent context.Context, ent *entry, runAt time.Time) {
	e.mu.Lock()
	switch strings.ToLower(strings.TrimSpace(ent.cfg.ConcurrencyPolicy)) {
	case "", "allow":
	case "forbid":
		if ent.running > 0 {
			e.mu.Unlock()
			e.record(AuditRecord{ScheduleID: ent.cfg.ID, RunAt: runAt, Status: "skipped", Reason: "concurrency_forbid"})
			return
		}
	case "replace":
		if ent.running > 0 && ent.cancel != nil {
			ent.cancel()
		}
	}
	runCtx, cancel := context.WithCancel(parent)
	ent.running++
	ent.cancel = cancel
	e.mu.Unlock()

	telemetry.IncCounter(telemetry.MetricSchedulerTriggers, 1, map[string]string{"schedule_id": ent.cfg.ID})
	go func() {
		defer func() {
			e.mu.Lock()
			ent.running--
			if ent.running == 0 {
				ent.cancel = nil
			}
			e.mu.Unlock()
			cancel()
		}()

		maxAttempts := ent.cfg.RetryLimit + 1
		if maxAttempts < 1 {
			maxAttempts = 1
		}
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			trigger := Trigger{
				ScheduleID:  ent.cfg.ID,
				Target:      ent.cfg.Target,
				Payload:     ent.cfg.PayloadTemplate,
				RunAt:       runAt,
				Attempt:     attempt,
				Tenant:      ent.cfg.Tenant,
				ClientID:    defaultString(ent.cfg.ClientID, "schedule:"+ent.cfg.ID),
				ThreadID:    defaultString(ent.cfg.ThreadID, "schedule:"+ent.cfg.ID),
				SessionMode: defaultString(ent.cfg.SessionMode, "persistent"),
			}
			err := e.executor(runCtx, trigger)
			if err == nil {
				e.record(AuditRecord{ScheduleID: ent.cfg.ID, RunAt: runAt, Attempt: attempt, Status: "success"})
				return
			}
			if attempt == maxAttempts {
				e.record(AuditRecord{ScheduleID: ent.cfg.ID, RunAt: runAt, Attempt: attempt, Status: "failed", Reason: err.Error()})
				return
			}
			e.record(AuditRecord{ScheduleID: ent.cfg.ID, RunAt: runAt, Attempt: attempt, Status: "retry", Reason: err.Error()})
			if ent.cfg.RetryBackoffMs > 0 {
				select {
				case <-runCtx.Done():
					e.record(AuditRecord{ScheduleID: ent.cfg.ID, RunAt: runAt, Attempt: attempt, Status: "cancelled", Reason: runCtx.Err().Error()})
					return
				case <-time.After(time.Duration(ent.cfg.RetryBackoffMs) * time.Millisecond):
				}
			}
		}
	}()
}

func (e *Engine) Audit() []AuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]AuditRecord(nil), e.audit...)
}

func (e *Engine) record(record AuditRecord) {
	e.mu.Lock()
	e.audit = append(e.audit, record)
	e.mu.Unlock()
}

// BusExecutor converts scheduler triggers into inbound bus envelopes.
func BusExecutor(b bus.Bus) Executor {
	return func(ctx context.Context, trigger Trigger) error {
		if b == nil {
			return fmt.Errorf("bus is nil")
		}
		meta := map[string]string{
			"schedule_id":  trigger.ScheduleID,
			"workflow_id":  trigger.Target,
			"target_agent": trigger.Target,
			"session_mode": trigger.SessionMode,
			"tenant":       trigger.Tenant,
			"request_id":   uuid.NewString(),
			"trace_id":     telemetry.NewTraceID(),
			"scheduled_at": trigger.RunAt.Format(time.RFC3339),
			"attempt":      strconv.Itoa(trigger.Attempt),
		}
		env := &bus.Envelope{
			ID:             uuid.NewString(),
			ClientID:       trigger.ClientID,
			ThreadID:       trigger.ThreadID,
			Role:           bus.RoleUser,
			Text:           trigger.Payload,
			SourceProtocol: "scheduler",
			Meta:           meta,
			CreatedAt:      trigger.RunAt,
		}
		return b.Publish(ctx, bus.InboundKey, env)
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nextRunAt(expr Cron, loc *time.Location, after time.Time) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	return expr.Next(after.In(loc)).UTC()
}
