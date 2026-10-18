package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func policyConfigWith(version string, minObs int) Config {
	c := Config{
		Providers: []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "http://example.com", AuthMode: "none", Enabled: true,
			Models: []ModelConfig{{ID: "m1", Model: "m", Enabled: true}}}},
		DecisionPolicies: []DecisionPolicyConfig{{
			ID:              "balanced",
			Version:         version,
			Weights:         DecisionPolicyWeights{RouterBaseline: 1},
			MinObservations: minObs,
		}},
	}
	return c
}

func TestValidate_DecisionPolicyVersionAccepted(t *testing.T) {
	c := policyConfigWith("2026-10-18.1", 0)
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid version rejected: %v", err)
	}
	// Version and min_observations survive a JSON round trip (persistence compat).
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.DecisionPolicies) != 1 || back.DecisionPolicies[0].Version != "2026-10-18.1" {
		t.Fatalf("version lost in round trip: %+v", back.DecisionPolicies)
	}
}

func TestValidate_DecisionPolicyVersionRejectsUnsafe(t *testing.T) {
	for _, bad := range []string{"has space", "new\nline", strings.Repeat("x", maxDecisionPolicyVersion+1)} {
		c := policyConfigWith(bad, 0)
		c.ApplyDefaults()
		if err := c.Validate(); err == nil {
			t.Fatalf("unsafe version %q was accepted", bad)
		}
	}
}

func TestValidate_DecisionShadowRequiresActiveMode(t *testing.T) {
	c := policyConfigWith("", 0)
	c.Decision.Shadow = true
	c.Decision.Mode = "off"
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatal("shadow mode accepted with decision.mode=off")
	}
	c.Decision.Mode = "local"
	c.Decision.Provider = "policy"
	c.Decision.Policy = "balanced"
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("shadow with local mode rejected: %v", err)
	}
}

func TestValidate_DecisionPolicyMinObservationsBounds(t *testing.T) {
	if err := func() error { c := policyConfigWith("", -1); c.ApplyDefaults(); return c.Validate() }(); err == nil {
		t.Fatal("negative min_observations accepted")
	}
	if err := func() error {
		c := policyConfigWith("", maxPolicyMinObservations+1)
		c.ApplyDefaults()
		return c.Validate()
	}(); err == nil {
		t.Fatal("oversized min_observations accepted")
	}
	if err := func() error {
		c := policyConfigWith("", maxPolicyMinObservations)
		c.ApplyDefaults()
		return c.Validate()
	}(); err != nil {
		t.Fatalf("max min_observations rejected: %v", err)
	}
}
