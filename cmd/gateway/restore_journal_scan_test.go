package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression (audit 2026-10-15): a restore killed after writing its
// saved pre-restore copies but before the first manifest persist
// leaves a journal directory with no manifest. The interrupted
// scan skipped it silently, so doctor reported a clean PASS ("no
// interrupted restore transactions") and restore --recover failed
// opaquely. Such directories must be discoverable and surfaced.
func TestScanRestoreJournalsFindsManifestlessDirs(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	orphan := filepath.Join(base, "nexaroute-restore-journal-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "saved-config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := findInterruptedRestoreJournals(); len(got) != 0 {
		t.Fatalf("interrupted = %v, want none (no manifest, nothing was installed)", got)
	}
	got := findUninspectableRestoreJournals()
	if len(got) != 1 || got[0] != orphan {
		t.Fatalf("uninspectable = %v, want [%s]", got, orphan)
	}
}
