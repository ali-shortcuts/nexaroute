package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAtomicModeAndNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Default()
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%#o want 0600", st.Mode().Perm())
	}

	if err := os.WriteFile(path+".bak", []byte("stale secret copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.MaxAttempts = 9
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("backup file must not be created; stat err=%v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".config.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files leaked: %v", leftovers)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.MaxAttempts != 9 {
		t.Fatalf("latest config was not persisted: %+v", loaded.Routing)
	}
}

func TestRoutingStrategiesValidate(t *testing.T) {
	for _, strategy := range []string{"ready_mesh", "ready_queue", "adaptive", "adaptive_round_robin", "priority", "round_robin", "least_latency"} {
		cfg := Default()
		cfg.Routing.Strategy = strategy
		if err := cfg.Validate(); err != nil {
			t.Fatalf("strategy %s rejected: %v", strategy, err)
		}
	}
	cfg := Default()
	cfg.Routing.Strategy = "not-real"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "routing.strategy") {
		t.Fatalf("bad strategy should fail, got %v", err)
	}
}

func TestLoadAppliesRuntimeEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveAtomic(path, Default()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_LISTEN", "0.0.0.0:19000")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "env-admin")
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "false")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:19000" || cfg.Admin.APIKey != "env-admin" || cfg.Admin.BindLocalOnly {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
}

func TestValidateRejectsProxyWithoutHost(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible",
		BaseURL: "https://example.com", ProxyURL: "http:/missing-host",
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "proxy_url") {
		t.Fatalf("proxy without host should fail validation, got %v", err)
	}
}

func TestReadyMeshRecoveryDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Routing.Strategy != "ready_mesh" {
		t.Fatalf("strategy=%q want ready_mesh", cfg.Routing.Strategy)
	}
	if !cfg.Routing.SessionAffinity || cfg.Routing.SessionTTLSeconds != 3600 || cfg.Routing.P2CWindow != 8 {
		t.Fatalf("unexpected ready mesh defaults: %+v", cfg.Routing)
	}
	if cfg.Routing.CapabilityFailureThreshold != 2 || cfg.Routing.CapabilityCooldownSeconds != 300 {
		t.Fatalf("unexpected capability defaults: %+v", cfg.Routing)
	}
	if cfg.Routing.CooldownSeconds != 1800 {
		t.Fatalf("cooldown=%d want 1800", cfg.Routing.CooldownSeconds)
	}
	if cfg.Probe.RecoveryAttempts != 5 || cfg.Probe.RecoveryRetryMS != 500 {
		t.Fatalf("unexpected recovery defaults: %+v", cfg.Probe)
	}
}

func TestValidateRejectsUnsafeResourceLimits(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"probe concurrency", func(c *Config) { c.Probe.Concurrency = maxProbeConcurrency + 1 }},
		{"probe tokens", func(c *Config) { c.Probe.MaxTokens = maxProbeTokens + 1 }},
		{"routing attempts", func(c *Config) { c.Routing.MaxAttempts = maxRoutingAttempts + 1 }},
		{"provider concurrency", func(c *Config) {
			c.Providers = []ProviderConfig{{
				ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com",
				Enabled: true, MaxConcurrency: maxProviderConcurrency + 1,
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected unsafe resource limit to be rejected")
			}
		})
	}
}

func TestValidateRejectsHeaderInjection(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com",
		Enabled: true, Headers: map[string]string{"X-Test": "ok\r\nInjected: yes"},
	}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid custom header") {
		t.Fatalf("header injection should fail validation, got %v", err)
	}
}

func TestValidateRejectsUnsafeGlobalInflightLimit(t *testing.T) {
	cfg := Default()
	cfg.Routing.MaxInflightRequests = 10001
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_inflight_requests") {
		t.Fatalf("expected max_inflight_requests validation error, got %v", err)
	}
}

func TestValidateRejectsAmbiguousLocalIDs(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"provider slash", func(c *Config) {
			c.Providers = []ProviderConfig{{
				ID: "bad/provider", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
			}}
		}},
		{"model slash", func(c *Config) {
			c.Providers = []ProviderConfig{{
				ID: "good-provider", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
				Models: []ModelConfig{{ID: "bad/model", Model: "vendor/model", Enabled: true, Weight: 1}},
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.edit(&cfg)
			cfg.ApplyDefaults()
			if err := cfg.Validate(); err == nil {
				t.Fatal("ambiguous local id should be rejected")
			}
		})
	}
}

func TestValidateAllowsNamespacedUpstreamModelAndAlias(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "provider-1", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
		Models: []ModelConfig{{
			ID: "model-1", Model: "vendor/model:latest", Aliases: []string{"team/coding"}, Enabled: true, Weight: 1,
		}},
	}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("namespaced upstream model/alias should remain valid: %v", err)
	}
}

func TestNegativeValuesAreRejectedInsteadOfDefaulted(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"max inflight", func(c *Config) { c.Routing.MaxInflightRequests = -1 }},
		{"max attempts", func(c *Config) { c.Routing.MaxAttempts = -1 }},
		{"session ttl", func(c *Config) { c.Routing.SessionTTLSeconds = -1 }},
		{"probe concurrency", func(c *Config) { c.Probe.Concurrency = -1 }},
		{"probe timeout", func(c *Config) { c.Probe.TimeoutMS = -1 }},
		{"provider concurrency", func(c *Config) {
			c.Providers = []ProviderConfig{{
				ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com",
				Enabled: true, MaxConcurrency: -1,
			}}
		}},
		{"model weight", func(c *Config) {
			c.Providers = []ProviderConfig{{
				ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com",
				Enabled: true, Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: -1}},
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.edit(&cfg)
			cfg.ApplyDefaults()
			if err := cfg.Validate(); err == nil {
				t.Fatal("negative value should be rejected, not silently defaulted")
			}
		})
	}
}
