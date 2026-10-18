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
		"ui-fail-1",                      // generation-failure job fixture
		"Retry risks duplicate billing",  // retry-safety verdict assertion
		"provider rejected the brief",    // error surfacing assertion
		"{not-json",                      // malformed payload fixture
		"ui-recover-1",                   // restart-recovery job fixture
		"job resubmitted after reload",   // no-resubmission assertion
	} {
		if !strings.Contains(text, want) {
			t.Errorf("browser E2E script missing item-9 scenario anchor %q", want)
		}
	}
}
