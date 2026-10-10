package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func privacyMockUpstream(hits *atomic.Int64, fail *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if fail.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
			return
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
}

type privacyLeakFixture struct {
	srv   *Server
	hitsA *atomic.Int64
	hitsB *atomic.Int64
	failB *atomic.Bool
	upA   *httptest.Server
	upB   *httptest.Server
}

func newPrivacyLeakGateway(t *testing.T, routePrivacy string, vkRequirePrivacy string, vkPlain string) *privacyLeakFixture {
	t.Helper()
	var hitsA, hitsB atomic.Int64
	var failB atomic.Bool
	upA := privacyMockUpstream(&hitsA, &atomic.Bool{})
	upB := privacyMockUpstream(&hitsB, &failB)
	t.Cleanup(upA.Close)
	t.Cleanup(upB.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.SessionAffinity = true
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 5
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "pool-all", Mode: "explicit", Deployments: []string{"pa/m", "pb/m"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "rp1", CandidatePool: "pool-all", Privacy: routePrivacy},
	}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-private", RouteProfile: "rp1", Enabled: &trueVal},
	}
	caps := config.Capabilities{Streaming: true, Tools: true}
	cfg.Providers = []config.ProviderConfig{
		{ID: "pa", Name: "ProviderA", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true,
			DataHandling: config.DataHandlingConfig{TrainsOnData: "yes"},
			Models:       []config.ModelConfig{{ID: "m", Model: "phys-a", Enabled: true, Weight: 1, Capabilities: caps}}},
		{ID: "pb", Name: "ProviderB", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true,
			DataHandling: config.DataHandlingConfig{TrainsOnData: "no"},
			Models:       []config.ModelConfig{{ID: "m", Model: "phys-b", Enabled: true, Weight: 1, Capabilities: caps}}},
	}
	if vkPlain != "" {
		cfg.ClientAuth.Enabled = true
		cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
			{ID: "vk-private", KeyHash: keyDigest(vkPlain), RequirePrivacy: vkRequirePrivacy, AllowedModels: []string{"nexa-private"}},
		}
	}
	s := testGateway(t, cfg)
	return &privacyLeakFixture{srv: s, hitsA: &hitsA, hitsB: &hitsB, failB: &failB, upA: upA, upB: upB}
}

func (f *privacyLeakFixture) doOpenAI(t *testing.T, body, session, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Session-Id", session)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rr := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rr, req)
	return rr
}

func assertPrivacyUnavailable(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "privacy_unavailable") {
		t.Fatalf("missing privacy_unavailable type: %s", body)
	}
	if !strings.Contains(body, "no deployment satisfies the required privacy class") {
		t.Fatalf("missing privacy message: %s", body)
	}
	for _, leak := range []string{"pa/m", "pb/m", "ProviderA", "ProviderB", "phys-a", "phys-b", `"pa"`, `"pb"`} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaks provider identity %q: %s", leak, body)
		}
	}
}

func TestPrivacyP2PrivateRouteLeak(t *testing.T) {
	f := newPrivacyLeakGateway(t, "no_training", "", "")
	// Stale session-affinity pin to the forbidden deployment A. The next
	// private request must re-check and ignore it.
	f.srv.rt.ObserveSession(router.Requirement{Model: "nexa-private", SessionKey: "sess-pin-1", RequireNoTraining: true}, "pa/m")

	normal := `{"model":"nexa-private","messages":[{"role":"user","content":"hi"}]}`
	stream := `{"model":"nexa-private","messages":[{"role":"user","content":"hi"}],"stream":true}`
	tools := `{"model":"nexa-private","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"x","parameters":{"type":"object","properties":{}}}}]}`

	// Phase 1: B healthy — 60 requests across variants must all succeed via B.
	bodies := []string{normal, stream, tools}
	for i := 0; i < 60; i++ {
		rr := f.doOpenAI(t, bodies[i%3], "sess-pin-1", "")
		if rr.Code != 200 {
			t.Fatalf("request %d status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if got := f.hitsA.Load(); got != 0 {
		t.Fatalf("privacy leak: forbidden upstream A received %d requests (want 0)", got)
	}
	if got := f.hitsB.Load(); got == 0 {
		t.Fatal("expected healthy upstream B to serve requests")
	}

	// Phase 2: force failures on B. B stays eligible until its breaker
	// opens, so the first few attempts surface the upstream error; once B
	// is cooling down the empty set must fail closed with
	// privacy_unavailable. A must never be called in either case.
	f.failB.Store(true)
	bBefore := f.hitsB.Load()
	sawPrivacy := false
	for i := 0; i < 40; i++ {
		rr := f.doOpenAI(t, bodies[i%3], "sess-pin-1", "")
		if rr.Code == http.StatusServiceUnavailable && strings.Contains(rr.Body.String(), "privacy_unavailable") {
			assertPrivacyUnavailable(t, rr)
			sawPrivacy = true
			continue
		}
		// Pre-breaker upstream failure from the eligible B only.
		if rr.Code == 0 || (rr.Code != 500 && rr.Code != 502 && rr.Code != 503) {
			t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
		}
		if got := f.hitsA.Load(); got != 0 {
			t.Fatalf("privacy leak under failover: A received %d requests (want 0)", got)
		}
	}
	if got := f.hitsA.Load(); got != 0 {
		t.Fatalf("privacy leak under failover: A received %d requests (want 0)", got)
	}
	if f.hitsB.Load() <= bBefore {
		t.Fatal("expected B to be attempted before privacy 503")
	}
	if !sawPrivacy {
		t.Fatal("expected privacy_unavailable once B cooled down")
	}
	// Steady state with B down must be privacy_unavailable.
	rr := f.doOpenAI(t, normal, "sess-pin-1", "")
	assertPrivacyUnavailable(t, rr)

	// Anthropic + Responses ingress must also fail closed with the same envelope.
	areq := httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(`{"model":"nexa-private","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	areq.RemoteAddr = "127.0.0.1:12345"
	areq.Header.Set("Content-Type", "application/json")
	areq.Header.Set("X-Session-Id", "sess-pin-1")
	arr := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(arr, areq)
	assertPrivacyUnavailable(t, arr)

	rreq := httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-private","input":"hello"}`))
	rreq.RemoteAddr = "127.0.0.1:12345"
	rreq.Header.Set("Content-Type", "application/json")
	rreq.Header.Set("X-Session-Id", "sess-pin-1")
	rrr := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rrr, rreq)
	assertPrivacyUnavailable(t, rrr)

	if got := f.hitsA.Load(); got != 0 {
		t.Fatalf("privacy leak across protocols: A=%d want 0", got)
	}

	// Privacy-safe observability: at least one privacy_excluded event, no prompt content.
	found := false
	for _, ev := range f.srv.bus.Snapshot() {
		if ev.ErrorType == "privacy_excluded" {
			found = true
			if strings.Contains(ev.Message, "hi") && strings.Contains(ev.Message, "content") {
				t.Fatalf("privacy event leaks prompt content: %+v", ev)
			}
		}
	}
	if !found {
		t.Fatal("expected privacy_excluded event")
	}
}

func TestPrivacyP2PrivateVirtualKeyLeak(t *testing.T) {
	const vk = "nrk_private_test_key_abc123"
	f := newPrivacyLeakGateway(t, "", "no_training", vk)
	f.srv.rt.ObserveSession(router.Requirement{Model: "nexa-private", SessionKey: "sess-vk-1", RequireNoTraining: true}, "pa/m")

	normal := `{"model":"nexa-private","messages":[{"role":"user","content":"hi"}]}`
	stream := `{"model":"nexa-private","messages":[{"role":"user","content":"hi"}],"stream":true}`
	tools := `{"model":"nexa-private","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"x","parameters":{"type":"object","properties":{}}}}]}`
	bodies := []string{normal, stream, tools}
	for i := 0; i < 60; i++ {
		rr := f.doOpenAI(t, bodies[i%3], "sess-vk-1", vk)
		if rr.Code != 200 {
			t.Fatalf("request %d status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if got := f.hitsA.Load(); got != 0 {
		t.Fatalf("key-privacy leak: A=%d want 0", got)
	}
	f.failB.Store(true)
	sawPrivacy := false
	for i := 0; i < 40; i++ {
		rr := f.doOpenAI(t, bodies[i%3], "sess-vk-1", vk)
		if rr.Code == http.StatusServiceUnavailable && strings.Contains(rr.Body.String(), "privacy_unavailable") {
			assertPrivacyUnavailable(t, rr)
			sawPrivacy = true
			continue
		}
		if got := f.hitsA.Load(); got != 0 {
			t.Fatalf("key-privacy leak under failover: A=%d want 0", got)
		}
	}
	if !sawPrivacy {
		t.Fatal("expected privacy_unavailable once B cooled down (key path)")
	}
	if got := f.hitsA.Load(); got != 0 {
		t.Fatalf("key-privacy leak under failover: A=%d want 0", got)
	}
	found := false
	for _, ev := range f.srv.bus.Snapshot() {
		if ev.ErrorType == "privacy_excluded" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected privacy_excluded event for private key path")
	}
}

func TestPrivacyP2ConcurrentLeakRace(t *testing.T) {
	f := newPrivacyLeakGateway(t, "no_training", "", "")
	f.srv.rt.ObserveSession(router.Requirement{Model: "nexa-private", SessionKey: "sess-race", RequireNoTraining: true}, "pa/m")
	var wg sync.WaitGroup
	errs := make(chan string, 320)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				rr := f.doOpenAI(t, `{"model":"nexa-private","messages":[{"role":"user","content":"hi"}]}`, "sess-race", "")
				if rr.Code != 200 {
					errs <- "status"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent request failed: %s A=%d B=%d", e, f.hitsA.Load(), f.hitsB.Load())
	}
	if got := f.hitsA.Load(); got != 0 {
		t.Fatalf("race leak: A=%d want 0 (B=%d)", got, f.hitsB.Load())
	}
}
