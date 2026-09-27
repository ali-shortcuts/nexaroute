package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestClaudeAbsorbsModelLifecycleFailuresAndFailsOver(t *testing.T) {
	var mu sync.Mutex
	var order []string
	appendAttempt := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	up := func(name string, handler http.HandlerFunc) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			appendAttempt(name)
			handler(w, r)
		}))
		t.Cleanup(s.Close)
		return s
	}

	a := up("A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"type":"about:blank","title":"Gone","status":410,"detail":"The model 'deepseek-ai/deepseek-v4-flash' has reached its end of life on 2026-08-07T09:00:00Z and is no longer available."}`)
	})
	b := up("B", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Model 'XiaomiMiMo/MiMo-V2.5' is currently unavailable.","type":"invalid_request_error","param":null,"code":"model_unavailable"}}`)
	})
	c := up("C", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	})
	d := up("D", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-d","object":"chat.completion","model":"physical-d","choices":[{"index":0,"message":{"role":"assistant","content":"D_ONLY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "openai_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "deepseek-ai/deepseek-v4-flash", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "XiaomiMiMo/MiMo-V2.5", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1}}},
		{ID: "C", Name: "C", Type: "openai_compatible", BaseURL: c.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-c", Aliases: []string{"coding"}, Enabled: true, Priority: 2, Weight: 1}}},
		{ID: "D", Name: "D", Type: "openai_compatible", BaseURL: d.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-d", Aliases: []string{"coding"}, Enabled: true, Priority: 3, Weight: 1}}},
	}

	s := testGateway(t, cfg)
	body := `{"model":"coding","max_tokens":16,"messages":[{"role":"user","content":"continue"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "D_ONLY") {
		t.Fatalf("final successful deployment response missing: %s", rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid Anthropic response: %v body=%s", err, rr.Body.String())
	}
	if response["model"] != "coding" {
		t.Fatalf("public model identity changed: %#v", response["model"])
	}
	for _, leaked := range []string{"410", "Gone", "end of life", "model_unavailable", "XiaomiMiMo", "deepseek-v4-flash"} {
		if strings.Contains(rr.Body.String(), leaked) {
			t.Fatalf("intermediate upstream failure leaked to Claude: %q in %s", leaked, rr.Body.String())
		}
	}
	mu.Lock()
	gotOrder := strings.Join(order, ",")
	mu.Unlock()
	if gotOrder != "A,B,C,D" {
		t.Fatalf("attempt order=%q want A,B,C,D", gotOrder)
	}
	if st := s.hm.Get("A/m"); st.Status != health.Retired || st.LastErrorClass != "model_retired" {
		t.Fatalf("A not retired: %+v", st)
	}
	if st := s.hm.Get("B/m"); st.Status != health.Degraded {
		t.Fatalf("B should be temporarily quarantined/degraded, got %+v", st)
	}
	if st := s.hm.Get("C/m"); st.Status != health.Cooldown {
		t.Fatalf("C should be in rate-limit cooldown, got %+v", st)
	}
	if got := rr.Header().Get("X-Gateway-Provider"); got != "D" {
		t.Fatalf("final provider header=%q want D", got)
	}
}

func TestClaudeCandidateExhaustionReturnsNormalizedError(t *testing.T) {
	secretA := "A_PRIVATE_PROVIDER_BODY"
	secretB := "B_PRIVATE_PROVIDER_BODY"
	mk := func(status int, body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, body)
		}))
		t.Cleanup(s.Close)
		return s
	}
	a := mk(http.StatusServiceUnavailable, `{"error":{"message":"`+secretA+`"}}`)
	b := mk(http.StatusTooManyRequests, `{"error":{"message":"`+secretB+`"}}`)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "anthropic_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "a", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "anthropic_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "b", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages",
		strings.NewReader(`{"model":"coding","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), secretA) || strings.Contains(rr.Body.String(), secretB) {
		t.Fatalf("raw provider terminal error leaked: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("normalized terminal message missing: %s", rr.Body.String())
	}
}

func TestLifecycleEventsAreStructuredAndSafe(t *testing.T) {
	// Compile-time/behavioral smoke: the event plane should carry lifecycle
	// categories, not provider payloads. The full route test above produces
	// these events; this test keeps the required kinds stable for the UI.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	s.bus.Add(structuredLifecycleEventForTest("model_retired", "A/m", "model_retired"))
	s.bus.Add(structuredLifecycleEventForTest("model_unavailable", "B/m", "model_temporarily_unavailable"))
	got := s.bus.Snapshot()
	if len(got) < 2 || got[len(got)-2].Kind != "model_retired" || got[len(got)-1].Kind != "model_unavailable" {
		t.Fatalf("lifecycle events not preserved: %+v", got)
	}
}

func structuredLifecycleEventForTest(kind, deployment, errorType string) events.Event {
	return events.Event{Kind: kind, Deployment: deployment, Message: kind, ErrorType: errorType, StatusCode: 400, LatencyMS: time.Millisecond.Milliseconds()}
}
