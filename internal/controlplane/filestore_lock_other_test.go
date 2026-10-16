//go:build !linux

package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// On platforms without flock support the file store fails closed: reads keep
// working, writes refuse with an explicit typed error instead of risking
// silent cross-process lost updates.
func TestFileStoreUnsupportedPlatformFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Put(ctx, "ns", 0, []byte("x")); !errors.Is(err, ErrCrossProcessLockUnsupported) {
		t.Fatalf("put error = %v, want ErrCrossProcessLockUnsupported", err)
	}
	if err := store.Delete(ctx, "ns", 0); !errors.Is(err, ErrCrossProcessLockUnsupported) {
		t.Fatalf("delete error = %v, want ErrCrossProcessLockUnsupported", err)
	}
	// A valid backup isolates the lock refusal from backup validation.
	dir := filepath.Dir(path)
	backup := filepath.Join(dir, "state.bak")
	if err := os.WriteFile(backup, []byte(`{"schema":1,"snapshots":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreFileStore(path, backup); !errors.Is(err, ErrCrossProcessLockUnsupported) {
		t.Fatalf("restore error = %v, want ErrCrossProcessLockUnsupported", err)
	}
	if _, err := store.Get(ctx, "ns"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get error = %v, want ErrNotFound (reads stay available)", err)
	}
}
