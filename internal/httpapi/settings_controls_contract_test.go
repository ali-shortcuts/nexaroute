package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

// TestSettingsControlsContract pins the class of bug from issue #430
// (audit #371 UI-F5/F8): a rendered settings control that its save
// handler never reads is a dead control that silently discards operator
// intent. The Settings "Failover" dropdown was rendered but
// save-settings preserved prevRouting.max_attempts instead of reading
// #setting-failover. This test extracts every setting-* control id
// rendered by settingsView and requires the save-settings handler to
// read each one; it also pins the failover -> max_attempts mapping, the
// removal of the dead select-all-models action, and FA/PS page-title
// parity for every page in the EN meta table.
func TestSettingsControlsContract(t *testing.T) {
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)

	viewStart := strings.Index(js, "function settingsView()")
	if viewStart < 0 {
		t.Fatal("settingsView not found")
	}
	viewEnd := strings.Index(js[viewStart:], "function bindProviders()")
	if viewEnd < 0 {
		t.Fatal("settingsView end not found")
	}
	view := js[viewStart : viewStart+viewEnd]

	saveStart := strings.Index(js, "'save-settings': async")
	if saveStart < 0 {
		t.Fatal("save-settings handler not found")
	}
	saveEnd := strings.Index(js[saveStart:], "function openConfirm")
	if saveEnd < 0 {
		t.Fatal("save-settings handler end not found")
	}
	save := js[saveStart : saveStart+saveEnd]

	idRe := regexp.MustCompile(`id="(setting-[a-z-]+)"`)
	ids := map[string]bool{}
	for _, m := range idRe.FindAllStringSubmatch(view, -1) {
		ids[m[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("no setting-* controls found in settingsView")
	}
	for id := range ids {
		if !strings.Contains(save, "$('#"+id+"')") {
			t.Errorf("dead control: settingsView renders #%s but save-settings never reads it", id)
		}
	}

	// UI-F5: failover must map explicitly onto max_attempts (enabled ->
	// previous value when >1, else the product default 4; disabled -> 1),
	// and the old silent-preserve expression must be gone.
	if !strings.Contains(save, "$('#setting-failover')") {
		t.Error("save-settings does not read #setting-failover (UI-F5)")
	}
	if strings.Contains(save, "max_attempts: Number(prevRouting.max_attempts || 1)") {
		t.Error("save-settings still silently preserves max_attempts (UI-F5/S7)")
	}
	if !strings.Contains(save, ": 1") || !strings.Contains(save, "4") {
		t.Error("failover mapping should produce max_attempts 1 (off) or the default 4 (on)")
	}

	// S8: the select-all-models action had no rendered control; it must
	// not linger as dead code.
	if strings.Contains(js, "select-all-models") {
		t.Error("select-all-models still present (dead action, S8)")
	}

	// UI-F8: every EN page must have a title entry in the FA and PS
	// META_I18N tables (pageMeta falls back to English otherwise).
	metaStart := strings.Index(js, "const META_I18N = {")
	if metaStart < 0 {
		t.Fatal("META_I18N not found")
	}
	metaBlock := js[metaStart : metaStart+strings.Index(js[metaStart:], "};")+2]
	pages := []string{"overview", "providers", "routing", "video", "activity", "playground",
		"policies", "access", "audit", "cost", "evals", "observability", "settings"}
	for _, lang := range []string{"fa", "ps"} {
		langStart := strings.Index(metaBlock, lang+": {")
		if langStart < 0 {
			t.Fatalf("META_I18N.%s not found", lang)
		}
		langBlock := metaBlock[langStart:]
		if lang == "fa" {
			if end := strings.Index(langBlock, "ps: {"); end >= 0 {
				langBlock = langBlock[:end]
			}
		}
		for _, page := range pages {
			if !strings.Contains(langBlock, page+": [") {
				t.Errorf("META_I18N.%s missing page title %q (UI-F8)", lang, page)
			}
		}
	}
}
