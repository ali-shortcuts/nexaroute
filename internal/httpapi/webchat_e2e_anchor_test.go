package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #417 item 8: the WebChat browser-E2E scenario lives in
// scripts/test-browser-e2e.py and runs in the CI verify job's
// real Playwright browser (the local sandbox cannot run it —
// see #502). This anchor keeps the scenario from silently
// disappearing: the failover-badge flow, the session pin and
// the honest reload semantics must all stay in the script.
func TestBrowserE2EWebChatFlow(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "test-browser-e2e.py"))
	if err != nil {
		t.Fatalf("read scripts/test-browser-e2e.py: %v", err)
	}
	s := string(script)
	for _, want := range []string{
		"def mock_chat(route)",
		`"**/v1/chat/completions"`,
		"fakeprov-a/m>fakeprov-b/m", // ordered failover trail in the mocked headers
		"attempts fakeprov-a/m → fakeprov-b/m",
		"fakeprov-b / fake-model · fakeprov-b/m", // badge resolves to the answering deployment
		"#pg-session",
		"No request yet.", // honest tab-only history after reload
		"kept only in this tab",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("browser E2E script missing WebChat scenario element %q", want)
		}
	}
}
