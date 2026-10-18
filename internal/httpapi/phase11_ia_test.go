package httpapi

import (
	"strings"
	"testing"
)

// Phase 11 (#411) information-architecture regression contracts. The default
// UI must stay calm: primary actions are always visible, rare controls live
// behind an explicit Advanced disclosure, Settings is grouped into tabs, and
// the primary navigation is organized into labeled groups. These tests pin the
// structural decisions so a later edit cannot silently regress them.

// TestPhase11SettingsTabsContract pins the tabbed Settings surface: the view
// renders a tablist plus one panel per group, the tab state is stored on the
// shared state object, and the panels are wired on render.
func TestPhase11SettingsTabsContract(t *testing.T) {
	js := readAppJS(t)

	view := extractFunctionLine(t, js, "settingsView")
	for _, want := range []string{
		`class="settings-tabs"`,
		`role="tablist"`,
		"settingsTabButtons(tab)",
		"settingsTabPanel('gateway'",
		"settingsTabPanel('routing'",
		"settingsTabPanel('security'",
		"settingsTabPanel('data'",
		"settingsTabPanel('expert'",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("settingsView missing %q", want)
		}
	}

	for _, want := range []string{
		"settingsTab: 'gateway'",
		`data-settings-tab="${k}"`,
		"function bindSettings()",
		"state.settingsTab = b.dataset.settingsTab",
		"if (state.page === 'settings') bindSettings();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing settings-tab wiring %q", want)
		}
	}
}

// TestPhase11SettingsAdvancedControlsRead pins that the rare routing/probe
// controls disclosed under Settings are still consumed by save-settings, so
// progressive disclosure never creates a dead control.
func TestPhase11SettingsAdvancedControlsRead(t *testing.T) {
	js := readAppJS(t)

	saveStart := strings.Index(js, "'save-settings': async")
	if saveStart < 0 {
		t.Fatal("save-settings handler not found")
	}
	saveEnd := strings.Index(js[saveStart:], "function openConfirm")
	if saveEnd < 0 {
		t.Fatal("save-settings handler end not found")
	}
	save := js[saveStart : saveStart+saveEnd]

	for _, id := range []string{
		"setting-request-timeout",
		"setting-retry-backoff",
		"setting-cooldown",
		"setting-probe-interval",
		"setting-probe-timeout",
	} {
		if !strings.Contains(save, "$('#"+id+"')") {
			t.Errorf("advanced Settings control #%s is rendered but never read by save-settings", id)
		}
	}
}

// TestPhase11ProviderProgressiveDisclosure pins which provider controls stay in
// the default view and which rare controls move behind the Advanced detail. The
// browser e2e suite fills the primary fields, so they must remain in the main
// flow; the auth-mode/concurrency/pool controls can be disclosed.
func TestPhase11ProviderProgressiveDisclosure(t *testing.T) {
	js := readAppJS(t)
	src := extractFunctionLine(t, js, "providerForm")

	adv := strings.Index(src, `<div class="expert-box">`)
	if adv < 0 {
		t.Fatal("providerForm has no Advanced disclosure")
	}
	if strings.Count(src, "<details>") != 1 {
		t.Errorf("providerForm should have exactly one Advanced <details>, got %d", strings.Count(src, "<details>"))
	}
	if !strings.Contains(src, "bt('prov_adv_title')") {
		t.Error("providerForm Advanced summary is not localized through bt('prov_adv_title')")
	}

	primary := []string{"p-name", "p-base", "p-type", "p-key", "p-clear-secret", "p-trains", "p-retention", "p-dh-note"}
	for _, id := range primary {
		pos := strings.Index(src, `id="`+id+`"`)
		if pos < 0 {
			t.Errorf("providerForm missing primary control #%s", id)
			continue
		}
		if pos > adv {
			t.Errorf("primary control #%s moved behind Advanced disclosure", id)
		}
	}

	for _, id := range []string{"p-auth", "p-max-concurrency", "p-edit-pool", "credential-pool-list"} {
		pos := strings.Index(src, `id="`+id+`"`)
		if pos < 0 {
			t.Errorf("providerForm missing rare control #%s", id)
			continue
		}
		if pos < adv {
			t.Errorf("rare control #%s should be disclosed under Advanced", id)
		}
	}
}

// TestPhase11OverviewMetricLinks pins that the Overview metric cards became
// navigation affordances (buttons with data-go) instead of inert stat tiles.
func TestPhase11OverviewMetricLinks(t *testing.T) {
	js := readAppJS(t)
	src := extractFunctionLine(t, js, "overviewView")

	if strings.Contains(src, `<article class="metric-card`) {
		t.Error("overview metric cards are still inert <article> tiles")
	}
	if got := strings.Count(src, `<button type="button" class="metric-card`); got != 4 {
		t.Errorf("expected 4 navigable metric cards, got %d", got)
	}
	for _, want := range []string{`data-go="activity"`, `data-go="observability"`, `data-go="routing"`} {
		if !strings.Contains(src, want) {
			t.Errorf("overview metric cards missing %q", want)
		}
	}
}

// TestPhase11NavGroupsContract pins the labeled primary-navigation groups and
// their EN/FA/PS chrome entries.
func TestPhase11NavGroupsContract(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-navgroup="operate"`, `data-navgroup="use"`, `data-navgroup="govern"`} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html missing nav group %q", want)
		}
	}

	js := readAppJS(t)
	if !strings.Contains(js, "[data-navgroup]") {
		t.Error("applyLang() does not localize navigation group labels")
	}
	block := extractBraceBlock(t, js, "const I18N = {")
	for _, locale := range []string{"en", "fa", "ps"} {
		sec := extractBraceBlock(t, block, locale+": {")
		for _, key := range []string{"navOperate", "navUse", "navGovern"} {
			if !strings.Contains(sec, key) {
				t.Errorf("I18N.%s missing %s", locale, key)
			}
		}
	}
}

func readAppJS(t *testing.T) string {
	t.Helper()
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(app)
}
