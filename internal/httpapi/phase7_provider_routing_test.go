package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Phase 7 (#399) provider and routing control-plane contracts. These pin the
// transparent-control-plane decisions: the routing page must explain
// eligibility from real backend snapshot data, provider cards must surface
// non-secret connection facts, and every backend exclusion bucket must have a
// UI label so a new bucket can never render as an unexplained raw key.

// TestPhase7RoutingEligibilityPanelWiring proves the routing page renders a
// decision/eligibility panel that reads the backend route_eligibility and
// eligibility_warnings snapshot fields (and never a client-only guess).
func TestPhase7RoutingEligibilityPanelWiring(t *testing.T) {
	js := readAppJS(t)

	view := extractFunctionLine(t, js, "routingView")
	if !strings.Contains(view, "routeEligibilityPanel()") {
		t.Error("routingView must render the routeEligibilityPanel() decision view")
	}

	panel := extractFunctionLine(t, js, "routeEligibilityPanel")
	for _, want := range []string{
		"state.snap.route_eligibility",
		"state.snap.eligibility_warnings",
		"p.eligible_now",
		"p.excluded",
		"p.candidates",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("routeEligibilityPanel does not consume %q", want)
		}
	}
	// The panel must not claim a healthy state it cannot see: it derives the
	// status label from the backend-provided per-profile state only.
	if !strings.Contains(panel, "stateLabel[p.state]") {
		t.Error("routeEligibilityPanel must map the backend profile state, not invent one")
	}
}

// TestPhase7ProviderCardSurfacesRealState proves the provider card shows
// non-secret connection facts from the Admin API summary plus provider-level
// health evidence when the backend reports it.
func TestPhase7ProviderCardSurfacesRealState(t *testing.T) {
	js := readAppJS(t)
	card := extractFunctionLine(t, js, "providerCard")

	for _, want := range []string{
		"p.auth_mode",
		"p.credential_count",
		"state.snap.provider_health",
		"health.last_error",
		"health.last_success",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("providerCard does not surface real backend field %q", want)
		}
	}
}

// TestPhase7EligibilityExclusionBucketsHaveLabels cross-checks the live
// backend exclusion buckets against the UI label mapping. Adding a new
// backend bucket without a matching UI reason is a silent truthfulness bug:
// the panel would render nothing for that exclusion.
func TestPhase7EligibilityExclusionBucketsHaveLabels(t *testing.T) {
	s := testGateway(t, eligTestConfig())
	snap := getSnapshot(t, s)

	re, ok := snap["route_eligibility"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot missing route_eligibility: %v", snap["route_eligibility"])
	}
	profiles, _ := re["profiles"].([]any)
	if len(profiles) == 0 {
		t.Fatal("route_eligibility has no profiles to check")
	}
	row, _ := profiles[0].(map[string]any)
	excluded, ok := row["excluded"].(map[string]any)
	if !ok || len(excluded) == 0 {
		t.Fatalf("profile row has no exclusion buckets: %v", row)
	}

	js := readAppJS(t)
	for key := range excluded {
		if !strings.Contains(js, "'"+key+"'") {
			t.Errorf("backend exclusion bucket %q has no UI label mapping in app.js", key)
		}
	}
}

// TestPhase7ProviderCardCouldNotEraseCredentialByOmission is a backend
// contract companion for acceptance criterion 2: a save that omits the
// credential preserves it. The UI sends preserve_secret when the editor did
// not touch secret fields; this asserts the server-side behavior the UI relies
// on so a regression cannot silently start erasing saved credentials.
func TestPhase7ProviderCardCouldNotEraseCredentialByOmission(t *testing.T) {
	const key = "P7_PRESERVE_SENTINEL_2f9a"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p7", Name: "P7", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1",
		APIKey: key, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	// Round-trip the exact shape the editor sends when nothing secret changed:
	// provider fields present, no api_key, preserve_secret=true.
	body := `{"provider":{"id":"p7","name":"P7 renamed","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m","model":"m","enabled":true,"weight":1,"capabilities":{}}]},"preserve_secret":true,"preserve_headers":true,"preserve_proxy":true}`
	rr := adminDo(t, s, http.MethodPut, "/admin/api/providers/p7", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), key) {
		t.Fatalf("save response leaked preserved credential: %s", rr.Body.String())
	}
	if got := s.currentConfig().Providers[0].APIKey; got != key {
		t.Fatalf("preserve_secret save erased credential by omission: got %q", got)
	}
}

// TestPhase7ProviderSummaryExposesNonSecretControlFields pins the Admin API
// contract the provider card depends on: auth mode, model count and credential
// count are exposed, while no secret value is.
func TestPhase7ProviderSummaryExposesNonSecretControlFields(t *testing.T) {
	const key = "P7_SUMMARY_SENTINEL_7c1e"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p7", Name: "P7", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1",
		APIKey: key, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "model-seven", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	rr := adminDo(t, s, http.MethodGet, "/admin/api/providers", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("providers status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), key) {
		t.Fatalf("providers list leaked credential: %s", rr.Body.String())
	}
	var out struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("expected 1 provider summary, got %d", len(out.Providers))
	}
	for _, field := range []string{"auth_mode", "model_count", "credential_count"} {
		if _, ok := out.Providers[0][field]; !ok {
			t.Errorf("provider summary missing non-secret control field %q", field)
		}
	}
}
