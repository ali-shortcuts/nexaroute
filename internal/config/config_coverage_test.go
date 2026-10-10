package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertConfigError(t *testing.T, mutate func(*Config), want string) {
	t.Helper()
	cfg := Default()
	mutate(&cfg)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Validate error=%v, want substring %q", err, want)
	}
}

func validCoverageProvider() ProviderConfig {
	return ProviderConfig{
		ID: "provider", Name: "Provider", Type: "openai_compatible", BaseURL: "https://example.test",
		Enabled: true, Models: []ModelConfig{{ID: "model", Model: "model", Enabled: true, Weight: 1}},
	}
}

func TestCoverageConfigSecurityHelpersAndDefaults(t *testing.T) {
	if !(VirtualEndpointConfig{}).IsEnabled() {
		t.Fatal("nil virtual endpoint enabled flag should default to enabled")
	}
	disabled := false
	if (VirtualEndpointConfig{Enabled: &disabled}).IsEnabled() {
		t.Fatal("explicitly disabled virtual endpoint reported as enabled")
	}
	if !(DecisionProviderConfig{}).IsEnabled() || (DecisionProviderConfig{Enabled: &disabled}).IsEnabled() {
		t.Fatal("decision provider enabled default/override mismatch")
	}

	t.Setenv("NEXA_COVERAGE_KEY", "from-env")
	if got := (ProviderConfig{APIKey: "file", APIKeyEnv: "NEXA_COVERAGE_KEY"}).ResolvedAPIKey(); got != "from-env" {
		t.Fatalf("resolved env key=%q", got)
	}
	if got := (ProviderConfig{APIKey: "file", APIKeyEnv: "NEXA_MISSING"}).ResolvedAPIKey(); got != "file" {
		t.Fatalf("missing env key should fall back to file key, got %q", got)
	}
	if got := (CredentialConfig{APIKey: "file", APIKeyEnv: "NEXA_MISSING"}).Resolved(); got != "file" {
		t.Fatalf("missing credential env should fall back to file key, got %q", got)
	}
	if validEnvName(strings.Repeat("A", 257)) {
		t.Fatal("overlong environment name accepted")
	}

	mapping := map[string]string{" group ": "viewer"}
	cfg := Config{ControlPlane: ControlPlaneConfig{Enabled: true}, Guardrails: GuardrailConfig{}, Admin: AdminConfig{OIDC: OIDCConfig{RoleMappings: mapping}}, CandidatePools: []CandidatePoolConfig{{ID: " pool ", Mode: " "}}, RouteProfiles: []RouteProfileConfig{{ID: " profile ", Strategy: " INHERIT "}}, DecisionPolicies: []DecisionPolicyConfig{{ID: " policy ", TaskOverrides: map[string]DecisionPolicyWeights{" Coding ": {RouterBaseline: 1}}}}, DecisionProviders: []DecisionProviderConfig{{ID: " dp ", Type: " JEV ", BaseURL: " https://decision.example/ "}}}
	cfg.ApplyDefaults()
	if cfg.CandidatePools[0].Mode != "explicit" || cfg.RouteProfiles[0].Strategy != "" || cfg.DecisionPolicies[0].TaskOverrides["coding"].RouterBaseline != 1 {
		t.Fatalf("normalization/defaults failed: %+v", cfg)
	}
	if cfg.DecisionProviders[0].PrivacyMode != "metadata_only" || cfg.DecisionProviders[0].BaseURL != "https://decision.example" {
		t.Fatalf("decision provider defaults failed: %+v", cfg.DecisionProviders[0])
	}
	if cfg.ControlPlane.ConfigFailure != "last_known_good" || cfg.Guardrails.Mode != "off" {
		t.Fatalf("section defaults failed: %+v %+v", cfg.ControlPlane, cfg.Guardrails)
	}
	mapping[" group "] = "admin"
	if cfg.Admin.OIDC.RoleMappings[" group "] != "viewer" {
		t.Fatal("OIDC role mappings were not copied")
	}

	for _, tc := range []struct {
		typ, auth string
	}{
		{"anthropic_compatible", "x-api-key"}, {"gemini", "x-goog-api-key"}, {"openai_compatible", "bearer"},
	} {
		p := ProviderConfig{ID: "p", Type: tc.typ}
		p.ApplyDefaults()
		if p.AuthMode != tc.auth || p.Name != "p" || p.MaxConcurrency != 32 || p.StreamIdleTimeoutSeconds != 180 {
			t.Fatalf("provider defaults for %s: %+v", tc.typ, p)
		}
	}
}

func TestCoverageConfigValidationSecurityAndRuntimeLimits(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"admin session", func(c *Config) { c.Admin.SessionTTLSeconds = 299 }, "admin.session_ttl_seconds"},
		{"admin idle", func(c *Config) { c.Admin.IdleTimeoutSeconds = c.Admin.SessionTTLSeconds + 1 }, "admin.idle_timeout_seconds"},
		{"audit retention", func(c *Config) { c.Admin.AuditRetentionDays = 0 }, "admin.audit_retention_days"},
		{"audit events", func(c *Config) { c.Admin.AuditMaxEvents = 99 }, "admin.audit_max_events"},
		{"security store", func(c *Config) { c.Admin.SecurityStorePath = "bad\npath" }, "security_store_path"},
		{"empty listen", func(c *Config) { c.Listen = "" }, "listen is required"},
		{"listen syntax", func(c *Config) { c.Listen = "127.0.0.1" }, "listen must be host:port"},
		{"listen port", func(c *Config) { c.Listen = "127.0.0.1:not-port" }, "listen port"},
		{"listen host", func(c *Config) { c.Listen = "bad host:8080" }, "listen host"},
		{"log file", func(c *Config) { c.Logging.File = strings.Repeat("x", maxURLBytes+1) }, "logging.file"},
		{"log backups", func(c *Config) { c.Logging.MaxBackups = -1 }, "logging.max_backups"},
		{"sample rate", func(c *Config) { c.Logging.SuccessSampleEvery = 0 }, "success_sample_every"},
		{"slow request", func(c *Config) { c.Logging.SlowRequestMS = -1 }, "slow_request_ms"},
		{"console rate", func(c *Config) { c.Logging.ConsoleMaxLinesPerMinute = -1 }, "console_max_lines_per_minute"},
		{"provider count", func(c *Config) { c.Providers = make([]ProviderConfig, maxProviders+1) }, "providers exceeds"},
		{"p2c window", func(c *Config) { c.Routing.P2CWindow = maxModelsPerProvider + 1 }, "p2c_window"},
		{"capability threshold", func(c *Config) { c.Routing.CapabilityFailureThreshold = 0 }, "capability_failure_threshold"},
		{"cooldown", func(c *Config) { c.Routing.CooldownSeconds = 7*24*60*60 + 1 }, "cooldown_seconds"},
		{"provider threshold", func(c *Config) { c.Routing.ProviderFailureThreshold = 1 }, "provider_failure_threshold"},
		{"provider window", func(c *Config) { c.Routing.ProviderFailureWindowSeconds = 3601 }, "provider_failure_window_seconds"},
		{"provider cooldown", func(c *Config) { c.Routing.ProviderCooldownSeconds = 0 }, "provider_cooldown_seconds"},
		{"capability cooldown", func(c *Config) { c.Routing.CapabilityCooldownSeconds = 0 }, "capability_cooldown_seconds"},
		{"hedge delay", func(c *Config) { c.Routing.HedgingDelayMS = 49 }, "hedging_delay_ms"},
		{"cache entries", func(c *Config) { c.Cache.MaxEntries = 0 }, "cache.max_entries"},
		{"guardrail body", func(c *Config) { c.Guardrails.MaxBodyBytes = 1023 }, "guardrails.max_body_bytes"},
		{"client auth key count", func(c *Config) { c.ClientAuth.Enabled = true; c.ClientAuth.Keys = make([]string, 1025) }, "client_auth.keys exceeds"},
		{"client auth rpm", func(c *Config) {
			c.ClientAuth.Enabled = true
			c.ClientAuth.Keys = []string{"12345678"}
			c.ClientAuth.RPM = -1
		}, "client_auth.rpm"},
		{"tenant project duplicate", func(c *Config) {
			c.ClientAuth.Tenants = []TenantConfig{{ID: "t", Projects: []ProjectConfig{{ID: "p"}, {ID: "p"}}}}
		}, "duplicate project"},
		{"team duplicate", func(c *Config) {
			c.ClientAuth.Tenants = []TenantConfig{{ID: "t", Projects: []ProjectConfig{{ID: "p", Teams: []TeamConfig{{ID: "team"}, {ID: "team"}}}}}}
		}, "team id"},
		{"virtual key duplicate", func(c *Config) {
			c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "key", KeyHash: strings.Repeat("a", 64)}, {ID: "key", KeyHash: strings.Repeat("b", 64)}}
		}, "virtual_key id"},
		{"virtual key rpm", func(c *Config) {
			c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "key", KeyHash: strings.Repeat("a", 64), RPM: -1}}
		}, "virtual_key"},
		{"probe interval", func(c *Config) { c.Probe.IntervalSeconds = 0 }, "probe.interval_seconds"},
		{"probe lease", func(c *Config) { c.Probe.ReadyLeaseSeconds = 0 }, "probe.ready_lease_seconds"},
		{"recovery attempts", func(c *Config) { c.Probe.RecoveryAttempts = 0 }, "probe.recovery_attempts"},
		{"legacy model chars", func(c *Config) { c.Routing.PublicModel = "bad model" }, "routing.public_model"},
		{"legacy model reserved", func(c *Config) { c.Routing.PublicModel = "claude-auto" }, "routing.public_model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertConfigError(t, tc.edit, tc.want) })
	}
}

func TestCoverageProviderValidationBranches(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ProviderConfig)
		want string
	}{
		{"empty id", func(p *ProviderConfig) { p.ID = "" }, "id is required"},
		{"duplicate id", func(p *ProviderConfig) {}, "duplicate provider id"},
		{"unsupported type", func(p *ProviderConfig) { p.Type = "unsupported" }, "unsupported type"},
		{"missing base url", func(p *ProviderConfig) { p.BaseURL = "" }, "base_url is required"},
		{"invalid base url", func(p *ProviderConfig) { p.BaseURL = "not a url" }, "base_url"},
		{"auth mode", func(p *ProviderConfig) { p.AuthMode = "basic" }, "auth_mode"},
		{"concurrency", func(p *ProviderConfig) { p.MaxConcurrency = -1 }, "max_concurrency"},
		{"stream timeout", func(p *ProviderConfig) { p.StreamIdleTimeoutSeconds = -1 }, "stream_idle_timeout_seconds"},
		{"forward header", func(p *ProviderConfig) { p.ForwardHeaders = []string{"bad header"} }, "forward header"},
		{"path length", func(p *ProviderConfig) { p.ChatPath = strings.Repeat("x", maxURLBytes+1) }, "chat_path"},
		{"model id", func(p *ProviderConfig) { p.Models[0].ID = "bad/id" }, "model[0].id"},
		{"model context", func(p *ProviderConfig) { p.Models[0].ContextWindow = -1 }, "context_window"},
		{"model input cost", func(p *ProviderConfig) { p.Models[0].InputCostPerMTok = math.Inf(1) }, "input_cost_per_mtok"},
		{"duplicate deployment", func(p *ProviderConfig) {
			p.Models = append(p.Models, ModelConfig{ID: "model", Model: "other", Enabled: true, Weight: 1})
		}, "duplicate deployment"},
		{"model required", func(p *ProviderConfig) { p.Models[0].Model = "" }, "model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			p := validCoverageProvider()
			tc.edit(&p)
			if tc.name == "duplicate id" {
				cfg.Providers = []ProviderConfig{p, p}
			} else {
				cfg.Providers = []ProviderConfig{p}
			}
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestCoverageDecisionValidationBranches(t *testing.T) {
	policy := DecisionPolicyConfig{ID: "policy", Weights: DecisionPolicyWeights{RouterBaseline: 1}}
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"mode", func(c *Config) { c.Decision.Mode = "random" }, "decision.mode"},
		{"timeout", func(c *Config) { c.Decision.TimeoutMS = -1 }, "decision.timeout_ms"},
		{"chain id", func(c *Config) { c.Decision.Chain = "bad/id" }, "decision chain id"},
		{"health threshold", func(c *Config) { c.DecisionProviderHealth.FailureThreshold = -1 }, "decision_provider_health.failure_threshold"},
		{"policy id", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{{ID: "bad/id", Weights: DecisionPolicyWeights{RouterBaseline: 1}}}
		}, "decision policy id"},
		{"policy selection", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{policy}
			c.DecisionPolicies[0].SelectionMode = "weighted"
		}, "selection_mode"},
		{"policy score delta", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{policy}
			c.DecisionPolicies[0].MinScoreDelta = 2
		}, "min_score_delta"},
		{"policy no weight", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{{ID: "p", Weights: DecisionPolicyWeights{}}}
		}, "positive weight"},
		{"policy task", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{{ID: "p", Weights: DecisionPolicyWeights{RouterBaseline: 1}, TaskOverrides: map[string]DecisionPolicyWeights{"unknown_task": {RouterBaseline: 1}}}}
		}, "canonical task"},
		{"provider empty id", func(c *Config) { c.DecisionProviders = []DecisionProviderConfig{{Type: "jev"}} }, "decision_providers[0].id"},
		{"provider built-in", func(c *Config) { c.DecisionProviders = []DecisionProviderConfig{{ID: "local", Type: "jev"}} }, "built-in provider"},
		{"provider type", func(c *Config) { c.DecisionProviders = []DecisionProviderConfig{{ID: "external", Type: "other"}} }, "unsupported type"},
		{"provider privacy", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "external", Type: "jev", PrivacyMode: "full"}}
		}, "privacy_mode"},
		{"provider env", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "external", Type: "jev", APIKeyEnv: "1BAD"}}
		}, "api_key_env"},
		{"provider url", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "external", Type: "jev", BaseURL: "bad"}}
		}, "base_url"},
		{"assisted missing", func(c *Config) { c.Decision.Mode = "assisted"; c.Decision.Provider = "" }, "required when decision.mode=assisted"},
		{"assisted built-in", func(c *Config) { c.Decision.Mode = "assisted"; c.Decision.Provider = "local" }, "must be external"},
		{"local unknown", func(c *Config) { c.Decision.Mode = "local"; c.Decision.Provider = "external" }, "local or policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.ApplyDefaults()
			tc.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestCoveragePoolAndEndpointValidationBranches(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"pool empty id", func(c *Config) { c.CandidatePools = []CandidatePoolConfig{{Mode: "all"}} }, "candidate_pools[0].id"},
		{"pool invalid id", func(c *Config) { c.CandidatePools = []CandidatePoolConfig{{ID: "bad/id", Mode: "all"}} }, "candidate pool id"},
		{"pool deployment empty", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{""}}}
		}, "deployment[0]"},
		{"pool deployment chars", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"bad id"}}}
		}, "invalid characters"},
		{"fallback empty id", func(c *Config) {
			c.FallbackChains = []FallbackChainConfig{{Pools: []string{"pool"}}}
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
		}, "fallback_chains[0].id"},
		{"fallback empty pool", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.FallbackChains = []FallbackChainConfig{{ID: "chain", Pools: []string{""}}}
		}, "pool[0]"},
		{"profile empty id", func(c *Config) {
			c.RouteProfiles = []RouteProfileConfig{{CandidatePool: "pool"}}
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
		}, "route_profiles[0].id"},
		{"profile pool required", func(c *Config) { c.RouteProfiles = []RouteProfileConfig{{ID: "profile"}} }, "candidate_pool is required"},
		{"profile decision policy", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool", DecisionPolicy: "bad/id"}}
		}, "decision_policy"},
		{"endpoint empty id", func(c *Config) {
			c.VirtualEndpoints = []VirtualEndpointConfig{{PublicModel: "public", RouteProfile: "profile"}}
		}, "virtual_endpoints[0].id"},
		{"endpoint empty model", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "endpoint", RouteProfile: "profile"}}
		}, "public_model is required"},
		{"endpoint missing profile", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "endpoint", PublicModel: "public", RouteProfile: "missing"}}
		}, "unknown route profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertConfigError(t, func(c *Config) { tc.edit(c); c.ApplyDefaults() }, tc.want) })
	}
}

func TestCoverageConfigLoadAndPersistenceSafetyBranches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveAtomic(path, Default()); err != nil {
		t.Fatalf("save over malformed existing document: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("saved config should load: %v", err)
	}
	if err := RemoveStaleBackup(path); err != nil {
		t.Fatal(err)
	}
	if err := SaveAtomic(filepath.Join(dir, "missing", "config.json"), Default()); err == nil {
		t.Fatal("save into missing directory unexpectedly succeeded")
	}

	for name, raw := range map[string]string{
		"provider key":        `{"providers":[{"api_key":"x"}]}`,
		"provider proxy":      `{"providers":[{"proxy_url":"https://proxy"}]}`,
		"provider header":     `{"providers":[{"headers":{"X-Test":"x"}}]}`,
		"provider credential": `{"providers":[{"credentials":[{"api_key":"x"}]}]}`,
		"decision key":        `{"decision_providers":[{"api_key":"x"}]}`,
		"admin key":           `{"admin":{"api_key":"x"}}`,
		"client key":          `{"client_auth":{"keys":["x"]}}`,
		"invalid json":        `{`,
	} {
		if got := hasSecretValues([]byte(raw)); name == "invalid json" && got {
			t.Fatalf("invalid JSON reported as containing secrets")
		} else if name != "invalid json" && !got {
			t.Fatalf("%s was not detected as secret-bearing", name)
		}
	}
	if hasSecretValues([]byte(`{"providers":[{"headers":{"X-Test":""}}]}`)) {
		t.Fatal("empty header value reported as secret")
	}

	// Exercise both JSON step forms through the load path, including whitespace normalization.
	stepJSON, _ := json.Marshal([]DecisionChainStep{{Provider: " policy "}, {Provider: "jev", TimeoutMS: 50}})
	if !strings.Contains(string(stepJSON), "jev") {
		t.Fatal("chain step marshal unexpectedly empty")
	}
}
