package httpapi

import (
	"strings"
	"testing"
)

// M2-6: the Audit page is wired into the embedded SPA shell.
func TestAuditPageAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `data-page="audit"`) {
		t.Fatal("index.html has no Audit nav entry")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"audit: auditView",
		"function auditView()",
		"function loadAudit()",
		"function auditLinkage()",
		"/admin/api/audit?limit=",
		"export-audit-json",
		"export-audit-csv",
		"nexaroute audit verify",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".audit-table") {
		t.Fatal("styles.css missing .audit-table rules")
	}
}
