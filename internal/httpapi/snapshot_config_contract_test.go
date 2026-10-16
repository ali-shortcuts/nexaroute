package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// TestAdminSnapshotConfigContract pins the snapshot config sections the
// admin UI reads (issue #429, audit #371 UI-F1..F4): policiesView reads
// config.guardrails and config.decision_policies; the Playground reads
// config.providers for per-model prices (models[].id +
// input/output_cost_per_mtok) and for data_handling.trains_on_data
// exclusions. Before the fix the snapshot emitted only
// probe/routing/decision, so every one of those panels silently rendered
// empty/off/unpriced. The contract also pins secret hygiene: no provider
// credential may appear anywhere in the snapshot body.
func TestAdminSnapshotConfigContract(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Guardrails = config.GuardrailConfig{
		Mode: "block", RejectPII: true, RejectSecrets: true, MaxBodyBytes: 12345,
	}
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{{
		ID: "pol1", Name: "Balanced", SelectionMode: "select_first",
		MinScoreDelta: 0.1,
		Weights:       config.DecisionPolicyWeights{Cost: 0.5, Latency: 0.5},
	}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P One", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:1", AuthMode: "bearer", Enabled: true,
		APIKey:       "sk-contract-secret-must-not-leak",
		DataHandling: config.DataHandlingConfig{TrainsOnData: "no", Retention: "none"},
		Models: []config.ModelConfig{
			{ID: "dm", Model: "real-model", Enabled: true, Weight: 1, InputCostPerMTok: 3, OutputCostPerMTok: 15},
		},
	}}
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d", rr.Code)
	}
	raw := rr.Body.String()
	if strings.Contains(raw, "sk-contract-secret-must-not-leak") {
		t.Fatal("snapshot leaked a provider API key")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	cfgMap, _ := body["config"].(map[string]any)
	if cfgMap == nil {
		t.Fatalf("snapshot has no config object: keys=%v", keysOf(body))
	}
	// UI-F1: guardrails must be present with the configured mode/flags.
	g, _ := cfgMap["guardrails"].(map[string]any)
	if g == nil {
		t.Fatalf("config.guardrails missing (UI-F1): config keys=%v", keysOf(cfgMap))
	}
	if g["mode"] != "block" || g["reject_pii"] != true || g["reject_secrets"] != true {
		t.Fatalf("guardrails serialized wrong: %v", g)
	}
	if n, _ := g["max_body_bytes"].(float64); n != 12345 {
		t.Fatalf("guardrails max_body_bytes=%v want 12345", g["max_body_bytes"])
	}
	// UI-F2: decision policies must be listed with UI-read fields.
	pols, _ := cfgMap["decision_policies"].([]any)
	if len(pols) != 1 {
		t.Fatalf("config.decision_policies=%v want 1 entry (UI-F2)", cfgMap["decision_policies"])
	}
	p0, _ := pols[0].(map[string]any)
	if p0["id"] != "pol1" || p0["selection_mode"] != "select_first" {
		t.Fatalf("decision policy serialized wrong: %v", p0)
	}
	w, _ := p0["weights"].(map[string]any)
	if w == nil || w["cost"] != 0.5 || w["latency"] != 0.5 {
		t.Fatalf("decision policy weights wrong: %v", p0["weights"])
	}
	// UI-F3/F4: providers with model ids + costs and data_handling.
	provs, _ := cfgMap["providers"].([]any)
	if len(provs) != 1 {
		t.Fatalf("config.providers=%v want 1 entry (UI-F3/F4)", cfgMap["providers"])
	}
	pr, _ := provs[0].(map[string]any)
	if pr["id"] != "p1" {
		t.Fatalf("provider id wrong: %v", pr)
	}
	dh, _ := pr["data_handling"].(map[string]any)
	if dh == nil || dh["trains_on_data"] != "no" {
		t.Fatalf("provider data_handling wrong (UI-F4): %v", pr["data_handling"])
	}
	models, _ := pr["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("provider models=%v want 1 entry (UI-F3)", pr["models"])
	}
	m0, _ := models[0].(map[string]any)
	if m0["id"] != "real-model" {
		t.Fatalf("model id=%v want real-model (Playground matches on id)", m0["id"])
	}
	if m0["input_cost_per_mtok"] != float64(3) || m0["output_cost_per_mtok"] != float64(15) {
		t.Fatalf("model costs wrong (UI-F3): %v", m0)
	}
}
