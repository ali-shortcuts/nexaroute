package nexaflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Task 8 scheduler: durable slots, no duplicate dispatch.
//
// A schedule fires once per interval slot. Tick claims the slot in the
// shared DeliveryLedger BEFORE starting anything: with a Postgres
// ledger the claim is atomic across processes and hosts; with a file
// ledger it is atomic across processes on one host. Exactly one
// claimant starts the run; every other claimant (same tick, another
// process, or a re-tick of the same slot) sees the claim taken and
// skips. The run itself is additionally started with the slot key as
// its idempotency key, so even a claim/start race that produced two
// start attempts yields one run.
//
// A slot whose Start fails is NOT retried by later ticks: the claim
// stands and the failure is reported in the TickResult, because
// silently re-firing a slot risks double execution of a side effect
// whose first attempt may have partially run. Operators re-fire
// explicitly with a manual trigger.
//
// Slots are computed by truncating now to the interval boundary, so
// Tick is idempotent for any now inside the same slot and needs no
// persisted last-fire timestamp (the ledger IS the durable state).

// Schedule is one recurring trigger. IntervalSeconds must be positive;
// Enabled=false schedules are skipped and leave in-flight runs alone.
type Schedule struct {
	DefinitionID    string `json:"definition_id"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Enabled         bool   `json:"enabled"`
}

// TickResult is one schedule's outcome for one Tick call.
type TickResult struct {
	DefinitionID string
	Slot         time.Time
	Started      bool
	RunID        string
	Err          error
}

// Scheduler dispatches due schedule slots through a delivery ledger.
type Scheduler struct {
	Ledger DeliveryLedger
	// Start creates the run for a claimed slot; slotKey is the run's
	// idempotency key. It is called at most once per slot per ledger.
	Start func(ctx context.Context, definitionID, slotKey string) (string, error)
}

func NewScheduler(ledger DeliveryLedger, start func(ctx context.Context, definitionID, slotKey string) (string, error)) (*Scheduler, error) {
	if ledger == nil {
		return nil, errors.New("nexaflow: scheduler requires a delivery ledger")
	}
	if start == nil {
		return nil, errors.New("nexaflow: scheduler requires a start function")
	}
	return &Scheduler{Ledger: ledger, Start: start}, nil
}

// Tick dispatches every due slot in schedules at now.
func (s *Scheduler) Tick(ctx context.Context, schedules []Schedule, now time.Time) []TickResult {
	var out []TickResult
	for _, sch := range schedules {
		if !sch.Enabled || sch.DefinitionID == "" || sch.IntervalSeconds <= 0 {
			continue
		}
		slot := now.Truncate(time.Duration(sch.IntervalSeconds) * time.Second)
		key := fmt.Sprintf("sched:%s:%d", sch.DefinitionID, slot.Unix())
		res := TickResult{DefinitionID: sch.DefinitionID, Slot: slot}
		claimed, err := s.Ledger.Claim(ctx, key)
		if err != nil {
			res.Err = fmt.Errorf("nexaflow: schedule claim: %w", err)
			out = append(out, res)
			continue
		}
		if !claimed {
			out = append(out, res)
			continue
		}
		runID, err := s.Start(ctx, sch.DefinitionID, key)
		if err != nil {
			res.Err = err
			out = append(out, res)
			continue
		}
		res.Started = true
		res.RunID = runID
		out = append(out, res)
	}
	return out
}
