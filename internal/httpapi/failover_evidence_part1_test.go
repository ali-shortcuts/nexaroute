package httpapi

// Part A1: instant failover evidence.
// After one valid failure on deployment A, the next request goes to B and
// never returns to A (no restart, no manual intervention).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestFailoverEvidence_InstantFailover_NeverBack(t *testing.T) {
	var aHits, bHits atomic.Int64
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","model":"b-up","choices":[{"index":0,"message":{"role":"assistant","content":"B_ONLY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer b.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.FailureThreshold = 1 // one valid failure opens the breaker
	cfg.Routing.CooldownSeconds = 1800
	cfg.Routing.ProviderFailureThreshold = 100 // isolate deployment circuit
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "openai_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	doReq := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
		return rr
	}

	first := doReq()
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "B_ONLY") {
		t.Fatalf("first request should fail over to B: status=%d body=%s", first.Code, first.Body.String())
	}
	if aHits.Load() != 1 {
		t.Fatalf("A should have been attempted exactly once, got %d", aHits.Load())
	}

	// Every subsequent request must go straight to B and never back to A.
	for i := 0; i < 5; i++ {
		rr := doReq()
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "B_ONLY") {
			t.Fatalf("request %d did not serve from B: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if got := aHits.Load(); got != 1 {
		t.Fatalf("traffic returned to failed deployment A: A hits=%d want 1, B hits=%d", got, bHits.Load())
	}
	if got := bHits.Load(); got != 6 {
		t.Fatalf("B should have served all 6 requests (1 failover + 5 direct), got %d", got)
	}
}
