package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/krill/krill/config"
)

type ScheduleStatus struct {
	Config       config.ScheduleConfig `json:"config"`
	NextRun      time.Time             `json:"next_run"`
	Running      int                   `json:"running"`
	Cancelled    bool                  `json:"cancelled"`
	LastRecorded *AuditRecord          `json:"last_recorded,omitempty"`
}

func (e *Engine) ScheduleStatuses() []ScheduleStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ScheduleStatus, 0, len(e.entries))
	for _, ent := range e.entries {
		out = append(out, e.scheduleStatusLocked(ent.cfg.ID))
	}
	return out
}

func (e *Engine) ScheduleStatus(id string) (ScheduleStatus, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ent := range e.entries {
		if ent.cfg.ID == id {
			status := e.scheduleStatusLocked(id)
			return status, true
		}
	}
	return ScheduleStatus{}, false
}

func (e *Engine) CreateOrUpdateSchedule(sc config.ScheduleConfig) (ScheduleStatus, error) {
	if strings.TrimSpace(sc.ID) == "" {
		return ScheduleStatus{}, fmt.Errorf("schedule_id is required")
	}
	cronExpr, err := Parse(sc.CronExpr)
	if err != nil {
		return ScheduleStatus{}, err
	}
	loc, err := time.LoadLocation(defaultString(sc.Timezone, "UTC"))
	if err != nil {
		return ScheduleStatus{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	for _, ent := range e.entries {
		if ent.cfg.ID != sc.ID {
			continue
		}
		ent.cfg = normalizeScheduleConfig(sc)
		ent.expr = cronExpr
		ent.loc = loc
		ent.nextRun = nextRunAt(cronExpr, loc, now.Add(-time.Minute))
		return e.scheduleStatusLocked(sc.ID), nil
	}
	e.entries = append(e.entries, &entry{
		cfg:     normalizeScheduleConfig(sc),
		expr:    cronExpr,
		loc:     loc,
		nextRun: nextRunAt(cronExpr, loc, now.Add(-time.Minute)),
	})
	return e.scheduleStatusLocked(sc.ID), nil
}

func (e *Engine) PauseSchedule(id string) (ScheduleStatus, error) {
	return e.setScheduleEnabled(id, false)
}

func (e *Engine) ResumeSchedule(id string) (ScheduleStatus, error) {
	return e.setScheduleEnabled(id, true)
}

func (e *Engine) CancelSchedule(id string) (ScheduleStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ent := range e.entries {
		if ent.cfg.ID != id {
			continue
		}
		ent.cfg.Enabled = false
		if ent.cancel != nil {
			ent.cancel()
		}
		e.recordLocked(AuditRecord{
			ScheduleID: id,
			RunAt:      e.clock.Now().UTC(),
			Status:     "cancelled",
			Reason:     "control_plane_cancelled",
		})
		return e.scheduleStatusLocked(id), nil
	}
	return ScheduleStatus{}, fmt.Errorf("schedule %q not found", id)
}

func (e *Engine) ScheduleHistory(id string) []AuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []AuditRecord
	for _, rec := range e.audit {
		if rec.ScheduleID == id {
			out = append(out, rec)
		}
	}
	return out
}

func (e *Engine) TriggerNow(ctx context.Context, id string) error {
	e.mu.Lock()
	var target *entry
	for _, ent := range e.entries {
		if ent.cfg.ID == id {
			target = ent
			break
		}
	}
	e.mu.Unlock()
	if target == nil {
		return fmt.Errorf("schedule %q not found", id)
	}
	e.launch(ctx, target, e.clock.Now().UTC())
	return nil
}

func normalizeScheduleConfig(sc config.ScheduleConfig) config.ScheduleConfig {
	if strings.TrimSpace(sc.ConcurrencyPolicy) == "" {
		sc.ConcurrencyPolicy = "allow"
	}
	if strings.TrimSpace(sc.MissedRunPolicy) == "" {
		sc.MissedRunPolicy = "skip"
	}
	if strings.TrimSpace(sc.SessionMode) == "" {
		sc.SessionMode = "persistent"
	}
	return sc
}

func (e *Engine) setScheduleEnabled(id string, enabled bool) (ScheduleStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ent := range e.entries {
		if ent.cfg.ID != id {
			continue
		}
		ent.cfg.Enabled = enabled
		e.recordLocked(AuditRecord{
			ScheduleID: id,
			RunAt:      e.clock.Now().UTC(),
			Status:     "updated",
			Reason:     fmt.Sprintf("enabled=%t", enabled),
		})
		return e.scheduleStatusLocked(id), nil
	}
	return ScheduleStatus{}, fmt.Errorf("schedule %q not found", id)
}

func (e *Engine) scheduleStatusLocked(id string) ScheduleStatus {
	for _, ent := range e.entries {
		if ent.cfg.ID != id {
			continue
		}
		status := ScheduleStatus{
			Config:    ent.cfg,
			NextRun:   ent.nextRun,
			Running:   ent.running,
			Cancelled: !ent.cfg.Enabled && ent.cancel != nil,
		}
		for i := len(e.audit) - 1; i >= 0; i-- {
			if e.audit[i].ScheduleID == id {
				rec := e.audit[i]
				status.LastRecorded = &rec
				break
			}
		}
		return status
	}
	return ScheduleStatus{}
}

func (e *Engine) recordLocked(record AuditRecord) {
	e.audit = append(e.audit, record)
}
