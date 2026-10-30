package httpapi

// Wave-2 coverage for the Gemini ingress (issue #663, continuing PR #730).
// Every test below targets blocks the fresh merged-tree profile showed as
// uncovered in internal/httpapi/gemini.go. Fixture-only: loopback fake
// upstreams, zero live calls, zero real credentials. No sleeps: timing
// tests use a request-timeout-bounded blocked upstream or a pre-cancelled
// context, never wall-clock waits.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// wave2GeminiServer builds a gateway with the given providers, like
// geminiTestGateway but multi-provider and with a mutate hook. MaxAttempts
// is left at the config default (4) so two-candidate tests exercise the
// max-attempts cap (max > len(candidates)).
func wave2GeminiServer(t *testing.T, providers []config.ProviderConfig, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Providers = providers
	if mutate != nil {
		mutate(&cfg)
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func wave2GeminiProvider(id, typ, url string) config.ProviderConfig {
	return config.ProviderConfig{
		ID: id, Name: id, Type: typ, BaseURL: url, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
	}
}

func wave2GeminiDeadProvider(t *testing.T, id string) (config.ProviderConfig, string) {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	return wave2GeminiProvider(id, "openai_compatible", deadURL), deadURL
}

func wave2GeminiChatOKServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wave3ChatOK)
	}))
}

func wave2GeminiBody() string {
	return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
}

func wave2GeminiError(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("error is not JSON: %v (%s)", err, rr.Body.String())
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("missing Gemini error envelope: %s", rr.Body.String())
	}
	return errObj
}

func wave2GeminiBusHas(t *testing.T, s *Server, kind, deployment string) bool {
	t.Helper()
	for _, e := range s.bus.Snapshot() {
		if e.Kind == kind && (deployment == "" || e.Deployment == deployment) {
			return true
		}
	}
	return false
}

func TestWave2GeminiModelsMethodNotAllowed(t *testing.T) {
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", "http://127.0.0.1:9")}, nil)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405 body=%s", rr.Code, rr.Body.String())
	}
	if got := wave2GeminiError(t, rr)["status"]; got != "INTERNAL" {
		t.Fatalf("status name=%v want INTERNAL (405 has no dedicated Gemini name)", got)
	}
}

func TestWave2GeminiModelsAuthRequired(t *testing.T) {
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", "http://127.0.0.1:9")},
		func(cfg *config.Config) {
			cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"test-client-key"}}
		})
	req := httptest.NewRequest(http.MethodGet, "http://gateway/v1beta/models", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", rr.Code, rr.Body.String())
	}
	if got := wave2GeminiError(t, rr)["status"]; got != "UNAUTHENTICATED" {
		t.Fatalf("status name=%v want UNAUTHENTICATED", got)
	}
}

func TestWave2GeminiRequestTooLarge(t *testing.T) {
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", "http://127.0.0.1:9")}, nil)
	body := strings.Repeat("x", (16<<20)+1)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413 body=%s", rr.Code, rr.Body.String()[:min(200, len(rr.Body.String()))])
	}
	if !strings.Contains(rr.Body.String(), "exceeds") {
		t.Fatalf("413 body missing limit message: %s", rr.Body.String()[:min(200, len(rr.Body.String()))])
	}
}

func TestWave2GeminiToolChoiceRequired(t *testing.T) {
	s, _ := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	tools := `"tools":[{"functionDeclarations":[{"name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}]`
	toolConfig := `"toolConfig":{"functionCallingConfig":{"mode":"ANY"}}`
	body := `{"contents":[{"role":"user","parts":[{"text":"run lookup"}]}],` + tools + `,` + toolConfig + `}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"functionCall"`) || !strings.Contains(rr.Body.String(), `"name":"lookup"`) {
		t.Fatalf("toolChoice=ANY request missing functionCall: %s", rr.Body.String())
	}
}

func TestWave2GeminiTooComplex(t *testing.T) {
	s, up := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	var b strings.Builder
	b.WriteString(`{"contents":[{"role":"user","parts":[`)
	for i := 0; i < 100001; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"text":"x"}`)
	}
	b.WriteString(`]}]}`)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", b.String(), "", "")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "too complex") {
		t.Fatalf("status=%d want 400 too complex body=%s", rr.Code, rr.Body.String()[:min(300, len(rr.Body.String()))])
	}
	up.mu.Lock()
	calls := up.calls
	up.mu.Unlock()
	if calls != 0 {
		t.Fatalf("too-complex request reached upstream %d times", calls)
	}
}

func TestWave2GeminiVisionNoEligible(t *testing.T) {
	var calls atomic.Int32
	up := wave2GeminiChatOKServer(t, &calls)
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}]}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("status=%d want 503 no-eligible body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("vision request with no vision-capable deployment reached upstream %d times", calls.Load())
	}
}

// wave2GeminiVirtualServer builds a gateway with a virtual endpoint
// "nexa-private" fronting an explicit pool of two providers, mirroring the
// privacy fail-closed fixture. trainsOnData controls the providers'
// DataHandling; routePrivacy sets the route profile's privacy class;
// enabled controls the virtual endpoint's Enabled flag; protocols
// restricts the endpoint's allowed ingress protocols (nil = unrestricted).
func wave2GeminiVirtualServer(t *testing.T, trainsOnData, routePrivacy string, enabled *bool, protocols []string) (*Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var hitsA, hitsB atomic.Int64
	mkUp := func(hits *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, wave3ChatOK)
		}))
	}
	upA := mkUp(&hitsA)
	upB := mkUp(&hitsB)
	t.Cleanup(upA.Close)
	t.Cleanup(upB.Close)
	mk := func(id, model, url string) config.ProviderConfig {
		return config.ProviderConfig{
			ID: id, Name: id, Type: "openai_compatible", BaseURL: url, AuthMode: "none", Enabled: true,
			DataHandling: config.DataHandlingConfig{TrainsOnData: trainsOnData},
			Models: []config.ModelConfig{{ID: "m", Model: model, Enabled: true, Weight: 1,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
		}
	}
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Routing.Strategy = "priority"
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "pool-all", Mode: "explicit", Deployments: []string{"pa/m", "pb/m"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "rp1", CandidatePool: "pool-all", Privacy: routePrivacy},
	}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-private", RouteProfile: "rp1", Enabled: enabled, Protocols: protocols},
	}
	cfg.Providers = []config.ProviderConfig{mk("pa", "phys-a", upA.URL), mk("pb", "phys-b", upB.URL)}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s, &hitsA, &hitsB
}

func TestWave2GeminiDisabledVirtualEndpoint(t *testing.T) {
	disabled := false
	s, hitsA, hitsB := wave2GeminiVirtualServer(t, "no", "", &disabled, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/nexa-private:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatalf("status=%d want 404 disabled body=%s", rr.Code, rr.Body.String())
	}
	if hitsA.Load()+hitsB.Load() != 0 {
		t.Fatalf("disabled-endpoint request reached upstream")
	}
}

func TestWave2GeminiProtocolRestrictedVirtualEndpoint(t *testing.T) {
	enabled := true
	s, hitsA, hitsB := wave2GeminiVirtualServer(t, "no", "", &enabled, []string{"openai"})
	rr := geminiPost(t, s, "http://gateway/v1beta/models/nexa-private:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "not allowed") {
		t.Fatalf("status=%d want 400 protocol-not-allowed body=%s", rr.Code, rr.Body.String())
	}
	if hitsA.Load()+hitsB.Load() != 0 {
		t.Fatalf("protocol-restricted request reached upstream")
	}
}

func TestWave2GeminiPrivacyUnavailable(t *testing.T) {
	enabled := true
	s, hitsA, hitsB := wave2GeminiVirtualServer(t, "yes", "no_training", &enabled, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/nexa-private:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "no deployment satisfies the required privacy class") {
		t.Fatalf("status=%d want 503 privacy body=%s", rr.Code, rr.Body.String())
	}
	if hitsA.Load()+hitsB.Load() != 0 {
		t.Fatalf("privacy-unavailable request reached upstream")
	}
}

func TestWave2GeminiVirtualEndpointSuccess(t *testing.T) {
	enabled := true
	s, _, _ := wave2GeminiVirtualServer(t, "no", "", &enabled, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/nexa-private:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("virtual-endpoint response missing translated text: %s", rr.Body.String())
	}
	if got := rr.Header().Get("X-Gateway-Virtual-Endpoint"); got != "ve1" {
		t.Fatalf("virtual-endpoint header=%q want ve1", got)
	}
	if got := rr.Header().Get("X-Gateway-Public-Model"); got != "nexa-private" {
		t.Fatalf("public-model header=%q want nexa-private", got)
	}
	if got := rr.Header().Get("X-Gateway-Route-Profile"); got != "rp1" {
		t.Fatalf("route-profile header=%q want rp1", got)
	}
	found := false
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "route_ok" && e.VirtualEndpoint == "ve1" && e.PublicModel == "nexa-private" && e.RouteProfile == "rp1" {
			found = true
		}
	}
	if !found {
		t.Fatal("route_ok event missing virtual-endpoint fields")
	}
}

func TestWave2GeminiTransportFailover(t *testing.T) {
	deadCfg, deadURL := wave2GeminiDeadProvider(t, "a")
	var goodCalls atomic.Int32
	good := wave3JSONServerNotURL(t, 200, wave3ChatOK, &goodCalls, deadURL)
	defer good.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{deadCfg, wave2GeminiProvider("b", "openai_compatible", good.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("status=%d body=%s want 200 ok", rr.Code, rr.Body.String())
	}
	if goodCalls.Load() != 1 {
		t.Fatalf("good candidate calls=%d want 1", goodCalls.Load())
	}
	if !wave2GeminiBusHas(t, s, "failover", "a/m") {
		t.Fatal("failover event missing for a/m")
	}
	if !wave2GeminiBusHas(t, s, "route_ok", "b/m") {
		t.Fatal("route_ok event missing for b/m")
	}
}

func TestWave2GeminiTransportTerminal502(t *testing.T) {
	deadCfg, _ := wave2GeminiDeadProvider(t, "a")
	s := wave2GeminiServer(t, []config.ProviderConfig{deadCfg}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "all candidate deployments failed") {
		t.Fatalf("status=%d want 502 all-failed body=%s", rr.Code, rr.Body.String())
	}
	if wave2GeminiBusHas(t, s, "failover", "") {
		t.Fatal("single-candidate terminal failure must not emit a failover event")
	}
}

func TestWave2GeminiReadyStrategyTransportQuarantine(t *testing.T) {
	deadCfg, _ := wave2GeminiDeadProvider(t, "a")
	s := wave2GeminiServer(t, []config.ProviderConfig{deadCfg},
		func(cfg *config.Config) { cfg.Routing.Strategy = "ready_queue" })
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "all candidate deployments failed") {
		t.Fatalf("status=%d want 502 all-failed body=%s", rr.Code, rr.Body.String())
	}
	if st := s.hm.Get("a/m"); st.Status != health.Degraded {
		t.Fatalf("health=%s want degraded (ready-strategy transport quarantine)", st.Status)
	}
}

func TestWave2GeminiClientCancelledTransport(t *testing.T) {
	var calls atomic.Int32
	up := wave2GeminiChatOKServer(t, &calls)
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://gateway/v1beta/models/m:generateContent", strings.NewReader(wave2GeminiBody()))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Body.Len() != 0 {
		t.Fatalf("cancelled request produced a response body: %s", rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("cancelled request reached upstream %d times", calls.Load())
	}
	if wave2GeminiBusHas(t, s, "failover", "") {
		t.Fatal("cancelled request must not emit a failover event")
	}
}

func TestWave2GeminiRepairThenTransportError(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			gc, _ := in["generationConfig"].(map[string]any)
			if _, hasTemp := gc["temperature"]; !hasTemp {
				t.Errorf("first Gemini payload missing generationConfig.temperature: %v", in)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported"}}`)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("upstream response writer does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "gemini", up.URL)}, nil)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.7}}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "all candidate deployments failed") {
		t.Fatalf("status=%d want 502 all-failed body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d want 2 (original + repaired retry)", calls.Load())
	}
	if !wave2GeminiBusHas(t, s, "compat_repair", "g/m") {
		t.Fatal("compat_repair event missing for the repaired-then-failed attempt")
	}
}

func TestWave2GeminiUpstream429(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", rr.Code, rr.Body.String())
	}
	if got := wave2GeminiError(t, rr)["status"]; got != "RESOURCE_EXHAUSTED" {
		t.Fatalf("status name=%v want RESOURCE_EXHAUSTED", got)
	}
	if !wave2GeminiBusHas(t, s, "route_fail", "g/m") {
		t.Fatal("route_fail event missing for g/m")
	}
}

func TestWave2GeminiUpstream403(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"forbidden"}}`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}
	if got := wave2GeminiError(t, rr)["status"]; got != "PERMISSION_DENIED" {
		t.Fatalf("status name=%v want PERMISSION_DENIED", got)
	}
}

func TestWave2GeminiUpstream500Priority(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 body=%s", rr.Code, rr.Body.String())
	}
	if !wave2GeminiBusHas(t, s, "route_fail", "g/m") {
		t.Fatal("route_fail event missing for g/m")
	}
}

func TestWave2GeminiUpstream500ReadyQuarantine(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)},
		func(cfg *config.Config) { cfg.Routing.Strategy = "ready_queue" })
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 body=%s", rr.Code, rr.Body.String())
	}
	if st := s.hm.Get("g/m"); st.Status != health.Degraded {
		t.Fatalf("health=%s want degraded (ready-strategy 500 quarantine)", st.Status)
	}
}

func TestWave2GeminiModelRetired(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"model_retired","message":"this model is retired"}}`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("status=%d want 503 retired body=%s", rr.Code, rr.Body.String())
	}
	if !wave2GeminiBusHas(t, s, "model_retired", "g/m") {
		t.Fatal("model_retired event missing for g/m")
	}
	if st := s.hm.Get("g/m"); st.Status != health.Retired {
		t.Fatalf("health=%s want retired", st.Status)
	}
}

func TestWave2GeminiModelUnavailable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"model_unavailable","message":"model down"}}`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("status=%d want 503 unavailable body=%s", rr.Code, rr.Body.String())
	}
	if !wave2GeminiBusHas(t, s, "model_unavailable", "g/m") {
		t.Fatal("model_unavailable event missing for g/m")
	}
}

func TestWave2GeminiHTTPFailover(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer upA.Close()
	var goodCalls atomic.Int32
	upB := wave2GeminiChatOKServer(t, &goodCalls)
	defer upB.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{
		wave2GeminiProvider("a", "openai_compatible", upA.URL),
		wave2GeminiProvider("b", "openai_compatible", upB.URL),
	}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("status=%d body=%s want 200 ok", rr.Code, rr.Body.String())
	}
	if goodCalls.Load() != 1 {
		t.Fatalf("good candidate calls=%d want 1", goodCalls.Load())
	}
	if !wave2GeminiBusHas(t, s, "failover", "a/m") {
		t.Fatal("failover event missing for a/m")
	}
	if !wave2GeminiBusHas(t, s, "route_ok", "b/m") {
		t.Fatal("route_ok event missing for b/m")
	}
}

func TestWave2GeminiNativeRawError(t *testing.T) {
	rawBody := `{"error":{"code":400,"message":"bad gemini request","status":"INVALID_ARGUMENT"}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, rawBody)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "gemini", up.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	if strings.TrimSpace(rr.Body.String()) != rawBody {
		t.Fatalf("native Gemini error not passed through raw: %s", rr.Body.String())
	}
}

func TestWave2GeminiMalformedResponsePriority(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `this is not a chat completion`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "upstream returned an invalid response") {
		t.Fatalf("status=%d want 502 invalid-response body=%s", rr.Code, rr.Body.String())
	}
}

func TestWave2GeminiMalformedResponseReadyQuarantine(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `this is not a chat completion`)
	}))
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)},
		func(cfg *config.Config) { cfg.Routing.Strategy = "ready_queue" })
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "upstream returned an invalid response") {
		t.Fatalf("status=%d want 502 invalid-response body=%s", rr.Code, rr.Body.String())
	}
	if st := s.hm.Get("g/m"); st.Status != health.Degraded {
		t.Fatalf("health=%s want degraded (ready-strategy malformed-response quarantine)", st.Status)
	}
}

func TestWave2GeminiUsageErrorFailover(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `this is not a chat completion`)
	}))
	defer upA.Close()
	var goodCalls atomic.Int32
	upB := wave2GeminiChatOKServer(t, &goodCalls)
	defer upB.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{
		wave2GeminiProvider("a", "openai_compatible", upA.URL),
		wave2GeminiProvider("b", "openai_compatible", upB.URL),
	}, nil)
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("status=%d body=%s want 200 ok after malformed-first failover", rr.Code, rr.Body.String())
	}
	if goodCalls.Load() != 1 {
		t.Fatalf("good candidate calls=%d want 1", goodCalls.Load())
	}
	if !wave2GeminiBusHas(t, s, "route_ok", "b/m") {
		t.Fatal("route_ok event missing for b/m")
	}
}

func TestWave2GeminiDeadline504(t *testing.T) {
	// The blocked handler releases on server-context cancellation OR on
	// test cleanup, so httptest Server.Close can never hang on it even
	// when the gateway answers without cancelling the upstream connection.
	released := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer func() { close(released); up.Close() }()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)},
		func(cfg *config.Config) { cfg.Routing.RequestTimeoutMS = 1000 })
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusGatewayTimeout || !strings.Contains(rr.Body.String(), "gateway request timeout") {
		t.Fatalf("status=%d want 504 gateway-timeout body=%s", rr.Code, rr.Body.String())
	}
	if got := wave2GeminiError(t, rr)["status"]; got != "DEADLINE_EXCEEDED" {
		t.Fatalf("status name=%v want DEADLINE_EXCEEDED", got)
	}
}

func TestWave2GeminiNoUsableDeployment(t *testing.T) {
	var calls atomic.Int32
	up := wave2GeminiChatOKServer(t, &calls)
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	// Defensive branch: a zero attempt budget means the attempt loop never
	// runs. config validation forbids this in operator configs, so it is
	// set post-construction to pin the fail-closed outcome.
	s.cfg.Routing.MaxAttempts = 0
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "no usable deployment") {
		t.Fatalf("status=%d want 502 no-usable-deployment body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("zero-attempt request reached upstream %d times", calls.Load())
	}
}

func TestWave2GeminiBuildCandidateIneligible(t *testing.T) {
	var calls atomic.Int32
	up := wave2GeminiChatOKServer(t, &calls)
	defer up.Close()
	s := wave2GeminiServer(t, []config.ProviderConfig{wave2GeminiProvider("g", "openai_compatible", up.URL)}, nil)
	// Seed a learned capability contract AFTER the config sync: the router
	// still admits g/m (config says tools-capable), but the attempt-time
	// capability check refuses it, so the candidate cannot be built.
	// LearnUnsupported silently no-ops on a key mismatch (staleness
	// guard), so the seeded contract's own key must be reused.
	key := s.capStore.Get("g/m").InvalidationKey
	s.capStore.LearnUnsupported("g/m", compat.CapTools, compat.SourceProbe, "verified", key)
	tools := `"tools":[{"functionDeclarations":[{"name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}]`
	body := `{"contents":[{"role":"user","parts":[{"text":"run lookup"}]}],` + tools + `}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "candidate could not serve this request") {
		t.Fatalf("status=%d want 502 candidate-could-not-serve body=%s", rr.Code, rr.Body.String())
	}
	if !wave2GeminiBusHas(t, s, "capability_skip", "g/m") {
		t.Fatal("capability_skip event missing for g/m")
	}
	if calls.Load() != 0 {
		t.Fatalf("ineligible candidate reached upstream %d times", calls.Load())
	}
}
