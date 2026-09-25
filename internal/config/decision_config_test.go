package config

import (
	"fmt"
	"testing"
)

func TestDecisionDefaultsOff(t *testing.T) {
	cfg := Default()
	if cfg.Decision.Mode != "off" || cfg.Decision.Provider != "local" || cfg.Decision.TimeoutMS != 400 {
		t.Fatalf("defaults=%+v", cfg.Decision)
	}
	if len(cfg.DecisionProviders) != 0 {
		t.Fatalf("providers=%v", cfg.DecisionProviders)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestDecisionExistingConfigsUnaffected(t *testing.T) {
	// A config without any decision section keeps legacy behavior: no
	// external provider, no network, routing unchanged.
	cfg := Default()
	cfg.Decision = DecisionConfig{}
	cfg.ApplyDefaults()
	if cfg.Decision.Mode != "off" {
		t.Fatalf("mode=%q", cfg.Decision.Mode)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func assistedConfig() Config {
	cfg := Default()
	cfg.Decision = DecisionConfig{Mode: "assisted", Provider: "jev-main", TimeoutMS: 400}
	cfg.DecisionProviders = []DecisionProviderConfig{{
		ID: "jev-main", Type: "jev", Enabled: true,
		APIKeyEnv: "JEV_API_KEY", PrivacyMode: "metadata_only",
	}}
	return cfg
}

func TestDecisionAssistedValid(t *testing.T) {
	cfg := assistedConfig()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid assisted config rejected: %v", err)
	}
}

func TestDecisionValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad mode", func(c *Config) { c.Decision.Mode = "smart" }},
		{"timeout low", func(c *Config) { c.Decision.TimeoutMS = 1 }},
		{"timeout high", func(c *Config) { c.Decision.TimeoutMS = 999999 }},
		{"assisted builtin", func(c *Config) { c.Decision.Provider = "local" }},
		{"assisted dangling", func(c *Config) { c.Decision.Provider = "nope" }},
		{"assisted disabled", func(c *Config) { c.DecisionProviders[0].Enabled = false }},
		{"duplicate ids", func(c *Config) {
			c.DecisionProviders = append(c.DecisionProviders, c.DecisionProviders[0])
		}},
		{"builtin collision local", func(c *Config) { c.DecisionProviders[0].ID = "local" }},
		{"builtin collision policy", func(c *Config) { c.DecisionProviders[0].ID = "policy" }},
		{"bad id charset", func(c *Config) { c.DecisionProviders[0].ID = "a/b" }},
		{"unsupported type", func(c *Config) { c.DecisionProviders[0].Type = "openai" }},
		{"bad privacy", func(c *Config) { c.DecisionProviders[0].PrivacyMode = "full_context" }},
		{"bad env name", func(c *Config) { c.DecisionProviders[0].APIKeyEnv = "9BAD-NAME" }},
		{"local points external", func(c *Config) { c.Decision.Mode = "local" }},
		{"off dangling", func(c *Config) {
			c.Decision.Mode = "off"
			c.Decision.Provider = "ghost"
		}},
	}
	for _, c := range cases {
		cfg := assistedConfig()
		c.mutate(&cfg)
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s: must be rejected", c.name)
		}
	}
}

func TestDecisionLocalModesValid(t *testing.T) {
	for _, p := range []string{"local", "policy"} {
		cfg := Default()
		cfg.Decision = DecisionConfig{Mode: "local", Provider: p, TimeoutMS: 400}
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("local+%s rejected: %v", p, err)
		}
	}
	// Off with a configured-but-idle external entry is valid (explicit
	// opt-in stays off; no traffic).
	cfg := assistedConfig()
	cfg.Decision.Mode = "off"
	cfg.Decision.Provider = "jev-main"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("off with idle entry rejected: %v", err)
	}
}

func TestDecisionPrivacyDefaults(t *testing.T) {
	cfg := assistedConfig()
	cfg.DecisionProviders[0].PrivacyMode = ""
	cfg.ApplyDefaults()
	if cfg.DecisionProviders[0].PrivacyMode != "metadata_only" {
		t.Fatalf("privacy=%q", cfg.DecisionProviders[0].PrivacyMode)
	}
}

func TestDecisionProviderCountBound(t *testing.T) {
	cfg := Default()
	for i := 0; i < 100; i++ {
		cfg.DecisionProviders = append(cfg.DecisionProviders, DecisionProviderConfig{
			ID:   fmt.Sprintf("jprov-%d", i),
			Type: "jev", PrivacyMode: "metadata_only",
		})
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("unbounded provider count must be rejected")
	}
}

func TestDecisionAPIKeyResolution(t *testing.T) {
	t.Setenv("NEXAROUTE_TEST_JEV_KEY", "env-key")
	p := DecisionProviderConfig{APIKey: "literal", APIKeyEnv: "NEXAROUTE_TEST_JEV_KEY"}
	if p.ResolvedAPIKey() != "env-key" {
		t.Fatal("env must win over literal")
	}
	p2 := DecisionProviderConfig{APIKey: "literal"}
	if p2.ResolvedAPIKey() != "literal" {
		t.Fatal("literal fallback broken")
	}
}
