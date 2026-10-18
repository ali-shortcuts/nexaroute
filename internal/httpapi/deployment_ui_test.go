package httpapi

import (
	"strings"
	"testing"
)

// #434 slice 2: the Overview page renders the deployment report
// from the admin snapshot — the same report /api/hello serves —
// so per-instance state is labeled per-instance where operators
// look, never implied global.
func TestDeploymentPanelWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"function deploymentPanel()",
		"state.snap.deployment_report",
		"${deploymentPanel()}", // mounted on the Overview page
		"bt('dp_title')",
		"'dp_title': 'Deployment & state scope'",
		"'dp_title': 'استقرار و دامنه وضعیت'",
		"video_jobs_ledger",  // the per-subsystem scopes render from the report
		"quota_reservations", // per-instance reservations are labeled, not hidden
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing deployment panel wiring %q", want)
		}
	}
}
