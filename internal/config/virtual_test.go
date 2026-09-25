package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVirtualEndpointValidation(t *testing.T) {
	// valid config
	cfg := Default()
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1"}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid virtual endpoint rejected: %v", err)
	}

	// duplicate public_model
	cfg2 := Default()
	cfg2.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg2.RouteProfiles = []RouteProfileConfig{{ID: "p1", CandidatePool: "pool1"}, {ID: "p2", CandidatePool: "pool1"}}
	cfg2.VirtualEndpoints = []VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "p1"},
		{ID: "ve2", PublicModel: "nexa-code", RouteProfile: "p2"},
	}
	cfg2.ApplyDefaults()
	if err := cfg2.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate virtual endpoint public_model") {
		t.Fatalf("duplicate public_model should be rejected, got %v", err)
	}

	// collision with physical model
	cfg3 := Default()
	cfg3.Providers = []ProviderConfig{{
		ID: "prov", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
		Models: []ModelConfig{{ID: "m1", Model: "my-model", Enabled: true, Weight: 1}},
	}}
	cfg3.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg3.RouteProfiles = []RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg3.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ve1", PublicModel: "my-model", RouteProfile: "profile1"}}
	cfg3.ApplyDefaults()
	if err := cfg3.Validate(); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("collision with physical model should be rejected, got %v", err)
	}

	// reserved auto
	cfg4 := Default()
	cfg4.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg4.RouteProfiles = []RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg4.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ve1", PublicModel: "auto", RouteProfile: "profile1"}}
	cfg4.ApplyDefaults()
	if err := cfg4.Validate(); err == nil || !strings.Contains(err.Error(), "auto") {
		t.Fatalf("reserved auto should be rejected, got %v", err)
	}

	// missing route profile
	cfg5 := Default()
	cfg5.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg5.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "missing"}}
	cfg5.ApplyDefaults()
	if err := cfg5.Validate(); err == nil || !strings.Contains(err.Error(), "unknown route profile") {
		t.Fatalf("missing route profile should be rejected, got %v", err)
	}

	// invalid public_model chars
	cfg6 := Default()
	cfg6.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg6.RouteProfiles = []RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg6.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ve1", PublicModel: "bad model", RouteProfile: "profile1"}}
	cfg6.ApplyDefaults()
	if err := cfg6.Validate(); err == nil {
		t.Fatalf("invalid public_model with space should be rejected")
	}
}

func TestRouteProfileValidation(t *testing.T) {
	cfg := Default()
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1", FallbackChain: "missing"}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown fallback chain") {
		t.Fatalf("missing fallback chain should be rejected, got %v", err)
	}

	cfg2 := Default()
	cfg2.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg2.RouteProfiles = []RouteProfileConfig{{ID: "profile1", CandidatePool: "missing"}}
	cfg2.ApplyDefaults()
	if err := cfg2.Validate(); err == nil || !strings.Contains(err.Error(), "unknown candidate pool") {
		t.Fatalf("missing candidate pool should be rejected, got %v", err)
	}
}

func TestCandidatePoolValidation(t *testing.T) {
	cfg := Default()
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "invalid"}}
	cfg.ApplyDefaults()
	// ApplyDefaults lowercases mode, so invalid remains invalid
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mode must be") {
		t.Fatalf("invalid pool mode should be rejected, got %v", err)
	}

	cfg2 := Default()
	cfg2.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"a", "b", "a"}}}
	// Duplicate deployments in pool is allowed? Our validation doesn't reject duplicate deployments, only pool IDs.
	// But we can test empty ID
	cfg2.ApplyDefaults()
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("duplicate deployments should be allowed (or filtered), got %v", err)
	}
}

func TestFallbackChainValidation(t *testing.T) {
	cfg := Default()
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}, {ID: "pool2", Mode: "all"}}
	cfg.FallbackChains = []FallbackChainConfig{{ID: "chain1", Pools: []string{"pool1", "pool1"}}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate pool") {
		t.Fatalf("duplicate pool in chain should be rejected, got %v", err)
	}

	cfg2 := Default()
	cfg2.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg2.FallbackChains = []FallbackChainConfig{{ID: "chain1", Pools: []string{"pool1", "missing"}}}
	cfg2.ApplyDefaults()
	if err := cfg2.Validate(); err == nil || !strings.Contains(err.Error(), "unknown pool") {
		t.Fatalf("unknown pool in chain should be rejected, got %v", err)
	}

	cfg3 := Default()
	cfg3.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg3.FallbackChains = []FallbackChainConfig{{ID: "chain1", Pools: []string{}}}
	cfg3.ApplyDefaults()
	if err := cfg3.Validate(); err == nil || !strings.Contains(err.Error(), "at least one pool") {
		t.Fatalf("empty chain should be rejected, got %v", err)
	}
}

func TestLegacyPublicModelMigration(t *testing.T) {
	cfg := Default()
	cfg.Routing.PublicModel = "nexaroute"
	cfg.ApplyDefaults()
	if len(cfg.VirtualEndpoints) != 1 {
		t.Fatalf("legacy public_model should synthesize virtual endpoint, got %d", len(cfg.VirtualEndpoints))
	}
	if cfg.VirtualEndpoints[0].PublicModel != "nexaroute" {
		t.Fatalf("synthesized public_model mismatch: %q", cfg.VirtualEndpoints[0].PublicModel)
	}
	if len(cfg.CandidatePools) != 1 || cfg.CandidatePools[0].Mode != "all" {
		t.Fatalf("synthesized pool should be all mode")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("synthesized config should validate: %v", err)
	}
}

func TestConfigRoundTripWithVirtualEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "coding", Mode: "explicit", Deployments: []string{"prov1/m1"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "coding-smart", CandidatePool: "coding"}}
	cfg.VirtualEndpoints = []VirtualEndpointConfig{{ID: "coding-prod", PublicModel: "nexa-code", RouteProfile: "coding-smart"}}
	cfg.Providers = []ProviderConfig{{
		ID: "prov1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
		Models: []ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}},
	}}
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(loaded.VirtualEndpoints) != 1 || loaded.VirtualEndpoints[0].PublicModel != "nexa-code" {
		t.Fatalf("round-trip lost virtual endpoint")
	}
	if len(loaded.RouteProfiles) != 1 || loaded.RouteProfiles[0].ID != "coding-smart" {
		t.Fatalf("round-trip lost route profile")
	}
	if len(loaded.CandidatePools) != 1 || loaded.CandidatePools[0].ID != "coding" {
		t.Fatalf("round-trip lost candidate pool")
	}
}

func TestExistingConfigsLoadUnchanged(t *testing.T) {
	// Empty new sections should produce current behavior
	cfg := Default()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	if len(cfg.VirtualEndpoints) != 0 || len(cfg.RouteProfiles) != 0 || len(cfg.CandidatePools) != 0 || len(cfg.FallbackChains) != 0 {
		t.Fatalf("default config should have no virtual endpoints when public_model empty")
	}
}
