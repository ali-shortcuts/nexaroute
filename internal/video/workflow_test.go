package video

import "testing"

func stepStatus(t *testing.T, steps []WorkflowStep, key string) string {
	t.Helper()
	for _, s := range steps {
		if s.Key == key {
			return s.Status
		}
	}
	t.Fatalf("step %q missing from %v", key, steps)
	return ""
}

func TestWorkflowStepsProjection(t *testing.T) {
	asset := Asset{ID: "a1"}
	cases := []struct {
		name string
		job  VideoJob
		want map[string]string
	}{
		{
			name: "queued job: only validation done, inapplicable steps skipped",
			job:  VideoJob{State: StateQueued, TotalShots: 1},
			want: map[string]string{"validate": "done", "select": "pending", "admit": "pending", "generate": "pending", "compose": "skipped", "narrate": "skipped", "deliver": "pending"},
		},
		{
			name: "processing multi-shot with provider job: submit done, generation active, composition pending",
			job:  VideoJob{State: StateProcessing, Provider: "fake", ProviderJobID: "pj-1", TotalShots: 2, JobType: JobCompose},
			want: map[string]string{"select": "done", "admit": "done", "submit": "done", "generate": "active", "compose": "pending", "deliver": "pending"},
		},
		{
			name: "completed single-shot with asset: chain done, compose skipped",
			job:  VideoJob{State: StateCompleted, Provider: "fake", ProviderJobID: "pj-1", TotalShots: 1, OutputAssets: []Asset{asset}},
			want: map[string]string{"generate": "done", "validate_output": "done", "compose": "skipped", "deliver": "done"},
		},
		{
			name: "completed without an asset surfaces attention on delivery",
			job:  VideoJob{State: StateCompleted, Provider: "fake", ProviderJobID: "pj-1", TotalShots: 1},
			want: map[string]string{"deliver": "attention"},
		},
		{
			name: "failed during generation marks generation failed",
			job:  VideoJob{State: StateFailed, Provider: "fake", ProviderJobID: "pj-1", TotalShots: 1},
			want: map[string]string{"submit": "done", "generate": "failed", "deliver": "pending"},
		},
		{
			name: "needs manual action marks the stuck step attention",
			job:  VideoJob{State: StateNeedsManualAction, Provider: "fake", ProviderJobID: "pj-1", TotalShots: 1},
			want: map[string]string{"generate": "attention"},
		},
		{
			name: "narration requested and asset present: narrate done",
			job: VideoJob{State: StateCompleted, Provider: "fake", ProviderJobID: "pj-1", TotalShots: 1,
				Request: VideoRequest{NarrationText: "hello"}, NarrationAsset: &asset, OutputAssets: []Asset{asset}},
			want: map[string]string{"narrate": "done"},
		},
		{
			name: "dry run executes nothing",
			job:  VideoJob{State: StateDryRun, TotalShots: 3},
			want: map[string]string{"validate": "done", "generate": "skipped", "deliver": "skipped"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := WorkflowSteps(tc.job)
			if len(steps) != 9 {
				t.Fatalf("steps = %d, want 9", len(steps))
			}
			for key, want := range tc.want {
				if got := stepStatus(t, steps, key); got != want {
					t.Errorf("step %s = %q, want %q (all: %v)", key, got, want, steps)
				}
			}
		})
	}
}
