package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileStoreRevisionConcurrencyAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controlplane.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := store.Put(ctx, "config", 0, []byte("v1"))
	if err != nil || first.Revision != 1 {
		t.Fatalf("first put: %+v %v", first, err)
	}
	if _, err := store.Put(ctx, "config", 0, []byte("stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale writer: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := store.Put(ctx, "config", 1, []byte("race")); results <- err }()
	}
	wg.Wait()
	close(results)
	conflicts := 0
	for err := range results {
		if errors.Is(err, ErrConflict) {
			conflicts++
		}
	}
	if conflicts != 7 {
		t.Fatalf("expected 7 stale conflicts, got %d", conflicts)
	}
	backup := path + ".bak"
	if err := BackupFileStore(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "config"); err == nil {
		t.Fatal("accepted corrupt store")
	}
	if err := RestoreFileStore(path, backup); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(ctx, "config")
	if err != nil || string(got.Payload) != "race" {
		t.Fatalf("restore: %+v %v", got, err)
	}
}

func TestFileStoreInterruptedTempDoesNotReplaceCommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controlplane.json")
	store, _ := NewFileStore(path)
	if _, err := store.Put(context.Background(), "x", 0, []byte("committed")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "x")
	if err != nil || string(got.Payload) != "committed" {
		t.Fatalf("temp file changed committed state: %+v %v", got, err)
	}
}
