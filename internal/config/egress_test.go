package config

import "testing"

func TestEgressConfigValidation(t *testing.T) {
	valid := EgressConfig{
		DenyHosts:  []string{"evil.example.com"},
		DenyCIDRs:  []string{"203.0.113.0/24", "198.51.100.7"},
		AllowHosts: []string{"trusted.example.com"},
		AllowCIDRs: []string{"192.0.2.0/24"},
	}
	if err := valid.validate(); err != nil {
		t.Errorf("expected valid egress config, got: %v", err)
	}

	invalid := EgressConfig{DenyCIDRs: []string{"not-a-cidr"}}
	if err := invalid.validate(); err == nil {
		t.Error("expected validation error for invalid CIDR")
	}

	invalid2 := EgressConfig{AllowCIDRs: []string{"999.999.999.0/24"}}
	if err := invalid2.validate(); err == nil {
		t.Error("expected validation error for invalid allow CIDR")
	}
}

func TestEgressResolvedIntoProviders(t *testing.T) {
	c := Default()
	c.Egress.DenyHosts = []string{"blocked.example.com"}
	c.Egress.DenyCIDRs = []string{"203.0.113.0/24"}
	c.Providers = []ProviderConfig{{
		ID:      "test",
		Name:    "Test",
		Type:    "openai_compatible",
		BaseURL: "https://api.example.com/v1",
		Enabled: true,
		Models:  []ModelConfig{{ID: "m1", Model: "model-1", Enabled: true}},
	}}
	c.ApplyDefaults()
	if got := c.Providers[0].EgressDenyHosts(); len(got) != 1 || got[0] != "blocked.example.com" {
		t.Errorf("expected resolved deny hosts, got %v", got)
	}
	if got := c.Providers[0].EgressDenyCIDRs(); len(got) != 1 || got[0] != "203.0.113.0/24" {
		t.Errorf("expected resolved deny CIDRs, got %v", got)
	}
}
