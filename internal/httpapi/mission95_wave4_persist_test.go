package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// /proc exists, so MkdirAll is a no-op, but it is read-only: CreateTemp
// fails deterministically even as root.
func TestWave4PersistCreateTempFailure(t *testing.T) {
	plane := newEvaluationPlane(config.EvaluationConfig{Enabled: true, StatePath: "/proc/wave4-state.json"}, nil)
	if err := plane.persist(); err == nil {
		t.Fatal("persist into /proc must fail at temp-file creation")
	}
}

func TestWave4BudgetSaveDirectoryErrors(t *testing.T) {
	// A regular file in the middle of the directory path breaks MkdirAll.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fileBudgetAlertStore{path: filepath.Join(blocker, "sub", "doc.json")}
	if _, err := store.Save(map[string]int64{"p/a": 3}, 0); err == nil {
		t.Fatal("Save below a regular file must fail at directory creation")
	}

	// /proc again: MkdirAll succeeds (directory exists), CreateTemp fails.
	procStore := &fileBudgetAlertStore{path: "/proc/wave4-budget.json"}
	if _, err := procStore.Save(map[string]int64{"p/a": 3}, 0); err == nil {
		t.Fatal("Save into /proc must fail at temp-file creation")
	}
}

func TestWave4BudgetSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "doc.json")
	store := &fileBudgetAlertStore{path: path}
	if _, err := store.Save(map[string]int64{"p/a": 7, "p/b": 2}, 0); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fired, _, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fired["p/a"] != 7 || fired["p/b"] != 2 || len(fired) != 2 {
		t.Fatalf("round trip = %v, want {p/a:7 p/b:2}", fired)
	}
	// The temp file must not linger after the rename.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".budget-alerts-") {
			t.Fatalf("temp file %q left behind", e.Name())
		}
	}
}
