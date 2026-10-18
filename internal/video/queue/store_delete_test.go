package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// #420 item 5 (retention): both JobStore implementations support
// Delete — the job and its idempotency mapping go away, the
// removal persists for the file store, and deleting a missing
// job is a no-op.
func TestStoreDelete(t *testing.T) {
	ctx := context.Background()
	j := video.VideoJob{JobID: "j1", IdempotencyKey: "k1", State: video.StateCompleted, CreatedAt: time.Now()}

	mem := NewMemoryStore()
	if err := mem.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := mem.Delete(ctx, "j1"); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Get(ctx, "j1"); err == nil {
		t.Fatal("memory store kept a deleted job")
	}
	if _, err := mem.GetByIdempotency(ctx, "k1"); err == nil {
		t.Fatal("memory store kept a deleted job's idempotency mapping")
	}
	if err := mem.Delete(ctx, "j1"); err != nil {
		t.Fatalf("deleting a missing job should be a no-op, got %v", err)
	}

	path := filepath.Join(t.TempDir(), "jobs.json")
	fs, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "j1"); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Get(ctx, "j1"); err == nil {
		t.Fatal("file store resurrected a deleted job after reopen")
	}
	if _, err := reopened.GetByIdempotency(ctx, "k1"); err == nil {
		t.Fatal("file store kept a deleted job's idempotency mapping after reopen")
	}
}
