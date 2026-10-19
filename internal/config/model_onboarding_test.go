package config

import (
	"strings"
	"testing"
)

// Issue #394: model onboarding provenance must be a closed enum so the
// admin UI can distinguish configured / discovered / tested / degraded
// states without an "unknown" fallback that hides state.
func TestModelOnboardingProvenanceValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m *ModelConfig)
		wantErr string
	}{
		{"empty defaults are accepted", func(m *ModelConfig) {}, ""},
		{"valid manual", func(m *ModelConfig) { m.Source = ModelSourceManual }, ""},
		{"valid discovered", func(m *ModelConfig) { m.Source = ModelSourceDiscovered }, ""},
		{"valid passed", func(m *ModelConfig) { m.TestState = ModelTestPassed }, ""},
		{"valid failed", func(m *ModelConfig) { m.TestState = ModelTestFailed }, ""},
		{"valid degraded", func(m *ModelConfig) { m.TestState = ModelTestDegraded }, ""},
		{"invalid source", func(m *ModelConfig) { m.Source = "imported" }, "must be manual or discovered"},
		{"invalid test state", func(m *ModelConfig) { m.TestState = "healthy" }, "must be untested, passed, failed or degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.ApplyDefaults()
			cfg.Probe.Enabled = false
			m := ModelConfig{ID: "m", Model: "m", Enabled: true, Weight: 1}
			tc.mutate(&m)
			cfg.Providers = []ProviderConfig{{
				ID: "p", Name: "P", Type: "openai_compatible",
				BaseURL: "https://example.invalid", Enabled: true,
				Models: []ModelConfig{m},
			}}
			cfg.ApplyDefaults()
			cfg.Probe.Enabled = false
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// The provenance helpers are the single source of truth for the enum used
// by both the loader and the Admin API.
func TestModelOnboardingProvenanceHelpers(t *testing.T) {
	for _, v := range []string{"", "manual", "discovered"} {
		if !ValidModelSource(v) {
			t.Errorf("ValidModelSource(%q) rejected", v)
		}
	}
	for _, v := range []string{"manual-source", "discovered-import", "TESTED"} {
		if ValidModelSource(v) {
			t.Errorf("ValidModelSource(%q) accepted", v)
		}
	}
	for _, v := range []string{"", "untested", "passed", "failed", "degraded"} {
		if !ValidModelTestState(v) {
			t.Errorf("ValidModelTestState(%q) rejected", v)
		}
	}
	for _, v := range []string{"pass", "healthy", "UNKNOWN"} {
		if ValidModelTestState(v) {
			t.Errorf("ValidModelTestState(%q) accepted", v)
		}
	}
}
