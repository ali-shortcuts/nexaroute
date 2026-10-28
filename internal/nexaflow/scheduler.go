package nexaflow

import (
	"context"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Scheduler dispatches schedule-triggered runs. It is claim-based: a fire
// slot is claimed in the store before any run starts, so two ticks for the
// same slot (two processes in one store, or a retried tick) dispatch at
// most one run. At most one run per schedule per tick is dispatched: if a
// slot is long overdue, ticks catch up one slot at a time instead of
// bursting.
//
// Schedule identity is definition id + trigger instance id, stable across
// republication: republishing a definition does not reset the fire clock.
// A renamed trigger instance is a new schedule and fires on the next tick.
type Scheduler struct {
	engine *Engine
	store  Store
	now    func() time.Time
}

// NewScheduler builds a Scheduler over an engine and store.
func NewScheduler(engine *Engine, store Store) *Scheduler {
	return &Scheduler{engine: engine, store: store, now: time.Now}
}

// Tick dispatches every due schedule once and returns the started runs.
// Runs execute as ServicePrincipal (see executor.go); node permissions are
// still enforced per node.
func (s *Scheduler) Tick(ctx context.Context) ([]*Run, error) {
	now := s.now().UTC()
	defs, err := s.store.ListPublished(ctx)
	if err != nil {
		return nil, err
	}
	var started []*Run
	for _, def := range defs {
		for _, decl := range def.Triggers {
			if decl.Kind != TriggerSchedule {
				continue
			}
			interval, _ := decl.Config["interval"].(string)
			daily, _ := decl.Config["daily"].(string)
			key := def.ID + ":" + decl.Name
			st, err := s.store.GetScheduleState(ctx, key)
			if err != nil {
				return started, err
			}
			slot, next, due := nextFire(st, interval, daily, now)
			if !due {
				continue
			}
			claimed, err := s.store.ClaimSchedule(ctx, key, slot)
			if err != nil {
				return started, err
			}
			if !claimed {
				continue
			}
			if err := s.store.SaveScheduleState(ctx, &ScheduleState{
				ScheduleKey: key, DefinitionID: def.ID, TriggerNode: decl.Name,
				LastFire: slot, NextFire: next,
			}); err != nil {
				return started, err
			}
			run, err := s.engine.StartRun(ctx, ServicePrincipal, def.ID, 0, TriggerSchedule, decl.Name, "sched:"+key+":"+slot.Format(time.RFC3339), map[string]any{
				"schedule_id": decl.Name,
				"fired_at":    slot.Format(time.RFC3339),
				"last_run":    lastRunString(st),
			})
			if err != nil {
				return started, err
			}
			started = append(started, run)
		}
	}
	return started, nil
}

func lastRunString(st *ScheduleState) string {
	if st == nil || st.LastFire.IsZero() {
		return ""
	}
	return st.LastFire.Format(time.RFC3339)
}

// nextFire computes the fire slot, the following slot, and whether the
// slot is due. Interval schedules fire every interval from the last fire
// (or now when never fired). Daily schedules fire at HH:MM UTC when that
// time has passed today and today has not fired yet.
func nextFire(st *ScheduleState, interval, daily string, now time.Time) (slot, next time.Time, due bool) {
	if interval != "" {
		d, err := time.ParseDuration(interval)
		if err != nil || d <= 0 {
			return time.Time{}, time.Time{}, false
		}
		if st == nil || st.NextFire.IsZero() {
			return now, now.Add(d), true
		}
		if !st.NextFire.After(now) {
			return st.NextFire, st.NextFire.Add(d), true
		}
		return time.Time{}, time.Time{}, false
	}
	if daily != "" {
		t, err := time.Parse("15:04", daily)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		slot = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
		if now.Before(slot) {
			return time.Time{}, time.Time{}, false
		}
		if st != nil && !st.LastFire.IsZero() {
			last := st.LastFire.UTC()
			if last.Year() == now.Year() && last.YearDay() == now.YearDay() {
				return time.Time{}, time.Time{}, false
			}
		}
		return slot, slot.Add(24 * time.Hour), true
	}
	return time.Time{}, time.Time{}, false
}

// SchedulePrincipal is the identity schedule runs execute as.
func SchedulePrincipal() authz.Identity { return ServicePrincipal }
