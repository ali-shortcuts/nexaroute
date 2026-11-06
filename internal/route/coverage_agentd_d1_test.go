package route

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Agent-D1 coverage: explicit empty pools, blank pool entries, suffix
// pool-entry matching, fallback-chain primary skip and unknown pools,
// primary-hit ResolveCandidates, and the GetExpanded fallback in
// AllFilteredCandidates.

func TestAgentD1ResolverExplicitEmptyPool(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "empty", Mode: "explicit"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	set, ok := r.GetExpanded("empty")
	if !ok {
		t.Fatal("explicit empty pool should still be known")
	}
	if len(set) != 0 {
		t.Fatalf("explicit empty pool expanded to %d deployments, want 0", len(set))
	}
}

func TestAgentD1ResolverSkipsBlankPoolEntries(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "p", Mode: "explicit", Deployments: []string{"", "  ", "prov-a/m1"}},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	set, ok := r.GetExpanded("p")
	if !ok {
		t.Fatal("pool should be known")
	}
	if len(set) != 1 {
		t.Fatalf("pool expanded to %d deployments, want 1 (blank entries skipped)", len(set))
	}
	if _, ok := set["prov-a/m1"]; !ok {
		t.Fatal("prov-a/m1 should be in the expanded pool")
	}
}

func TestAgentD1ResolverSuffixPoolEntryMatch(t *testing.T) {
	// "m1" matches "prov-a/m1" only via the "/"+entry suffix rule:
	// it is neither the deployment id, the model id ("model-a"),
	// nor an alias ("alias-a").
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "p", Mode: "explicit", Deployments: []string{"m1"}},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	set, _ := r.GetExpanded("p")
	if _, ok := set["prov-a/m1"]; !ok {
		t.Fatalf("suffix entry m1 should match prov-a/m1, expanded = %v", set)
	}
	if len(set) != 1 {
		t.Fatalf("expanded = %v, want exactly prov-a/m1", set)
	}
}

func TestAgentD1ResolverFallbackChainSkipsPrimaryPool(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "primary", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
			{ID: "secondary", Mode: "explicit", Deployments: []string{"prov-b/m2"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "rp", CandidatePool: "primary", FallbackChain: "chain"},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-x", RouteProfile: "rp"},
		},
		FallbackChains: []config.FallbackChainConfig{
			{ID: "chain", Pools: []string{"primary", "secondary"}},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, ok := r.Resolve("nexa-x")
	if !ok {
		t.Fatal("should resolve nexa-x")
	}
	if len(resolved.OrderedPoolIDs) != 2 || resolved.OrderedPoolIDs[0] != "primary" || resolved.OrderedPoolIDs[1] != "secondary" {
		t.Fatalf("OrderedPoolIDs = %v, want [primary secondary]", resolved.OrderedPoolIDs)
	}
	if len(resolved.FallbackAllowed) != 1 {
		t.Fatalf("FallbackAllowed has %d entries, want 1 (primary skipped, not duplicated)", len(resolved.FallbackAllowed))
	}
	if _, ok := resolved.FallbackAllowed[0]["prov-b/m2"]; !ok {
		t.Fatalf("fallback allowed = %v, want prov-b/m2", resolved.FallbackAllowed[0])
	}
}

func TestAgentD1ResolverFallbackChainUnknownPoolGetsEmptySet(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "primary", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
			{ID: "secondary", Mode: "explicit", Deployments: []string{"prov-b/m2"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "rp", CandidatePool: "primary", FallbackChain: "chain"},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-x", RouteProfile: "rp"},
		},
		FallbackChains: []config.FallbackChainConfig{
			{ID: "chain", Pools: []string{"secondary", "ghost-pool"}},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, ok := r.Resolve("nexa-x")
	if !ok {
		t.Fatal("should resolve nexa-x")
	}
	if len(resolved.FallbackAllowed) != 2 {
		t.Fatalf("FallbackAllowed has %d entries, want 2", len(resolved.FallbackAllowed))
	}
	if len(resolved.FallbackAllowed[1]) != 0 {
		t.Fatalf("unknown fallback pool should contribute an empty allowed set, got %v", resolved.FallbackAllowed[1])
	}
	if len(resolved.OrderedPoolIDs) != 3 || resolved.OrderedPoolIDs[2] != "ghost-pool" {
		t.Fatalf("OrderedPoolIDs = %v, want ghost-pool last", resolved.OrderedPoolIDs)
	}
}

func TestAgentD1ResolveCandidatesPrimaryHit(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "coding", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
			{ID: "other", Mode: "explicit", Deployments: []string{"prov-b/m2"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "rp", CandidatePool: "coding"},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "rp"},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	resolved, ok := r.Resolve("nexa-code")
	if !ok {
		t.Fatal("should resolve nexa-code")
	}
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "prov-a/m1"}},
		{Deployment: router.Deployment{ID: "prov-b/m2"}},
	}
	got, poolID := r.ResolveCandidates(candidates, resolved)
	if poolID != "coding" {
		t.Fatalf("pool = %q, want coding (primary hit)", poolID)
	}
	if len(got) != 1 || got[0].Deployment.ID != "prov-a/m1" {
		t.Fatalf("candidates = %v, want only prov-a/m1", got)
	}
}

func TestAgentD1AllFilteredCandidatesFallsBackToExpandedPool(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "primary", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
			{ID: "secondary", Mode: "explicit", Deployments: []string{"prov-b/m2"}},
			{ID: "tertiary", Mode: "explicit", Deployments: []string{"prov-c/m3"}},
		},
	}
	cfg.ApplyDefaults()
	r := NewResolver(cfg, deployments())
	// A resolved route whose OrderedPoolIDs outrun FallbackAllowed:
	// the tertiary pool has no precomputed fallback entry, so
	// AllFilteredCandidates must expand it via GetExpanded.
	resolved := ResolvedRoute{
		PrimaryPoolID:      "primary",
		PrimaryMode:        "explicit",
		AllowedDeployments: map[string]struct{}{"prov-a/m1": {}},
		OrderedPoolIDs:     []string{"primary", "secondary", "tertiary"},
		FallbackAllowed: []map[string]struct{}{
			{"prov-b/m2": {}},
		},
	}
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "prov-c/m3"}},
	}
	got := r.AllFilteredCandidates(candidates, resolved)
	if len(got) != 1 || got[0].Deployment.ID != "prov-c/m3" {
		t.Fatalf("AllFilteredCandidates = %v, want prov-c/m3 via the GetExpanded fallback", got)
	}
}
