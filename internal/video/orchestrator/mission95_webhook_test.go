package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// mission95WebhookStore is a minimal {List, Update} store for webhook Apply
// tests: it replays a scripted List result per call and records Updates.
type mission95WebhookStore struct {
	lists     [][]video.VideoJob
	listErrs  []error
	updated   []video.VideoJob
	updateErr error
	calls     int
}

func (s *mission95WebhookStore) List(_ context.Context) ([]video.VideoJob, error) {
	i := s.calls
	s.calls++
	if i < len(s.listErrs) && s.listErrs[i] != nil {
		return nil, s.listErrs[i]
	}
	if i < len(s.lists) {
		return s.lists[i], nil
	}
	return nil, nil
}

func (s *mission95WebhookStore) Update(_ context.Context, j video.VideoJob) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updated = append(s.updated, j)
	return nil
}

func mission95WebhookJob() video.VideoJob {
	return video.VideoJob{
		JobID: "wj", ProviderJobID: "remote-1", State: video.StateSubmitted,
		EstimatedCostUSD: 1, Provider: "fake", Model: "model-a",
	}
}

func mission95WebhookEvent(state video.JobState, actual *float64) video.ProviderJobStatus {
	return video.ProviderJobStatus{
		ProviderJobID: "remote-1", State: state, Progress: 1, ActualCostUSD: actual,
	}
}

func TestMission95WebhookApplyBranches(t *testing.T) {
	ctx := context.Background()
	// Empty event id is rejected before any store access.
	t.Run("empty event id", func(t *testing.T) {
		store := &mission95WebhookStore{}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "", mission95WebhookEvent(video.StateCompleted, nil)); err == nil || !strings.Contains(err.Error(), "event id is required") {
			t.Fatalf("empty event error=%v", err)
		}
		if store.calls != 0 {
			t.Fatalf("empty event store calls=%d, want 0", store.calls)
		}
	})
	// No job matches the provider job id.
	t.Run("no matching job", func(t *testing.T) {
		job := mission95WebhookJob()
		job.ProviderJobID = "other"
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}}}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err == nil || !strings.Contains(err.Error(), "webhook job not found") {
			t.Fatalf("no-match error=%v", err)
		}
	})
	// W1: the re-List after finding the job fails; Apply surfaces it.
	t.Run("relist error", func(t *testing.T) {
		job := mission95WebhookJob()
		store := &mission95WebhookStore{
			lists:    [][]video.VideoJob{{job}},
			listErrs: []error{nil, errors.New("list down")},
		}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err == nil || !strings.Contains(err.Error(), "list down") {
			t.Fatalf("relist error=%v", err)
		}
	})
	// W2: the job vanished between List calls.
	t.Run("relist not found", func(t *testing.T) {
		job := mission95WebhookJob()
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {}}}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err == nil || !strings.Contains(err.Error(), "webhook job not found") {
			t.Fatalf("relist not-found error=%v", err)
		}
	})
	// W3: a force-failed job ignores the event and is never updated,
	// including on a repeat event (which hits the seen set).
	t.Run("force failed ignored", func(t *testing.T) {
		job := mission95WebhookJob()
		job.OperatorForceFailed = true
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err != nil {
			t.Fatalf("force-failed first error=%v", err)
		}
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err != nil {
			t.Fatalf("force-failed repeat error=%v", err)
		}
		if len(store.updated) != 0 {
			t.Fatalf("force-failed updates=%d, want 0", len(store.updated))
		}
	})
	// A completed job replayed with a completion is a no-op, never
	// double-settling, and the repeat event id is seen.
	t.Run("completed replay noop", func(t *testing.T) {
		job := mission95WebhookJob()
		job.State = video.StateCompleted
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		ledger := &mission95Ledger{}
		p := NewWebhookProcessor(store)
		p.Ledger = ledger
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err != nil {
			t.Fatalf("completed replay error=%v", err)
		}
		if len(store.updated) != 0 || ledger.settleActualCalls != 0 || ledger.settleEstimateCalls != 0 {
			t.Fatalf("completed replay updated=%d settleActual=%d settleEstimate=%d", len(store.updated), ledger.settleActualCalls, ledger.settleEstimateCalls)
		}
	})
	// An invalid state transition is rejected.
	t.Run("invalid transition", func(t *testing.T) {
		job := mission95WebhookJob()
		job.State = video.StateCompleted
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateProcessing, nil)); err == nil || !strings.Contains(err.Error(), "invalid webhook state transition") {
			t.Fatalf("invalid transition error=%v", err)
		}
	})
	// W4: explicit actual cost with a failing actual settlement errors.
	t.Run("settle actual error", func(t *testing.T) {
		job := mission95WebhookJob()
		actual := 2.0
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		ledger := &mission95Ledger{settleActualErr: errors.New("ledger down")}
		p := NewWebhookProcessor(store)
		p.Ledger = ledger
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, &actual)); err == nil || !strings.Contains(err.Error(), "ledger down") {
			t.Fatalf("settle actual error=%v", err)
		}
	})
	// W5: no actual cost with a failing estimate settlement errors.
	t.Run("settle estimate error", func(t *testing.T) {
		job := mission95WebhookJob()
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		ledger := &mission95Ledger{settleEstimateErr: errors.New("ledger down")}
		p := NewWebhookProcessor(store)
		p.Ledger = ledger
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err == nil || !strings.Contains(err.Error(), "ledger down") {
			t.Fatalf("settle estimate error=%v", err)
		}
	})
	// W6: an Update failure surfaces.
	t.Run("update error", func(t *testing.T) {
		job := mission95WebhookJob()
		store := &mission95WebhookStore{
			lists: [][]video.VideoJob{{job}, {job}}, updateErr: errors.New("store down"),
		}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, nil)); err == nil || !strings.Contains(err.Error(), "store down") {
			t.Fatalf("update error=%v", err)
		}
	})
	// W7: legacy usage metadata actual cost reconciles without a ledger.
	t.Run("legacy usage actual", func(t *testing.T) {
		job := mission95WebhookJob()
		event := mission95WebhookEvent(video.StateCompleted, nil)
		event.Usage = map[string]string{"actual_cost_usd": "2.5"}
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		p := NewWebhookProcessor(store)
		if err := p.Apply(ctx, "ev-1", event); err != nil {
			t.Fatalf("legacy usage error=%v", err)
		}
		if len(store.updated) != 1 {
			t.Fatalf("legacy usage updates=%d, want 1", len(store.updated))
		}
		updated := store.updated[0]
		if !updated.CostReconciled || updated.ActualCostUSD != 2.5 || updated.CostBasis != "actual" {
			t.Fatalf("legacy usage job=%+v", updated)
		}
		if updated.CostDeltaUSD == nil || *updated.CostDeltaUSD != 1.5 {
			t.Fatalf("legacy usage delta=%+v, want 1.5", updated.CostDeltaUSD)
		}
	})
	// W8: explicit actual cost settles through the ledger.
	t.Run("explicit actual settles", func(t *testing.T) {
		job := mission95WebhookJob()
		actual := 3.0
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		ledger := &mission95Ledger{}
		p := NewWebhookProcessor(store)
		p.Ledger = ledger
		if err := p.Apply(ctx, "ev-1", mission95WebhookEvent(video.StateCompleted, &actual)); err != nil {
			t.Fatalf("explicit actual error=%v", err)
		}
		if ledger.settleActualCalls != 1 {
			t.Fatalf("settle actual calls=%d, want 1", ledger.settleActualCalls)
		}
		if len(store.updated) != 1 || !store.updated[0].CostReconciled || store.updated[0].ActualCostUSD != 3.0 {
			t.Fatalf("explicit actual job=%+v", store.updated)
		}
	})
	// A non-completed event updates progress and state without settling.
	t.Run("processing event no settle", func(t *testing.T) {
		job := mission95WebhookJob()
		store := &mission95WebhookStore{lists: [][]video.VideoJob{{job}, {job}}}
		ledger := &mission95Ledger{}
		p := NewWebhookProcessor(store)
		p.Ledger = ledger
		event := mission95WebhookEvent(video.StateProcessing, nil)
		event.Error = "transient upstream note"
		if err := p.Apply(ctx, "ev-1", event); err != nil {
			t.Fatalf("processing event error=%v", err)
		}
		if len(store.updated) != 1 || store.updated[0].State != video.StateProcessing || store.updated[0].LastError != "transient upstream note" {
			t.Fatalf("processing event job=%+v", store.updated)
		}
		if ledger.settleActualCalls != 0 || ledger.settleEstimateCalls != 0 {
			t.Fatalf("processing event settled actual=%d estimate=%d", ledger.settleActualCalls, ledger.settleEstimateCalls)
		}
	})
}
