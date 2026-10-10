package httpapi

import (
	"net/http"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestCoveragePhase3AdminCRUDValidationAndReferences(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"p/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile", CandidatePool: "pool", FallbackChain: "chain"}}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "chain", Pools: []string{"pool"}}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve", PublicModel: "model", RouteProfile: "profile"}}
	s := testGateway(t, cfg)

	cases := []struct {
		name, method, path, body string
		status                   int
	}{
		{"virtual bad json", http.MethodPost, "/admin/api/virtual-endpoints", "{", 400},
		{"virtual missing id", http.MethodPost, "/admin/api/virtual-endpoints", `{"public_model":"m","route_profile":"profile"}`, 400},
		{"virtual mismatch", http.MethodPut, "/admin/api/virtual-endpoints/ve", `{"id":"other"}`, 400},
		{"virtual missing", http.MethodGet, "/admin/api/virtual-endpoints/missing", "", 404},
		{"virtual method", http.MethodPatch, "/admin/api/virtual-endpoints/ve", "", 405},
		{"profile mismatch", http.MethodPut, "/admin/api/route-profiles/profile", `{"id":"other"}`, 400},
		{"profile missing", http.MethodGet, "/admin/api/route-profiles/missing", "", 404},
		{"pool referenced", http.MethodDelete, "/admin/api/candidate-pools/pool", "", 409},
		{"chain referenced", http.MethodDelete, "/admin/api/fallback-chains/chain", "", 409},
		{"pool missing", http.MethodGet, "/admin/api/candidate-pools/missing", "", 404},
		{"chain missing", http.MethodGet, "/admin/api/fallback-chains/missing", "", 404},
		{"fallback missing pools", http.MethodPost, "/admin/api/fallback-chains", `{"id":"new"}`, 400},
		{"fallback method", http.MethodPatch, "/admin/api/fallback-chains/chain", "", 405},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := adminRequest(s, tc.method, tc.path, tc.body)
			if rr.Code != tc.status {
				t.Fatalf("status=%d want %d body=%s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
	if rr := adminRequest(s, http.MethodDelete, "/admin/api/virtual-endpoints/ve", ""); rr.Code != 200 {
		t.Fatalf("delete virtual=%d %s", rr.Code, rr.Body.String())
	}
	if rr := adminRequest(s, http.MethodDelete, "/admin/api/route-profiles/profile", ""); rr.Code != 200 {
		t.Fatalf("delete profile after endpoint=%d %s", rr.Code, rr.Body.String())
	}
	if rr := adminRequest(s, http.MethodDelete, "/admin/api/fallback-chains/chain", ""); rr.Code != 200 {
		t.Fatalf("delete chain after profile=%d %s", rr.Code, rr.Body.String())
	}
}

func TestCoveragePhase3LegacyAdminEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.PublicModel = "legacy"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true}}}}
	s := testGateway(t, cfg)
	if rr := adminRequest(s, http.MethodGet, "/admin/api/endpoint", ""); rr.Code != 200 {
		t.Fatalf("get=%d %s", rr.Code, rr.Body.String())
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{"{", 400},
		{`{"model":"auto"}`, 400},
		{`{"model":"bad model"}`, 400},
		{`{"model":"ok","rotate_key":true}`, 200},
		{`{"rotate":true}`, 200},
	} {
		rr := adminRequest(s, http.MethodPost, "/admin/api/endpoint", tc.body)
		if rr.Code != tc.status {
			t.Fatalf("body=%s status=%d want %d response=%s", tc.body, rr.Code, tc.status, rr.Body.String())
		}
	}
	if rr := adminRequest(s, http.MethodPatch, "/admin/api/endpoint", ""); rr.Code != 405 {
		t.Fatalf("method=%d", rr.Code)
	}
}
