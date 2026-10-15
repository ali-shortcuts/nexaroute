package httpapi

import (
	"strings"
	"testing"
)

// M2-12: EN/FA/PS chrome localization, RTL switching, persistence.
func TestI18nAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="lang-switch"`, `value="fa"`, `value="ps"`, `id="brand-sub"`, `id="skip-link"`} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"nexaroute_lang",
		"META_I18N",
		"function applyLang()",
		"document.documentElement.dir",
		"'نمای کلی'",
		"'مسیریابی'",
		"'کتنه'",
		"'روټینګ'",
		"pageMeta(state.page)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	// Every page in the English meta table needs fa + ps entries.
	for _, page := range []string{"overview", "providers", "routing", "video", "activity", "settings"} {
		if !strings.Contains(js, page+": ['") {
			t.Errorf("no localized meta entry for page %q", page)
		}
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), `[dir="rtl"] .sidebar`) || !strings.Contains(string(css), ".lang-switch") {
		t.Error("styles.css missing RTL shell rules or .lang-switch")
	}
}
