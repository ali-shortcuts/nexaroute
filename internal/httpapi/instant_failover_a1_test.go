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
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// TestInstantFailoverA1 is the v0.12.0 A1 regression test (issue #73):
// deployment A returns a valid retryable failure, the next eligible request
// routes to B and never reselects A during its quarantine window.
//
// It uses deterministic fake upstreams only (no live provider) and covers:
//   - non-streaming instant failover with request order A -> B,
//   - A excluded (quarantined) on the follow-up request,
//   - the streaming pre-commit boundary (A answers 200 then dies before any
//     client-visible frame; the gateway must fail over to B pre-commit).
func TestInstantFailoverA1(t *testing.T) {
	t.Run("nonstreaming", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		var aHits, bHits atomic.Int64
		track := func(name string, hits *atomic.Int64, h http.HandlerFunc) *httptest.Server {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				hits.Add(1)
				h(w, r)
			}))
			t.Cleanup(s.Close)
			return s
		}
		// A: one valid retryable failure (503 provider_overloaded, failover-eligible).
		a := track("A", &aHits, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream overloaded","type":"server_error","code":"overloaded"}}`)
		})
		// B: healthy deployment that always succeeds.
		b := track("B", &bHits, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-b","object":"chat.completion","model":"physical-b","choices":[{"index":0,"message":{"role":"assistant","content":"B_ONLY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		})

		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Routing.Strategy = "priority"
		cfg.Routing.MaxAttempts = 2
		cfg.Routing.RetryBackoffMS = 0
		cfg.Routing.SessionAffinity = false
		// One valid failure must quarantine A immediately so the follow-up
		// request provably excludes it.
		cfg.Routing.FailureThreshold = 1
		cfg.Routing.ProviderFailureThreshold = 100
		cfg.Providers = []config.ProviderConfig{
			{ID: "A", Name: "A", Type: "openai_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-a", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
			{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-b", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1}}},
		}
		s := testGateway(t, cfg)
		call := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			return rr
		}

		rr := call()
		if rr.Code != http.StatusOK {
			t.Fatalf("first request status=%d body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "B_ONLY") {
			t.Fatalf("first request did not fail over to B: %s", rr.Body.String())
		}
		mu.Lock()
		got := strings.Join(order, ",")
		mu.Unlock()
		if got != "A,B" {
			t.Fatalf("request order=%q want A,B", got)
		}
		if aHits.Load() != 1 || bHits.Load() != 1 {
			t.Fatalf("attempts a=%d b=%d want 1,1", aHits.Load(), bHits.Load())
		}
		if st := s.hm.Get("A/m"); st.Status != health.Cooldown {
			t.Fatalf("A should be quarantined after one valid failure, got %+v", st)
		}

		// Follow-up request during A's quarantine window must never reselect A.
		rr = call()
		if rr.Code != http.StatusOK {
			t.Fatalf("second request status=%d body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "B_ONLY") {
			t.Fatalf("second request did not serve from B: %s", rr.Body.String())
		}
		if aHits.Load() != 1 {
			t.Fatalf("quarantined A was reselected: a=%d want 1", aHits.Load())
		}
		if bHits.Load() != 2 {
			t.Fatalf("B should serve the quarantined follow-up: b=%d want 2", bHits.Load())
		}
	})

	t.Run("streaming_precommit", func(t *testing.T) {
		var mu sync.Mutex
		var order []string
		var aHits, bHits atomic.Int64
		track := func(name string, hits *atomic.Int64, h http.HandlerFunc) *httptest.Server {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				hits.Add(1)
				h(w, r)
			}))
			t.Cleanup(s.Close)
			return s
		}
		// A answers 200 then dies before any client-visible canonical event:
		// a malformed first frame that must stay eligible for pre-commit failover.
		a := track("A", &aHits, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\n\n")
		})
		b := track("B", &bHits, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, strings.Join([]string{
				"event: response.output_text.delta",
				`data: {"type":"response.output_text.delta","delta":"B_STREAM"}`,
				"",
				"event: response.completed",
				`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"B_STREAM"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
				"",
			}, "\n"))
		})

		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Routing.Strategy = "priority"
		cfg.Routing.MaxAttempts = 2
		cfg.Routing.RetryBackoffMS = 0
		cfg.Routing.SessionAffinity = false
		cfg.Routing.FailureThreshold = 1
		cfg.Routing.ProviderFailureThreshold = 100
		cfg.Providers = []config.ProviderConfig{
			{ID: "first", Name: "First", Type: "openai_responses", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "first-model", Enabled: true, Weight: 1, Priority: 0, Capabilities: config.Capabilities{Streaming: true}}}},
			{ID: "fallback", Name: "Fallback", Type: "openai_responses", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "fallback-model", Enabled: true, Weight: 1, Priority: 1, Capabilities: config.Capabilities{Streaming: true}}}},
		}
		s := testGateway(t, cfg)
		streamCall := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			return rr
		}

		rr := streamCall()
		if rr.Code != http.StatusOK {
			t.Fatalf("streaming request status=%d body=%s", rr.Code, rr.Body.String())
		}
		mu.Lock()
		got := strings.Join(order, ",")
		mu.Unlock()
		if got != "A,B" {
			t.Fatalf("streaming request order=%q want A,B", got)
		}
		if aHits.Load() != 1 || bHits.Load() != 1 {
			t.Fatalf("streaming attempts a=%d b=%d want 1,1", aHits.Load(), bHits.Load())
		}
		out := rr.Body.String()
		if !strings.Contains(out, "B_STREAM") || !strings.Contains(out, "data: [DONE]") {
			t.Fatalf("fallback stream was not completed pre-commit: %s", out)
		}
		if strings.Contains(out, "gateway_stream_error") || strings.Contains(out, "invalid_request_error") {
			t.Fatalf("pre-commit candidate leaked an error frame: %s", out)
		}
		if st := s.hm.Get("first/m"); st.Status != health.Cooldown {
			t.Fatalf("pre-commit failed A should be quarantined, got %+v", st)
		}

		// Follow-up streaming request must not reselect quarantined A.
		rr = streamCall()
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "B_STREAM") {
			t.Fatalf("quarantined follow-up stream failed: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if aHits.Load() != 1 {
			t.Fatalf("quarantined A was reselected on follow-up stream: a=%d want 1", aHits.Load())
		}
		if bHits.Load() != 2 {
			t.Fatalf("B should serve the quarantined follow-up stream: b=%d want 2", bHits.Load())
		}
	})
}
