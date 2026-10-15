package video

import (
	"strings"
	"testing"
)

func validVideoRequest() VideoRequest {
	return VideoRequest{ProjectID: "project", Prompt: "scene", Mode: ModeTextToVideo, DurationSeconds: 4, AspectRatio: Aspect16x9, FPS: 24}
}

func TestVideoRequestValidationRejectsInvalidBoundaries(t *testing.T) {
	cases := []struct {
		name string
		edit func(*VideoRequest)
	}{
		{"missing project", func(r *VideoRequest) { r.ProjectID = " " }},
		{"missing prompt and shots", func(r *VideoRequest) { r.Prompt = "" }},
		{"unsupported mode", func(r *VideoRequest) { r.Mode = Mode("unknown") }},
		{"zero duration", func(r *VideoRequest) { r.DurationSeconds = 0 }},
		{"too long", func(r *VideoRequest) { r.DurationSeconds = 3601 }},
		{"bad aspect", func(r *VideoRequest) { r.AspectRatio = AspectRatio("4:3") }},
		{"bad fps low", func(r *VideoRequest) { r.FPS = 0 - 1 }},
		{"bad fps high", func(r *VideoRequest) { r.FPS = 121 }},
		{"negative cost", func(r *VideoRequest) { r.MaxCostUSD = -0.01 }},
		{"bad callback scheme", func(r *VideoRequest) { r.CallbackURL = "http://example.com/callback" }},
		{"bad callback host", func(r *VideoRequest) { r.CallbackURL = "https:///callback" }},
		{"bad shot", func(r *VideoRequest) { r.ShotPlan = []Shot{{ShotID: "", Prompt: "x", DurationSeconds: 1}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validVideoRequest()
			tc.edit(&r)
			if err := r.Validate(); err == nil || !strings.Contains(err.Error(), ErrInvalidRequest.Error()) {
				t.Fatalf("expected invalid request, got %v", err)
			}
		})
	}
}

func TestVideoRequestValidationAcceptsShotPlanAndNormalizesDefaults(t *testing.T) {
	r := VideoRequest{ProjectID: " p ", ShotPlan: []Shot{{ShotID: "s1", Prompt: "shot", DurationSeconds: 1}}, DurationSeconds: 1}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	n := r.Normalized()
	if n.ProjectID != "p" || n.Mode != ModeTextToVideo || n.AspectRatio != Aspect16x9 || n.FPS != 24 || n.OutputFormat != "mp4" || n.Priority != PriorityNormal {
		t.Fatalf("normalization failed: %+v", n)
	}
	tooMany := validVideoRequest()
	tooMany.ShotPlan = make([]Shot, 257)
	if err := tooMany.Validate(); err == nil {
		t.Fatal("oversized shot plan accepted")
	}
}

func TestVideoJobTransitionMatrixAndInvalidTransitions(t *testing.T) {
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
	}
	for from, tos := range allowed {
		if !CanTransition(from, from) {
			t.Fatalf("same state must be allowed: %s", from)
		}
		for _, to := range tos {
			if !CanTransition(from, to) {
				t.Fatalf("expected transition %s -> %s", from, to)
			}
		}
	}
	if CanTransition(JobState("unknown"), StateQueued) || CanTransition(StateCompleted, StateQueued) {
		t.Fatal("invalid transition accepted")
	}
	job := NewJob("job", validVideoRequest())
	if err := job.Transition(StateSubmitted); err == nil {
		t.Fatal("queued -> submitted should be rejected")
	}
	if err := job.Transition(StateAdmitted); err != nil {
		t.Fatal(err)
	}
}
