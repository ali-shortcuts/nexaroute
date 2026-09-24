package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func openAIUpstream(body string, delay time.Duration, calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
}

func singleProviderOpenAI(t *testing.T, url string, modelID string, extra config.ModelConfig) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	m := config.ModelConfig{ID: modelID, Model: "upstream-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}
	if extra.ID != "" {
		m = extra
	}
	cfg.Providers = []config.ProviderConfig{{ID: "a", Name: "A", Type: "openai_compatible", BaseURL: url, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{m}}}
	return cfg
}

func doOpenAIRequest(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

const openAIOKBody = `{"id":"c","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

// --- Hedging -------------------------------------------------------------

func TestHedgeSecondaryWinsWhenPrimarySlow(t *testing.T) {
	var slowCalls, fastCalls atomic.Int64
	slow := openAIUpstream(openAIOKBody, 500*time.Millisecond, &slowCalls)
	defer slow.Close()
	fast := openAIUpstream(openAIOKBody, 0, &fastCalls)
	defer fast.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 50
	cfg.Routing.MaxAttempts = 4
	cfg.Providers = []config.ProviderConfig{
		{ID: "slow", Name: "S", Type: "openai_compatible", BaseURL: slow.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}},
		{ID: "fast", Name: "F", Type: "openai_compatible", BaseURL: fast.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}},
	}
	s := testGateway(t, cfg)
	rr := doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "fast/m" {
		t.Fatalf("expected hedged winner fast/m, got %q", got)
	}
	if fastCalls.Load() != 1 {
		t.Fatalf("fast upstream calls=%d", fastCalls.Load())
	}
	if slowCalls.Load() != 1 {
		t.Fatalf("slow upstream calls=%d", slowCalls.Load())
	}
	// Abandonment is a routing decision, not provider evidence: the slow
	// primary must stay healthy.
	if st := s.hm.Get("slow/m"); st.Status != health.Healthy || st.ConsecutiveFailures != 0 {
		t.Fatalf("slow primary must not be poisoned: %+v", st)
	}
	kinds := map[string]int{}
	for _, ev := range s.bus.SnapshotLimit(64) {
		kinds[ev.Kind]++
	}
	if kinds["hedge_launch"] != 1 {
		t.Fatalf("expected one hedge_launch event, got %+v", kinds)
	}
}

func TestHedgeNotLaunchedWhenPrimaryFast(t *testing.T) {
	var calls atomic.Int64
	up := openAIUpstream(openAIOKBody, 0, &calls)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 50
	s := testGateway(t, cfg)
	rr := doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	for _, ev := range s.bus.SnapshotLimit(64) {
		if ev.Kind == "hedge_launch" {
			t.Fatal("hedge must not launch with a single candidate")
		}
	}
}

// TestHedgePrimaryWinIsNotDelayedByLoser pins the race fast path: when the
// primary wins a hedged race the client is served as soon as the primary
// answers, and the losing leg is cancelled instead of awaited. The losing leg
// here is a stalled provider, so awaiting it would push client latency up to
// the loser's full response time (regression: the winner path used to block
// until the loser answered).
func TestHedgePrimaryWinIsNotDelayedByLoser(t *testing.T) {
	var primaryCalls, loserCalls atomic.Int64
	var loserCancelledAtMillis atomic.Int64 // 0 = never cancelled
	primary := openAIUpstream(openAIOKBody, 200*time.Millisecond, &primaryCalls)
	defer primary.Close()

	start := time.Now()
	loser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loserCalls.Add(1)
		// Drain the body first: net/http only starts its background
		// disconnect read once the request body is fully consumed, and
		// without it a cancelled client would go unnoticed here.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(3 * time.Second): // stalled provider
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, openAIOKBody)
		case <-r.Context().Done():
			loserCancelledAtMillis.Store(time.Since(start).Milliseconds())
		}
	}))
	defer loser.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 50
	cfg.Routing.MaxAttempts = 4
	cfg.Providers = []config.ProviderConfig{
		{ID: "primary", Name: "P", Type: "openai_compatible", BaseURL: primary.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}},
		{ID: "loser", Name: "L", Type: "openai_compatible", BaseURL: loser.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}},
	}
	s := testGateway(t, cfg)
	rr := doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	elapsed := time.Since(start)

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "primary/m" {
		t.Fatalf("expected primary to win, got %q", got)
	}
	if elapsed > time.Second {
		t.Fatalf("client waited %v for the winning primary; the losing hedge leg must not delay the response", elapsed)
	}
	if loserCalls.Load() != 1 {
		t.Fatalf("loser upstream calls=%d, want 1", loserCalls.Load())
	}

	deadline := time.Now().Add(2 * time.Second)
	for loserCancelledAtMillis.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if loserCancelledAtMillis.Load() == 0 {
		t.Fatal("losing hedge leg was not cancelled after the primary won")
	}
	kinds := map[string]int{}
	for _, ev := range s.bus.SnapshotLimit(64) {
		kinds[ev.Kind]++
	}
	if kinds["hedge_launch"] != 1 {
		t.Fatalf("expected one hedge_launch event, got %+v", kinds)
	}
	for kinds["hedged_abandoned"] == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		kinds = map[string]int{}
		for _, ev := range s.bus.SnapshotLimit(64) {
			kinds[ev.Kind]++
		}
	}
	if kinds["hedged_abandoned"] != 1 {
		t.Fatalf("expected one hedged_abandoned event, got %+v", kinds)
	}
}

// --- Client auth ---------------------------------------------------------

func TestClientAuthRejectsMissingAndWrongKeys(t *testing.T) {
	var calls atomic.Int64
	up := openAIUpstream(openAIOKBody, 0, &calls)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"sk-test-12345678"}, RPM: 0}
	s := testGateway(t, cfg)
	rr := doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing key must 401, got %d", rr.Code)
	}
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-wrong-key-999")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key must 401, got %d", rr.Code)
	}
	req = httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-12345678")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid key must 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	// x-api-key is accepted too.
	req = httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-test-12345678")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("x-api-key must 200, got %d", rr.Code)
	}
}

func TestClientAuthRPMCeiling(t *testing.T) {
	var calls atomic.Int64
	up := openAIUpstream(openAIOKBody, 0, &calls)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"sk-test-12345678"}, RPM: 1}
	s := testGateway(t, cfg)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test-12345678")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if i == 0 && rr.Code != http.StatusOK {
			t.Fatalf("first request must pass, got %d", rr.Code)
		}
		if i == 1 && rr.Code != http.StatusTooManyRequests {
			t.Fatalf("second request must 429, got %d", rr.Code)
		}
	}
}

// --- Response cache ------------------------------------------------------

func TestResponseCacheHitAndInvalidation(t *testing.T) {
	var calls atomic.Int64
	up := openAIUpstream(openAIOKBody, 0, &calls)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.Cache = config.CacheConfig{Enabled: true, TTLSeconds: 60, MaxEntries: 16, MaxBodyBytes: 1 << 20}
	s := testGateway(t, cfg)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	rr := doOpenAIRequest(s, body)
	if rr.Code != 200 || rr.Header().Get("X-NexaRoute-Cache") == "HIT" {
		t.Fatalf("first request must miss: code=%d", rr.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d after first", calls.Load())
	}
	rr = doOpenAIRequest(s, body)
	if rr.Header().Get("X-NexaRoute-Cache") != "HIT" {
		t.Fatalf("second identical request must hit: %s", rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("cache hit must not touch upstream, calls=%d", calls.Load())
	}
	// Streaming and sampling variation bypass the cache (and go upstream).
	rr = doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rr.Header().Get("X-NexaRoute-Cache") == "HIT" {
		t.Fatal("streaming must bypass cache")
	}
	if calls.Load() != 2 {
		t.Fatalf("stream bypass must hit upstream, calls=%d", calls.Load())
	}
	rr = doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.9}`)
	if rr.Header().Get("X-NexaRoute-Cache") == "HIT" {
		t.Fatal("non-deterministic temperature must bypass cache")
	}
	if calls.Load() != 3 {
		t.Fatalf("temperature bypass must hit upstream, calls=%d", calls.Load())
	}
	// Config swap invalidates wholesale.
	if err := s.applyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	rr = doOpenAIRequest(s, body)
	if rr.Header().Get("X-NexaRoute-Cache") == "HIT" {
		t.Fatal("config swap must invalidate cache")
	}
	if calls.Load() != 4 {
		t.Fatalf("expected upstream refill after invalidation, calls=%d", calls.Load())
	}
}

// --- Context-window pre-routing ------------------------------------------

func TestContextWindowRoutingFiltersTooSmallDeployment(t *testing.T) {
	var smallHits, bigHits atomic.Int64
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { smallHits.Add(1); io.WriteString(w, openAIOKBody) }))
	defer small.Close()
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bigHits.Add(1); io.WriteString(w, openAIOKBody) }))
	defer big.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	long := strings.Repeat("x", 4000) // ~1000 estimated tokens
	cfg.Providers = []config.ProviderConfig{
		{ID: "small", Name: "S", Type: "openai_compatible", BaseURL: small.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0, ContextWindow: 100, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}},
		{ID: "big", Name: "B", Type: "openai_compatible", BaseURL: big.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1, ContextWindow: 100000, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}},
	}
	s := testGateway(t, cfg)
	body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": long}}})
	rr := doOpenAIRequest(s, string(body))
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "big/m" {
		t.Fatalf("expected big deployment, got %q", got)
	}
	if smallHits.Load() != 0 {
		t.Fatalf("small deployment must be pre-filtered, hits=%d", smallHits.Load())
	}
	// A short request may still use the small deployment.
	rr = doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 || rr.Header().Get("X-Gateway-Deployment") != "small/m" {
		t.Fatalf("short request should reach small deployment: %s", rr.Header().Get("X-Gateway-Deployment"))
	}
}

// --- Usage accounting -----------------------------------------------------

func TestUsageRecordedFromOpenAINonStream(t *testing.T) {
	var calls atomic.Int64
	up := openAIUpstream(openAIOKBody, 0, &calls)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.Providers[0].Models[0].InputCostPerMTok = 3
	cfg.Providers[0].Models[0].OutputCostPerMTok = 15
	s := testGateway(t, cfg)
	if rr := doOpenAIRequest(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	snap := s.usageSnapshotWithPrices(cfg)
	if snap.TotalPromptTokens != 11 || snap.TotalCompletionTokens != 7 || snap.TotalRequests != 1 {
		t.Fatalf("unexpected usage snapshot: %+v", snap)
	}
	if snap.TotalEstimatedCostUSD <= 0 {
		t.Fatalf("expected positive estimated cost, got %f", snap.TotalEstimatedCostUSD)
	}
}

// --- Config validation ----------------------------------------------------

func TestConfigRejectsBadHedgingAndCacheAndClientAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.HedgingDelayMS = 10
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "hedging_delay_ms") {
		t.Fatalf("expected hedging_delay_ms validation error, got %v", err)
	}
	cfg = config.Default()
	cfg.Cache.Enabled = true
	cfg.Cache.TTLSeconds = 0
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero ttl must be defaulted by ApplyDefaults, got %v", err)
	}
	cfg = config.Default()
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = nil
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "client_auth.keys") {
		t.Fatalf("expected client auth key validation error, got %v", err)
	}
	cfg = config.Default()
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = []string{"short"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("short client keys must be rejected")
	}
}
