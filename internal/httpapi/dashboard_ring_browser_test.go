package httpapi

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// TestDashboardLiveRingB1B2BrowserEvidence renders the real dashboard served
// by a real gateway in headless Chromium and writes the screenshots directly
// to docs/browser-evidence/ from this test command. It then asserts the
// dumped DOM contains the live-ring contracts (no fabricated telemetry: the
// page is served by the real backend; animation hooks are verified present,
// not simulated).
func TestDashboardLiveRingB1B2BrowserEvidence(t *testing.T) {
	chromium, err := exec.LookPath("chromium")
	if err != nil {
		t.Skip("chromium not installed; cannot capture browser evidence")
	}
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	evidenceDir := filepath.Join(repoRoot, "docs", "browser-evidence")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	shots := []struct {
		name string
		size string
	}{
		{"live-ring-b1b2-desktop.png", "1440,900"},
		{"live-ring-b1b2-phone.png", "390,844"},
	}
	for _, shot := range shots {
		out := filepath.Join(evidenceDir, shot.name)
		cmd := exec.Command(chromium,
			"--headless=new", "--disable-gpu", "--no-sandbox",
			"--hide-scrollbars", "--virtual-time-budget=5000",
			"--window-size="+shot.size,
			"--screenshot="+out,
			server.URL+"/",
		)
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("chromium screenshot %s: %v\n%s", shot.name, err, combined)
		}
		info, err := os.Stat(out)
		if err != nil || info.Size() == 0 {
			t.Fatalf("screenshot %s missing or empty: %v", out, err)
		}
		t.Logf("browser evidence: %s (%d bytes)", out, info.Size())
	}

	// Real rendered DOM (not view-source): the ring must be present.
	domCmd := exec.Command(chromium,
		"--headless=new", "--disable-gpu", "--no-sandbox",
		"--virtual-time-budget=5000",
		"--dump-dom", server.URL+"/",
	)
	domOut, err := domCmd.Output()
	if err != nil {
		t.Fatalf("chromium dump-dom: %v", err)
	}
	for _, want := range []string{
		"NEXA ROUTER", `id="ringSvg"`, `id="ringCoalesced"`, `id="ringStatus"`,
	} {
		if !strings.Contains(string(domOut), want) {
			t.Fatalf("rendered dashboard DOM missing live-ring contract %q", want)
		}
	}
}
