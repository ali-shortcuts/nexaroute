package httpapi

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func shadowPolicyConfig(shadow bool, upA, upB string) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "cost-aware"
	cfg.Decision.Shadow = shadow
	cfg.Decision.TimeoutMS = 10
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{{
		ID:      "cost-aware",
		Version: "shadow.v1",
		Weights: config.DecisionPolicyWeights{Cost: 1},
	}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1, InputCostPerMTok: 100, OutputCostPerMTok: 100}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Priority: 0, Weight: 1, InputCostPerMTok: 1, OutputCostPerMTok: 1}}},
	}
	return cfg
}

func runShadowRequest(t *testing.T, shadow bool) (hitsA, hitsB int64, events []string, changed bool) {
	t.Helper()
	var a, b atomic.Int64
	upA := makeUpstream(t, &a, false)
	defer upA.Close()
	upB := makeUpstream(t, &b, false)
	defer upB.Close()

	s := testGateway(t, shadowPolicyConfig(shadow, upA.URL, upB.URL))
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"auto","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "shadow-test")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("request failed: %d %s", rr.Code, rr.Body.String())
	}
	for _, ev := range s.bus.Snapshot() {
		events = append(events, ev.Kind)
		if ev.Kind == "decision_shadow" || ev.Kind == "decision_ok" {
			changed = ev.DecisionChangedPrimary
		}
	}
	return a.Load(), b.Load(), events, changed
}

// TestShadowModeObservesWithoutApplying proves the policy engine evaluated a
// route change under shadow mode (event recorded) but served traffic stayed on
// the production primary, while the same policy outside shadow mode did move
// traffic to the preferred deployment.
func TestShadowModeObservesWithoutApplying(t *testing.T) {
	hitsA, hitsB, events, changed := runShadowRequest(t, true)
	if hitsA != 1 || hitsB != 0 {
		t.Fatalf("shadow changed served traffic: hitsA=%d hitsB=%d", hitsA, hitsB)
	}
	if !containsKind(events, "decision_shadow") {
		t.Fatalf("no decision_shadow event emitted; events=%v", events)
	}
	if containsKind(events, "decision_ok") {
		t.Fatalf("shadow emitted an applied decision_ok event; events=%v", events)
	}
	if !changed {
		t.Fatalf("shadow decision did not record that the policy would change the primary; events=%v", events)
	}

	// Control: the identical policy without shadow must move traffic.
	cA, cB, _, _ := runShadowRequest(t, false)
	if cA != 0 || cB != 1 {
		t.Fatalf("non-shadow policy did not apply: hitsA=%d hitsB=%d", cA, cB)
	}
}

func containsKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
