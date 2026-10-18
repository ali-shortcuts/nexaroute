package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// Phase 7 remainder (#399): the unified model inventory, the explicit status
// taxonomy, the per-request decision timeline and the guided first-route
// wizard. These contracts pin that every surface is grounded in real backend
// data (never an invented client-side guess) and that the new body copy stays
// translated across EN/FA/PS.

// TestPhase7RemainderModelInventorySummary proves the Admin API exposes the
// additive, non-secret per-model metadata the model inventory page needs, and
// that the legacy models []string contract is preserved.
func TestPhase7RemainderModelInventorySummary(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "acme", Name: "Acme", Type: "openai_compatible", BaseURL: "https://example.test/v1",
		AuthMode: "bearer", APIKey: "super-secret-credential", Enabled: true, ModelsPath: "/models",
		Models: []config.ModelConfig{{
			ID: "fast", Model: "acme/fast-1", Aliases: []string{"fast", "turbo"}, Enabled: true,
			Weight: 3, Priority: 2, ContextWindow: 128000, InputCostPerMTok: 1.25, OutputCostPerMTok: 5,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true},
		}},
	}}
	s := testGateway(t, cfg)

	rr := adminDo(t, s, http.MethodGet, "/admin/api/providers", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("providers status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "super-secret-credential") {
		t.Fatal("provider summary leaked a raw credential")
	}
	out := decodeJSON(t, rr)
	providers, _ := out["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("want 1 provider, got %d", len(providers))
	}
	p, _ := providers[0].(map[string]any)

	// Legacy contract stays: models is still a []string of model names.
	legacy, ok := p["models"].([]any)
	if !ok || len(legacy) != 1 || legacy[0] != "acme/fast-1" {
		t.Fatalf("legacy models []string contract broken: %v", p["models"])
	}
	if p["discoverable"] != true {
		t.Errorf("discoverable must reflect a configured ModelsPath, got %v", p["discoverable"])
	}

	meta, ok := p["models_meta"].([]any)
	if !ok || len(meta) != 1 {
		t.Fatalf("models_meta missing/empty: %v", p["models_meta"])
	}
	m, _ := meta[0].(map[string]any)
	for field, want := range map[string]any{
		"id": "fast", "model": "acme/fast-1", "enabled": true,
		"priority": float64(2), "weight": float64(3), "context_window": float64(128000),
		"input_cost_per_mtok": float64(1.25), "output_cost_per_mtok": float64(5),
	} {
		if got := m[field]; got != want {
			t.Errorf("models_meta[%q]=%v, want %v", field, got, want)
		}
	}
	aliases, _ := m["aliases"].([]any)
	if len(aliases) != 2 || aliases[0] != "fast" || aliases[1] != "turbo" {
		t.Errorf("models_meta aliases=%v, want [fast turbo]", m["aliases"])
	}
	caps, _ := m["capabilities"].(map[string]any)
	if caps["streaming"] != true || caps["tools"] != true || caps["vision"] != true {
		t.Errorf("models_meta capabilities=%v, want streaming/tools/vision", m["capabilities"])
	}
}

// TestPhase7RemainderModelInventoryWiring proves the inventory page reads the
// provider-qualified metadata and the deployment health keyed by the same
// provider/model id the router uses, and renders through the bounded window.
func TestPhase7RemainderModelInventoryWiring(t *testing.T) {
	js := readAppJS(t)

	rows := extractFunctionLine(t, js, "modelInventoryRows")
	for _, want := range []string{"state.providers", "models_meta", "deploymentHealthFor(p.id, m.id || m.model)"} {
		if !strings.Contains(rows, want) {
			t.Errorf("modelInventoryRows does not consume %q", want)
		}
	}

	row := extractFunctionLine(t, js, "modelInventoryRow")
	for _, want := range []string{"m.aliases", "m.capabilities", "p.discoverable", "modelStatusTaxonomy", "r.health"} {
		if !strings.Contains(row, want) {
			t.Errorf("modelInventoryRow does not surface %q", want)
		}
	}

	view := extractFunctionLine(t, js, "modelsView")
	for _, want := range []string{"modelInventoryRows", "boundedWindow", "state.modelQuery", "mi_bounded"} {
		if !strings.Contains(view, want) {
			t.Errorf("modelsView does not use %q", want)
		}
	}

	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `data-page="models"`) {
		t.Error("index.html must expose the model inventory nav entry")
	}
	if !strings.Contains(js, "models: modelsView") || !strings.Contains(js, "bindModels()") {
		t.Error("app.js must register and bind the models view")
	}
}

// TestPhase7RemainderStatusTaxonomy proves the UI maps every backend health
// status (never inferring health from reachability) and ships translated
// taxonomy labels in all three locales.
func TestPhase7RemainderStatusTaxonomy(t *testing.T) {
	js := readAppJS(t)

	provider := extractFunctionLine(t, js, "providerStatusTaxonomy")
	model := extractFunctionLine(t, js, "modelStatusTaxonomy")
	for _, status := range []string{"healthy", "degraded", "half_open", "cooldown", "retired"} {
		if !strings.Contains(provider, "'"+status+"'") {
			t.Errorf("providerStatusTaxonomy does not handle backend status %q", status)
		}
		if !strings.Contains(model, "'"+status+"'") {
			t.Errorf("modelStatusTaxonomy does not handle backend status %q", status)
		}
	}
	// Disabled and never-probed states must stay distinct from healthy.
	for _, want := range []string{"st_disabled", "st_unverified", "st_tested", "st_configured"} {
		if !strings.Contains(provider, want) {
			t.Errorf("providerStatusTaxonomy missing taxonomy bucket %q", want)
		}
	}
	if !strings.Contains(model, "st_discovered") {
		t.Error("modelStatusTaxonomy must distinguish discovered from probed models")
	}

	legend := extractFunctionLine(t, js, "statusTaxonomyLegend")
	if !strings.Contains(legend, "taxonomyKind") {
		t.Error("statusTaxonomyLegend must colour buckets through taxonomyKind")
	}

	block := extractBraceBlock(t, js, "const BODY_I18N = {")
	locales := map[string]string{}
	for _, locale := range []string{"en", "fa", "ps"} {
		locales[locale] = extractBraceBlock(t, block, locale+": {")
	}
	for _, key := range []string{
		"st_configured", "st_discovered", "st_tested", "st_healthy", "st_degraded",
		"st_cooldown", "st_unsupported", "st_unverified", "st_disabled",
		"st_legend_title", "st_legend_desc",
		"mi_eyebrow", "mi_desc", "mi_col_status", "mi_col_health", "mi_bounded", "mi_more",
		"mi_probe", "mi_no_probe",
		"req_title", "req_desc", "req_selected", "req_action", "req_chain", "req_outcome",
		"req_calls", "req_candidates", "req_confidence", "req_retry", "req_event", "req_bounded",
		"wz_title", "wz_desc", "wz_provider", "wz_alias", "wz_mode", "wz_models", "wz_cost",
		"wz_cost_hint", "wz_cost_unpriced", "wz_cost_free", "wz_need_provider", "wz_need_models",
		"wz_need_alias", "wz_no_models", "wz_start", "wz_create",
	} {
		for locale, sec := range locales {
			if !strings.Contains(sec, "'"+key+"':") {
				t.Errorf("BODY_I18N.%s missing Phase 7 remainder key %q", locale, key)
			}
		}
	}
}

// TestPhase7RemainderRequestTimeline proves the decision timeline consumes the
// exact event fields the backend emits, and that those fields really survive a
// bus round-trip as JSON with the names the UI reads.
func TestPhase7RemainderRequestTimeline(t *testing.T) {
	js := readAppJS(t)

	timeline := extractFunctionLine(t, js, "requestDecisionTimeline")
	for _, want := range []string{"state.snap.events", "request_id", "requestDecisionRow", "boundedWindow"} {
		if !strings.Contains(timeline, want) {
			t.Errorf("requestDecisionTimeline does not consume %q", want)
		}
	}
	row := extractFunctionLine(t, js, "requestDecisionRow")
	for _, want := range []string{
		"decision_selected_provider", "decision_selected", "decision_action",
		"decision_chain_id", "decision_chain_outcome", "decision_chain_calls",
		"decision_candidate_count", "decision_confidence",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("requestDecisionRow does not consume event field %q", want)
		}
	}
	if !strings.Contains(extractFunctionLine(t, js, "activityView"), "requestDecisionTimeline()") {
		t.Error("activityView must render the per-request decision timeline")
	}

	// Real bus round-trip: the UI field names must match the JSON the emitter
	// produces, or the timeline would silently render empty.
	b := events.New(4)
	b.Add(events.Event{
		Kind:                     "request_failover",
		RequestID:                "req-1",
		DecisionSelectedProvider: "acme/fast",
		DecisionAction:           "failover",
		DecisionChainID:          "chain-1",
		DecisionChainOutcome:     "recovered",
		DecisionChainCalls:       2,
		DecisionCandidateCount:   3,
		DecisionConfidence:       0.91,
	})
	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 event, got %d", len(snap))
	}
	raw, err := json.Marshal(snap[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"request_id", "decision_selected_provider", "decision_action",
		"decision_chain_id", "decision_chain_outcome", "decision_chain_calls",
		"decision_candidate_count", "decision_confidence",
	} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("emitted event JSON missing field %q the timeline reads", field)
		}
	}
}

// TestPhase7RemainderWizard proves the guided first-route wizard creates a real
// simple route, requires a public alias and at least one model, and shows a
// cost warning derived from declared model pricing before saving.
func TestPhase7RemainderWizard(t *testing.T) {
	js := readAppJS(t)

	wiz := extractFunctionLine(t, js, "openFirstRouteWizard")
	for _, want := range []string{
		"/admin/api/simple-routes", "firstRouteCostWarning", "wz-provider",
		"wz-alias", "wz-mode", "wz-model-list", "wz-cost", "wz_need_alias", "wz_need_models",
	} {
		if !strings.Contains(wiz, want) {
			t.Errorf("openFirstRouteWizard does not use %q", want)
		}
	}

	cost := extractFunctionLine(t, js, "firstRouteCostWarning")
	for _, want := range []string{
		"input_cost_per_mtok", "output_cost_per_mtok", "wz_cost_unpriced", "wz_cost_free",
	} {
		if !strings.Contains(cost, want) {
			t.Errorf("firstRouteCostWarning does not read %q", want)
		}
	}

	cands := extractFunctionLine(t, js, "firstRouteCandidates")
	if !strings.Contains(cands, "p.models_meta") || !strings.Contains(cands, "enabled") {
		t.Error("firstRouteCandidates must offer only enabled models from provider metadata")
	}

	if !strings.Contains(extractFunctionLine(t, js, "routingView"), `data-action="first-route-wizard"`) {
		t.Error("routingView must launch the guided first-route wizard")
	}
	if !strings.Contains(extractFunctionLine(t, js, "paletteCommands"), "openFirstRouteWizard") {
		t.Error("the command palette must expose the guided first-route wizard")
	}
}

// TestPhase7RemainderBoundedRendering proves large lists render through the
// shared bounded window with an explicit "show more" affordance, so an
// unbounded provider fleet cannot grow the DOM without limit.
func TestPhase7RemainderBoundedRendering(t *testing.T) {
	js := readAppJS(t)

	for _, fn := range []string{"boundedWindow", "nextWindow"} {
		if !strings.Contains(js, "function "+fn+"(") {
			t.Errorf("app.js missing bounded list helper %s", fn)
		}
	}
	if !strings.Contains(extractFunctionLine(t, js, "modelsView"), "boundedWindow('models'") {
		t.Error("modelsView must render the inventory through the bounded window")
	}
	if !strings.Contains(extractFunctionLine(t, js, "requestDecisionTimeline"), "boundedWindow('requests'") {
		t.Error("requestDecisionTimeline must render through the bounded window")
	}
	for _, action := range []string{"'show-more-models'", "'show-more-requests'"} {
		if !strings.Contains(js, action) {
			t.Errorf("app.js missing %s binding", action)
		}
	}
}
