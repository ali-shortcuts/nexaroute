package httpapi

import (
	"strings"
	"testing"
)

// M2-2: the Access page is wired into the embedded SPA shell.
func TestAccessPageAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `data-page="access"`) {
		t.Fatal("index.html has no Access nav entry")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"access: accessView",
		"access: ['Access'",
		"function accessView()",
		"function bindAccess()",
		"key-project",
		"key-team",
		"allowed_models",
		"allowed_routes",
		"require_privacy",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".access-table") {
		t.Fatal("styles.css missing .access-table rules")
	}
}
