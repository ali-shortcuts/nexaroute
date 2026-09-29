package route

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Audit item 7: route was at 56.3% — list/getter helpers had zero coverage.
func auditResolver() *Resolver {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "primary", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
			{ID: "secondary", Mode: "explicit", Deployments: []string{"prov-b/m2"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "rp1", CandidatePool: "primary", FallbackChain: "fc1"},
		},
		FallbackChains: []config.FallbackChainConfig{
			{ID: "fc1", Pools: []string{"primary", "secondary"}},
		},
		VirtualEndpoints: []config.VirtualEndpointConfig{
			{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "rp1"},
		},
	}
	cfg.ApplyDefaults()
	return NewResolver(cfg, deployments())
}

func TestAuditResolveByID(t *testing.T) {
	r := auditResolver()
	ve, ok := r.ResolveByID("ve1")
	if !ok || ve.PublicModel != "nexa-code" {
		t.Fatalf("ResolveByID = %+v, %v", ve, ok)
	}
	if _, ok := r.ResolveByID("missing"); ok {
		t.Fatal("missing ID should not resolve")
	}
}

func TestAuditListHelpers(t *testing.T) {
	r := auditResolver()
	if len(r.ListVirtualEndpoints()) != 1 {
		t.Fatalf("VEs = %d, want 1", len(r.ListVirtualEndpoints()))
	}
	if len(r.ListRouteProfiles()) != 1 {
		t.Fatalf("profiles = %d, want 1", len(r.ListRouteProfiles()))
	}
	if len(r.ListCandidatePools()) != 2 {
		t.Fatalf("pools = %d, want 2", len(r.ListCandidatePools()))
	}
	if len(r.ListFallbackChains()) != 1 {
		t.Fatalf("chains = %d, want 1", len(r.ListFallbackChains()))
	}
}

func TestAuditGetExpanded(t *testing.T) {
	r := auditResolver()
	set, ok := r.GetExpanded("primary")
	if !ok || len(set) == 0 {
		t.Fatalf("expanded = %v, %v", set, ok)
	}
	if _, ok := set["prov-a/m1"]; !ok {
		t.Fatal("prov-a/m1 should be in expanded primary")
	}
	if _, ok := r.GetExpanded("missing"); ok {
		t.Fatal("missing pool should not expand")
	}
}

func TestAuditAllFilteredCandidates(t *testing.T) {
	r := auditResolver()
	resolved, ok := r.Resolve("nexa-code")
	if !ok {
		t.Fatal("should resolve nexa-code")
	}
	cands := []router.Scored{
		{Deployment: router.Deployment{ID: "prov-a/m1"}},
		{Deployment: router.Deployment{ID: "prov-b/m2"}},
		{Deployment: router.Deployment{ID: "prov-c/m3"}},
	}
	out := r.AllFilteredCandidates(cands, resolved)
	if len(out) == 0 {
		t.Fatal("expected some filtered candidates")
	}
	seen := map[string]bool{}
	for _, c := range out {
		if seen[c.Deployment.ID] {
			t.Fatalf("duplicate %s", c.Deployment.ID)
		}
		seen[c.Deployment.ID] = true
	}
	if !seen["prov-a/m1"] || !seen["prov-b/m2"] {
		t.Fatalf("primary+fallback should be present: %v", seen)
	}
	if seen["prov-c/m3"] {
		t.Fatal("prov-c/m3 must be filtered out")
	}
}

func TestAuditFilterCandidatesModes(t *testing.T) {
	cands := []router.Scored{{Deployment: router.Deployment{ID: "a"}}, {Deployment: router.Deployment{ID: "b"}}}
	if got := FilterCandidates(cands, nil, "all"); len(got) != 2 {
		t.Fatalf("all/nil = %d, want 2", len(got))
	}
	if got := FilterCandidates(cands, map[string]struct{}{}, "explicit"); len(got) != 0 {
		t.Fatalf("explicit/empty = %d, want 0", len(got))
	}
	if got := FilterCandidates(cands, map[string]struct{}{"a": {}}, "explicit"); len(got) != 1 {
		t.Fatalf("explicit/a = %d, want 1", len(got))
	}
}
