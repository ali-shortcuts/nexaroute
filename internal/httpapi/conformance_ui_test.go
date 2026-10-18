package httpapi

import (
	"strings"
	"testing"
)

// #436 slice 2: the Providers page renders the conformance panel
// from the snapshot's manifest — the same evidence source as
// docs/COMPATIBILITY.md — with effective (staleness-applied)
// states and per-dimension claims.
func TestConformancePanelWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"function conformancePanel()",
		"state.snap.conformance",
		"${conformancePanel()}", // mounted on the Providers page
		"effective_status",      // staleness-applied state is what renders
		"native passthrough",    // native vs translation honesty
		"bt('cf_title')",
		"'cf_title': 'Provider conformance'",
		"'cf_title': 'انطباق ارائه‌دهنده'",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing conformance panel wiring %q", want)
		}
	}
}
