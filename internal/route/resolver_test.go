package route

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func deployments() []router.Deployment {
	return []router.Deployment{
		{ID: "prov-a/m1", ProviderID: "prov-a", Model: "model-a", Aliases: []string{"alias-a"}},
		{ID: "prov-b/m2", ProviderID: "prov-b", Model: "model-b", Aliases: []string{"alias-b"}},
		{ID: "prov-c/m3", ProviderID: "prov-c", Model: "model-c"},
	}
}

func TestResolverExplicitPool(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "coding", Mode: "explicit", Deployments: []string{"prov-a/m1", "model-b"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "coding-smart", CandidatePool: "coding"},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "coding-smart"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, ok := r.Resolve("nexa-code")
	if !ok {
		t.Fatal("should resolve nexa-code")
	}
	if resolved.VirtualEndpointID != "ve1" {
		t.Fatalf("wrong VE ID %q", resolved.VirtualEndpointID)
	}
	if len(resolved.AllowedDeployments) != 2 {
		t.Fatalf("expected 2 allowed, got %d", len(resolved.AllowedDeployments))
	}
	if _, ok := resolved.AllowedDeployments["prov-a/m1"]; !ok {
		t.Fatal("prov-a/m1 should be allowed")
	}
	if _, ok := resolved.AllowedDeployments["prov-b/m2"]; !ok {
		t.Fatal("prov-b/m2 should be allowed via model-b")
	}
	if _, ok := resolved.AllowedDeployments["prov-c/m3"]; ok {
		t.Fatal("prov-c/m3 should NOT be allowed")
	}
}

func TestResolverAllMode(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "all", Mode: "all"},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "default", CandidatePool: "all"},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-all", RouteProfile: "default"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, ok := r.Resolve("nexa-all")
	if !ok {
		t.Fatal("should resolve")
	}
	if len(resolved.AllowedDeployments) != 3 {
		t.Fatalf("all mode should allow 3, got %d", len(resolved.AllowedDeployments))
	}
}

func TestResolverFallbackChain(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "primary", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
			{ID: "secondary", Mode: "explicit", Deployments: []string{"prov-b/m2"}},
		},
		FallbackChains: []config.FallbackChainConfig{
			{ID: "chain1", Pools: []string{"secondary"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "profile1", CandidatePool: "primary", FallbackChain: "chain1"},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, ok := r.Resolve("nexa-code")
	if !ok {
		t.Fatal("should resolve")
	}
	if len(resolved.OrderedPoolIDs) != 2 {
		t.Fatalf("ordered should be 2, got %v", resolved.OrderedPoolIDs)
	}
	if resolved.OrderedPoolIDs[0] != "primary" || resolved.OrderedPoolIDs[1] != "secondary" {
		t.Fatalf("wrong order %v", resolved.OrderedPoolIDs)
	}
	// Simulate filtering: only secondary healthy
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "prov-b/m2"}},
	}
	filtered, poolID := r.ResolveCandidates(candidates, resolved)
	if len(filtered) != 1 || poolID != "secondary" {
		t.Fatalf("fallback should pick secondary, got pool %q len %d", poolID, len(filtered))
	}
}

func TestResolverNoCandidateOutsidePool(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "coding", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "p1", CandidatePool: "coding"},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "p1"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, _ := r.Resolve("nexa-code")
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "prov-a/m1"}},
		{Deployment: router.Deployment{ID: "prov-b/m2"}},
		{Deployment: router.Deployment{ID: "prov-c/m3"}},
	}
	filtered := FilterCandidates(candidates, resolved.AllowedDeployments, resolved.PrimaryMode)
	if len(filtered) != 1 || filtered[0].Deployment.ID != "prov-a/m1" {
		t.Fatalf("should only allow prov-a/m1, got %v", filtered)
	}
}

func TestResolverUnknownModel(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}},
		RouteProfiles:  []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	_, ok := r.Resolve("unknown-model")
	if ok {
		t.Fatal("unknown should not resolve")
	}
}

func TestResolverAliasMatching(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "pool1", Mode: "explicit", Deployments: []string{"alias-a"}},
		},
		RouteProfiles: []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, _ := r.Resolve("nexa-code")
	if _, ok := resolved.AllowedDeployments["prov-a/m1"]; !ok {
		t.Fatal("alias-a should match prov-a/m1")
	}
}
