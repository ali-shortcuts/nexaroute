package config

import (
	"strings"
	"testing"
)

func eligProvider(id, trains string, models ...ModelConfig) ProviderConfig {
	return ProviderConfig{
		ID: id, Name: id, Type: "openai_compatible", BaseURL: "https://example.com/v1",
		Enabled: true, APIKeyEnv: "K",
		DataHandling: DataHandlingConfig{TrainsOnData: trains},
		Models:       models,
	}
}

func eligModel(id, model string) ModelConfig {
	return ModelConfig{ID: id, Model: model, Enabled: true}
}

func hasEligibilityWarning(cfg *Config, substr string) bool {
	for _, w := range cfg.EligibilityWarnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestEligibilityPoolWithNoMatchingDeployments(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "no", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"ghost/model"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
	cfg.ApplyDefaults()
	if !hasEligibilityWarning(&cfg, "candidate_pools[pool]") {
		t.Fatalf("expected pool warning, got %v", cfg.EligibilityWarnings)
	}
	if !hasEligibilityWarning(&cfg, "route_profiles[r1]") {
		t.Fatalf("expected route-dead warning, got %v", cfg.EligibilityWarnings)
	}
}

func TestEligibilityNoEnabledDeployments(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "off", Name: "off", Type: "openai_compatible", BaseURL: "https://example.com",
		Enabled: false, Models: []ModelConfig{eligModel("m", "model-x")},
	}}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
	cfg.ApplyDefaults()
	if len(cfg.EligibilityWarnings) != 1 || !hasEligibilityWarning(&cfg, "no enabled model deployments") {
		t.Fatalf("expected exactly the global warning, got %v", cfg.EligibilityWarnings)
	}

	// Zero providers at all: same single diagnosis, no cascade.
	cfg2 := Default()
	cfg2.ApplyDefaults()
	if len(cfg2.EligibilityWarnings) != 1 || !hasEligibilityWarning(&cfg2, "no enabled model deployments") {
		t.Fatalf("expected exactly the global warning for empty config, got %v", cfg2.EligibilityWarnings)
	}
}

func TestEligibilityEmptyPrimaryUsableFallbackNoRouteDead(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "no", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{
		{ID: "empty", Mode: "explicit", Deployments: []string{"ghost/model"}},
		{ID: "good", Mode: "explicit", Deployments: []string{"a/m"}},
	}
	cfg.FallbackChains = []FallbackChainConfig{{ID: "chain", Pools: []string{"good"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "empty", FallbackChain: "chain"}}
	cfg.ApplyDefaults()
	if !hasEligibilityWarning(&cfg, "candidate_pools[empty]") {
		t.Fatalf("expected warning for the empty primary pool, got %v", cfg.EligibilityWarnings)
	}
	if hasEligibilityWarning(&cfg, "route_profiles[r1]") {
		t.Fatalf("usable fallback must not produce a route-dead warning, got %v", cfg.EligibilityWarnings)
	}
}

func TestEligibilityWholeChainEmptyRouteDead(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "no", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{
		{ID: "e1", Mode: "explicit", Deployments: []string{"ghost/1"}},
		{ID: "e2", Mode: "explicit", Deployments: []string{"ghost/2"}},
	}
	cfg.FallbackChains = []FallbackChainConfig{{ID: "chain", Pools: []string{"e2"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "e1", FallbackChain: "chain"}}
	cfg.ApplyDefaults()
	if !hasEligibilityWarning(&cfg, "route_profiles[r1]") ||
		!hasEligibilityWarning(&cfg, "no usable deployments") {
		t.Fatalf("expected route-dead warning, got %v", cfg.EligibilityWarnings)
	}
}

func TestEligibilityUnreferencedEmptyPoolSilent(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "no", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{
		{ID: "used", Mode: "explicit", Deployments: []string{"a/m"}},
		{ID: "orphan", Mode: "explicit", Deployments: []string{"ghost/model"}},
	}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "used"}}
	cfg.ApplyDefaults()
	if len(cfg.EligibilityWarnings) != 0 {
		t.Fatalf("unreferenced empty pool must not warn, got %v", cfg.EligibilityWarnings)
	}
}

func TestEligibilityNoTrainingProfile(t *testing.T) {
	for _, trains := range []string{"yes", "", "unknown"} {
		cfg := Default()
		cfg.Providers = []ProviderConfig{eligProvider("a", trains, eligModel("m", "model-a"))}
		cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
		cfg.RouteProfiles = []RouteProfileConfig{{ID: "strict", CandidatePool: "pool", Privacy: "no_training"}}
		cfg.ApplyDefaults()
		if !hasEligibilityWarning(&cfg, "route_profiles[strict]") ||
			!hasEligibilityWarning(&cfg, "fail closed") {
			t.Fatalf("trains=%q: expected no_training warning, got %v", trains, cfg.EligibilityWarnings)
		}
	}
	// Satisfying provider: no warning.
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "no", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "strict", CandidatePool: "pool", Privacy: "no_training"}}
	cfg.ApplyDefaults()
	if len(cfg.EligibilityWarnings) != 0 {
		t.Fatalf("satisfying no_training profile must not warn, got %v", cfg.EligibilityWarnings)
	}
}

func TestEligibilityVirtualKeyNoTraining(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "yes", eligModel("m", "model-a"))}
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", RequirePrivacy: "no_training"}}
	cfg.ApplyDefaults()
	if !hasEligibilityWarning(&cfg, "client_auth.virtual_keys[k1]") {
		t.Fatalf("expected virtual key warning, got %v", cfg.EligibilityWarnings)
	}
	// Revoked key: silent.
	cfg.ClientAuth.VirtualKeys[0].Revoked = true
	cfg.ApplyDefaults()
	if hasEligibilityWarning(&cfg, "virtual_keys") {
		t.Fatalf("revoked key must not warn, got %v", cfg.EligibilityWarnings)
	}
}

func TestEligibilityFreshReadyMeshInstallNoHealthWarning(t *testing.T) {
	// Enabled provider/model under the default ready_mesh strategy, with
	// no health information anywhere (config analysis cannot see health):
	// the static analysis must produce no warning at all.
	cfg := Default()
	if cfg.Routing.Strategy != "ready_mesh" {
		t.Fatalf("test premise broken: default strategy is %q", cfg.Routing.Strategy)
	}
	cfg.Providers = []ProviderConfig{eligProvider("a", "no", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
	cfg.ApplyDefaults()
	if len(cfg.EligibilityWarnings) != 0 {
		t.Fatalf("healthy fresh config must not warn, got %v", cfg.EligibilityWarnings)
	}
}

func TestEligibilityWarningsDeterministicDedupedSecretSafe(t *testing.T) {
	cfg := Default()
	p := eligProvider("a", "yes", eligModel("m", "model-a"))
	p.BaseURL = "https://user:passw0rd-canary@api.example.com/v1"
	p.APIKey = "sk-CANARY-SECRET-999"
	cfg.Providers = []ProviderConfig{p}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"ghost/model"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool", Privacy: "no_training"}}
	cfg.ApplyDefaults()
	first := append([]string(nil), cfg.EligibilityWarnings...)
	cfg.ApplyDefaults() // repeated application must not duplicate or reorder
	if len(first) != len(cfg.EligibilityWarnings) {
		t.Fatalf("warnings changed across ApplyDefaults: %v vs %v", first, cfg.EligibilityWarnings)
	}
	seen := map[string]bool{}
	for i, w := range cfg.EligibilityWarnings {
		if w != first[i] {
			t.Fatalf("warning order changed across ApplyDefaults at %d", i)
		}
		if seen[w] {
			t.Fatalf("duplicate warning: %s", w)
		}
		seen[w] = true
		if strings.Contains(w, "passw0rd-canary") || strings.Contains(w, "sk-CANARY-SECRET-999") {
			t.Fatalf("warning leaks secret material: %s", w)
		}
	}
	if len(first) == 0 {
		t.Fatal("expected warnings for this broken config")
	}
}

func TestEligibilityWarningsDoNotFailStrictValidation(t *testing.T) {
	// A config with a real eligibility problem: after ApplyDefaults its
	// EligibilityWarnings are genuinely populated. ValidateStrict must
	// stay advisory-only for them and pass.
	cfg := Default()
	cfg.Providers = []ProviderConfig{eligProvider("a", "no", eligModel("m", "model-a"))}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"ghost/model"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
	cfg.ApplyDefaults()
	if len(cfg.EligibilityWarnings) == 0 {
		t.Fatal("test premise broken: expected eligibility warnings")
	}
	// Eligibility warnings must never join the ordinary Warnings set
	// that strict mode consults.
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "candidate_pools[") || strings.Contains(w, "route_profiles[") {
			t.Fatalf("eligibility warning leaked into Warnings (strict set): %s", w)
		}
	}
	// Validate a copy of THIS config — carrying its computed
	// EligibilityWarnings — with only the unrelated ordinary Warnings
	// (implicit-default notices from Default()) cleared, so strict mode
	// has nothing else to reject. If ValidateStrict consulted the
	// eligibility list, it would fail here.
	strict := cfg
	strict.Warnings = nil
	if len(strict.EligibilityWarnings) == 0 {
		t.Fatal("test premise broken: copy lost the computed eligibility warnings")
	}
	if err := strict.ValidateStrict(); err != nil {
		t.Fatalf("ValidateStrict must ignore populated eligibility warnings, got: %v", err)
	}
}
