package httpapi

// Wave-2 coverage for simple-route admin endpoints (issue #663,
// continuing PR #730). Targets the merged-tree profile's uncovered
// blocks in internal/httpapi/simple_routes.go. The admin surface on a
// test gateway needs no credentials; requests mirror the preview
// tests' loopback pattern.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave2SimpleRequest(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, rd)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var resp map[string]any
	if strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	}
	return rr.Code, resp
}

func TestWave2SimpleRouteCreateValidation(t *testing.T) {
	s := testGateway(t, config.Default())
	code, _ := wave2SimpleRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"","public_model":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("empty id: status=%d want 400", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"r1","public_model":"m1","mode":"ordered","deployments":["pA/m","pB/m"]}`)
	if code != http.StatusCreated {
		t.Fatalf("valid create: status=%d want 201", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"r1","public_model":"m1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate id: status=%d want 400", code)
	}
}

func TestWave2SimpleRouteCollisions(t *testing.T) {
	cfg := config.Default()
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "r2-pool", Mode: "explicit", Deployments: []string{"pA/m"}}}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "r3-fallback", Pools: []string{"pA/m"}}}
	s := testGateway(t, cfg)
	code, _ := wave2SimpleRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"r2","public_model":"m2"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("pool collision: status=%d want 400", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"r3","public_model":"m3"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("chain collision: status=%d want 400", code)
	}
}

func TestWave2SimpleRouteByIDErrors(t *testing.T) {
	s := testGateway(t, config.Default())
	code, _ := wave2SimpleRequest(t, s, http.MethodPut, "/admin/api/simple-routes/", `{"public_model":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("empty id: status=%d want 400", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodPut, "/admin/api/simple-routes/aaa",
		`{"id":"bbb","public_model":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("id mismatch: status=%d want 400", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodPut, "/admin/api/simple-routes/aaa",
		`{"public_model":""}`)
	if code != http.StatusBadRequest {
		t.Fatalf("normalize error: status=%d want 400", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodPut, "/admin/api/simple-routes/ghost",
		`{"public_model":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("not found: status=%d want 400", code)
	}
}

func TestWave2SimpleRouteDeleteAdvancedManaged(t *testing.T) {
	cfg := config.Default()
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "adv", PublicModel: "adv", RouteProfile: "other-profile"}}
	s := testGateway(t, cfg)
	code, _ := wave2SimpleRequest(t, s, http.MethodDelete, "/admin/api/simple-routes/adv", "")
	if code != http.StatusBadRequest {
		t.Fatalf("advanced-managed delete: status=%d want 400", code)
	}
}

func TestWave2SimpleRouteDeleteKeepsUnrelated(t *testing.T) {
	cfg := config.Default()
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "other-ve", PublicModel: "other", RouteProfile: "other-profile"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "other-profile", CandidatePool: "other-pool"}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "other-pool", Mode: "explicit", Deployments: []string{"pA/m"}}}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "other-chain", Pools: []string{"other-pool"}}}
	s := testGateway(t, cfg)
	code, _ := wave2SimpleRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"r4","public_model":"m4","deployments":["pA/m"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: status=%d want 201", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodDelete, "/admin/api/simple-routes/r4", "")
	if code != http.StatusOK {
		t.Fatalf("delete: status=%d want 200", code)
	}
	// The unrelated advanced objects must survive the removal sweep.
	cfgAfter := s.currentConfig()
	for _, id := range []string{"other-ve"} {
		found := false
		for _, ve := range cfgAfter.VirtualEndpoints {
			if ve.ID == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("virtual endpoint %q must survive deleting r4", id)
		}
	}
}

func TestWave2SimpleRouteDeleteKeepsChainReferencedPool(t *testing.T) {
	cfg := config.Default()
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "keeper-chain", Pools: []string{"r5-pool"}}}
	s := testGateway(t, cfg)
	code, _ := wave2SimpleRequest(t, s, http.MethodPost, "/admin/api/simple-routes",
		`{"id":"r5","public_model":"m5","deployments":["pA/m"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: status=%d want 201", code)
	}
	code, _ = wave2SimpleRequest(t, s, http.MethodDelete, "/admin/api/simple-routes/r5", "")
	if code != http.StatusOK {
		t.Fatalf("delete: status=%d want 200", code)
	}
	cfgAfter := s.currentConfig()
	found := false
	for _, p := range cfgAfter.CandidatePools {
		if p.ID == "r5-pool" {
			found = true
		}
	}
	if !found {
		t.Fatal("pool referenced by a surviving chain must be kept, not swept")
	}
}
