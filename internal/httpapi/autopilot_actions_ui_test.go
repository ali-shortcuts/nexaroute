package httpapi

import (
	"strings"
	"testing"
)

// #418: the Autopilot card carries the operator gate actions,
// wired to the real endpoint with the action names the
// handler accepts.
func TestAutopilotActionWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		`data-action="autopilot-act" data-arg="evaluate"`,
		`data-action="autopilot-act" data-arg="promote"`,
		`data-action="autopilot-act" data-arg="rollback"`,
		"'autopilot-act': async e =>",
		"api('/admin/api/autopilot', { method: 'POST', ...body({ action: act }) })",
		"bt('pol_auto_evaluate')",
		"bt('pol_auto_promote')",
		"bt('pol_auto_dorollback')",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing autopilot action wiring %q", want)
		}
	}
}
