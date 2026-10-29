package httpapi

// Wave-2 coverage for the route-preview simulation (issue #663,
// continuing PR #730). Targets the merged-tree profile's uncovered
// blocks in internal/httpapi/route_preview.go. The preview surface never
// calls a provider: every upstream below answers 502 and the tests
// assert zero hits.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// wave2PreviewServer mirrors codingPreviewServer with extension points:
// extra virtual endpoints, profiles, pools, fallback chains, and a third
// provider (pC) for added-deployment comparisons.
func wave2PreviewServer(t *testing.T, extraVEs []config.VirtualEndpointConfig, extraProfiles []config.RouteProfileConfig, extraPools []config.CandidatePoolConfig, extraChains []config.FallbackChainConfig, withC bool) (*Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.MaxAttempts = 3
	var deployments []string
	providers := []struct {
		id, name string
		tools    bool
	}{{"pA", "A", true}, {"pB", "B", false}}
	if withC {
		// pC is registered as a provider but deliberately NOT added to
		// coding-pool: it exists only so a fallback chain can ADD it to
		// a comparison (the added_deployments loop).
		providers = append(providers, struct {
			id, name string
			tools    bool
		}{"pC", "C", false})
	}
	for i, p := range providers {
		id := p.id + "/m"
		if p.id != "pC" {
			deployments = append(deployments, id)
		}
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: p.id, Name: p.name, Type: "openai_compatible", BaseURL: up.URL,
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{
				ID: "m", Model: "physical-" + p.name, Aliases: []string{"physical-" + p.name, "shared-alias"}, Enabled: true,
				Priority: i, Weight: 1, ContextWindow: 200000,
				Capabilities: config.Capabilities{Streaming: true, Tools: p.tools},
			}},
		})
	}
	trueVal := true
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "coding-pool", Name: "Coding pool", Mode: "explicit", Deployments: deployments},
		{ID: "a-only-pool", Name: "A only pool", Mode: "explicit", Deployments: []string{"pA/m"}},
	}
	cfg.CandidatePools = append(cfg.CandidatePools, extraPools...)
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "coding-profile", CandidatePool: "coding-pool"},
		{ID: "a-only-profile", CandidatePool: "a-only-pool"},
		{ID: "private-profile", CandidatePool: "coding-pool", Privacy: "no_training"},
	}
	cfg.RouteProfiles = append(cfg.RouteProfiles, extraProfiles...)
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding", PublicModel: "coding", RouteProfile: "coding-profile", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.VirtualEndpoints = append(cfg.VirtualEndpoints, extraVEs...)
	cfg.FallbackChains = append(cfg.FallbackChains, extraChains...)
	return testGateway(t, cfg), &hits
}

func wave2PreviewComparison(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	comp, ok := resp["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("comparison missing: %#v", resp)
	}
	return comp
}

func TestWave2PreviewDisabledVirtualEndpoint(t *testing.T) {
	disabled := false
	s, hits := wave2PreviewServer(t,
		[]config.VirtualEndpointConfig{{ID: "coding-off", PublicModel: "coding-off", RouteProfile: "coding-profile", Enabled: &disabled}},
		nil, nil, nil, false)
	code, resp, _ := previewRequest(t, s, "model=coding-off&protocol=openai")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if errText, _ := resp["error"].(string); !strings.Contains(errText, "disabled") {
		t.Fatalf("error=%q want disabled-endpoint error", errText)
	}
	if resp["selected"] != nil {
		t.Fatalf("selected=%v want nil", resp["selected"])
	}
	if elig, _ := resp["eligible_candidates"].([]any); len(elig) != 0 {
		t.Fatalf("eligible_candidates=%v want empty", elig)
	}
	if excl, _ := resp["excluded_count"].(float64); excl != 0 {
		t.Fatalf("excluded_count=%v want 0", excl)
	}
	if _, ok := resp["virtual_endpoint"]; !ok {
		t.Fatal("virtual_endpoint missing from disabled-endpoint preview")
	}
	if hits.Load() != 0 {
		t.Fatalf("preview called upstream %d times, want 0", hits.Load())
	}
}

func TestWave2PreviewCompareMissingPool(t *testing.T) {
	s, hits := wave2PreviewServer(t, nil,
		[]config.RouteProfileConfig{{ID: "bad-profile", CandidatePool: "missing-pool"}},
		nil, nil, false)
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&compare_profile=bad-profile")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	comp := wave2PreviewComparison(t, resp)
	if errText, _ := comp["error"].(string); !strings.Contains(errText, "could not be expanded") {
		t.Fatalf("comparison error=%q want pool-expansion error", errText)
	}
	if comp["selected"] != nil || comp["selected_changed"] != false {
		t.Fatalf("comparison=%v want nil selection, unchanged", comp)
	}
	for _, k := range []string{"eligible_candidates", "added_deployments", "removed_deployments"} {
		if v, _ := comp[k].([]any); len(v) != 0 {
			t.Fatalf("comparison %s=%v want empty", k, v)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("preview called upstream %d times, want 0", hits.Load())
	}
}

func TestWave2PreviewCompareMissingChain(t *testing.T) {
	s, _ := wave2PreviewServer(t, nil,
		[]config.RouteProfileConfig{{ID: "ghost-chain-profile", CandidatePool: "coding-pool", FallbackChain: "no-such-chain"}},
		nil, nil, false)
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&compare_profile=ghost-chain-profile")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	comp := wave2PreviewComparison(t, resp)
	if errText, _ := comp["error"].(string); !strings.Contains(errText, "could not be expanded") {
		t.Fatalf("comparison error=%q want pool-expansion error", errText)
	}
	if comp["selected"] != nil {
		t.Fatalf("comparison selected=%v want nil", comp["selected"])
	}
}

func TestWave2PreviewExpandPoolsNilResolver(t *testing.T) {
	// The handler always builds a resolver, so the nil-resolver guard in
	// expandProfilePools is reachable only as a pure function.
	members, ok := expandProfilePools(config.Default(), nil, config.RouteProfileConfig{ID: "p", CandidatePool: "coding-pool"})
	if ok || members != nil {
		t.Fatalf("nil resolver: members=%v ok=%v want nil,false", members, ok)
	}
}

func TestWave2PreviewCompareFallbackChainAddsDeployment(t *testing.T) {
	s, hits := wave2PreviewServer(t, nil,
		[]config.RouteProfileConfig{{ID: "chain-profile", CandidatePool: "coding-pool", FallbackChain: "chain1"}},
		[]config.CandidatePoolConfig{{ID: "c-pool", Mode: "explicit", Deployments: []string{"pC/m"}}},
		[]config.FallbackChainConfig{{ID: "chain1", Pools: []string{"c-pool"}}},
		true)
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&compare_profile=chain-profile")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	comp := wave2PreviewComparison(t, resp)
	if comp["fallback_chain"] != "chain1" {
		t.Fatalf("fallback_chain=%v want chain1", comp["fallback_chain"])
	}
	added, _ := comp["added_deployments"].([]any)
	if len(added) != 1 || added[0] != "pC/m" {
		t.Fatalf("added_deployments=%v want [pC/m]", added)
	}
	if comp["selected_changed"] != false {
		t.Fatalf("selected_changed=%v want false (pA/m still first)", comp["selected_changed"])
	}
	if hits.Load() != 0 {
		t.Fatalf("preview called upstream %d times, want 0", hits.Load())
	}
}

func TestWave2PreviewSessionPinPromotion(t *testing.T) {
	s, hits := wave2PreviewServer(t, nil, nil, nil, nil, false)
	// Pin the second candidate for this session; the funnel must promote
	// it to the front even though the baseline order picks pA/m.
	s.rt.ObserveSession(router.Requirement{Model: "coding", SessionKey: "pin-secret"}, "pB/m")
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&session_key=pin-secret")
	if code != http.StatusOK {
		t.Fatalf("status=%d want 200", code)
	}
	if got := previewSelectedID(t, resp); got != "pB/m" {
		t.Fatalf("selected=%q want pB/m (pinned promotion)", got)
	}
	if strings.Contains(jsonString(resp), "pin-secret") {
		t.Fatal("preview echoed the session key")
	}
	if hits.Load() != 0 {
		t.Fatalf("preview called upstream %d times, want 0", hits.Load())
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
