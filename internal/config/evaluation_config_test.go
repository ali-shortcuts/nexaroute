package config

import (
	"path/filepath"
	"testing"
)

func TestEvaluationDefaultsAreDisabledAndBounded(t *testing.T) {
	cfg := Default()
	cfg.ApplyDefaults()
	if cfg.Evaluation.Enabled {
		t.Fatal("evaluation must be opt-in: a default gateway evaluates nothing")
	}
	if cfg.Evaluation.MaxRuns != 64 || cfg.Evaluation.MaxScorecards != 1024 || cfg.Evaluation.MaxArtifacts != 128 {
		t.Fatalf("unexpected evaluation defaults: %+v", cfg.Evaluation)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default evaluation config must validate: %v", err)
	}
}

func TestEvaluationApplyDefaultsClampsToSafeBounds(t *testing.T) {
	cfg := Default()
	cfg.Evaluation = EvaluationConfig{
		Enabled:         true,
		MaxRuns:         1 << 20,
		MaxScorecards:   1 << 20,
		MaxArtifacts:    1 << 20,
		ImportPath:      "  /tmp/scorecards.json  ",
		StatePath:       " /tmp/eval-state.json ",
		LatencyTargetMS: 800,
		TTFTTargetMS:    200,
	}
	cfg.ApplyDefaults()
	if cfg.Evaluation.MaxRuns != maxEvaluationRuns || cfg.Evaluation.MaxScorecards != maxEvaluationScorecards || cfg.Evaluation.MaxArtifacts != maxEvaluationArtifacts {
		t.Fatalf("evaluation bounds not clamped: %+v", cfg.Evaluation)
	}
	if cfg.Evaluation.ImportPath != "/tmp/scorecards.json" || cfg.Evaluation.StatePath != "/tmp/eval-state.json" {
		t.Fatalf("evaluation paths not normalized: %+v", cfg.Evaluation)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("clamped evaluation config must validate: %v", err)
	}
}

func TestEvaluationValidateRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"negative max runs", func(c *Config) { c.Evaluation.MaxRuns = -1 }},
		{"negative max scorecards", func(c *Config) { c.Evaluation.MaxScorecards = -1 }},
		{"negative max artifacts", func(c *Config) { c.Evaluation.MaxArtifacts = -1 }},
		{"oversized max runs", func(c *Config) { c.Evaluation.MaxRuns = maxEvaluationRuns + 1 }},
		{"oversized max scorecards", func(c *Config) { c.Evaluation.MaxScorecards = maxEvaluationScorecards + 1 }},
		{"oversized max artifacts", func(c *Config) { c.Evaluation.MaxArtifacts = maxEvaluationArtifacts + 1 }},
		{"negative latency target", func(c *Config) { c.Evaluation.LatencyTargetMS = -1 }},
		{"oversized ttft target", func(c *Config) { c.Evaluation.TTFTTargetMS = 1e12 }},
		{"overlong import path", func(c *Config) { c.Evaluation.ImportPath = string(make([]byte, maxEvaluationImportBytes+1)) }},
		{"overlong state path", func(c *Config) { c.Evaluation.StatePath = string(make([]byte, maxEvaluationImportBytes+1)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.ApplyDefaults()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("unsafe evaluation config accepted: %+v", cfg.Evaluation)
			}
		})
	}
}

func TestEvaluationConfigRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.Providers = []ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
	cfg.Evaluation = EvaluationConfig{
		Enabled:         true,
		MaxRuns:         8,
		MaxScorecards:   16,
		MaxArtifacts:    4,
		ImportPath:      "/tmp/scorecards.json",
		StatePath:       "/tmp/eval-state.json",
		LatencyTargetMS: 1200,
		TTFTTargetMS:    300,
	}
	cfg.ApplyDefaults()
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Evaluation != cfg.Evaluation {
		t.Fatalf("evaluation config did not round-trip:\n want %+v\n got  %+v", cfg.Evaluation, loaded.Evaluation)
	}
}
