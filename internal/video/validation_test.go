package video

import (
	"errors"
	"strings"
	"testing"
)

func TestVideoRequestValidateBoundaries(t *testing.T) {
	valid := VideoRequest{ProjectID: "proj", Prompt: "a cat", DurationSeconds: 10}
	if err := valid.Validate(); err != nil {
		t.Fatalf("baseline request must be valid: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(r *VideoRequest)
		wantIn string
	}{
		{"missing project", func(r *VideoRequest) { r.ProjectID = "  " }, "project_id is required"},
		{"no prompt no shots", func(r *VideoRequest) { r.Prompt = "" }, "prompt or shot_plan is required"},
		{"bad mode", func(r *VideoRequest) { r.Mode = "hologram" }, "unsupported mode"},
		{"zero duration", func(r *VideoRequest) { r.DurationSeconds = 0 }, "duration_seconds"},
		{"negative duration", func(r *VideoRequest) { r.DurationSeconds = -5 }, "duration_seconds"},
		{"duration too long", func(r *VideoRequest) { r.DurationSeconds = 3601 }, "duration_seconds"},
		{"bad aspect", func(r *VideoRequest) { r.AspectRatio = "4:3" }, "unsupported aspect_ratio"},
		{"fps too low", func(r *VideoRequest) { r.FPS = -1 }, "fps"},
		{"fps too high", func(r *VideoRequest) { r.FPS = 121 }, "fps"},
		{"negative cost", func(r *VideoRequest) { r.MaxCostUSD = -0.01 }, "max_cost_usd"},
		{"http callback", func(r *VideoRequest) { r.CallbackURL = "http://example.com/hook" }, "callback_url must be an https URL"},
		{"hostless callback", func(r *VideoRequest) { r.CallbackURL = "https:///hook" }, "callback_url must be an https URL"},
		{"garbage callback", func(r *VideoRequest) { r.CallbackURL = "://nope" }, "callback_url must be an https URL"},
		{"shot missing prompt", func(r *VideoRequest) {
			r.ShotPlan = []Shot{{ShotID: "s1", Prompt: "", DurationSeconds: 5}}
		}, "invalid shot 0"},
		{"shot missing id", func(r *VideoRequest) {
			r.ShotPlan = []Shot{{ShotID: " ", Prompt: "x", DurationSeconds: 5}}
		}, "invalid shot 0"},
		{"shot zero duration", func(r *VideoRequest) {
			r.ShotPlan = []Shot{{ShotID: "s1", Prompt: "x", DurationSeconds: 0}}
		}, "invalid shot 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			r.ShotPlan = nil
			tc.mutate(&r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.wantIn)
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error must wrap ErrInvalidRequest: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error %q must mention %q", err.Error(), tc.wantIn)
			}
		})
	}

	// Boundary values that must be ACCEPTED.
	accepted := []struct {
		name   string
		mutate func(r *VideoRequest)
	}{
		{"duration exactly 3600", func(r *VideoRequest) { r.DurationSeconds = 3600 }},
		{"fps exactly 1", func(r *VideoRequest) { r.FPS = 1 }},
		{"fps exactly 120", func(r *VideoRequest) { r.FPS = 120 }},
		{"zero cost cap", func(r *VideoRequest) { r.MaxCostUSD = 0 }},
		{"https callback", func(r *VideoRequest) { r.CallbackURL = "https://example.com/hook" }},
		{"shots instead of prompt", func(r *VideoRequest) {
			r.Prompt = ""
			r.ShotPlan = []Shot{{ShotID: "s1", Prompt: "x", DurationSeconds: 5}}
		}},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			r := valid
			tc.mutate(&r)
			if err := r.Validate(); err != nil {
				t.Fatalf("expected valid, got: %v", err)
			}
		})
	}

	// Shot plan size cap.
	big := valid
	big.ShotPlan = make([]Shot, 257)
	for i := range big.ShotPlan {
		big.ShotPlan[i] = Shot{ShotID: "s", Prompt: "x", DurationSeconds: 1}
	}
	if err := big.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds 256 shots") {
		t.Fatalf("257 shots must be rejected, got: %v", err)
	}
	big.ShotPlan = big.ShotPlan[:256]
	if err := big.Validate(); err != nil {
		t.Fatalf("256 shots must be accepted, got: %v", err)
	}
}
