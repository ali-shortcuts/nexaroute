package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestWave4RoutePreviewValidation(t *testing.T) {
	s, _ := codingPreviewServer(t)
	// The handler's own 405: direct call, since the authorization matrix
	// denies non-GET admin methods before the handler runs.
	{
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/route-preview?model=coding", nil)
		rr := httptest.NewRecorder()
		s.adminRoutePreview(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	// A text parameter beyond the 512-character cap.
	if code, resp, _ := previewRequest(t, s, "model=coding&session_key="+strings.Repeat("k", 513)); code != http.StatusBadRequest {
		t.Fatalf("oversize session_key: status=%d body=%v, want 400", code, resp)
	}
	// A provider_type beyond its tighter 64-character cap.
	if code, resp, _ := previewRequest(t, s, "model=coding&provider_type="+strings.Repeat("p", 65)); code != http.StatusBadRequest {
		t.Fatalf("oversize provider_type: status=%d body=%v, want 400", code, resp)
	}
}

// A deployment the funnel includes but the virtual endpoint's pools do
// not admit is relabeled in the trace instead of silently vanishing.
func TestWave4RoutePreviewPoolExclusionRelabel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	for i, name := range []string{"A", "B"} {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "p" + name, Name: name, Type: "openai_compatible", BaseURL: up.URL,
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "physical-" + name, Enabled: true,
				Priority: i, Weight: 1, ContextWindow: 200000,
				Capabilities: config.Capabilities{Streaming: true}}},
		})
	}
	trueVal := true
	// Only pA/m is a pool member; pB/m stays funnel-eligible but out of
	// the virtual endpoint's pools.
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool-a", Name: "Pool A", Mode: "explicit", Deployments: []string{"pA/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile-a", CandidatePool: "pool-a"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding", PublicModel: "coding", RouteProfile: "profile-a", Enabled: &trueVal, Protocols: []string{"openai"}}}
	s := testGateway(t, cfg)
	code, resp, rr := previewRequest(t, s, "model=coding&protocol=openai")
	if code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", code, rr.Body.String())
	}
	expl := previewExplanationOf(t, resp)
	byID := map[string]previewDecision{}
	for _, cd := range expl.Decisions {
		byID[cd.DeploymentID] = cd
	}
	b, ok := byID["pB/m"]
	if !ok {
		t.Fatalf("decisions = %+v, want a pB/m verdict", expl.Decisions)
	}
	if b.Included || b.ExcludedReason != "not_in_virtual_endpoint_pools" {
		t.Fatalf("pB/m = %+v, want excluded as not_in_virtual_endpoint_pools", b)
	}
	if resp["excluded_count"] == float64(0) {
		t.Fatalf("excluded_count = %v, want >= 1", resp["excluded_count"])
	}
	eligible, ok := resp["eligible_candidates"].([]any)
	if !ok || len(eligible) != 1 {
		t.Fatalf("eligible = %v, want exactly pA/m", resp["eligible_candidates"])
	}
	if eligible[0].(map[string]any)["deployment"] != "pA/m" {
		t.Fatalf("eligible = %v, want pA/m", eligible)
	}
	selected, ok := resp["selected"].(map[string]any)
	if !ok || selected["deployment"] != "pA/m" {
		t.Fatalf("selected = %v, want pA/m", resp["selected"])
	}
}
