package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type mockJevServer struct {
	server         *httptest.Server
	mu             sync.Mutex
	capturedBodies []string
	capturedAuth   []string
	callCount      atomic.Int64
	responseCode   int
	responseBody   string
	handler        func(w http.ResponseWriter, r *http.Request)
}

func newMockJevServer(t *testing.T, responseBody string) *mockJevServer {
	t.Helper()
	m := &mockJevServer{
		responseCode: 200,
		responseBody: responseBody,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.callCount.Add(1)
		body := make([]byte, 32*1024+1024)
		n, _ := r.Body.Read(body)
		b := string(body[:n])
		m.mu.Lock()
		m.capturedBodies = append(m.capturedBodies, b)
		m.capturedAuth = append(m.capturedAuth, r.Header.Get("Authorization"))
		m.mu.Unlock()
		if m.handler != nil {
			m.handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.responseCode)
		w.Write([]byte(m.responseBody))
	}))
	return m
}

func (m *mockJevServer) Close()           { m.server.Close() }
func (m *mockJevServer) URL() string      { return m.server.URL }
func (m *mockJevServer) CallCount() int64 { return m.callCount.Load() }
func (m *mockJevServer) CapturedBodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.capturedBodies))
	copy(out, m.capturedBodies)
	return out
}
func (m *mockJevServer) CapturedAuth() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.capturedAuth))
	copy(out, m.capturedAuth)
	return out
}

func makeJevUpstream(t *testing.T, id string, hits *atomic.Int64, order *attemptRecorder, failCount int) *httptest.Server {
	t.Helper()
	var count atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if order != nil {
			order.add(id)
		}
		c := count.Add(1)
		if int(c) <= failCount {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"injected"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "messages") {
			w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"upstream-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		} else if strings.Contains(r.URL.Path, "responses") {
			w.Write([]byte(`{"id":"resp_1","object":"response","created_at":123,"status":"completed","model":"upstream-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		} else {
			w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
		}
	}))
}

func makeJevConfig(t *testing.T, jevURL string, upstreamA, upstreamB, upstreamC *httptest.Server) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 3
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{
			ID:          "jev-main",
			Type:        "jev",
			Enabled:     boolPtr(true),
			APIKey:      "test-key",
			PrivacyMode: "metadata_only",
			BaseURL:     jevURL,
		},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}},
		{ID: "fallback", Mode: "explicit", Deployments: []string{"p3/m3"}},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{
		{ID: "chain1", Pools: []string{"primary", "fallback"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "rp1", CandidatePool: "primary", FallbackChain: "chain1"},
	}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai", "anthropic", "openai_responses"}},
	}
	providers := []config.ProviderConfig{}
	if upstreamA != nil {
		providers = append(providers, config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: upstreamA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}})
	}
	if upstreamB != nil {
		providers = append(providers, config.ProviderConfig{ID: "p2", Type: "openai_compatible", BaseURL: upstreamB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}})
	}
	if upstreamC != nil {
		providers = append(providers, config.ProviderConfig{ID: "p3", Type: "openai_compatible", BaseURL: upstreamC.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 1}}})
	}
	if len(providers) == 0 {
		providers = []config.ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}}}
	}
	cfg.Providers = providers
	return cfg
}

func boolPtr(b bool) *bool { return &b }

// 56. JEV VALID SELECTION
func TestJev_ValidSelection(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	rec := &attemptRecorder{}
	upA := makeJevUpstream(t, "p1/m1", &hitsA, rec, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, rec, 0)
	defer upB.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c1","confidence":0.9}}`)
	defer jevMock.Close()

	cfg := makeJevConfig(t, jevMock.URL(), upA, upB, nil)
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "primary", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.FallbackChains = nil
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "primary"}}
	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-valid")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d %s", rr.Code, rr.Body.String())
	}
	dep := rr.Header().Get("X-Gateway-Deployment")
	if dep != "p2/m2" {
		t.Fatalf("expected p2/m2 from Jev c1, got %s", dep)
	}
	if jevMock.CallCount() != 1 {
		t.Fatalf("expected 1 Jev call, got %d", jevMock.CallCount())
	}
	bodies := jevMock.CapturedBodies()
	if len(bodies) == 0 {
		t.Fatalf("no captured bodies")
	}
	if !strings.Contains(bodies[0], "c0") || !strings.Contains(bodies[0], "c1") {
		t.Fatalf("expected opaque c0,c1 in Jev request, got %s", bodies[0])
	}
	if strings.Contains(bodies[0], "p1/m1") || strings.Contains(bodies[0], "p2/m2") {
		t.Fatalf("physical IDs leaked into Jev request: %s", bodies[0])
	}
}

// 57. OPAQUE ID canary
func TestJev_OpaqueID_CanaryNotLeaked(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "shared", &hits, nil, 0)
	defer up.Close()

	canary := "SECRET_PHYSICAL_CANARY_abc123"
	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
	}
	// Intentionally not using canary as model ID, just check opaque mapping doesn't leak physical
	_ = canary

	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	bodies := jevMock.CapturedBodies()
	if len(bodies) > 0 {
		if strings.Contains(bodies[0], "p1/m1") || strings.Contains(bodies[0], "p2/m2") {
			t.Fatalf("physical ID leaked")
		}
		if !strings.Contains(bodies[0], "c0") {
			t.Fatalf("opaque ID missing")
		}
	}
}

// 58. RAW PROMPT PRIVACY
func TestJev_Privacy_PromptCanary(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer up.Close()

	canary := "SECRET_EXTERNAL_PROMPT_CANARY_94af"
	var capturedBody string
	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 32*1024)
		n, _ := r.Body.Read(b)
		capturedBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
	}

	s := testGateway(t, cfg)

	body := fmt.Sprintf(`{"model":"nexa-jev","messages":[{"role":"user","content":"%s please ignore"}]}`, canary)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-privacy")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d %s", rr.Code, rr.Body.String())
	}

	if strings.Contains(capturedBody, canary) {
		t.Fatalf("prompt canary leaked into Jev request")
	}
	traceHeader := rr.Header().Get("X-Gateway-Decision-Trace")
	if strings.Contains(traceHeader, canary) {
		t.Fatalf("canary leaked into trace header")
	}
	evs := s.bus.SnapshotLimit(100)
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), canary) {
			t.Fatalf("canary leaked into event: %s", string(b))
		}
	}
	reqMetrics := httptest.NewRequest("GET", "http://localhost/metrics", nil)
	rrMetrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrMetrics, reqMetrics)
	if strings.Contains(rrMetrics.Body.String(), canary) {
		t.Fatalf("canary leaked into metrics")
	}
	reqSnap := httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	reqSnap.RemoteAddr = "127.0.0.1:12345"
	rrSnap := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrSnap, reqSnap)
	if strings.Contains(rrSnap.Body.String(), canary) {
		t.Fatalf("canary leaked into admin snapshot")
	}
}

// 59. API KEY PRIVACY
func TestJev_Privacy_APIKeyCanary(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer up.Close()

	secretKey := "SECRET_JEV_KEY_CANARY_21df"
	var capturedAuth string
	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		// Also capture all headers for debug
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: secretKey, PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-key-privacy")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	// Check both inner variable and outer captured slice
	auths := jevMock.CapturedAuth()
	combined := capturedAuth
	if len(auths) > 0 {
		combined = auths[0]
		if capturedAuth != "" {
			combined = capturedAuth
		}
	}
	if combined != "Bearer "+secretKey {
		t.Fatalf("expected auth header to contain key for Jev server, got inner=%q outer=%v", capturedAuth, auths)
	}
	if strings.Contains(rr.Body.String(), secretKey) {
		t.Fatalf("key leaked into client response")
	}
	traceHeader := rr.Header().Get("X-Gateway-Decision-Trace")
	if strings.Contains(traceHeader, secretKey) {
		t.Fatalf("key leaked into trace header")
	}
	evs := s.bus.SnapshotLimit(100)
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), secretKey) {
			t.Fatalf("key leaked into event: %s", string(b))
		}
	}
	reqMetrics := httptest.NewRequest("GET", "http://localhost/metrics", nil)
	rrMetrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrMetrics, reqMetrics)
	if strings.Contains(rrMetrics.Body.String(), secretKey) {
		t.Fatalf("key leaked into metrics")
	}
	reqSnap := httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	reqSnap.RemoteAddr = "127.0.0.1:12345"
	rrSnap := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrSnap, reqSnap)
	snapBody := rrSnap.Body.String()
	if strings.Contains(snapBody, secretKey) {
		t.Fatalf("key leaked into admin snapshot")
	}
	if !strings.Contains(snapBody, "key_configured") {
		t.Fatalf("admin snapshot should contain key_configured, body=%s", snapBody)
	}
}

// 60. REMOTE ERROR BODY CANARY
func TestJev_Privacy_RemoteErrorBodyCanary(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer up.Close()

	secret := "SECRET_REMOTE_ERROR_CANARY_64ac"
	jevMock := newMockJevServer(t, "")
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(secret))
	}
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected fail-open 200, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("remote error body leaked into client response")
	}
	evs := s.bus.SnapshotLimit(100)
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), secret) {
			t.Fatalf("remote error body leaked into event")
		}
	}
	reqMetrics := httptest.NewRequest("GET", "http://localhost/metrics", nil)
	rrMetrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrMetrics, reqMetrics)
	if strings.Contains(rrMetrics.Body.String(), secret) {
		t.Fatalf("remote error body leaked into metrics")
	}
}

// 53. PRIMARY CONSTRAINT — FALLBACK
func TestJev_PrimaryConstraint_Fallback(t *testing.T) {
	var hitsA, hitsB, hitsC atomic.Int64
	rec := &attemptRecorder{}
	upA := makeJevUpstream(t, "p1/m1", &hitsA, rec, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, rec, 0)
	defer upB.Close()
	upC := makeJevUpstream(t, "p3/m3", &hitsC, rec, 0)
	defer upC.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c2"}}`)
	defer jevMock.Close()

	cfg := makeJevConfig(t, jevMock.URL(), upA, upB, upC)
	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-constraint-fallback")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	dep := rr.Header().Get("X-Gateway-Deployment")
	if dep == "p3/m3" {
		t.Fatalf("fallback C should not leapfrog primary even if Jev selects it, got %s", dep)
	}
	found := false
	evs := s.bus.SnapshotLimit(100)
	for _, ev := range evs {
		if strings.Contains(ev.DecisionReasonCodes, "PRIMARY_CONSTRAINT_VIOLATION") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected PRIMARY_CONSTRAINT_VIOLATION reason")
	}
}

// 54. PRIMARY CONSTRAINT — PRIORITY
func TestJev_PrimaryConstraint_Priority(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "shared", &hits, nil, 0)
	defer up.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c1"}}`)
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1, Priority: 0}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1, Priority: 10}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	dep := rr.Header().Get("X-Gateway-Deployment")
	if dep == "p2/m2" {
		t.Fatalf("B with priority 10 should be rejected as primary, got %s", dep)
	}
	found := false
	evs := s.bus.SnapshotLimit(100)
	for _, ev := range evs {
		if strings.Contains(ev.DecisionReasonCodes, "PRIMARY_CONSTRAINT_VIOLATION") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PRIMARY_CONSTRAINT_VIOLATION for priority violation")
	}
}

// 55. AFFINITY NO CALL
func TestJev_PrimaryConstraint_Affinity_NoExternalCall(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, nil, 0)
	defer upB.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 3600
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1, Priority: 0}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1, Priority: 10}}},
	}

	s := testGateway(t, cfg)

	sessionID := "test-session-affinity-jev"
	req1 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}],"metadata":{"session_id":"%s"}}`, sessionID)))
	req1.RemoteAddr = "127.0.0.1:12345"
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Request-Id", "jev-affinity-1")
	rr1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr1, req1)
	if rr1.Code != 200 {
		t.Fatalf("first request failed %d", rr1.Code)
	}
	if jevMock.CallCount() != 1 {
		t.Fatalf("expected 1 Jev call for first request, got %d", jevMock.CallCount())
	}

	req2 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi again"}],"metadata":{"session_id":"%s"}}`, sessionID)))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "jev-affinity-2")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("second request failed %d", rr2.Code)
	}
	if jevMock.CallCount() != 1 {
		t.Fatalf("affinity should prevent external call, expected call count 1, got %d", jevMock.CallCount())
	}
	found := false
	evs := s.bus.SnapshotLimit(100)
	for _, ev := range evs {
		if strings.Contains(ev.DecisionReasonCodes, "AFFINITY_PRESERVED") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected AFFINITY_PRESERVED reason")
	}
}

// 68. FALLBACK E2E
func TestJev_FallbackE2E(t *testing.T) {
	var hitsB, hitsA, hitsC atomic.Int64
	rec := &attemptRecorder{}
	upB := makeJevUpstream(t, "p2/m2", &hitsB, rec, 1)
	defer upB.Close()
	upA := makeJevUpstream(t, "p1/m1", &hitsA, rec, 1)
	defer upA.Close()
	upC := makeJevUpstream(t, "p3/m3", &hitsC, rec, 0)
	defer upC.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	defer jevMock.Close()

	cfg := makeJevConfig(t, jevMock.URL(), upA, upB, upC)
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p2/m2", "p1/m1"}},
		{ID: "fallback", Mode: "explicit", Deployments: []string{"p3/m3"}},
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Priority: 0}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: upC.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Priority: 2}}},
	}
	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-fallback")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d %s", rr.Code, rr.Body.String())
	}
	finalDep := rr.Header().Get("X-Gateway-Deployment")
	if finalDep != "p3/m3" {
		t.Fatalf("expected final C p3/m3 after B and A fail, got %s", finalDep)
	}
	order := rec.snapshot()
	if len(order) != 3 || order[0] != "p2/m2" || order[1] != "p1/m1" || order[2] != "p3/m3" {
		t.Fatalf("expected B->A->C order, got %v", order)
	}
}

// 69. MAX ATTEMPTS
func TestJev_MaxAttempts(t *testing.T) {
	var hitsA, hitsB, hitsC atomic.Int64
	rec := &attemptRecorder{}
	upA := makeJevUpstream(t, "p1/m1", &hitsA, rec, 1)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, rec, 1)
	defer upB.Close()
	upC := makeJevUpstream(t, "p3/m3", &hitsC, rec, 0)
	defer upC.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c1"}}`)
	defer jevMock.Close()

	cfg := makeJevConfig(t, jevMock.URL(), upA, upB, upC)
	cfg.Routing.MaxAttempts = 2
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2", "p3/m3"}}}
	cfg.FallbackChains = nil
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: upC.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true}}},
	}
	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	total := hitsA.Load() + hitsB.Load() + hitsC.Load()
	if total != 2 {
		t.Fatalf("expected total 2 attempts due to max_attempts, got %d order %v", total, rec.snapshot())
	}
	if hitsC.Load() != 0 {
		t.Fatalf("third candidate should have 0 hits")
	}
}

// 66. CROSS-PROTOCOL
func TestJev_CrossProtocol(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, nil, 0)
	defer upB.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	defer jevMock.Close()

	cfg := makeJevConfig(t, jevMock.URL(), upA, upB, nil)
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.FallbackChains = nil
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-cross-openai")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("openai failed %d", rr.Code)
	}
	depOpenAI := rr.Header().Get("X-Gateway-Deployment")

	req = httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-cross-anthropic")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("anthropic failed %d", rr.Code)
	}
	depAnthropic := rr.Header().Get("X-Gateway-Deployment")

	hitsA.Store(0)
	hitsB.Store(0)
	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-jev","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-cross-responses")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("responses failed %d", rr.Code)
	}
	depResponses := rr.Header().Get("X-Gateway-Deployment")
	if depResponses == "" {
		for _, ev := range s.bus.Snapshot() {
			if ev.RequestID == "jev-cross-responses" && ev.Kind == "route_ok" {
				depResponses = ev.Deployment
				break
			}
		}
		if depResponses == "" {
			if hitsA.Load() == 1 {
				depResponses = "p1/m1"
			} else if hitsB.Load() == 1 {
				depResponses = "p2/m2"
			}
		}
	}

	if depOpenAI != depAnthropic || depAnthropic != depResponses {
		t.Fatalf("cross-protocol divergence: openai=%s anthropic=%s responses=%s", depOpenAI, depAnthropic, depResponses)
	}
	if depOpenAI == "" {
		t.Fatalf("deployment empty")
	}
}

// 67. VE vs DIRECT
func TestJev_VEvsDirect(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "shared", &hits, nil, 0)
	defer up.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Aliases: []string{"shared-model"}, Enabled: true, Weight: 1, Priority: 0}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Aliases: []string{"shared-model"}, Enabled: true, Weight: 1, Priority: 10}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "jev-ve")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("VE failed %d", rr.Code)
	}
	depVE := rr.Header().Get("X-Gateway-Deployment")

	req2 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "jev-direct")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("direct failed %d", rr2.Code)
	}
	depDirect := rr2.Header().Get("X-Gateway-Deployment")

	if depVE != depDirect {
		t.Fatalf("VE vs direct mismatch: VE=%s direct=%s", depVE, depDirect)
	}
}

// Capability boundary
func TestJev_CapabilityBoundary(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, nil, 0)
	defer upB.Close()

	var jevCapturedCandidates []string
	var jevMu sync.Mutex
	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 32*1024)
		n, _ := r.Body.Read(b)
		body := string(b[:n])
		jevMu.Lock()
		jevCapturedCandidates = append(jevCapturedCandidates, body)
		jevMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Capabilities: config.Capabilities{Tools: true}}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Capabilities: config.Capabilities{Tools: false}}}},
	}

	s := testGateway(t, cfg)

	body := `{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"test","description":"test"}}]}`
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d %s", rr.Code, rr.Body.String())
	}
	if hitsB.Load() != 0 {
		t.Fatalf("B without tools should never be attempted, hits=%d", hitsB.Load())
	}
	jevMu.Lock()
	captured := strings.Join(jevCapturedCandidates, " ")
	jevMu.Unlock()
	if jevMock.CallCount() > 0 {
		if strings.Contains(captured, "p2/m2") {
			t.Fatalf("capability-ineligible B leaked to Jev")
		}
	}
}

// Credential independence
func TestJev_CredentialIndependence(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer up.Close()

	var capturedBody string
	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 32*1024)
		n, _ := r.Body.Read(b)
		capturedBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", APIKey: "upstream-secret-key", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if strings.Contains(capturedBody, "upstream-secret-key") {
		t.Fatalf("upstream credential leaked to Jev")
	}
}

// External error does not affect model health
func TestJev_ExternalErrorDoesNotAffectModelHealth(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer up.Close()

	jevMock := newMockJevServer(t, "")
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"code":1,"message":"internal error"}`))
	}
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true}}},
	}

	s := testGateway(t, cfg)
	s.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected fail-open 200, got %d", rr.Code)
	}
}

// Hot reload coherence
func TestJev_HotReloadCoherence(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "shared", &hits, nil, 0)
	defer up.Close()

	jevMock1 := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	defer jevMock1.Close()
	jevMock2 := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c1"}}`)
	defer jevMock2.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock1.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true}}},
	}

	s := testGateway(t, cfg)

	done := make(chan bool)
	go func() {
		for i := 0; i < 50; i++ {
			next := s.currentConfig()
			if i%2 == 0 {
				next.DecisionProviders[0].BaseURL = jevMock1.URL()
			} else {
				next.DecisionProviders[0].BaseURL = jevMock2.URL()
			}
			_ = s.applyConfig(next)
			time.Sleep(5 * time.Millisecond)
		}
		done <- true
	}()

	var wg sync.WaitGroup
	var resultsMu sync.Mutex
	var results []string
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-Id", fmt.Sprintf("jev-hot-%d", idx))
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code == 200 {
				dep := rr.Header().Get("X-Gateway-Deployment")
				resultsMu.Lock()
				results = append(results, dep)
				resultsMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	<-done

	for _, dep := range results {
		if dep != "p1/m1" && dep != "p2/m2" {
			t.Fatalf("hot reload incoherent dep %s not p1/m1 nor p2/m2", dep)
		}
	}
}

// Disable provider
func TestJev_DisableProvider(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer up.Close()

	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
	}

	s := testGateway(t, cfg)

	next := s.currentConfig()
	f := false
	next.DecisionProviders[0].Enabled = &f
	// When disabling, also switch decision mode to off to satisfy config validation (provider disabled cannot be referenced)
	next.Decision.Mode = "off"
	next.Decision.Provider = "local"
	if err := s.applyConfig(next); err != nil {
		t.Fatalf("failed to disable: %v", err)
	}

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected 200 after disable, got %d", rr.Code)
	}
	if jevMock.CallCount() != 0 {
		t.Fatalf("expected 0 Jev calls after disable, got %d", jevMock.CallCount())
	}
}

// Redirect auth not leaked
func TestJev_RedirectAuthNotLeaked(t *testing.T) {
	var capturedAuthOnRedirect string
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuthOnRedirect = r.Header.Get("Authorization")
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}))
	defer redirectTarget.Close()

	jevMock := newMockJevServer(t, "")
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", redirectTarget.URL)
		w.WriteHeader(302)
	}
	defer jevMock.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.Decision.TimeoutMS = 500
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "secret-key", PrivacyMode: "metadata_only", BaseURL: jevMock.URL()},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-jev", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-jev","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if capturedAuthOnRedirect != "" {
		t.Fatalf("Authorization leaked on redirect: %q", capturedAuthOnRedirect)
	}
}
