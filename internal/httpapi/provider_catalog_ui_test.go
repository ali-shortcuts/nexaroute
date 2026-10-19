package httpapi

import (
	"strings"
	"testing"
)

// Issue #394 UI contracts: the provider page renders a searchable preset
// catalog from the real presets API, onboarding states come from real
// backend counts, and "Test connection" must run real per-model probes —
// never mark a provider healthy from URL reachability alone.

func TestProviderCatalogUIRendersBackendPresets(t *testing.T) {
	js := readAppJS(t)

	view := extractFunctionLine(t, js, "providersView")
	for _, want := range []string{
		"state.presetCatalog",
		"state.presetQuery",
		"state.presetCategory",
		"presetCard",
		"preset-filter",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("providersView does not consume %q", want)
		}
	}
	card := extractFunctionLine(t, js, "presetCard")
	for _, want := range []string{
		"p.status",             // verified/partial visibility
		"p.capabilities",       // real dialect capability labels
		"p.supports_discovery", // discovery support honesty
		"bt('pr_cat_use')",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("presetCard does not surface %q", want)
		}
	}
}

func TestProviderCardSurfacesOnboardingStateCounts(t *testing.T) {
	js := readAppJS(t)
	card := extractFunctionLine(t, js, "providerCard")
	chips := extractFunctionLine(t, js, "modelStateChips")
	if !strings.Contains(card, "p.model_states") {
		t.Error("providerCard must render backend-reported onboarding state counts")
	}
	for _, want := range []string{
		"manual", "discovered", "passed", "degraded", "failed",
	} {
		if !strings.Contains(chips, want) {
			t.Errorf("modelStateChips missing state %q", want)
		}
	}
	if !strings.Contains(chips, "filter(x => st[x[0]] > 0)") {
		t.Error("state chips must never render fabricated zero states")
	}
}

// The test button must ask the backend for a real per-model generation
// probe (provider-test), show a cost/rate-limit warning first, and guide
// to manual model entry when there is nothing to test. It must not claim
// health from the URL reachability endpoint (provider-check).
func TestProviderTestUsesRealPerModelProbes(t *testing.T) {
	js := readAppJS(t)
	fn := extractFunctionLine(t, js, "testProvider")

	if strings.Contains(fn, "provider-check") {
		t.Error("testProvider must not mark health from URL reachability (provider-check)")
	}
	if !strings.Contains(fn, "/admin/api/provider-test") {
		t.Error("testProvider must run per-model probes through provider-test")
	}
	if !strings.Contains(fn, "rate limits or incur") {
		t.Error("testProvider must warn about cost and rate limits before testing")
	}
	if !strings.Contains(fn, "add a model manually") {
		t.Error("testProvider without models must request manual model entry, not claim health")
	}
	if !strings.Contains(fn, "test_models") {
		t.Error("testProvider must select the tested models explicitly")
	}
}

func TestModelOnboardingProvenanceWireFRoundTrip(t *testing.T) {
	js := readAppJS(t)
	// Discovered rows must be tagged source=discovered so the backend can
	// persist onboarding provenance; manual entries stay manual.
	disc := extractFunctionLine(t, js, "detectModels")
	if !strings.Contains(disc, "source: 'discovered'") {
		t.Error("detectModels must tag discovered models with the discovered source")
	}
	manual := extractFunctionLine(t, js, "readProvider")
	if !strings.Contains(manual, "source: x.dataset.source || 'manual'") {
		t.Error("readProvider must preserve discovered provenance and default manual entries to manual")
	}
	rows := extractFunctionLine(t, js, "modelRow")
	if !strings.Contains(rows, `data-source="${esc(x.source || 'manual')}"`) {
		t.Error("modelRow must carry source provenance into the saved model form")
	}
	if !strings.Contains(manual, "modelTestResults") {
		t.Error("readProvider must carry over per-model test outcomes from provider-test runs")
	}
	if !strings.Contains(js, "modelTestResults[model] = result ?") {
		t.Error("per-model probe outcomes must be collected for the provider form before save")
	}
	if !strings.Contains(js, "save the provider to persist") {
		t.Error("the UI must make clear that probe outcomes persist only when the provider is saved")
	}
}

// The preset-grid rule is appended after the legacy responsive grid rules,
// so it must explicitly restore one column on mobile instead of leaving the
// catalog at three columns (which can overflow a 390px viewport).
func TestProviderPresetGridCollapsesOnMobile(t *testing.T) {
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	body := string(css)
	threeCols := strings.Index(body, ".preset-grid{grid-template-columns:repeat(3,minmax(0,1fr))}")
	mobileOneCol := strings.Index(body, "@media(max-width:700px){.preset-grid{grid-template-columns:1fr}}")
	if threeCols < 0 {
		t.Fatal("preset catalog tablet grid rule not found")
	}
	if mobileOneCol <= threeCols {
		t.Fatal("mobile one-column rule must appear after the three-column catalog rule")
	}
}

func TestProviderAdvancedSettingsCanBeEditedWithoutLeakingSavedSecrets(t *testing.T) {
	js := readAppJS(t)
	form := extractFunctionLine(t, js, "providerForm")
	for _, want := range []string{
		"p-stream-idle-timeout", "stream_idle_timeout_seconds",
		"p-edit-headers", "p-headers", "p-replace-proxy", "p-proxy",
		"bt('pr_headers_edit')", "bt('pr_headers_hint')", "bt('pr_proxy_edit')", "bt('pr_proxy_hint')",
	} {
		if !strings.Contains(form, want) {
			t.Errorf("providerForm is missing safe advanced-setting control %q", want)
		}
	}

	read := extractFunctionLine(t, js, "readProvider")
	for _, want := range []string{
		"JSON.parse", "JSON object with string values",
		"preserve_headers: !editHeaders", "preserve_proxy: !replaceProxy",
		"provider.headers = headers", "provider.proxy_url",
		"stream_idle_timeout_seconds", "#model-list .model-select:checked",
		"data-model-capability",
	} {
		if !strings.Contains(read, want) {
			t.Errorf("readProvider does not persist advanced setting %q", want)
		}
	}

	row := extractFunctionLine(t, js, "modelRow")
	for _, want := range []string{
		"cap('streaming'", "cap('tools'", "cap('vision'", "cap('reasoning'",
		"bt('pr_model_caps')",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("modelRow lacks editable capability %q", want)
		}
	}
}
