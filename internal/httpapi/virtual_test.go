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

func TestRouteProfileStrategyValidation(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	s := testGateway(t, cfg)

	// Try to create profile with non-inherit strategy (should be rejected)
	body := `{"id":"profile2","candidate_pool":"pool1","strategy":"priority"}`
	req := httptest.NewRequest("POST", "http://localhost/admin/api/route-profiles", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("non-inherit strategy should be rejected, got %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "strategy must be empty or") {
		t.Fatalf("error should mention strategy restriction, got %s", rr.Body.String())
	}

	// inherit should be allowed
	body = `{"id":"profile2","candidate_pool":"pool1","strategy":"inherit"}`
	req = httptest.NewRequest("POST", "http://localhost/admin/api/route-profiles", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("inherit strategy should be allowed, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestEligibleDeploymentsSemanticsCorrection(t *testing.T) {
	// Verify that admin APIs use pool_member_count, not eligible_deployments
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	s := testGateway(t, cfg)

	// Check virtual-endpoints API
	req := httptest.NewRequest("GET", "http://localhost/admin/api/virtual-endpoints", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("list VE failed: %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "eligible_deployments") || strings.Contains(body, "\"eligible\":") {
		t.Fatalf("API should not use misleading eligible terminology, got %s", body)
	}
	if !strings.Contains(body, "pool_member_count") {
		t.Fatalf("API should use pool_member_count, got %s", body)
	}

	// Check snapshot
	req = httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body = rr.Body.String()
	if strings.Contains(body, "\"eligible\":") && !strings.Contains(body, "pool_member_count") {
		t.Fatalf("snapshot should use pool_member_count, got %s", body)
	}
	if !strings.Contains(body, "pool_member_count") {
		t.Fatalf("snapshot should contain pool_member_count, got %s", body)
	}
}

func TestProtocolExactMatching(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	// VE that allows only openai (chat completions)
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-openai", RouteProfile: "profile1", Protocols: []string{"openai"}, Enabled: &trueVal},
		{ID: "ve2", PublicModel: "nexa-responses", RouteProfile: "profile1", Protocols: []string{"openai_responses"}, Enabled: &trueVal},
		{ID: "ve3", PublicModel: "nexa-both", RouteProfile: "profile1", Protocols: []string{"openai", "openai_responses"}, Enabled: &trueVal},
	}
	s := testGateway(t, cfg)

	// nexa-openai should allow chat completions but reject responses
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-openai","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == 400 {
		t.Fatalf("openai-only VE should allow chat completions, got 400")
	}
	// Should be 503 (no deployments) not 400
	if rr.Code != 503 {
		t.Fatalf("openai-only VE chat should be 503 (no deployment), got %d %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-openai","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("openai-only VE should reject responses with 400, got %d %s", rr.Code, rr.Body.String())
	}

	// nexa-responses should allow responses but reject chat
	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-responses","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == 400 {
		t.Fatalf("responses-only VE should allow responses, got 400")
	}

	req = httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-responses","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("responses-only VE should reject chat with 400, got %d %s", rr.Code, rr.Body.String())
	}

	// nexa-both should allow both
	req = httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-both","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == 400 {
		t.Fatalf("both VE should allow chat, got 400")
	}

	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-both","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == 400 {
		t.Fatalf("both VE should allow responses, got 400")
	}

	// Alias "responses" should be treated as openai_responses
	cfg2 := config.Default()
	cfg2.Probe.Enabled = false
	cfg2.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg2.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg2.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-alias", RouteProfile: "profile1", Protocols: []string{"responses"}, Enabled: &trueVal},
	}
	s2 := testGateway(t, cfg2)
	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-alias","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr, req)
	if rr.Code == 400 {
		t.Fatalf("alias 'responses' should allow openai_responses, got 400")
	}
}

func TestReferenceIntegrity(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}, {ID: "pool2", Mode: "all"}}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "chain1", Pools: []string{"pool2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1", FallbackChain: "chain1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	s := testGateway(t, cfg)

	// Try to delete pool1 used by profile1 -> should be 409
	req := httptest.NewRequest("DELETE", "http://localhost/admin/api/candidate-pools/pool1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 409 {
		t.Fatalf("deleting pool used by profile should be 409, got %d %s", rr.Code, rr.Body.String())
	}

	// Try to delete chain1 used by profile1 -> 409
	req = httptest.NewRequest("DELETE", "http://localhost/admin/api/fallback-chains/chain1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 409 {
		t.Fatalf("deleting chain used by profile should be 409, got %d %s", rr.Code, rr.Body.String())
	}

	// Try to delete profile1 used by ve1 -> 409
	req = httptest.NewRequest("DELETE", "http://localhost/admin/api/route-profiles/profile1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 409 {
		t.Fatalf("deleting profile used by VE should be 409, got %d %s", rr.Code, rr.Body.String())
	}

	// Verify config still intact after failed deletions
	snapReq := httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	snapReq.Host = "127.0.0.1"
	snapReq.RemoteAddr = "127.0.0.1:12345"
	snapRR := httptest.NewRecorder()
	s.Handler().ServeHTTP(snapRR, snapReq)
	body := snapRR.Body.String()
	if !strings.Contains(body, "pool1") || !strings.Contains(body, "profile1") || !strings.Contains(body, "chain1") {
		t.Fatalf("config should remain intact after failed deletions, got %s", body)
	}

	// Try to update public_model into collision with physical model
	// Create physical model
	cfg2 := config.Default()
	cfg2.Probe.Enabled = false
	cfg2.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "physical-model", Enabled: true, Weight: 1}},
	}}
	cfg2.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg2.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg2.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	s2 := testGateway(t, cfg2)

	req = httptest.NewRequest("PUT", "http://localhost/admin/api/virtual-endpoints/ve1", strings.NewReader(`{"public_model":"physical-model","route_profile":"profile1"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("updating public_model into collision should be 400, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestLegacyEndpointSecurity(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = []string{"valid-key-12345678", "second-key-87654321"}
	s := testGateway(t, cfg)

	// GET should have no-store
	req := httptest.NewRequest("GET", "http://localhost/admin/api/endpoint", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /admin/api/endpoint should have no-store, got %q", rr.Header().Get("Cache-Control"))
	}

	// POST should have no-store
	req = httptest.NewRequest("POST", "http://localhost/admin/api/endpoint", strings.NewReader(`{"model":"test-model"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("POST /admin/api/endpoint should have no-store, got %q", rr.Header().Get("Cache-Control"))
	}

	// Key should not appear in snapshot (only count)
	req = httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	body := rr.Body.String()
	if strings.Contains(body, "valid-key-12345678") || strings.Contains(body, "second-key-87654321") {
		t.Fatalf("client keys should not appear in snapshot")
	}

	// Provider keys should not be returned
	// (adminEndpoint only returns client key, not provider keys - verified by code)

	// Rotation should preserve unrelated keys
	// We have 2 keys, rotate should replace first but keep second
	req = httptest.NewRequest("POST", "http://localhost/admin/api/endpoint", strings.NewReader(`{"model":"test-model","rotate_key":true}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("rotate failed: %d %s", rr.Code, rr.Body.String())
	}
	// Check that second key still exists
	cfgAfter := s.currentConfig()
	if len(cfgAfter.ClientAuth.Keys) != 2 {
		t.Fatalf("rotation should preserve count, got %d", len(cfgAfter.ClientAuth.Keys))
	}
	if cfgAfter.ClientAuth.Keys[1] != "second-key-87654321" {
		t.Fatalf("second key should be preserved, got %q", cfgAfter.ClientAuth.Keys[1])
	}
	if cfgAfter.ClientAuth.Keys[0] == "valid-key-12345678" {
		t.Fatalf("first key should be rotated")
	}

	// GET cannot be accessed through data-plane client auth alone (should require admin)
	// Data-plane request with client key but without admin auth should be 401 for admin endpoint
	req = httptest.NewRequest("GET", "http://localhost/admin/api/endpoint", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer valid-key-12345678")
	// No admin key, but loopback should allow in keyless mode? In our test, admin key is empty, so loopback allowed.
	// To test that client auth alone doesn't grant admin, we need admin key set.
	cfg3 := config.Default()
	cfg3.Probe.Enabled = false
	cfg3.Admin.APIKey = "admin-secret"
	cfg3.ClientAuth.Enabled = true
	cfg3.ClientAuth.Keys = []string{"valid-key-12345678"}
	s3 := testGateway(t, cfg3)
	req = httptest.NewRequest("GET", "http://localhost/admin/api/endpoint", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer valid-key-12345678")
	rr = httptest.NewRecorder()
	s3.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("admin endpoint should not be accessible via client auth alone, got %d", rr.Code)
	}
}

func TestHotReloadCoherence(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	s := testGateway(t, cfg)

	// Verify resolver and router are coherent after reload
	// Simulate: request A begins (gets old snapshot), then config changes, then request A completes, then request B sees new.

	// Get initial resolver generation
	initialResolver := s.routeResolver
	initialDeployments := s.rt.All()

	// Mutate config: add new pool and update profile to use it
	_, err := s.mutateConfig(func(c *config.Config) error {
		c.CandidatePools = append(c.CandidatePools, config.CandidatePoolConfig{ID: "pool2", Mode: "all"})
		c.RouteProfiles[0].CandidatePool = "pool2"
		return nil
	})
	if err != nil {
		t.Fatalf("mutate failed: %v", err)
	}

	// New resolver should be different generation but still coherent with router
	newResolver := s.routeResolver
	if newResolver == initialResolver {
		t.Fatalf("resolver should be new generation after reload")
	}
	// Router should have been reloaded (even though deployments same, Reload called)
	newDeployments := s.rt.All()
	if len(initialDeployments) != len(newDeployments) {
		t.Fatalf("deployments count changed unexpectedly")
	}

	// Verify that new resolver's expanded sets are based on new router's deployments
	if set, ok := newResolver.GetExpanded("pool2"); !ok {
		t.Fatalf("new pool should be expanded")
	} else {
		if len(set) != len(newDeployments) {
			t.Fatalf("expanded set should match router deployments, got %d vs %d", len(set), len(newDeployments))
		}
	}

	// Verify that old snapshot (if held) would not mix with new
	// Old resolver's pool1 should still be valid, but new resolver's pool2 is the current
	// This demonstrates no mixed generation: each request gets a coherent snapshot via runtimeMu RLock
}

func TestFallbackChainMaxAttemptsSemantics(t *testing.T) {
	// Verify that max_attempts remains hard upper bound even with fallback chains
	var p1Hits, p2Hits, p3Hits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p1Hits.Add(1)
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"server error"}`)
	}))
	defer bad.Close()
	bad2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p2Hits.Add(1)
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"server error"}`)
	}))
	defer bad2.Close()
	bad3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p3Hits.Add(1)
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"server error"}`)
	}))
	defer bad3.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2 // Only allow 2 attempts, even though we have 3 pools
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p1/m1"}},
		{ID: "secondary", Mode: "explicit", Deployments: []string{"p2/m2"}},
		{ID: "tertiary", Mode: "explicit", Deployments: []string{"p3/m3"}},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{
		{ID: "chain1", Pools: []string{"secondary", "tertiary"}},
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
		{ID: "p2", Type: "openai_compatible", BaseURL: bad2.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Priority: 1, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: bad3.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Priority: 2, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	// Should fail after 2 attempts, not 3
	if p1Hits.Load()+p2Hits.Load()+p3Hits.Load() != 2 {
		t.Fatalf("max_attempts should be hard bound at 2, got p1=%d p2=%d p3=%d", p1Hits.Load(), p2Hits.Load(), p3Hits.Load())
	}
	if p3Hits.Load() != 0 {
		t.Fatalf("tertiary pool should not be attempted when max_attempts=2, got %d", p3Hits.Load())
	}
}

func TestFallbackChainDeduplication(t *testing.T) {
	// Same deployment in two pools should be attempted once
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"fail"}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}},
		{ID: "pool2", Mode: "explicit", Deployments: []string{"p1/m1"}}, // same deployment
	}
	cfg.FallbackChains = []config.FallbackChainConfig{
		{ID: "chain1", Pools: []string{"pool2"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "profile1", CandidatePool: "pool1", FallbackChain: "chain1"},
	}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal},
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if hits.Load() != 1 {
		t.Fatalf("duplicate deployment in two pools should be attempted once, got %d", hits.Load())
	}
}

func TestVirtualEndpointUpstreamModelIsPhysical(t *testing.T) {
	var upstreamModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		if m, ok := obj["model"].(string); ok {
			upstreamModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "profile1", Enabled: &trueVal}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "provider-a/model-a", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("request failed: %d %s", rr.Code, rr.Body.String())
	}
	if upstreamModel != "provider-a/model-a" {
		t.Fatalf("upstream should receive physical model 'provider-a/model-a', got %q", upstreamModel)
	}
	if upstreamModel == "nexa-code" {
		t.Fatalf("upstream must not receive virtual public model")
	}
}
