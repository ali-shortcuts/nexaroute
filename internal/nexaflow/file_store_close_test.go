package nexaflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreClosedWriterCannotOverwriteReopenedStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.json")
	old, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.SaveDraft(ctx, Definition{ID: "initial"}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.SaveDraft(ctx, Definition{ID: "current"}); err != nil {
		t.Fatal(err)
	}
	if err := old.SaveDraft(ctx, Definition{ID: "stale"}); !errors.Is(err, os.ErrClosed) {
		t.Errorf("closed writer returned %v; want os.ErrClosed", err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetDraft(ctx, "current"); err != nil {
		t.Errorf("current writer's draft lost after reopen: %v", err)
	}
	if _, err := reopened.GetDraft(ctx, "stale"); !errors.Is(err, ErrNotFound) {
		t.Errorf("closed writer persisted a stale draft: %v", err)
	}
}
