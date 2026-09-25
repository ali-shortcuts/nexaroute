package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// TestCrossProtocolSameSelection proves equivalent OpenAI Chat, Anthropic
// Messages, and OpenAI Responses requests — same eligible set, same mock Jev
// selection — resolve to the same physical deployment.
func TestCrossProtocolSameSelection(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)
	s.SyncCapabilityContracts()

	// The responses ingress does not echo X-Gateway-Deployment (pre-existing
	// behavior), so each leg is resolved through the upstream model it
	// executed: "up-b" is p2/b, the mock Jev choice "c1".
	do := func(path, body string) (int, string) {
		before := len(upCap.attemptModels())
		req := httptest.NewRequest("POST", "http://gateway"+path, strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		models := upCap.attemptModels()[before:]
		if len(models) != 1 {
			t.Fatalf("%s executed %d upstream attempts, want 1: %v", path, len(models), models)
		}
		return rr.Code, models[0]
	}
	code, chatUp := do("/v1/chat/completions", `{"model":"client-model","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`)
	if code != 200 {
		t.Fatalf("chat status=%d", code)
	}
	code, anthUp := do("/v1/messages", `{"model":"client-model","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
	if code != 200 {
		t.Fatalf("anthropic status=%d", code)
	}
	code, respUp := do("/v1/responses", `{"model":"client-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"max_output_tokens":8}`)
	if code != 200 {
		t.Fatalf("responses status=%d", code)
	}
	if chatUp != "up-b" || anthUp != "up-b" || respUp != "up-b" {
		t.Fatalf("chat=%q anthropic=%q responses=%q, want all up-b (p2/b)", chatUp, anthUp, respUp)
	}
	if cap.hits.Load() != 3 {
		t.Fatalf("jev hits=%d, want 3 (one per protocol, no retry)", cap.hits.Load())
	}
}

// TestVirtualVsDirectSameSelection proves the virtual catch-alls (auto and
// the client alias — NexaRoute's virtual endpoints) map a Jev choice to the
// same physical deployment as a direct deployment-ID request over the same
// eligible set.
func TestVirtualVsDirectSameSelection(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, _ := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	do := func(model string) (int, string) {
		req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr.Code, rr.Header().Get("X-Gateway-Deployment")
	}
	code, viaAlias := do("client-model")
	if code != 200 {
		t.Fatalf("alias status=%d", code)
	}
	code, viaAuto := do("auto")
	if code != 200 {
		t.Fatalf("auto status=%d", code)
	}
	if viaAlias != "p2/b" || viaAuto != "p2/b" {
		t.Fatalf("alias=%q auto=%q, want both p2/b", viaAlias, viaAuto)
	}
}

// TestFallbackExactOrder proves the external decision only alters the
// primary: with B selected and B,A failing, execution is exactly B,A,C.
func TestFallbackExactOrder(t *testing.T) {
	upCap := &upstreamCapture{fail: map[string]int{"up-b": 500, "up-a": 500}}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	cfg.Routing.MaxAttempts = 4
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
	if got := upCap.attemptModels(); len(got) != 3 || got[0] != "up-b" || got[1] != "up-a" || got[2] != "up-c" {
		t.Fatalf("attempt order=%v, want [up-b up-a up-c]", got)
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("jev hits=%d", cap.hits.Load())
	}
}

// TestMaxAttemptsUnchanged proves the external decision never alters the
// attempt budget.
func TestMaxAttemptsUnchanged(t *testing.T) {
	upCap := &upstreamCapture{fail: map[string]int{"up-a": 500, "up-b": 500, "up-c": 500}}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, _ := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID: "p3", Name: "P3", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "c", Model: "up-c", Aliases: []string{"client-model"},
			Enabled: true, Weight: 1, Priority: 0,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}}},
	})
	s := assistedGateway(t, cfg, jev.URL)

	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == 200 {
		t.Fatalf("all upstreams fail: status=%d", rr.Code)
	}
	if got := upCap.attemptModels(); len(got) != 2 {
		t.Fatalf("attempts=%v, want exactly 2 (max_attempts)", got)
	} else if got[0] != "up-b" {
		t.Fatalf("attempts=%v, want Jev-selected up-b first", got)
	}
}

// TestCapabilityAndHealthBoundary proves candidates removed by hard
// eligibility (capability mismatch, circuit/cooldown) are never sent to Jev.
func TestCapabilityAndHealthBoundary(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	// p2/b lacks tools (capability boundary); p3/c is cooled down (circuit
	// boundary); only p1/a stays eligible.
	cfg.Providers[1].Models[0].Capabilities.Tools = false
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID: "p3", Name: "P3", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "c", Model: "up-c", Aliases: []string{"client-model"},
			Enabled: true, Weight: 1, Priority: 0,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}}},
	})
	s := assistedGateway(t, cfg, jev.URL)
	s.hm.ForceCooldown("p3/c", "test", time.Minute)

	toolsBody := `{"model":"client-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"max_tokens":8}`
	rr, req := chatRequest(toolsBody)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Only p1/a is eligible (p2 lacks tools): single candidate => no call.
	if cap.hits.Load() != 0 {
		t.Fatalf("jev hits=%d, want 0 (single eligible candidate)", cap.hits.Load())
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "p1/a" {
		t.Fatalf("deployment=%q", got)
	}
}

// TestIneligibleCandidatesExcludedFromJevPayload proves the mock server only
// ever sees eligible candidates when several compete.
func TestIneligibleCandidatesExcludedFromJevPayload(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c0", 0.5))
	cfg := twoDeploymentConfig(up.URL)
	// Third deployment without tools stays configured but ineligible.
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID: "p3", Name: "P3", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "c", Model: "up-c", Aliases: []string{"client-model"},
			Enabled: true, Weight: 1, Priority: 0, Capabilities: config.Capabilities{}}},
	})
	s := assistedGateway(t, cfg, jev.URL)

	toolsBody := `{"model":"client-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"max_tokens":8}`
	rr, req := chatRequest(toolsBody)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	sent := cap.lastBody(t)
	if strings.Contains(sent, `"c2"`) {
		t.Fatalf("ineligible candidate sent to Jev: %s", sent)
	}
}

// TestExternalFailureDoesNotTouchModelHealth proves a Jev timeout neither
// penalizes models nor disrupts routing.
func TestExternalFailureDoesNotTouchModelHealth(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	// Blocking Jev: the 50ms decision timeout fires first.
	release := make(chan struct{})
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	defer jevSrv.Close()
	defer close(release)
	cfg := twoDeploymentConfig(up.URL)
	cfg.Decision.TimeoutMS = 50
	s := assistedGateway(t, cfg, jevSrv.URL)

	beforeA := s.hm.Get("p1/a")
	beforeB := s.hm.Get("p2/b")
	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("must fail open, status=%d", rr.Code)
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "p1/a" {
		t.Fatalf("deployment=%q, want router order", got)
	}
	afterA := s.hm.Get("p1/a")
	afterB := s.hm.Get("p2/b")
	if afterA.Failures != beforeA.Failures || afterB.Failures != beforeB.Failures {
		t.Fatalf("model failures changed by external timeout: A %d->%d B %d->%d",
			beforeA.Failures, afterA.Failures, beforeB.Failures, afterB.Failures)
	}
	if afterA.ConsecutiveFailures != 0 || afterB.ConsecutiveFailures != 0 {
		t.Fatal("external timeout must not create consecutive-failure evidence")
	}
}

// TestExecutedSelectionRecordsNormalModelHealth proves that when Jev selects
// B and B fails upstream, B records a normal model failure (external and
// model health stay distinct but both function).
func TestExecutedSelectionRecordsNormalModelHealth(t *testing.T) {
	upCap := &upstreamCapture{fail: map[string]int{"up-b": 500}}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, _ := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	s := assistedGateway(t, cfg, jev.URL)

	before := s.hm.Get("p2/b")
	rr, req := chatRequest(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d (failover to p1/a must succeed)", rr.Code)
	}
	after := s.hm.Get("p2/b")
	if after.Failures != before.Failures+1 {
		t.Fatalf("executed failure not recorded: %+v -> %+v", before, after)
	}
}

// TestLocalModesUnchanged proves off/local modes behave identically with the
// decision plane present but not assisting.
func TestLocalModesUnchanged(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	mk := func(mode, provider string) *Server {
		cfg := twoDeploymentConfig(up.URL)
		cfg.Decision.Mode = mode
		cfg.Decision.Provider = provider
		return testGateway(t, cfg)
	}
	run := func(s *Server) string {
		req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status=%d", rr.Code)
		}
		return rr.Header().Get("X-Gateway-Deployment")
	}
	offDep := run(mk("off", "local"))
	localDep := run(mk("local", "local"))
	policyDep := run(mk("local", "policy"))
	if offDep != "p1/a" || localDep != "p1/a" || policyDep != "p1/a" {
		t.Fatalf("off=%q local=%q policy=%q, want all p1/a", offDep, localDep, policyDep)
	}
	// Off mode emits no decision events at all.
	s := mk("off", "local")
	run(s)
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "external_decision" || e.Kind == "local_decision" {
			t.Fatalf("off mode emitted decision event: %+v", e)
		}
	}
	// Built-in selections never pollute the external_* metric series.
	s = mk("local", "policy")
	run(s)
	if got := metricsDump(t, s); strings.Contains(got, "nexaroute_external_decision_requests_total{") {
		t.Fatalf("local/policy decisions must not appear in external metrics:\n%s", got)
	}
}

// TestRouterSessionPinExposed verifies the decision-layer pin seam.
func TestRouterSessionPinExposed(t *testing.T) {
	up := httptest.NewServer((&upstreamCapture{}).handler())
	defer up.Close()
	cfg := twoDeploymentConfig(up.URL)
	cfg.Decision.Mode = "off"
	s := testGateway(t, cfg)
	req := router.Requirement{Model: "client-model", SessionKey: "sess-1"}
	if pin := s.rt.SessionPin(req); pin != "" {
		t.Fatalf("pin=%q, want empty before observation", pin)
	}
	s.rt.ObserveSession(req, "p2/b")
	if pin := s.rt.SessionPin(req); pin != "p2/b" {
		t.Fatalf("pin=%q, want p2/b", pin)
	}
}

// TestCacheHitMakesNoExternalCall proves a served-from-cache request never
// consults the external provider: the decision hook sits after the
// cache-serve early return on every cached ingress path.
func TestCacheHitMakesNoExternalCall(t *testing.T) {
	upCap := &upstreamCapture{}
	up := httptest.NewServer(upCap.handler())
	defer up.Close()
	jev, cap := startMockJev(t, 200, jevSelectResponse("c1", 0.9))
	cfg := twoDeploymentConfig(up.URL)
	cfg.Cache = config.CacheConfig{Enabled: true, TTLSeconds: 60, MaxEntries: 16, MaxBodyBytes: 1 << 20}
	s := assistedGateway(t, cfg, jev.URL)

	body := `{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`
	rr, req := chatRequest(body)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Header().Get("X-NexaRoute-Cache") == "HIT" {
		t.Fatalf("first request must miss: code=%d", rr.Code)
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("miss must consult Jev once, hits=%d", cap.hits.Load())
	}
	rr, req = chatRequest(body)
	s.Handler().ServeHTTP(rr, req)
	if rr.Header().Get("X-NexaRoute-Cache") != "HIT" {
		t.Fatalf("second identical request must hit: %s", rr.Body.String())
	}
	if cap.hits.Load() != 1 {
		t.Fatalf("cache hit must not consult Jev, hits=%d", cap.hits.Load())
	}
	if n := len(upCap.attemptModels()); n != 1 {
		t.Fatalf("cache hit must not touch upstream, attempts=%d", n)
	}
}
