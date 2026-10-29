package queue

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// A store path that is a directory fails to read with a non-NotExist
// error, which NewFileStore must surface instead of treating the store
// as empty.
func TestFileStoreNewLoadReadError(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewFileStore(dir); err == nil {
		t.Fatal("NewFileStore(directory) = nil error, want a read error")
	}
}

func TestFileStoreCreateDuplicateAndIdempotency(t *testing.T) {
	ctx := context.Background()
	s, err := NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, queueJob("j1", "k1")); err != nil {
		t.Fatalf("Create j1: %v", err)
	}
	if err := s.Create(ctx, queueJob("j1", "other")); err == nil || err.Error() != "job already exists" {
		t.Fatalf("duplicate job id err = %v, want \"job already exists\"", err)
	}
	if err := s.Create(ctx, queueJob("j2", "k1")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate idempotency err = %v, want %v", err, ErrIdempotencyConflict)
	}
	got, err := s.GetByIdempotency(ctx, "k1")
	if err != nil || got.JobID != "j1" {
		t.Fatalf("GetByIdempotency after conflicts = %+v, %v, want j1", got, err)
	}
}

func TestFileStoreUpdateMissing(t *testing.T) {
	ctx := context.Background()
	s, err := NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, queueJob("missing", "")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing err = %v, want %v", err, ErrNotFound)
	}
}

// A job that cannot be serialized must fail Create without leaving a
// record or a held idempotency key behind: NaN progress is not valid
// JSON, so persist's marshal fails and Create rolls back in memory.
func TestFileStorePersistMarshalFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := queueJob("bad", "k-bad")
	bad.Progress = math.NaN()
	if err := s.Create(ctx, bad); err == nil {
		t.Fatal("Create NaN-progress job = nil error, want a marshal error")
	}
	if _, err := s.Get(ctx, "bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after failed Create = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.GetByIdempotency(ctx, "k-bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByIdempotency after failed Create = %v, want %v", err, ErrNotFound)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("store file state = %v, want no file after failed persist", err)
	}
	// The freed idempotency key is reusable by a valid job.
	if err := s.Create(ctx, queueJob("good", "k-bad")); err != nil {
		t.Fatalf("Create after rollback: %v", err)
	}
}

// Persist writes a temp file and renames it over the store path. When
// the store path is a non-empty directory the rename fails, and both
// Create and TryClaim must roll their in-memory change back instead of
// reporting success against unpersisted state.
func TestFileStorePersistRenameFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	occupied := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(occupied, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &FileStore{
		path: occupied,
		jobs: map[string]video.VideoJob{"j1": queueJob("j1", "k1")},
		idem: map[string]string{"k1": "j1"},
	}

	if err := s.Create(ctx, queueJob("j2", "k2")); err == nil {
		t.Fatal("Create over non-empty directory = nil error, want a rename error")
	}
	if _, err := s.Get(ctx, "j2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after failed Create = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.GetByIdempotency(ctx, "k2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByIdempotency after failed Create = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.Get(ctx, "j1"); err != nil {
		t.Fatalf("pre-existing job lost after failed Create: %v", err)
	}

	_, claimed, err := s.TryClaim(ctx, "j1", "owner-1", time.Hour, time.Now())
	if err == nil {
		t.Fatal("TryClaim with failing persist = nil error, want a rename error")
	}
	if claimed {
		t.Fatal("TryClaim with failing persist claimed the job")
	}
	got, err := s.Get(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseOwner != "" {
		t.Fatalf("LeaseOwner after failed claim = %q, want empty (rolled back)", got.LeaseOwner)
	}
}

// Concurrent claims of one job must produce exactly one winner: the
// store lock serializes check-and-set, so losers observe the lease
// instead of overwriting it.
func TestFileStoreConcurrentClaimSingleWinner(t *testing.T) {
	ctx := context.Background()
	s, err := NewFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, queueJob("j1", "k1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	const workers = 16
	claimed := make([]bool, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct owners: a same-owner call would renew rather
			// than compete, so identical owners cannot test exclusion.
			_, claimed[i], errs[i] = s.TryClaim(ctx, "j1", "owner-"+string(rune('a'+i)), time.Hour, time.Now())
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if claimed[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	got, err := s.Get(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseOwner == "" || len(got.LeaseOwner) < len("owner-") || got.LeaseOwner[:len("owner-")] != "owner-" {
		t.Fatalf("LeaseOwner = %q, want one of the competing owners", got.LeaseOwner)
	}
}
