package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluationSuitesPathDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	cfg.ApplyDefaults()
	if cfg.Evaluation.SuitesPath != "" {
		t.Fatalf("suites path must default empty: %q", cfg.Evaluation.SuitesPath)
	}
	if cfg.Evaluation.TelemetryIngestEnabled {
		t.Fatal("telemetry ingestion must be off by default")
	}
	if cfg.Evaluation.TelemetryMinSamples != defaultTelemetryMinSamples {
		t.Fatalf("telemetry min samples default = %d, want %d", cfg.Evaluation.TelemetryMinSamples, defaultTelemetryMinSamples)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default evaluation config must validate: %v", err)
	}

	// Suites path is trimmed like the other evaluation paths.
	cfg2 := Default()
	cfg2.Evaluation.SuitesPath = "  /etc/nexaroute/suites  "
	cfg2.ApplyDefaults()
	if cfg2.Evaluation.SuitesPath != "/etc/nexaroute/suites" {
		t.Fatalf("suites path not normalized: %q", cfg2.Evaluation.SuitesPath)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"overlong suites path", func(c *Config) { c.Evaluation.SuitesPath = string(make([]byte, maxEvaluationSuitesPath+1)) }},
		{"suites path control chars", func(c *Config) { c.Evaluation.SuitesPath = "/tmp/suites\npayload" }},
		{"zero telemetry min samples", func(c *Config) { c.Evaluation.TelemetryMinSamples = 0 }},
		{"negative telemetry min samples", func(c *Config) { c.Evaluation.TelemetryMinSamples = -1 }},
		{"oversized telemetry min samples", func(c *Config) { c.Evaluation.TelemetryMinSamples = maxTelemetryMinSamples + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Evaluation.SuitesPath = "/tmp/suites"
			cfg.ApplyDefaults()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("unsafe evaluation config accepted: %+v", cfg.Evaluation)
			}
		})
	}
}

func TestEvaluationTelemetryOptInRoundTrips(t *testing.T) {
	cfg := Default()
	cfg.Evaluation.Enabled = true
	cfg.Evaluation.TelemetryIngestEnabled = true
	cfg.Evaluation.TelemetryMinSamples = 7
	cfg.Evaluation.SuitesPath = "/etc/nexaroute/suites"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg.Evaluation)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "telemetry_ingest_enabled") || !strings.Contains(string(raw), "suites_path") {
		t.Fatalf("new evaluation keys missing from JSON: %s", raw)
	}
}
