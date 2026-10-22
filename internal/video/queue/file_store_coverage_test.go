package queue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func fileStoreWithJobs(t *testing.T, jobs ...video.VideoJob) (*FileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if err := s.Create(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	return s, path
}

func TestFileStoreListReturnsAllJobs(t *testing.T) {
	s, _ := fileStoreWithJobs(t,
		video.VideoJob{JobID: "a", IdempotencyKey: "k-a"},
		video.VideoJob{JobID: "b"},
		video.VideoJob{JobID: "c"},
	)
	jobs, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("list returned %d jobs, want 3", len(jobs))
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		seen[j.JobID] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !seen[id] {
			t.Fatalf("list missing job %s", id)
		}
	}
}

func TestFileStoreDeleteRemovesJobAndFreesIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	s, path := fileStoreWithJobs(t, video.VideoJob{JobID: "a", IdempotencyKey: "k-a"})
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
	// The idempotency key is free again: a new create with the same key
	// must not resolve to the deleted job.
	if err := s.Create(ctx, video.VideoJob{JobID: "a2", IdempotencyKey: "k-a"}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Get(ctx, "a2")
	if err != nil {
		t.Fatal(err)
	}
	if stored.IdempotencyKey != "k-a" {
		t.Fatalf("new job idempotency key = %q, want k-a", stored.IdempotencyKey)
	}
	// Deleting a missing job is a no-op.
	if err := s.Delete(ctx, "missing"); err != nil {
		t.Fatalf("delete missing job: %v", err)
	}
	// The deletion persists across a reopen.
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after reopen: %v, want ErrNotFound", err)
	}
}

func TestFileStoreTryClaimLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	s, _ := fileStoreWithJobs(t, video.VideoJob{JobID: "j"})

	claimed, ok, err := s.TryClaim(ctx, "j", "owner-1", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if claimed.LeaseOwner != "owner-1" || !claimed.LeaseUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("lease = %+v", claimed)
	}
	// A live foreign lease blocks a second owner.
	if _, ok, err := s.TryClaim(ctx, "j", "owner-2", time.Minute, now.Add(10*time.Second)); err != nil || ok {
		t.Fatalf("foreign claim during live lease: ok=%v err=%v", ok, err)
	}
	// The same owner may renew its own lease.
	renewed, ok, err := s.TryClaim(ctx, "j", "owner-1", 2*time.Minute, now.Add(10*time.Second))
	if err != nil || !ok {
		t.Fatalf("owner renew: ok=%v err=%v", ok, err)
	}
	if !renewed.LeaseUntil.Equal(now.Add(10 * time.Second).Add(2 * time.Minute)) {
		t.Fatalf("renewed lease until = %v", renewed.LeaseUntil)
	}
	// After expiry another owner can claim.
	if _, ok, err := s.TryClaim(ctx, "j", "owner-2", time.Minute, now.Add(3*time.Minute)); err != nil || !ok {
		t.Fatalf("claim after expiry: ok=%v err=%v", ok, err)
	}
	// Unknown job: ErrNotFound.
	if _, _, err := s.TryClaim(ctx, "nope", "owner-1", time.Minute, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim unknown job: %v, want ErrNotFound", err)
	}
}

func TestFileStoreDeleteOnCorruptPathRollsBack(t *testing.T) {
	// Deleting with an unwritable backing file must surface the persist
	// error and keep the job (the in-memory rollback contract).
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Create(ctx, video.VideoJob{JobID: "a", IdempotencyKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make directory read-only")
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if err := s.Delete(ctx, "a"); err == nil {
		t.Skip("persist succeeded anyway (privileged user); rollback path not exercised")
	}
	if _, err := s.Get(ctx, "a"); err != nil {
		t.Fatalf("job lost after failed delete persist: %v", err)
	}
}
