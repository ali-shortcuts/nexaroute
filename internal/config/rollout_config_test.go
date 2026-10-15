package config

import "testing"

// M5-3 rollout validation: percent range, canary required, and
// canary membership in an explicit candidate pool.

func rolloutConfig(canary string, percent int) Config {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "pa", Name: "A", Type: "openai_compatible", BaseURL: "https://a.example.com", Enabled: true,
		Models: []ModelConfig{{ID: "m", Model: "model-a", Enabled: true}},
	}, {
		ID: "pb", Name: "B", Type: "openai_compatible", BaseURL: "https://b.example.com", Enabled: true,
		Models: []ModelConfig{{ID: "m", Model: "model-b", Enabled: true}},
	}}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"pa/m", "pb/m"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{
		ID: "r1", CandidatePool: "pool",
		CanaryDeploymentID: canary, CanaryPercent: percent,
	}}
	cfg.ApplyDefaults()
	return cfg
}

func TestRolloutValidation(t *testing.T) {
	if err := rolloutConfig("pb/m", 25).Validate(); err != nil {
		t.Fatalf("valid rollout rejected: %v", err)
	}
	if err := rolloutConfig("pb/m", 101).Validate(); err == nil {
		t.Fatal("percent 101 accepted")
	}
	if err := rolloutConfig("pb/m", -1).Validate(); err == nil {
		t.Fatal("percent -1 accepted")
	}
	if err := rolloutConfig("", 25).Validate(); err == nil {
		t.Fatal("percent without canary accepted")
	}
	if err := rolloutConfig("pz/m", 25).Validate(); err == nil {
		t.Fatal("canary outside the pool accepted")
	}
	if err := rolloutConfig("", 0).Validate(); err != nil {
		t.Fatalf("rollout disabled should validate: %v", err)
	}
}
