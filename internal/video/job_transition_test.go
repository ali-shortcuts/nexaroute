package video

import "testing"

// TestCanTransitionMatrix pins the complete 16x16 job state machine.
// Every pair not listed as allowed must be rejected, including all outgoing
// transitions from the terminal states (completed, cancelled, expired,
// dry_run).
func TestCanTransitionMatrix(t *testing.T) {
	all := []JobState{
		StateQueued, StateAdmitted, StatePlanning, StateWaitingForBudget,
		StateSubmitted, StateProcessing, StatePolling, StateComposing,
		StateUploading, StateCompleted, StateFailed, StateCancelRequested,
		StateCancelled, StateExpired, StateNeedsManualAction, StateDryRun,
	}
	allowed := map[JobState][]JobState{
		StateQueued:            {StateAdmitted, StateWaitingForBudget, StateFailed, StateCancelled, StateNeedsManualAction},
		StateAdmitted:          {StatePlanning, StateSubmitted, StateFailed, StateCancelRequested, StateNeedsManualAction},
		StatePlanning:          {StateSubmitted, StateFailed, StateNeedsManualAction},
		StateWaitingForBudget:  {StateQueued, StateFailed, StateNeedsManualAction},
		StateSubmitted:         {StateProcessing, StatePolling, StateCompleted, StateComposing, StateUploading, StateCancelRequested, StateCancelled, StateFailed, StateNeedsManualAction},
		StateProcessing:        {StateProcessing, StatePolling, StateComposing, StateUploading, StateCompleted, StateCancelRequested, StateCancelled, StateFailed, StateExpired, StateNeedsManualAction},
		StatePolling:           {StateProcessing, StatePolling, StateComposing, StateUploading, StateCompleted, StateCancelRequested, StateCancelled, StateFailed, StateExpired, StateNeedsManualAction},
		StateComposing:         {StateUploading, StateCompleted, StateFailed, StateNeedsManualAction},
		StateUploading:         {StateCompleted, StateCancelled, StateFailed, StateNeedsManualAction},
		StateCancelRequested:   {StateCancelled, StateFailed, StateNeedsManualAction},
		StateNeedsManualAction: {StateCancelled},
		StateFailed:            {StateQueued, StateCancelled},
		// Terminal: no outgoing transitions.
		StateCompleted: {},
		StateCancelled: {},
		StateExpired:   {},
		StateDryRun:    {},
	}
	for _, from := range all {
		allowedSet := map[JobState]bool{}
		for _, to := range allowed[from] {
			allowedSet[to] = true
		}
		for _, to := range all {
			want := allowedSet[to] || from == to // identity is always allowed
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	// Unknown states never transition anywhere (except identity).
	if CanTransition(JobState("bogus"), StateQueued) {
		t.Error("unknown from-state must not transition")
	}
	if CanTransition(StateQueued, JobState("bogus")) {
		t.Error("unknown to-state must not be reachable")
	}
}

func TestTransitionMethodEnforcesMatrix(t *testing.T) {
	j := NewJob("job-1", VideoRequest{})
	if j.State != StateQueued {
		t.Fatalf("new job must start queued, got %s", j.State)
	}
	if err := j.Transition(StateCompleted); err == nil {
		t.Fatal("queued -> completed must be rejected")
	}
	if j.State != StateQueued {
		t.Fatalf("rejected transition must not change state, got %s", j.State)
	}
	if err := j.Transition(StateAdmitted); err != nil {
		t.Fatalf("queued -> admitted must succeed: %v", err)
	}
	// Terminal state is sticky.
	done := NewJob("job-2", VideoRequest{})
	done.State = StateCompleted
	if err := done.Transition(StateQueued); err == nil {
		t.Fatal("completed must be terminal")
	}
}
