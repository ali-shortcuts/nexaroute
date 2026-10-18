package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// TestDecision_PolicyVersionRecordedInEvent proves the applied policy version
// reaches the request trace event, not just the in-process trace object.
func TestDecision_PolicyVersionRecordedInEvent(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeUpstream(t, &hitsA, false)
	defer upA.Close()
	upB := makeUpstream(t, &hitsB, false)
	defer upB.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 10
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{{
		ID:      "balanced",
		Version: "2026-10-18.1",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
	}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "policy-version-test")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("request failed: %d %s", rr.Code, rr.Body.String())
	}

	var found bool
	for _, ev := range s.bus.Snapshot() {
		if ev.DecisionPolicyID == "balanced" {
			found = true
			if ev.DecisionPolicyVersion != "2026-10-18.1" {
				t.Fatalf("decision event policy version = %q, want 2026-10-18.1", ev.DecisionPolicyVersion)
			}
			if !strings.Contains(ev.DecisionBreakdown, "2026-10-18.1") {
				t.Fatalf("decision breakdown missing policy version: %s", ev.DecisionBreakdown)
			}
		}
	}
	if !found {
		t.Fatalf("no decision event carried the applied policy id; events=%v", s.bus.Snapshot())
	}
}

// TestFailover_EventCarriesClassifiedTrigger proves a real upstream fallback
// records a bounded classification on the failover event.
func TestFailover_EventCarriesClassifiedTrigger(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		w.WriteHeader(500)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer upA.Close()
	upB := makeUpstream(t, &hitsB, false)
	defer upB.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Priority: 1, Weight: 1}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "failover-class-test")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("request failed: %d %s", rr.Code, rr.Body.String())
	}
	if hitsB.Load() != 1 {
		t.Fatalf("expected fallback to p2, hits=%d", hitsB.Load())
	}

	var found bool
	for _, ev := range s.bus.Snapshot() {
		if ev.RequestID == "failover-class-test" && ev.Kind == "failover" {
			found = true
			if ev.FailoverClass == "" {
				t.Fatalf("failover event is missing a classified trigger: %+v", ev)
			}
		}
	}
	if !found {
		t.Fatalf("no failover event emitted; events=%v", s.bus.Snapshot())
	}
}
