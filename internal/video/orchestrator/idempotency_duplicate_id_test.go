package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// Regression for the coverage-run failure
// "video job store is temporarily unavailable: job already exists" in
// TestConcurrentCreateSameIdempotencyKeyCreatesOneJob.
//
// Root cause, in two layers:
//  1. Job IDs were timestamp-only (time.Now().UnixNano()), so two
//     concurrent creates could collide on the ID itself.
//  2. The orchestrator only resolved ErrIdempotencyConflict to the
//     winning job. A plain "job already exists" (job_id checked
//     before the idempotency key in every store, and reported first
//     by the Postgres store when both constraints fire) was returned
//     as a store failure, even when the caller's idempotency key
//     already had a persisted winner.
//
// Both layers are fixed: newVideoJobID uses crypto-random IDs, and a
// plain duplicate ID now resolves to the idempotency winner when one
// exists, while an unrelated duplicate ID still fails.

func TestNewVideoJobIDIsUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		id := newVideoJobID()
		if !strings.HasPrefix(id, "video-") {
			t.Fatalf("job ID %q lacks video- prefix", id)
		}
		if seen[id] {
			t.Fatalf("duplicate job ID generated: %q", id)
		}
		seen[id] = true
	}
}

// firstMissStore makes the first GetByIdempotency miss (so Create
// runs and hits the injected duplicate-ID error) while later lookups
// find the winner. This reproduces the concurrent race: the winner
// lands between the loser's pre-check and its Create.
type firstMissStore struct {
	*queue.MemoryStore
	lookups   int
	createErr error
}

func (s *firstMissStore) GetByIdempotency(ctx context.Context, key string) (video.VideoJob, error) {
	s.lookups++
	if s.lookups == 1 {
		return video.VideoJob{}, queue.ErrNotFound
	}
	return s.MemoryStore.GetByIdempotency(ctx, key)
}

func (s *firstMissStore) Create(ctx context.Context, j video.VideoJob) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.MemoryStore.Create(ctx, j)
}

// A plain duplicate-ID error with a persisted idempotency winner must
// return that winner (same job, no duplicate queue admission), exactly
// like an ErrIdempotencyConflict.
func TestCreateDuplicateJobIDWithIdempotencyWinnerReturnsWinner(t *testing.T) {
	ctx := context.Background()
	base := queue.NewMemoryStore()
	winner := queueJobForTest("winner")
	winner.IdempotencyKey = "dup-winner"
	if err := base.Create(ctx, winner); err != nil {
		t.Fatal(err)
	}
	store := &firstMissStore{MemoryStore: base, createErr: queue.ErrJobAlreadyExists}
	r := requestWithDefaults()
	r.IdempotencyKey = "dup-winner"
	o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
	got, err := o.Create(ctx, r)
	if err != nil {
		t.Fatalf("duplicate-ID create with winner failed: %v", err)
	}
	if got.JobID != winner.JobID {
		t.Fatalf("returned job %q, want winner %q", got.JobID, winner.JobID)
	}
	if o.Queue.Len() != 0 {
		t.Fatalf("queue admission occurred for a duplicate: len=%d, want 0", o.Queue.Len())
	}
}

// A plain duplicate-ID error with NO idempotency winner must stay a
// failure: an unrelated job already owning that ID is not an
// idempotent request, and must never be silently treated as one.
func TestCreateDuplicateJobIDWithoutWinnerStillFails(t *testing.T) {
	ctx := context.Background()
	store := &mission95Store{MemoryStore: queue.NewMemoryStore()}
	// An unrelated job owns some ID, but nothing owns this key.
	unrelated := queueJobForTest("unrelated")
	if err := store.MemoryStore.Create(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	store.createErr = queue.ErrJobAlreadyExists
	r := requestWithDefaults()
	r.IdempotencyKey = "no-such-winner"
	o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
	if _, err := o.Create(ctx, r); err == nil || !strings.Contains(err.Error(), "job already exists") {
		t.Fatalf("unrelated duplicate ID error=%v, want job already exists", err)
	}
}

// The store-level sentinel: duplicate job IDs are now identifiable
// with errors.Is across memory, file and Postgres stores, instead of
// relying on string comparison.
func TestMemoryStoreDuplicateJobIDIsSentinel(t *testing.T) {
	ctx := context.Background()
	s := queue.NewMemoryStore()
	j := video.VideoJob{JobID: "dup-id", IdempotencyKey: "k-dup"}
	if err := s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	err := s.Create(ctx, video.VideoJob{JobID: "dup-id", IdempotencyKey: "k-other"})
	if err == nil || !strings.Contains(err.Error(), "job already exists") {
		t.Fatalf("duplicate job ID error=%v, want job already exists", err)
	}
	if !isJobAlreadyExists(err) {
		t.Fatalf("duplicate job ID error is not the sentinel: %v", err)
	}
}

func isJobAlreadyExists(err error) bool {
	return errors.Is(err, queue.ErrJobAlreadyExists)
}
