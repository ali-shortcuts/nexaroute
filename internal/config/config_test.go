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
		{"retry backoff", func(c *Config) { c.Routing.RetryBackoffMS = -1 }},
		{"hedge delay", func(c *Config) { c.Routing.HedgeDelayMS = -1 }},
		{"retry budget ratio", func(c *Config) { c.Routing.RetryBudgetRatio = -0.5 }},
		{"stream max duration", func(c *Config) { c.Routing.StreamMaxDurationSeconds = -30 }},
		{"recovery retry", func(c *Config) { c.Probe.RecoveryRetryMS = -1 }},
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

func TestExplicitZeroRoutingWeightsAndRetryDelaysArePreserved(t *testing.T) {
	cfg := Default()
	cfg.Routing.LatencyWeight = 0
	cfg.Routing.FailureWeight = 0
	cfg.Routing.CapacityWeight = 0
	cfg.Routing.RetryBackoffMS = 0
	cfg.Probe.RecoveryRetryMS = 0

	cfg.ApplyDefaults()

	if cfg.Routing.LatencyWeight != 0 ||
		cfg.Routing.FailureWeight != 0 ||
		cfg.Routing.CapacityWeight != 0 ||
		cfg.Routing.RetryBackoffMS != 0 ||
		cfg.Probe.RecoveryRetryMS != 0 {
		t.Fatalf("explicit zero values were overwritten: routing=%+v probe=%+v", cfg.Routing, cfg.Probe)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit zero weights/delays should validate: %v", err)
	}
}

func TestExplicitEmptyForwardHeadersRemainEmpty(t *testing.T) {
	p := ProviderConfig{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: "https://example.com",
		ForwardHeaders: []string{}, Enabled: true,
	}
	p.ApplyDefaults()
	if p.ForwardHeaders == nil {
		t.Fatal("explicit empty forward_headers lost nil-vs-empty distinction")
	}
	if len(p.ForwardHeaders) != 0 {
		t.Fatalf("explicit empty forward_headers was overwritten: %#v", p.ForwardHeaders)
	}

	p2 := ProviderConfig{ID: "b", Name: "B", Type: "anthropic_compatible", BaseURL: "https://example.com", Enabled: true}
	p2.ApplyDefaults()
	if len(p2.ForwardHeaders) == 0 {
		t.Fatal("missing forward_headers should still receive Anthropic defaults")
	}
}

func TestForwardHeadersExplicitEmptySurvivesSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: "https://example.com",
		ForwardHeaders: []string{}, Enabled: true,
	}}
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers[0].ForwardHeaders == nil {
		t.Fatal("explicit empty forward_headers was lost during JSON round trip")
	}
	if len(loaded.Providers[0].ForwardHeaders) != 0 {
		t.Fatalf("explicit empty forward_headers was repopulated: %#v", loaded.Providers[0].ForwardHeaders)
	}
}

func TestLoadRejectsInvalidAdminBindEnvironmentBoolean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.Admin.BindLocalOnly = false
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "definitely-not-a-bool")
	loaded, err := Load(path)
	if err == nil {
		t.Fatalf("invalid security-sensitive env override was silently accepted: %+v", loaded.Admin)
	}
	if !strings.Contains(err.Error(), "NEXAROUTE_ADMIN_BIND_LOCAL_ONLY") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoggingZeroControlsArePreserved(t *testing.T) {
	cfg := Default()
	cfg.Logging.MaxBackups = 0
	cfg.Logging.SlowRequestMS = 0
	cfg.Logging.ConsoleMaxLinesPerMinute = 0
	cfg.ApplyDefaults()
	if cfg.Logging.MaxBackups != 0 || cfg.Logging.SlowRequestMS != 0 || cfg.Logging.ConsoleMaxLinesPerMinute != 0 {
		t.Fatalf("explicit zero logging controls were overwritten: %+v", cfg.Logging)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit zero logging controls should be valid: %v", err)
	}
}

func TestLoggingValidationRejectsUnsafeRetention(t *testing.T) {
	cfg := Default()
	cfg.Logging.MaxSizeMB = 2048
	if err := cfg.Validate(); err == nil {
		t.Fatal("oversized log rotation should be rejected")
	}
	cfg = Default()
	cfg.Logging.AccessMode = "everything"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown access logging mode should be rejected")
	}
}

func TestResilienceDefaultsAndExplicitZeros(t *testing.T) {
	cfg := Default()
	if cfg.Routing.RetryBudgetRatio != 0.2 {
		t.Fatalf("default retry budget ratio must be 0.2, got %v", cfg.Routing.RetryBudgetRatio)
	}
	if cfg.Routing.StreamMaxDurationSeconds != 1800 {
		t.Fatalf("default stream max duration must be 1800s, got %d", cfg.Routing.StreamMaxDurationSeconds)
	}
	if cfg.Routing.HedgeDelayMS != 0 {
		t.Fatalf("hedging must be off by default, got %d", cfg.Routing.HedgeDelayMS)
	}
	if cfg.Routing.AdmissionQueueTimeoutMS != 0 {
		t.Fatalf("default admission queue must wait while the client is connected (0), got %d", cfg.Routing.AdmissionQueueTimeoutMS)
	}
	if cfg.Routing.ProviderQueueTimeoutMS != 30000 {
		t.Fatalf("default provider queue timeout must be 30000ms, got %d", cfg.Routing.ProviderQueueTimeoutMS)
	}
	if cfg.Routing.MaxInflightRequests != 256 {
		t.Fatalf("default inflight must be 256, got %d", cfg.Routing.MaxInflightRequests)
	}
	// Explicit zeros keep their "disabled" meaning through ApplyDefaults.
	cfg.Routing.RetryBudgetRatio = 0
	cfg.Routing.StreamMaxDurationSeconds = 0
	cfg.ApplyDefaults()
	if cfg.Routing.RetryBudgetRatio != 0 || cfg.Routing.StreamMaxDurationSeconds != 0 {
		t.Fatalf("explicit zero resilience values were overwritten: %+v", cfg.Routing)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit zero resilience values should validate: %v", err)
	}
	for _, bad := range []func(*Config){
		func(c *Config) { c.Routing.HedgeDelayMS = 60001 },
		func(c *Config) { c.Routing.RetryBudgetRatio = 1.5 },
		func(c *Config) { c.Routing.RetryBudgetRatio = 0.005 },
		func(c *Config) { c.Routing.StreamMaxDurationSeconds = 59 },
		func(c *Config) { c.Routing.StreamMaxDurationSeconds = 86401 },
		func(c *Config) { c.Routing.AdmissionQueueTimeoutMS = 60001 },
		func(c *Config) { c.Routing.ProviderQueueTimeoutMS = 600001 },
	} {
		cfg := Default()
		bad(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatal("out-of-range resilience value should be rejected")
		}
	}
}
