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

func TestP3ConcurrentProviderModelFailureIsolation(t *testing.T) {
	var sharedBad, sharedGood, fallbackCalls atomic.Int64
	shared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"bad-upstream"`) {
			sharedBad.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"bad model unavailable","type":"server_error"}}`)
			return
		}
		sharedGood.Add(1)
		_, _ = io.WriteString(w, `{"id":"good","object":"chat.completion","model":"good-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"GOOD"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer shared.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fallback","object":"chat.completion","model":"fallback-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"FALLBACK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fallback.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	// Keep a burst of one-model failures from opening a provider-wide circuit;
	// the test is specifically about provider/model isolation.
	cfg.Routing.ProviderFailureThreshold = 100
	cfg.Providers = []config.ProviderConfig{
		{ID: "shared", Name: "Shared", Type: "openai_compatible", BaseURL: shared.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
			{ID: "bad", Model: "bad-upstream", Aliases: []string{"bad"}, Enabled: true, Priority: 0, Weight: 1},
			{ID: "good", Model: "good-upstream", Aliases: []string{"good"}, Enabled: true, Priority: 0, Weight: 1},
		}},
		{ID: "fallback", Name: "Fallback", Type: "openai_compatible", BaseURL: fallback.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "bad", Model: "fallback-upstream", Aliases: []string{"bad"}, Enabled: true, Priority: 1, Weight: 1}}},
	}
	s := testGateway(t, cfg)

	const perModel = 16
	errCh := make(chan string, perModel*2)
	call := func(model, want string) {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
			errCh <- model + ": status=" + rr.Result().Status + " body=" + rr.Body.String()
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < perModel; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); call("bad", "FALLBACK") }()
		go func() { defer wg.Done(); call("good", "GOOD") }()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if sharedBad.Load() == 0 || fallbackCalls.Load() == 0 {
		t.Fatalf("failed model was not retried through fallback: shared_bad=%d fallback=%d", sharedBad.Load(), fallbackCalls.Load())
	}
	if sharedGood.Load() != perModel {
		t.Fatalf("healthy sibling traffic was disrupted: good calls=%d want=%d", sharedGood.Load(), perModel)
	}
	if status := s.hm.Get("shared/good").Status; status == health.Cooldown || status == health.Retired {
		t.Fatalf("healthy sibling model inherited bad-model state: %+v", s.hm.Get("shared/good"))
	}
}

func TestP3CanonicalStreamPrecommitFailureFailsOver(t *testing.T) {
	var firstCalls, fallbackCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// A malformed first frame arrives after HTTP headers but before any
		// client-visible canonical event. It must remain eligible for failover.
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\n\n")
	}))
	defer first.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			"event: response.output_text.delta",
			`data: {"type":"response.output_text.delta","delta":"FALLBACK_STREAM"}`,
			"",
			"event: response.completed",
			`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"FALLBACK_STREAM"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			"",
		}, "\n"))
	}))
	defer fallback.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		{ID: "first", Name: "First", Type: "openai_responses", BaseURL: first.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "first-model", Enabled: true, Weight: 1, Priority: 0, Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "fallback", Name: "Fallback", Type: "openai_responses", BaseURL: fallback.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "fallback-model", Enabled: true, Weight: 1, Priority: 1, Capabilities: config.Capabilities{Streaming: true}}}},
	}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if firstCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("attempts first=%d fallback=%d", firstCalls.Load(), fallbackCalls.Load())
	}
	out := rr.Body.String()
	if !strings.Contains(out, "FALLBACK_STREAM") || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("fallback stream was not completed: %s", out)
	}
	if strings.Contains(out, "gateway_stream_error") || strings.Contains(out, "invalid_request_error") {
		t.Fatalf("precommit candidate leaked an error frame: %s", out)
	}
}
