package config

import (
	"strings"
	"testing"
)

// Phase 19 (issue #419): task-intelligence route-profile fields are validated
// fail-closed — a misconfigured mode, pool reference, or override can never
// silently steer routing.
func TestTaskIntelValidationTable(t *testing.T) {
	cases := []struct {
		name    string
		profile RouteProfileConfig
		wantErr string
	}{
		{"defaults ok", RouteProfileConfig{ID: "r1", CandidatePool: "pool1"}, ""},
		{"off ok", RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "off"}, ""},
		{"suggest ok", RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "suggest"}, ""},
		{
			"apply with pools ok",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
				TaskPools:              map[string]string{"coding": "pool1", "long_context": "pool2"},
				TaskIntelMinConfidence: 0.7},
			"",
		},
		{
			"override ok",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply", TaskOverride: "coding"},
			"",
		},
		{
			"bad mode",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "on"},
			"task_intel_mode",
		},
		{
			"bad min confidence high",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply", TaskIntelMinConfidence: 1.5},
			"task_intel_min_confidence",
		},
		{
			"bad min confidence negative",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply", TaskIntelMinConfidence: -0.1},
			"task_intel_min_confidence",
		},
		{
			"unknown task type in pools",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
				TaskPools: map[string]string{"codnig": "pool1"}},
			"unknown task type",
		},
		{
			"unknown pool in pools",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
				TaskPools: map[string]string{"coding": "nope"}},
			"unknown candidate pool",
		},
		{
			"bad override",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply", TaskOverride: "codnig"},
			"task_override",
		},
		{
			"pools with mode off rejected",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1",
				TaskPools: map[string]string{"coding": "pool1"}},
			"task_intel_mode off",
		},
		{
			"override with mode off rejected",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskOverride: "coding"},
			"task_intel_mode off",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Providers = []ProviderConfig{{
				ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
				AuthMode: "bearer", Enabled: true,
				Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
			}}
			cfg.CandidatePools = []CandidatePoolConfig{
				{ID: "pool1", Mode: "all"},
				{ID: "pool2", Mode: "all"},
			}
			cfg.RouteProfiles = []RouteProfileConfig{tc.profile}
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

// The new fields round-trip through JSON unchanged (operator config files).
func TestTaskIntelFieldsJSONRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "bearer", Enabled: true,
		Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}, {ID: "pool2", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{
		ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
		TaskIntelMinConfidence: 0.75,
		TaskPools:              map[string]string{"coding": "pool2"},
		TaskOverride:           "",
	}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rp := cfg.RouteProfiles[0]
	if rp.TaskIntelMode != "apply" || rp.TaskIntelMinConfidence != 0.75 || rp.TaskPools["coding"] != "pool2" {
		t.Fatalf("fields not preserved: %+v", rp)
	}
}

// Phase 19 eval gate (#510): task_eval_gates are validated fail-closed —
// a gate needs a known task type, a bounded suite ID, and a mapped
// pool for the same task, and cannot be configured with mode off.
func TestTaskEvalGatesValidation(t *testing.T) {
	cases := []struct {
		name    string
		profile RouteProfileConfig
		wantErr string
	}{
		{
			"gate ok",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
				TaskPools:     map[string]string{"coding": "pool1"},
				TaskEvalGates: map[string]string{"coding": "coding-suite"}},
			"",
		},
		{
			"gate unknown task type",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
				TaskPools:     map[string]string{"coding": "pool1"},
				TaskEvalGates: map[string]string{"codnig": "coding-suite"}},
			"task_eval_gates has unknown task type",
		},
		{
			"gate empty suite",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
				TaskPools:     map[string]string{"coding": "pool1"},
				TaskEvalGates: map[string]string{"coding": "  "}},
			"must name an evaluation suite",
		},
		{
			"gate without pool",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1", TaskIntelMode: "apply",
				TaskPools:     map[string]string{"coding": "pool1"},
				TaskEvalGates: map[string]string{"long_context": "ctx-suite"}},
			"has no task_pools entry to gate",
		},
		{
			"gate with mode off rejected",
			RouteProfileConfig{ID: "r1", CandidatePool: "pool1",
				TaskPools:     map[string]string{"coding": "pool1"},
				TaskEvalGates: map[string]string{"coding": "coding-suite"}},
			"task_intel_mode off",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Providers = []ProviderConfig{{
				ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
				AuthMode: "bearer", Enabled: true,
				Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
			}}
			cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}, {ID: "pool2", Mode: "all"}}
			cfg.RouteProfiles = []RouteProfileConfig{tc.profile}
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}
