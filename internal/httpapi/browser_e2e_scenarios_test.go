package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #414 item 9: the CI browser E2E must keep its Video Studio
// failure scenarios — a generation failure surfacing its error
// and retry-safety verdict, a malformed providers payload
// degrading inline, and restart recovery without resubmission.
// If a scenario is dropped from the script, this fails.
func TestBrowserE2EVideoFailureScenarios(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "test-browser-e2e.py"))
	if err != nil {
		t.Fatalf("read scripts/test-browser-e2e.py: %v", err)
	}
	text := string(script)
	for _, want := range []string{
		"ui-fail-1",                     // generation-failure job fixture
		"Retry risks duplicate billing", // retry-safety verdict assertion
		"provider rejected the brief",   // error surfacing assertion
		"{not-json",                     // malformed payload fixture
		"ui-recover-1",                  // restart-recovery job fixture
		"job resubmitted after reload",  // no-resubmission assertion
	} {
		if !strings.Contains(text, want) {
			t.Errorf("browser E2E script missing item-9 scenario anchor %q", want)
		}
	}
}

// #413 item 3: the CI browser E2E's core flows stay pinned —
// provider CRUD with preservation, model detection, route
// creation, the Activity empty state, evidence screenshots
// and the suite's own pass marker. If a flow is dropped from
// the script, this fails in the unit stage, long before the
// browser job would be missed.
func TestBrowserE2ECoreFlows(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "test-browser-e2e.py"))
	if err != nil {
		t.Fatalf("read scripts/test-browser-e2e.py: %v", err)
	}
	text := string(script)
	for _, want := range []string{
		"PRIVACY P3 TESTS",             // provider privacy + preservation section
		"Edit provider",                // edit flow opens with stored values
		"detect-models",                // model discovery action
		"Create route",                 // route creation flow
		"No real activity yet",         // first-run Activity empty state
		"providers-1440x900-dark.png",  // evidence screenshot (dark)
		"providers-1440x900-light.png", // evidence screenshot (light)
		"BROWSER E2E PASS",             // the suite's terminal pass marker
	} {
		if !strings.Contains(text, want) {
			t.Errorf("browser E2E script missing core-flow anchor %q", want)
		}
	}
}
