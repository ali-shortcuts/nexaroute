package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func autopilotBaseConfig() Config {
	cfg := Default()
	cfg.Probe.Enabled = false
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.DecisionPolicies = []DecisionPolicyConfig{
		{ID: "balanced", Version: "2026-10-18.1", Weights: DecisionPolicyWeights{RouterBaseline: 1}},
		{ID: "canary-bad", Version: "2026-10-18.2", Weights: DecisionPolicyWeights{Cost: 1}},
	}
	return cfg
}

func TestAutopilotConfigValidRoundTrips(t *testing.T) {
	cfg := autopilotBaseConfig()
	cfg.Autopilot = AutopilotConfig{
		Enabled:           true,
		PolicyID:          "balanced",
		CandidatePolicyID: "canary-bad",
		CanaryPercent:     10,
		CanaryMaxRequests: 50,
		MinSamples:        20,
		MinPromoteSamples: 100,
		MaxErrorRate:      0.25,
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid autopilot config rejected: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Autopilot.CandidatePolicyID != "canary-bad" || back.Autopilot.CanaryPercent != 10 {
		t.Fatalf("autopilot config lost in round trip: %+v", back.Autopilot)
	}
}

func TestAutopilotConfigRejectsUnknownPolicies(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing target", func(c *Config) {
			c.Decision.Mode = ""
			c.Decision.Provider = ""
			c.Decision.Policy = ""
			c.Autopilot = AutopilotConfig{Enabled: true, CandidatePolicyID: "canary-bad"}
		}, "autopilot.policy_id is required"},
		{"unknown target", func(c *Config) {
			c.Autopilot = AutopilotConfig{Enabled: true, PolicyID: "nope"}
		}, "references unknown decision policy"},
		{"unknown candidate", func(c *Config) {
			c.Autopilot = AutopilotConfig{Enabled: true, PolicyID: "balanced", CandidatePolicyID: "ghost"}
		}, "candidate_policy_id"},
		{"candidate equals target", func(c *Config) {
			c.Autopilot = AutopilotConfig{Enabled: true, PolicyID: "balanced", CandidatePolicyID: "balanced"}
		}, "must differ"},
		{"canary percent out of range", func(c *Config) {
			c.Autopilot = AutopilotConfig{Enabled: true, PolicyID: "balanced", CandidatePolicyID: "canary-bad", CanaryPercent: 140}
		}, "canary_percent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := autopilotBaseConfig()
			tc.mutate(&cfg)
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error=%q want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}
