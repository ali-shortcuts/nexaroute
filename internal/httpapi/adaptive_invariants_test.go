package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M5-3 hedging-cap invariant: hedging is structurally capped at
// ONE partner for the FIRST attempt — with a generous attempts
// budget and three candidates, a slow primary still produces at
// most 2 upstream calls, and the abandoned loser records NO health
// failure (abandonment is a routing decision, not provider
// evidence).
func TestHedgingAmplificationCapAndLoserHealth(t *testing.T) {
	var primaryCalls, hedgeCalls, thirdCalls atomic.Int64
	// The fast hedge waits until the primary has reached its upstream
	// handler before it returns. Without this rendezvous, a saturated Go
	// scheduler can delay the primary goroutine beyond the 20ms hedge timer;
	// the fast result then cancels the not-yet-dispatched primary, producing
	// 0/1 even though this test intends to exercise a real losing primary.
	primaryArrived := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		select {
		case primaryArrived <- struct{}{}:
		default:
		}
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"slow","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"slow-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hedgeCalls.Add(1)
		select {
		case <-primaryArrived:
		case <-time.After(2 * time.Second):
			http.Error(w, "primary attempt did not reach upstream before hedge response", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fast","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fast-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fast.Close()
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"third","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"third-answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer third.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 5 // attempts budget is NOT the limiter here
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 20
	cfg.Routing.RetryBackoffMS = 1
	cfg.Providers = []config.ProviderConfig{
		hedgeBudgetProvider("primary", slow.URL, 0),
		hedgeBudgetProvider("hedge", fast.URL, 1),
		hedgeBudgetProvider("third", third.URL, 2),
	}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "fast-answer") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if primaryCalls.Load() != 1 || hedgeCalls.Load() != 1 {
		t.Fatalf("expected exactly one primary + one hedge call, got %d/%d", primaryCalls.Load(), hedgeCalls.Load())
	}
	if thirdCalls.Load() != 0 {
		t.Fatalf("hedging exceeded its one-partner cap: third candidate called %d times", thirdCalls.Load())
	}
	for _, st := range srv.hm.Snapshot() {
		if st.Deployment == "primary/m" && (st.Failures != 0 || st.ConsecutiveFailures != 0) {
			t.Fatalf("hedge loser recorded health failure evidence: %+v", st)
		}
	}
}

// M5-3 adaptive invariant: an upstream's Retry-After is honored
// but CAPPED by routing.max_retry_after_seconds before it reaches
// the client.
func TestRetryAfterIsCappedForClients(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRetryAfterSeconds = 5
	cfg.Providers = []config.ProviderConfig{{
		ID: "rl", Name: "RL", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "rl-model", Aliases: []string{"client"}, Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	ra := rr.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("Retry-After header missing on 429")
	}
	if secs, err := strconv.Atoi(ra); err != nil || secs > 5 {
		t.Fatalf("Retry-After not capped at 5s: %q (err=%v)", ra, err)
	}
}
