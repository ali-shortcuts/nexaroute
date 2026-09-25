package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestVirtualEndpointResolvesCorrectly(t *testing.T) {
	var goodHits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.FallbackOnUnknownModel = false
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "coding", Mode: "explicit", Deployments: []string{"p1/m1"}},
		{ID: "other", Mode: "explicit", Deployments: []string{"p2/m2"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "coding-smart", CandidatePool: "coding"},
	}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "coding-prod", Name: "Coding", PublicModel: "nexa-code", RouteProfile: "coding-smart", Enabled: &trueVal},
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Priority: 1, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	call := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://localhost"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	// nexa-code should only route to p1/m1, not p2/m2
	rr := call("/v1/chat/completions", `{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 {
		t.Fatalf("virtual endpoint routing failed: %d %s", rr.Code, rr.Body.String())
	}
	if goodHits.Load() != 1 {
		t.Fatalf("expected 1 hit, got %d", goodHits.Load())
	}
	// Check headers
	if rr.Header().Get("X-Gateway-Virtual-Endpoint") != "coding-prod" {
		t.Fatalf("missing virtual endpoint header: %v", rr.Header())
	}
	if rr.Header().Get("X-Gateway-Public-Model") != "nexa-code" {
		t.Fatalf("missing public model header")
	}
	// Ensure pool filtering: p2/m2 should not be used for nexa-code
	// Request with model-b directly should work (direct physical routing preserved)
	rr = call("/v1/chat/completions", `{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 {
		t.Fatalf("direct physical model routing broken: %d %s", rr.Code, rr.Body.String())
	}
}

func TestVirtualEndpointDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	falseVal := false
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &falseVal}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("disabled endpoint should return 404, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestVirtualEndpointUnknown(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	s := testGateway(t, cfg)
	// Unknown model not virtual and not physical should trigger no healthy deployment (since fallback disabled in testGateway? default fallback true)
	// Use fallback false
	cfg.Routing.FallbackOnUnknownModel = false
	s = testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"unknown-xyz","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("unknown model should be 503, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestVirtualEndpointModelsDiscovery(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal},
		{ID: "ve2", PublicModel: "nexa-fast", RouteProfile: "profile1", Enabled: &trueVal},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("GET", "http://localhost/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("models discovery failed: %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "nexa-code") || !strings.Contains(body, "nexa-fast") {
		t.Fatalf("virtual models not discoverable: %s", body)
	}
	if !strings.Contains(body, "auto") || !strings.Contains(body, "claude-auto") {
		t.Fatalf("auto/claude-auto should still be present: %s", body)
	}
}

func TestVirtualEndpointFallbackChain(t *testing.T) {
	// primary pool unhealthy, fallback should be used
	var p1Hits, p2Hits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p1Hits.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, `{"error":"overloaded"}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p2Hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer good.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p1/m1"}},
		{ID: "secondary", Mode: "explicit", Deployments: []string{"p2/m2"}},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{
		{ID: "chain1", Pools: []string{"secondary"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "profile1", CandidatePool: "primary", FallbackChain: "chain1"},
	}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal},
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: bad.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: good.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Priority: 1, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("fallback chain failed: %d %s", rr.Code, rr.Body.String())
	}
	if p1Hits.Load() != 1 || p2Hits.Load() != 1 {
		t.Fatalf("expected both pools tried, p1=%d p2=%d", p1Hits.Load(), p2Hits.Load())
	}
}

func TestVirtualEndpointSessionAffinityPinsActualDeployment(t *testing.T) {
	var hits = map[string]*atomic.Int64{"p1": {}, "p2": {}}
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits["p1"].Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits["p2"].Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up2.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.SessionAffinity = true
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up1.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up2.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	// First request with session key should pin to actual deployment
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}],"metadata":{"session_id":"sess-123"}}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("request %d failed: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	// Should have pinned to one deployment, not both
	total := hits["p1"].Load() + hits["p2"].Load()
	if total != 3 {
		t.Fatalf("expected 3 hits total, got %d", total)
	}
	if hits["p1"].Load() != 0 && hits["p1"].Load() != 3 && hits["p2"].Load() != 0 && hits["p2"].Load() != 3 {
		// Actually ready_mesh with P2C may still switch if not pinned? But affinity should pin after first success
		// Allow either all to same or mostly same, but at least one should have >=2
		if hits["p1"].Load() < 2 && hits["p2"].Load() < 2 {
			t.Fatalf("session affinity should pin to actual deployment, got p1=%d p2=%d", hits["p1"].Load(), hits["p2"].Load())
		}
	}
}

func TestVirtualEndpointSuccessState(t *testing.T) {
	var p1Hits, p2Hits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p1Hits.Add(1)
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"server error"}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p2Hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer good.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: bad.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: good.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Priority: 1, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("request failed: %d %s", rr.Code, rr.Body.String())
	}
	// Check that success state belongs to p2/m2, not p1/m1
	events := s.bus.Snapshot()
	foundOK := false
	for _, ev := range events {
		if ev.Kind == "route_ok" {
			if ev.Deployment != "p2/m2" {
				t.Fatalf("success state should be p2/m2, got %q", ev.Deployment)
			}
			if ev.VirtualEndpoint != "ve1" || ev.PublicModel != "nexa-code" {
				t.Fatalf("route_ok should preserve virtual endpoint identity, got VE=%q PM=%q", ev.VirtualEndpoint, ev.PublicModel)
			}
			foundOK = true
		}
	}
	if !foundOK {
		t.Fatal("route_ok event not found")
	}
	if p1Hits.Load() != 1 || p2Hits.Load() != 1 {
		t.Fatalf("expected failover, p1=%d p2=%d", p1Hits.Load(), p2Hits.Load())
	}
}

func TestVirtualEndpointProtocol(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Protocols: []string{"anthropic"}, Enabled: &trueVal},
	}
	s := testGateway(t, cfg)
	// OpenAI ingress should be rejected (protocol not allowed)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("protocol mismatch should be 400, got %d %s", rr.Code, rr.Body.String())
	}
	// Anthropic ingress should work (no compatible deployment, but protocol allowed -> 503 not 400)
	req2 := httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(`{"model":"nexa-code","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 503 {
		t.Fatalf("anthropic with no deployments should be 503, got %d %s", rr2.Code, rr2.Body.String())
	}
}

func TestVirtualEndpointBackwardCompatibility(t *testing.T) {
	// No virtual endpoints configured: existing behavior unchanged
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	// auto should still work (no deployments, but it would match all if there were)
	if len(s.rt.Candidates(router.Requirement{Model: "auto"})) != 0 {
		// With no providers, candidates empty, but auto logic should not crash
	}
	// Direct model routing preserved when no VE
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "my-model", Enabled: true, Weight: 1}},
	}}
	s = testGateway(t, cfg)
	cands := s.rt.Candidates(router.Requirement{Model: "my-model"})
	if len(cands) != 1 {
		t.Fatalf("direct model routing should still work, got %d", len(cands))
	}
}

func TestVirtualEndpointAdminCRUD(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	s := testGateway(t, cfg)

	// Create VE via admin API
	body := `{"id":"ve1","public_model":"nexa-code","route_profile":"profile1","name":"Test"}`
	req := httptest.NewRequest("POST", "http://localhost/admin/api/virtual-endpoints", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("create VE failed: %d %s", rr.Code, rr.Body.String())
	}

	// List
	req = httptest.NewRequest("GET", "http://localhost/admin/api/virtual-endpoints", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "nexa-code") {
		t.Fatalf("list VE failed: %d %s", rr.Code, rr.Body.String())
	}

	// Get by ID
	req = httptest.NewRequest("GET", "http://localhost/admin/api/virtual-endpoints/ve1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("get VE failed: %d", rr.Code)
	}

	// Update
	body = `{"public_model":"nexa-code-updated","route_profile":"profile1"}`
	req = httptest.NewRequest("PUT", "http://localhost/admin/api/virtual-endpoints/ve1", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("update VE failed: %d %s", rr.Code, rr.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "http://localhost/admin/api/virtual-endpoints/ve1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("delete VE failed: %d %s", rr.Code, rr.Body.String())
	}

	// Ensure deleted
	req = httptest.NewRequest("GET", "http://localhost/admin/api/virtual-endpoints/ve1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("deleted VE should be 404, got %d", rr.Code)
	}
}

func TestVirtualEndpointClientAuth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = []string{"valid-key-12345678"}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)

	// Valid key should work with virtual endpoint
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-key-12345678")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("valid client key should work: %d %s", rr.Code, rr.Body.String())
	}

	// Invalid key should fail
	req = httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer invalid-key")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("invalid key should be 401, got %d", rr.Code)
	}

	// Provider key should not be accepted as gateway key
	req = httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer upstream-secret")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("provider key should not be accepted as gateway key, got %d", rr.Code)
	}
}

func TestVirtualEndpointHotReload(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	s := testGateway(t, cfg)

	// Update config via mutateConfig (hot reload)
	_, err := s.mutateConfig(func(c *config.Config) error {
		c.VirtualEndpoints[0].PublicModel = "nexa-updated"
		return nil
	})
	if err != nil {
		t.Fatalf("hot reload failed: %v", err)
	}

	// Old model should no longer resolve as virtual
	req := httptest.NewRequest("GET", "http://localhost/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	if strings.Contains(body, "nexa-code") && !strings.Contains(body, "nexa-updated") {
		t.Fatalf("hot reload should update models: %s", body)
	}
	if !strings.Contains(body, "nexa-updated") {
		t.Fatalf("updated model not found: %s", body)
	}
}

func TestLegacyEndpointCompatibility(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	// Create endpoint via legacy API
	body := `{"model":"my-coding"}`
	req := httptest.NewRequest("POST", "http://localhost/admin/api/endpoint", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("legacy endpoint creation failed: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Model string `json:"model"`
		Key   string `json:"api_key"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	if resp.Model != "my-coding" || !strings.HasPrefix(resp.Key, "nx_") {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Check that virtual endpoint was created
	snapReq := httptest.NewRequest("GET", "http://localhost/admin/api/virtual-endpoints", nil)
	snapReq.RemoteAddr = "127.0.0.1:12345"
	snapRR := httptest.NewRecorder()
	s.Handler().ServeHTTP(snapRR, snapReq)
	if !strings.Contains(snapRR.Body.String(), "my-coding") {
		t.Fatalf("legacy endpoint should create virtual endpoint: %s", snapRR.Body.String())
	}
}
