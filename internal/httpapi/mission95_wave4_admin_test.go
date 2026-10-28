package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave4AdminGateway(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	return testGateway(t, cfg)
}

// The admin middleware denies unmapped method+path pairs (POST on a
// read-only path) before the handler runs, so the handler's own 405 is
// reached only by calling the handler directly.
func TestWave4AdminSnapshotMethodNotAllowed(t *testing.T) {
	s := wave4AdminGateway(t)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/snapshot", nil)
	rr := httptest.NewRecorder()
	s.adminSnapshot(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestWave4AdminSnapshotClampsLimits(t *testing.T) {
	s := wave4AdminGateway(t)
	rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot?limit=99999&events=99999", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := coverageAdminJSON(t, rr)
	deps, ok := out["deployments"].([]any)
	if !ok || len(deps) != 1 {
		t.Fatalf("deployments = %v, want the single configured deployment", out["deployments"])
	}
}

// Decision-provider key resolution: an env-backed key reports configured,
// while a provider with no key reports not-configured even though its
// registry health message is the excluded "api key not configured" string.
func TestWave4AdminSnapshotDecisionProviderKeys(t *testing.T) {
	t.Setenv("WAVE4_JEV_KEY", "test-key-value")
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-env", Type: "jev", APIKeyEnv: "WAVE4_JEV_KEY"},
		{ID: "jev-none", Type: "jev"},
	}
	s := testGateway(t, cfg)
	rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := coverageAdminJSON(t, rr)
	decision, ok := out["decision"].(map[string]any)
	if !ok {
		t.Fatalf("decision section = %v", out["decision"])
	}
	ext, ok := decision["external_decision_providers"].([]any)
	if !ok || len(ext) != 2 {
		t.Fatalf("external providers = %v, want 2", decision["external_decision_providers"])
	}
	byID := map[string]map[string]any{}
	for _, e := range ext {
		m := e.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if byID["jev-env"]["key_configured"] != true {
		t.Fatalf("jev-env = %v, want key_configured true", byID["jev-env"])
	}
	if byID["jev-none"]["key_configured"] != false {
		t.Fatalf("jev-none = %v, want key_configured false", byID["jev-none"])
	}
	if _, ok := byID["jev-none"]["health_message"]; ok {
		t.Fatalf("jev-none = %v, the excluded health message must not leak", byID["jev-none"])
	}
}

// Chain-step and provider-state snapshots surface cooldown and failure
// timestamps once the provider state records enough failures to trip
// the cooldown threshold (3 by default).
func TestWave4AdminSnapshotChainCooldownState(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	cfg.DecisionChains = []config.DecisionChainConfig{{ID: "c1", Steps: []config.DecisionChainStep{{Provider: "policy", TimeoutMS: 100}}}}
	s := testGateway(t, cfg)
	for i := 0; i < 3; i++ {
		s.decisionOrchestrator.ProviderState().RecordFailure("policy")
	}
	rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := coverageAdminJSON(t, rr)
	decision := out["decision"].(map[string]any)
	chains := decision["chains"].([]any)
	if len(chains) != 1 {
		t.Fatalf("chains = %v, want 1", chains)
	}
	steps := chains[0].(map[string]any)["steps"].([]any)
	step := steps[0].(map[string]any)
	if step["state"] != "cooldown" || step["cooldown_active"] != true || step["consecutive_failures"] != float64(3) {
		t.Fatalf("chain step = %v, want cooldown/true/3", step)
	}
	if _, ok := step["cooldown_until"]; !ok {
		t.Fatalf("chain step = %v, want cooldown_until", step)
	}
	if _, ok := step["last_failure"]; !ok {
		t.Fatalf("chain step = %v, want last_failure", step)
	}
	state := decision["provider_state"].(map[string]any)
	pol, ok := state["policy"].(map[string]any)
	if !ok {
		t.Fatalf("provider_state = %v, want a policy entry", state)
	}
	if _, ok := pol["cooldown_until"]; !ok {
		t.Fatalf("policy state = %v, want cooldown_until", pol)
	}
	if _, ok := pol["last_failure"]; !ok {
		t.Fatalf("policy state = %v, want last_failure", pol)
	}
}

func TestWave4AdminCompatReset(t *testing.T) {
	s := wave4AdminGateway(t)
	// GET is denied by the middleware as an unmapped method+path pair,
	// so the handler's own 405 is reached by a direct call.
	{
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/compat/reset", nil)
		rr := httptest.NewRecorder()
		s.adminCompatReset(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET reset status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/compat/reset", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/compat/reset", `{"deployment":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty reset status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := coverageAdminJSON(t, rr)
	if out["ok"] != true || out["reset"] != "all" {
		t.Fatalf("empty reset = %v, want ok/all", out)
	}
	rr = coverageAdminRequest(s, http.MethodPost, "/admin/api/compat/reset", `{"deployment":"all"}`)
	out = coverageAdminJSON(t, rr)
	if out["ok"] != true || out["reset"] != "all" {
		t.Fatalf("all reset = %v, want ok/all", out)
	}
	rr = coverageAdminRequest(s, http.MethodPost, "/admin/api/compat/reset", `{"deployment":"p/a"}`)
	out = coverageAdminJSON(t, rr)
	if out["ok"] != true || out["reset"] != "p/a" {
		t.Fatalf("single reset = %v, want ok/p/a", out)
	}
}
