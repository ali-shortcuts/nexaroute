package route

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Finding F7: admin/list accessors and AllFilteredCandidates had 0% coverage.
// These tests assert observable resolution/listing behavior, not line mirrors.
func testF7Config() config.Config {
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
			{ID: "ve1", Name: "VE One", PublicModel: "nexa-code", RouteProfile: "profile1"},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func TestResolveByIDAndListAccessors(t *testing.T) {
	r := NewResolver(testF7Config(), deployments())
	ve, ok := r.ResolveByID("ve1")
	if !ok || ve.PublicModel != "nexa-code" {
		t.Fatalf("ResolveByID missed: %+v %v", ve, ok)
	}
	if _, ok := r.ResolveByID("missing"); ok {
		t.Fatal("unknown ID should not resolve")
	}
	if got := r.ListVirtualEndpoints(); len(got) != 1 || got[0].ID != "ve1" {
		t.Fatalf("ListVirtualEndpoints wrong: %+v", got)
	}
	if got := r.ListRouteProfiles(); len(got) != 1 || got[0].ID != "profile1" {
		t.Fatalf("ListRouteProfiles wrong: %+v", got)
	}
	if got := r.ListCandidatePools(); len(got) != 2 {
		t.Fatalf("ListCandidatePools wrong: %+v", got)
	}
	if got := r.ListFallbackChains(); len(got) != 1 || got[0].ID != "chain1" {
		t.Fatalf("ListFallbackChains wrong: %+v", got)
	}
	// List results must be copies: mutating them must not affect the resolver.
	ves := r.ListVirtualEndpoints()
	ves[0].ID = "mutated"
	if again := r.ListVirtualEndpoints(); again[0].ID != "ve1" {
		t.Fatal("ListVirtualEndpoints did not return a copy")
	}
}

func TestGetExpandedCopySemantics(t *testing.T) {
	r := NewResolver(testF7Config(), deployments())
	set, ok := r.GetExpanded("primary")
	if !ok || len(set) != 1 {
		t.Fatalf("GetExpanded missed: %v %v", set, ok)
	}
	set["injected"] = struct{}{}
	again, _ := r.GetExpanded("primary")
	if _, bad := again["injected"]; bad {
		t.Fatal("GetExpanded did not return a copy")
	}
	if _, ok := r.GetExpanded("missing-pool"); ok {
		t.Fatal("unknown pool should report missing")
	}
}

func TestResolveMissingReferencesNotFound(t *testing.T) {
	cfg := testF7Config()
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "primary", FallbackChain: "chain1"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "dangling-profile"}}
	cfg.ApplyDefaults()
	if _, ok := NewResolver(cfg, deployments()).Resolve("nexa-code"); ok {
		t.Fatal("dangling route profile should not resolve")
	}
	cfg2 := testF7Config()
	cfg2.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "dangling-pool", FallbackChain: "chain1"}}
	cfg2.ApplyDefaults()
	if _, ok := NewResolver(cfg2, deployments()).Resolve("nexa-code"); ok {
		t.Fatal("dangling candidate pool should not resolve")
	}
}

func TestAllFilteredCandidatesDedupAndOrder(t *testing.T) {
	r := NewResolver(testF7Config(), deployments())
	resolved, ok := r.Resolve("nexa-code")
	if !ok {
		t.Fatal("should resolve")
	}
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "prov-a/m1"}},
		{Deployment: router.Deployment{ID: "prov-b/m2"}},
		{Deployment: router.Deployment{ID: "prov-c/m3"}},
	}
	got := r.AllFilteredCandidates(candidates, resolved)
	if len(got) != 2 || got[0].Deployment.ID != "prov-a/m1" || got[1].Deployment.ID != "prov-b/m2" {
		t.Fatalf("primary-then-fallback order wrong: %+v", got)
	}
	// A deployment present in both pools must appear once.
	overlap := ResolvedRoute{
		PrimaryPoolID:      "primary",
		PrimaryMode:        "explicit",
		OrderedPoolIDs:     []string{"primary", "secondary"},
		AllowedDeployments: map[string]struct{}{"prov-a/m1": {}},
		FallbackAllowed:    []map[string]struct{}{{ "prov-a/m1": {}, "prov-b/m2": {} }},
	}
	deduped := r.AllFilteredCandidates(candidates, overlap)
	if len(deduped) != 2 {
		t.Fatalf("dedup failed: %+v", deduped)
	}
	if len(r.AllFilteredCandidates([]router.Scored{{Deployment: router.Deployment{ID: "prov-c/m3"}}}, resolved)) != 0 {
		t.Fatal("outside-pool candidates should yield empty")
	}
}

func TestFilterCandidatesModes(t *testing.T) {
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "a"}},
		{Deployment: router.Deployment{ID: "b"}},
	}
	if got := FilterCandidates(candidates, nil, "all"); len(got) != 2 {
		t.Fatal("all mode with nil set should allow everything")
	}
	if got := FilterCandidates(candidates, nil, "explicit"); len(got) != 2 {
		t.Fatal("nil set should pass through for explicit mode")
	}
	if got := FilterCandidates(candidates, map[string]struct{}{}, "explicit"); got != nil {
		t.Fatal("empty set should yield nil")
	}
}

func TestResolveCandidatesFallsBackToExpanded(t *testing.T) {
	r := NewResolver(testF7Config(), deployments())
	// Deliberately omit FallbackAllowed so ResolveCandidates must consult the
	// expanded index (covers the r.GetExpanded branch).
	resolved := ResolvedRoute{
		PrimaryPoolID:      "primary",
		PrimaryMode:        "explicit",
		OrderedPoolIDs:     []string{"primary", "secondary"},
		AllowedDeployments: map[string]struct{}{},
	}
	candidates := []router.Scored{{Deployment: router.Deployment{ID: "prov-b/m2"}}}
	filtered, pool := r.ResolveCandidates(candidates, resolved)
	if len(filtered) != 1 || pool != "secondary" {
		t.Fatalf("expanded fallback failed: %+v %q", filtered, pool)
	}
	if got, pool := r.ResolveCandidates([]router.Scored{{Deployment: router.Deployment{ID: "ghost"}}}, resolved); len(got) != 0 || pool != "" {
		t.Fatal("no eligible candidate should yield empty pool")
	}
}
