package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// jevCapture records every mock-Jev request for privacy and protocol
// assertions.
type jevCapture struct {
	hits   atomic.Int64
	mu     sync.Mutex
	bodies []string
	auths  []string
	uas    []string
}

func (c *jevCapture) record(r *http.Request) {
	c.hits.Add(1)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	c.mu.Lock()
	c.bodies = append(c.bodies, string(body))
	c.auths = append(c.auths, r.Header.Get("Authorization"))
	c.uas = append(c.uas, r.Header.Get("User-Agent"))
	c.mu.Unlock()
}

func (c *jevCapture) lastBody(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("mock Jev saw no requests")
	}
	return c.bodies[len(c.bodies)-1]
}

// startMockJev serves a fixed model-route response and captures requests.
func startMockJev(t *testing.T, status int, resp string) (*httptest.Server, *jevCapture) {
	t.Helper()
	cap := &jevCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func jevSelectResponse(choice string, confidence float64) string {
	return `{"code":0,"message":"ok","data":{"decision":"` + choice + `","confidence":` + jsonNumber(confidence) + `}}`
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// assistedGateway builds a test gateway and swaps its decision runtime to
// point at the mock Jev endpoint (the production code path has no endpoint
// knob; tests inject via the internal constructor only).
func assistedGateway(t *testing.T, cfg config.Config, jevURL string) *Server {
	t.Helper()
	s := testGateway(t, cfg)
	rt, err := buildDecisionRuntimeWithOptions(s.currentConfig(), decisionBuildOptions{jevEndpoint: jevURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.decision.Store(rt)
	return s
}

// twoDeploymentConfig returns a deterministic ready_queue config with two
// healthy deployments sharing one client alias.
func twoDeploymentConfig(upURL string) config.Config {
	cfg := config.Default()
	cfg.Admin.APIKey = "adm-decision-test"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	mkModel := func(id, upstream string) config.ModelConfig {
		return config.ModelConfig{ID: id, Model: upstream, Aliases: []string{"client-model"},
			Enabled: true, Weight: 1, Priority: 0,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}}
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: upURL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{mkModel("a", "up-a")}},
		{ID: "p2", Name: "P2", Type: "openai_compatible", BaseURL: upURL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{mkModel("b", "up-b")}},
	}
	cfg.Decision = config.DecisionConfig{Mode: "assisted", Provider: "jev-main", TimeoutMS: 2000}
	cfg.DecisionProviders = []config.DecisionProviderConfig{{
		ID: "jev-main", Type: "jev", Enabled: true,
		APIKey: "test-jev-key", PrivacyMode: "metadata_only",
	}}
	return cfg
}

// openAIUpstreamMock serves chat completions and records requested models.
type upstreamCapture struct {
	mu     sync.Mutex
	models []string
	fail   map[string]int // upstream model -> status to serve
}

func (u *upstreamCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		model, _ := in["model"].(string)
		u.mu.Lock()
		u.models = append(u.models, model)
		code := u.fail[model]
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if code >= 400 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream fail"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
}

func (u *upstreamCapture) attemptModels() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

func chatRequest(body string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	return rr, req
}

func TestBuildDecisionRuntimeDefaults(t *testing.T) {
	rt, err := buildDecisionRuntime(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if rt.mode != "off" || rt.selected != nil {
		t.Fatalf("default runtime must be disabled: %+v", rt)
	}
	if len(rt.status) != 0 {
		t.Fatalf("status=%v", rt.status)
	}
}

func TestBuildDecisionRuntimeUnknownProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Decision = config.DecisionConfig{Mode: "local", Provider: "ghost", TimeoutMS: 400}
	if _, err := buildDecisionRuntime(cfg); err == nil {
		t.Fatal("unknown provider must fail the build")
	}
}

func TestBuildDecisionRuntimeStatusRows(t *testing.T) {
	cfg := config.Default()
	cfg.Decision = config.DecisionConfig{Mode: "off", Provider: "local", TimeoutMS: 400}
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "j1", Type: "jev", Enabled: true, APIKey: "k", PrivacyMode: "metadata_only"},
		{ID: "j2", Type: "jev", Enabled: false, PrivacyMode: "metadata_only"},
		{ID: "j3", Type: "jev", Enabled: true, PrivacyMode: "metadata_only"},
	}
	rt, err := buildDecisionRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.status) != 3 {
		t.Fatalf("status=%v", rt.status)
	}
	if rt.status[0].Health != "healthy" || !rt.status[0].KeyConfigured {
		t.Fatalf("row0=%+v", rt.status[0])
	}
	if rt.status[1].Health != "disabled" {
		t.Fatalf("row1=%+v", rt.status[1])
	}
	if rt.status[2].Health != "unavailable" || rt.status[2].KeyConfigured {
		t.Fatalf("row2=%+v", rt.status[2])
	}
	for _, row := range rt.status {
		b, _ := json.Marshal(row)
		if strings.Contains(string(b), `"k"`) {
			t.Fatalf("status row leaks key material: %s", b)
		}
	}
}

func TestDecisionCandidatesMapping(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	cfg := twoDeploymentConfig(up.URL)
	cfg.Decision.Mode = "off"
	s := testGateway(t, cfg)
	req := router.Requirement{Model: "client-model"}
	_, cands := s.routeSnapshot(req)
	if len(cands) != 2 {
		t.Fatalf("candidates=%d", len(cands))
	}
	dc := decisionCandidatesFromScored(cands)
	if dc[0].ID != "p1/a" || dc[1].ID != "p2/b" {
		t.Fatalf("ids=%v", dc)
	}
	// Priority tiers serve as pools.
	if dc[0].PoolOrdinal != 0 || dc[0].Priority != 0 {
		t.Fatalf("pool/priority=%+v", dc[0])
	}
	if !dc[0].Tools || !dc[0].Streaming {
		t.Fatalf("caps=%+v", dc[0])
	}
	if dc[0].Observations == 0 {
		t.Fatal("healthy deployments must carry observations")
	}
}

func TestDecisionFeaturesFromRequirement(t *testing.T) {
	req := router.Requirement{Tools: true, Vision: true, EstimatedInputTokens: 100, MaxOutputTokens: 50}
	f := decisionFeaturesFromRequirement(req, true)
	if !f.Tools || !f.Vision || !f.StructuredOut {
		t.Fatalf("features=%+v", f)
	}
	if f.EstimatedContextTokens != 150 || f.EstimatedInputTokens != 100 || f.MaxOutputTokens != 50 {
		t.Fatalf("features=%+v", f)
	}
	// Same requirement across protocols yields the same task kind (strict).
	if decision.DeriveTaskKind(f) != decision.TaskVision {
		t.Fatalf("kind=%q", decision.DeriveTaskKind(f))
	}
}

func TestAssistedValidSelectionEndToEnd(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "p2/b" {
		t.Fatalf("deployment=%q, want p2/b (Jev selected c1)", got)
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("jev hits=%d, want exactly 1", cap.hits.Load())
	}
	models := upCap.attemptModels()
	if len(models) != 1 || models[0] != "up-b" {
		t.Fatalf("upstream attempts=%v", models)
	}
}

func TestAssistedFailureFailsOpen(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 500, `{"code":1,"message":"bad"}`)
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("external failure must fail open, got status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "p1/a" {
		t.Fatalf("deployment=%q, want router order p1/a", got)
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("jev hits=%d, want 1 (no retry)", cap.hits.Load())
	}
}

func TestAssistedAffinityPreventsExternalCall(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	cfg.Routing.Strategy = "ready_mesh" // pin-honoring strategy
	s := assistedGateway(t, cfg, jev.URL)

	// Request 1 with a session: Jev selects, execution pins the session.
	rr1, req1 := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	req1.Header.Set("x-session-id", "sess-affinity-1")
	s.Handler().ServeHTTP(rr1, req1)
	if rr1.Code != 200 {
		t.Fatalf("status=%d", rr1.Code)
	}
	first := rr1.Header().Get("X-Gateway-Deployment")
	if cap.hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1", cap.hits.Load())
	}
	// Request 2, same session: pin authoritative, no external call.
	rr2, req2 := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi again"}],"max_tokens":8}`)
	req2.Header.Set("x-session-id", "sess-affinity-1")
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("status=%d", rr2.Code)
	}
	if got := rr2.Header().Get("X-Gateway-Deployment"); got != first {
		t.Fatalf("deployment=%q, want pinned %q", got, first)
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("jev hits=%d after pinned request, want still 1 (no second call)", cap.hits.Load())
	}
	// The decision event must record the affinity preservation.
	found := false
	for _, e := range s.bus.Snapshot() {
		if strings.Contains(e.Message, "AFFINITY_PRESERVED") {
			found = true
		}
	}
	if !found {
		t.Fatal("no AFFINITY_PRESERVED decision event recorded")
	}
}

func TestAssistedConstraintViolationFailsOpen(t *testing.T) {
	// Fallback-pool deployment C (priority 1 => pool 1): Jev cannot be
	// consulted about it, and a forged out-of-band choice is rejected.
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	// c9 is unknown; the adapter fails before the orchestrator validator.
	jev, _ := startMockJev(t, 200, jevSelectResponse("c9", 0.99))
	cfg := twoDeploymentConfig(up.URL)
	up2 := cfg.Providers[0]
	_ = up2
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID: "p3", Name: "P3", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "c", Model: "up-c", Aliases: []string{"client-model"},
			Enabled: true, Weight: 1, Priority: 5,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}}},
	})
	s := assistedGateway(t, cfg, jev.URL)
	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "p1/a" {
		t.Fatalf("deployment=%q, want p1/a (fail open)", got)
	}
}

func TestAdminSnapshotExposesSafeDecisionStatus(t *testing.T) {
	up := httptest.NewServer((&upstreamCapture{}).handler())
	defer up.Close()
	jev, _ := startMockJev(t, 200, jevSelectResponse("c0", 0.5))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	req := httptest.NewRequest("GET", "http://gateway/admin/api/snapshot", nil)
	req.Header.Set("x-admin-key", "adm-decision-test")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	var snap map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	dec, _ := snap["decision"].(map[string]any)
	if dec["mode"] != "assisted" || dec["provider"] != "jev-main" {
		t.Fatalf("decision=%v", dec)
	}
	ext, _ := snap["external_decision_providers"].([]any)
	if len(ext) != 1 {
		t.Fatalf("external=%v", ext)
	}
	row := ext[0].(map[string]any)
	if row["id"] != "jev-main" || row["type"] != "jev" || row["enabled"] != true ||
		row["health"] != "healthy" || row["key_configured"] != true || row["privacy_mode"] != "metadata_only" {
		t.Fatalf("row=%v", row)
	}
	if strings.Contains(rr.Body.String(), "test-jev-key") {
		t.Fatal("admin snapshot leaks the Jev API key")
	}
}

func TestMetricsExposeBoundedDecisionCounters(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, _ := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	mreq := httptest.NewRequest("GET", "http://gateway/metrics", nil)
	mrr := httptest.NewRecorder()
	s.Handler().ServeHTTP(mrr, mreq)
	body := mrr.Body.String()
	if !strings.Contains(body, `nexaroute_external_decision_requests_total{type="jev",outcome="selected"} 1`) {
		t.Fatalf("missing decision counter:\n%s", body)
	}
	if !strings.Contains(body, `nexaroute_external_decision_latency_seconds_count{type="jev"} 1`) {
		t.Fatalf("missing latency count:\n%s", body)
	}
	// No high-cardinality labels.
	for _, bad := range []string{"jev-main", "p1/a", "p2/b", "up-a", "client-model"} {
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "external_decision") && strings.Contains(line, bad) {
				t.Fatalf("metric leaks %q: %s", bad, line)
			}
		}
	}
}

func TestHotReloadDisablesExternalCalls(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if cap.hits.Load() != 1 {
		t.Fatalf("hits=%d", cap.hits.Load())
	}
	// Real reload path: assisted -> off.
	if _, err := s.mutateConfig(func(c *config.Config) error {
		c.Decision.Mode = "off"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rr2, req2 := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("status=%d", rr2.Code)
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("hits=%d after disable, want still 1", cap.hits.Load())
	}
	if got := rr2.Header().Get("X-Gateway-Deployment"); got != "p1/a" {
		t.Fatalf("deployment=%q, want router order", got)
	}
}

func TestHotReloadRejectsBadDecisionConfig(t *testing.T) {
	up := httptest.NewServer((&upstreamCapture{}).handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	_, err := s.mutateConfig(func(c *config.Config) error {
		c.DecisionProviders[0].Enabled = false // selected provider disabled
		return nil
	})
	if err == nil {
		t.Fatal("disabling the selected assisted provider must be rejected")
	}
	// Old runtime still active.
	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || cap.hits.Load() != 1 {
		t.Fatalf("status=%d hits=%d (old runtime must survive)", rr.Code, cap.hits.Load())
	}
}

func TestCredentialSnapshotCoherenceAcrossReload(t *testing.T) {
	t.Setenv("NEXAROUTE_JEV_KEY_A", "key-A-value")
	t.Setenv("NEXAROUTE_JEV_KEY_B", "key-B-value")
	jev, cap := startMockJev(t, 200, jevSelectResponse("c0", 0.5))
	mkCfg := func(env string) config.Config {
		cfg := config.Default()
		cfg.Decision = config.DecisionConfig{Mode: "assisted", Provider: "jev-main", TimeoutMS: 2000}
		cfg.DecisionProviders = []config.DecisionProviderConfig{{
			ID: "jev-main", Type: "jev", Enabled: true,
			APIKeyEnv: env, PrivacyMode: "metadata_only",
		}}
		return cfg
	}
	rtOld, err := buildDecisionRuntimeWithOptions(mkCfg("NEXAROUTE_JEV_KEY_A"), decisionBuildOptions{jevEndpoint: jev.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rtNew, err := buildDecisionRuntimeWithOptions(mkCfg("NEXAROUTE_JEV_KEY_B"), decisionBuildOptions{jevEndpoint: jev.URL}, rtOld.metrics)
	if err != nil {
		t.Fatal(err)
	}
	req := decision.DecisionRequest{
		Candidates:        []decision.Candidate{{ID: "A"}, {ID: "B"}},
		AllowedPrimaryIDs: []string{"A", "B"},
	}
	// Old in-flight snapshot still uses the old credential after reload.
	if _, err := rtOld.selected.Decide(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := rtNew.selected.Decide(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.auths) != 2 || cap.auths[0] != "Bearer key-A-value" || cap.auths[1] != "Bearer key-B-value" {
		t.Fatalf("auths=%v", cap.auths)
	}
}
