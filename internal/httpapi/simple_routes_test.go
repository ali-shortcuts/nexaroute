package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func simpleRouteRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func simpleRouteTestConfig() config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com/v1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: "https://example.org/v1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
	}
	return cfg
}

func TestSimpleRouteCreateUpdateDeleteAtomic(t *testing.T) {
	s := testGateway(t, simpleRouteTestConfig())

	rr := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", "{\"id\":\"route-coding\",\"name\":\"Coding\",\"public_model\":\"coding\",\"mode\":\"automatic\",\"deployments\":[\"p1/m1\",\"p2/m2\"],\"enabled\":true}")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d %s", rr.Code, rr.Body.String())
	}
	cfg := s.currentConfig()
	if len(cfg.VirtualEndpoints) != 1 || cfg.VirtualEndpoints[0].ID != "route-coding" {
		t.Fatalf("create: virtual endpoint missing: %+v", cfg.VirtualEndpoints)
	}
	if len(cfg.RouteProfiles) != 1 || cfg.RouteProfiles[0].ID != "route-coding-profile" {
		t.Fatalf("create: profile missing: %+v", cfg.RouteProfiles)
	}
	if len(cfg.CandidatePools) != 1 || cfg.CandidatePools[0].ID != "route-coding-pool" || len(cfg.CandidatePools[0].Deployments) != 2 {
		t.Fatalf("create: pool mismatch: %+v", cfg.CandidatePools)
	}
	if len(cfg.FallbackChains) != 0 {
		t.Fatalf("create: unexpected fallback chain: %+v", cfg.FallbackChains)
	}

	rr = simpleRouteRequest(t, s, http.MethodPut, "/admin/api/simple-routes/route-coding", "{\"name\":\"Coding\",\"public_model\":\"coding\",\"mode\":\"ordered\",\"deployments\":[\"p2/m2\",\"p1/m1\"],\"enabled\":true}")
	if rr.Code != http.StatusOK {
		t.Fatalf("update: got %d %s", rr.Code, rr.Body.String())
	}
	cfg = s.currentConfig()
	if len(cfg.CandidatePools) != 2 {
		t.Fatalf("ordered update: expected 2 stage pools, got %+v", cfg.CandidatePools)
	}
	if cfg.CandidatePools[0].ID != "route-coding-stage-1" || cfg.CandidatePools[0].Deployments[0] != "p2/m2" {
		t.Fatalf("ordered update: first stage mismatch: %+v", cfg.CandidatePools[0])
	}
	if len(cfg.FallbackChains) != 1 || cfg.FallbackChains[0].ID != "route-coding-fallback" || len(cfg.FallbackChains[0].Pools) != 2 {
		t.Fatalf("ordered update: fallback mismatch: %+v", cfg.FallbackChains)
	}
	if cfg.RouteProfiles[0].CandidatePool != "route-coding-stage-1" || cfg.RouteProfiles[0].FallbackChain != "route-coding-fallback" {
		t.Fatalf("ordered update: profile mismatch: %+v", cfg.RouteProfiles[0])
	}

	rr = simpleRouteRequest(t, s, http.MethodDelete, "/admin/api/simple-routes/route-coding", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: got %d %s", rr.Code, rr.Body.String())
	}
	cfg = s.currentConfig()
	if len(cfg.VirtualEndpoints) != 0 || len(cfg.RouteProfiles) != 0 || len(cfg.CandidatePools) != 0 || len(cfg.FallbackChains) != 0 {
		t.Fatalf("delete left managed primitives: ve=%v rp=%v cp=%v fc=%v", cfg.VirtualEndpoints, cfg.RouteProfiles, cfg.CandidatePools, cfg.FallbackChains)
	}
}

func TestSimpleRouteRejectsAdvancedManagedEndpoint(t *testing.T) {
	cfg := simpleRouteTestConfig()
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "custom-pool", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "custom-profile", CandidatePool: "custom-pool"}}
	on := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "advanced", Name: "Advanced", PublicModel: "advanced", RouteProfile: "custom-profile", Enabled: &on}}
	s := testGateway(t, cfg)

	rr := simpleRouteRequest(t, s, http.MethodPut, "/admin/api/simple-routes/advanced", "{\"name\":\"Advanced\",\"public_model\":\"advanced\",\"mode\":\"automatic\",\"deployments\":[\"p2/m2\"]}")
	if rr.Code == http.StatusOK {
		t.Fatalf("advanced-managed endpoint must not be overwritten by simple-route API")
	}
	got := s.currentConfig()
	if got.VirtualEndpoints[0].RouteProfile != "custom-profile" || got.CandidatePools[0].Deployments[0] != "p1/m1" {
		t.Fatalf("advanced route mutated after rejected update: %+v %+v", got.VirtualEndpoints, got.CandidatePools)
	}
}

func TestSimpleRouteRejectsPartialInvalidMutation(t *testing.T) {
	s := testGateway(t, simpleRouteTestConfig())
	rr := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", "{\"id\":\"route-bad\",\"name\":\"Bad\",\"public_model\":\"bad route with spaces\",\"mode\":\"automatic\",\"deployments\":[\"p1/m1\"]}")
	if rr.Code == http.StatusCreated {
		t.Fatalf("invalid route unexpectedly created")
	}
	cfg := s.currentConfig()
	if len(cfg.VirtualEndpoints) != 0 || len(cfg.RouteProfiles) != 0 || len(cfg.CandidatePools) != 0 {
		t.Fatalf("invalid mutation partially persisted: ve=%v rp=%v cp=%v", cfg.VirtualEndpoints, cfg.RouteProfiles, cfg.CandidatePools)
	}
}


func TestSimpleRouteInvalidUpdatePreservesOldRoute(t *testing.T) {
	s := testGateway(t, simpleRouteTestConfig())
	rr := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", "{\"id\":\"route-safe\",\"name\":\"Safe\",\"public_model\":\"safe\",\"mode\":\"automatic\",\"deployments\":[\"p1/m1\"]}")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d %s", rr.Code, rr.Body.String())
	}

	rr = simpleRouteRequest(t, s, http.MethodPut, "/admin/api/simple-routes/route-safe", "{\"name\":\"Broken\",\"public_model\":\"invalid model with spaces\",\"mode\":\"ordered\",\"deployments\":[\"p2/m2\",\"p1/m1\"]}")
	if rr.Code == http.StatusOK {
		t.Fatalf("invalid update unexpectedly succeeded")
	}

	cfg := s.currentConfig()
	if len(cfg.VirtualEndpoints) != 1 || cfg.VirtualEndpoints[0].PublicModel != "safe" {
		t.Fatalf("old endpoint was not preserved: %+v", cfg.VirtualEndpoints)
	}
	if len(cfg.CandidatePools) != 1 || cfg.CandidatePools[0].ID != "route-safe-pool" || len(cfg.CandidatePools[0].Deployments) != 1 || cfg.CandidatePools[0].Deployments[0] != "p1/m1" {
		t.Fatalf("old pool was not preserved: %+v", cfg.CandidatePools)
	}
	if len(cfg.FallbackChains) != 0 {
		t.Fatalf("invalid update leaked fallback objects: %+v", cfg.FallbackChains)
	}
}

func TestSimpleRouteCleanupPreservesSharedManagedPool(t *testing.T) {
	s := testGateway(t, simpleRouteTestConfig())
	rr := simpleRouteRequest(t, s, http.MethodPost, "/admin/api/simple-routes", "{\"id\":\"route-shared\",\"name\":\"Shared\",\"public_model\":\"shared\",\"mode\":\"automatic\",\"deployments\":[\"p1/m1\"]}")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d %s", rr.Code, rr.Body.String())
	}

	if _, err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.RouteProfiles = append(cfg.RouteProfiles, config.RouteProfileConfig{ID: "advanced-profile", Name: "Advanced", CandidatePool: "route-shared-pool"})
		return nil
	}); err != nil {
		t.Fatalf("add advanced reference: %v", err)
	}

	rr = simpleRouteRequest(t, s, http.MethodDelete, "/admin/api/simple-routes/route-shared", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete: got %d %s", rr.Code, rr.Body.String())
	}

	cfg := s.currentConfig()
	if len(cfg.VirtualEndpoints) != 0 {
		t.Fatalf("simple endpoint remained: %+v", cfg.VirtualEndpoints)
	}
	if len(cfg.RouteProfiles) != 1 || cfg.RouteProfiles[0].ID != "advanced-profile" {
		t.Fatalf("advanced profile lost: %+v", cfg.RouteProfiles)
	}
	if len(cfg.CandidatePools) != 1 || cfg.CandidatePools[0].ID != "route-shared-pool" {
		t.Fatalf("shared pool should be preserved: %+v", cfg.CandidatePools)
	}
}
