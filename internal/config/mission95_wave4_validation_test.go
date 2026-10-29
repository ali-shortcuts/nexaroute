package config

import (
	"fmt"
	"strings"
	"testing"
)

func wave4Float(v float64) *float64 { return &v }

func wave4ValidProvider(id string) ProviderConfig {
	return ProviderConfig{ID: id, Name: id, Type: "openai_compatible", BaseURL: "https://example.com/v1", Enabled: true, MaxConcurrency: 16, StreamIdleTimeoutSeconds: 60}
}

func wave4ValidModel(id string) ModelConfig {
	return ModelConfig{ID: id, Model: "model-" + id, Weight: 1}
}

// Each mutation targets one previously uncovered branch of
// validatePlatformPlanes; the asserted substring pins the exact check.
func TestWave4PlatformPlanesBranches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		substr string
	}{
		{
			"state-path-too-long",
			func(c *Config) { c.BudgetAlerts.StatePath = strings.Repeat("x", 4097) },
			"budget_alerts.state_path",
		},
		{
			"too-many-tenants",
			func(c *Config) {
				for i := 0; i < 1025; i++ {
					c.ClientAuth.Tenants = append(c.ClientAuth.Tenants, TenantConfig{})
				}
			},
			"client_auth.tenants exceeds safe limit 1024",
		},
		{
			"too-many-virtual-keys",
			func(c *Config) {
				for i := 0; i < 10001; i++ {
					c.ClientAuth.VirtualKeys = append(c.ClientAuth.VirtualKeys, VirtualKeyConfig{})
				}
			},
			"client_auth.virtual_keys exceeds safe limit 10000",
		},
		{
			"invalid-project-id",
			func(c *Config) {
				c.ClientAuth.Tenants = []TenantConfig{{ID: "t1", Projects: []ProjectConfig{{ID: "bad id!"}}}}
			},
			`client_auth.project id "bad id!" is invalid`,
		},
		{
			"pricing-empty-version",
			func(c *Config) {
				c.Pricing.Versions = []PriceVersionConfig{{Version: "", EffectiveFrom: "2026-01-01T00:00:00Z"}}
			},
			"pricing.versions: version names must be non-empty",
		},
		{
			"pricing-deployment-without-slash",
			func(c *Config) {
				c.Pricing.Versions = []PriceVersionConfig{{Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z", Prices: map[string]PriceEntryConfig{"justname": {}}}}
			},
			`deployment "justname" must be provider/model`,
		},
		{
			"pricing-unknown-deployment",
			func(c *Config) {
				c.Providers = []ProviderConfig{wave4ValidProvider("a")}
				c.Providers[0].Models = []ModelConfig{wave4ValidModel("m")}
				c.Pricing.Versions = []PriceVersionConfig{{Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z", Prices: map[string]PriceEntryConfig{"b/m": {}}}}
			},
			`unknown deployment "b/m"`,
		},
		{
			"pricing-cache-rate-out-of-range",
			func(c *Config) {
				c.Providers = []ProviderConfig{wave4ValidProvider("a")}
				c.Providers[0].Models = []ModelConfig{wave4ValidModel("m")}
				c.Pricing.Versions = []PriceVersionConfig{{Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z", Prices: map[string]PriceEntryConfig{"a/m": {CacheReadPerMTok: wave4Float(-1)}}}}
			},
			"cache_read_per_mtok must be between 0 and 1e6",
		},
		{
			"virtual-key-tpm-out-of-range",
			func(c *Config) {
				c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", KeyHash: strings.Repeat("a", 64), TPM: 1000000001}}
			},
			`client_auth.virtual_key "k1" tpm must be between 0 and 1000000000`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.validatePlatformPlanes()
			if err == nil || !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("err = %v, want %q", err, tc.substr)
			}
		})
	}
}

// A virtual key with a recognized role passes the role gate: the whole
// platform-planes validation succeeds on the default config plus that key.
func TestWave4PlatformPlanesValidRolePasses(t *testing.T) {
	cfg := Default()
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", KeyHash: strings.Repeat("a", 64), Role: "viewer"}}
	if err := cfg.validatePlatformPlanes(); err != nil {
		t.Fatalf("validatePlatformPlanes = %v, want nil for a valid viewer key", err)
	}
}

// Each mutation targets one previously uncovered branch of
// validateProviders; the asserted substring pins the exact check.
func TestWave4ProvidersBranches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		substr string
	}{
		{
			"provider-id-too-long",
			func(c *Config) {
				p := wave4ValidProvider(strings.Repeat("p", 300))
				c.Providers = []ProviderConfig{p}
			},
			"id/name is too long",
		},
		{
			"provider-url-too-long",
			func(c *Config) {
				p := wave4ValidProvider("a")
				p.BaseURL = "https://example.com/" + strings.Repeat("x", 5000)
				c.Providers = []ProviderConfig{p}
			},
			"URL is too long",
		},
		{
			"too-many-models-per-provider",
			func(c *Config) {
				p := wave4ValidProvider("a")
				for i := 0; i < 4097; i++ {
					p.Models = append(p.Models, ModelConfig{})
				}
				c.Providers = []ProviderConfig{p}
			},
			"models exceeds safe limit 4096",
		},
		{
			"total-deployments-exceeded",
			func(c *Config) {
				for pi := 0; pi < 5; pi++ {
					p := wave4ValidProvider(fmt.Sprintf("p%d", pi))
					for mi := 0; mi < 4096; mi++ {
						p.Models = append(p.Models, wave4ValidModel(fmt.Sprintf("m%d", mi)))
					}
					c.Providers = append(c.Providers, p)
				}
			},
			"total deployments exceeds safe limit 20000",
		},
		{
			"too-many-credentials",
			func(c *Config) {
				p := wave4ValidProvider("a")
				for i := 0; i < 257; i++ {
					p.Credentials = append(p.Credentials, CredentialConfig{})
				}
				c.Providers = []ProviderConfig{p}
			},
			"credentials exceeds safe limit 256",
		},
		{
			"too-many-headers",
			func(c *Config) {
				p := wave4ValidProvider("a")
				p.Headers = map[string]string{}
				for i := 0; i < 129; i++ {
					p.Headers[fmt.Sprintf("X-H-%d", i)] = "v"
				}
				c.Providers = []ProviderConfig{p}
			},
			"headers exceeds safe limit 128",
		},
		{
			"model-id-empty",
			func(c *Config) {
				p := wave4ValidProvider("a")
				p.Models = []ModelConfig{{Model: "m", Weight: 1}}
				c.Providers = []ProviderConfig{p}
			},
			`model[0].id is required`,
		},
		{
			"model-identifiers-too-long",
			func(c *Config) {
				p := wave4ValidProvider("a")
				p.Models = []ModelConfig{{ID: strings.Repeat("m", 300), Model: "m", Weight: 1}}
				c.Providers = []ProviderConfig{p}
			},
			"identifiers/aliases exceed safe limits",
		},
		{
			"total-aliases-exceeded",
			func(c *Config) {
				p := wave4ValidProvider("a")
				for mi := 0; mi < 782; mi++ {
					m := wave4ValidModel(fmt.Sprintf("m%d", mi))
					for ai := 0; ai < 128; ai++ {
						m.Aliases = append(m.Aliases, "a")
					}
					p.Models = append(p.Models, m)
				}
				c.Providers = []ProviderConfig{p}
			},
			"total model aliases exceeds safe limit 100000",
		},
		{
			"alias-too-long",
			func(c *Config) {
				p := wave4ValidProvider("a")
				m := wave4ValidModel("m")
				m.Aliases = []string{strings.Repeat("a", 300)}
				p.Models = []ModelConfig{m}
				c.Providers = []ProviderConfig{p}
			},
			"alias is too long",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.validateProviders()
			if err == nil || !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("err = %v, want %q", err, tc.substr)
			}
		})
	}
}

// A route profile pointing at a pool that does not exist expands to
// nothing: the profile is reported dead, but no pool warning is invented
// for the unknown pool (unknown pools are a Validate error, not a warning).
func TestWave4EligibilityUnknownPoolExpandsEmpty(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "yes", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "ghost"}}
	cfg.ApplyDefaults()
	if !hasEligibilityWarning(&cfg, "route_profiles[r1]") {
		t.Fatalf("expected route-dead warning, got %v", cfg.EligibilityWarnings)
	}
	if hasEligibilityWarning(&cfg, "candidate_pools[ghost]") {
		t.Fatalf("unknown pool must not produce a pool warning, got %v", cfg.EligibilityWarnings)
	}
}

// Virtual-key privacy warnings follow the key's allowed routes: unknown
// routes contribute nothing (and warn nothing), a reachable training
// deployment warns, and a reachable no-training deployment stays silent.
func TestWave4EligibilityVirtualKeyAllowedRoutes(t *testing.T) {
	base := func(trains string) Config {
		cfg := Default()
		cfg.Providers = []ProviderConfig{eligProvider("a", trains, eligModel("m", "model-a"))}
		cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
		cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
		return cfg
	}
	cfg := base("yes")
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", RequirePrivacy: "no_training", AllowedRoutes: []string{"ghost-route"}}}
	cfg.ApplyDefaults()
	if hasEligibilityWarning(&cfg, "virtual_keys[k1]") {
		t.Fatalf("unknown allowed route must not warn for the key, got %v", cfg.EligibilityWarnings)
	}
	cfg = base("yes")
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", RequirePrivacy: "no_training", AllowedRoutes: []string{"r1"}}}
	cfg.ApplyDefaults()
	if !hasEligibilityWarning(&cfg, "virtual_keys[k1]") {
		t.Fatalf("expected a no_training key warning, got %v", cfg.EligibilityWarnings)
	}
	cfg = base("no")
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", RequirePrivacy: "no_training", AllowedRoutes: []string{"r1"}}}
	cfg.ApplyDefaults()
	if hasEligibilityWarning(&cfg, "virtual_keys[k1]") {
		t.Fatalf("no-training deployment must silence the key warning, got %v", cfg.EligibilityWarnings)
	}
}
