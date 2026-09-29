package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Audit item 7: cmd/gateway was at 12.9% — dashboardURL/openExistingUI and
// strict-config wiring had no coverage.
func TestAuditDashboardURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := dashboardURL(path); got == "" {
		t.Fatal("empty dashboard URL")
	}
	if got := dashboardURL(filepath.Join(dir, "missing.json")); got != "http://127.0.0.1:8080/" {
		t.Fatalf("missing config fallback = %q", got)
	}
}

func TestAuditOpenExistingUIUnreachable(t *testing.T) {
	var buf bytes.Buffer
	// Unroutable port: exercises the WaitReady-failure branch without a browser.
	openExistingUI("http://127.0.0.1:1/", &buf)
	if buf.Len() == 0 {
		t.Fatal("expected fallback message")
	}
}

func TestAuditStrictConfigFileEnforcement(t *testing.T) {
	dir := t.TempDir()
	// Minimal config with empty routing/probe sections triggers warnings.
	raw := `{"providers":[]}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "1")
	if !config.StrictMode() {
		t.Fatal("strict mode should be on")
	}
	if _, _, err := config.LoadWithWarnings(path); err == nil {
		t.Fatal("strict mode should reject implicit defaults")
	}
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "")
	if !config.StrictMode() {
		// off by default
	} else {
		t.Fatal("strict mode should be off")
	}
	if _, warnings, err := config.LoadWithWarnings(path); err != nil {
		t.Fatalf("non-strict load should succeed: %v", err)
	} else if len(warnings) == 0 {
		t.Fatal("expected implicit-default warnings")
	}
}

func TestAuditInspectRawDefaults(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"providers":[]}`, true},
		{`{"routing":{"strategy":"ready_mesh"},"probe":{"max_tokens":1,"interval_seconds":120},"providers":[]}`, false},
		{`{"routing":{"public_model":"legacy-x"},"providers":[]}`, true},
	} {
		got := config.InspectRawDefaults([]byte(tc.raw))
		if (len(got) > 0) != tc.want {
			t.Fatalf("raw %s warnings=%v, wantWarn=%v", tc.raw, got, tc.want)
		}
	}
}
