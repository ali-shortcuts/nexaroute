//go:build integration

// Real-PostgreSQL integration tests for the video job store adapter
// (issue #609, architecture slice 4). They run only with the
// `integration` build tag and a DSN in NEXAROUTE_TEST_POSTGRES_DSN
// pointing at a DISPOSABLE database (CI provides one via a service
// container). Ordinary unit runs never touch a real server.
//
// Evidence base: duplicate create under concurrency (exactly one job
// wins the idempotency key), concurrent lease claim (exactly one
// winner), lease expiration, restart/recovery persistence across a
// close/reopen, terminal-overwrite protection, and idempotent
// completion replay.
package queue

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func videoPGDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	return dsn
}

func openVideoPGStore(t *testing.T) *PostgresStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := OpenPostgresStore(ctx, videoPGDSN(t))
	if err != nil {
		t.Fatalf("open postgres video job store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func resetVideoJobsTable(t *testing.T, s *PostgresStore) {
	t.Helper()
	if _, err := s.db.Exec(`TRUNCATE ` + videoJobsTable); err != nil {
		t.Fatalf("truncate video jobs: %v", err)
	}
}

func pgTestJob(id, idem string) video.VideoJob {
	return video.VideoJob{
		JobID:          id,
		IdempotencyKey: idem,
		State:          video.StateQueued,
		CreatedAt:      time.Now().UTC().Truncate(time.Millisecond),
	}
}

// Duplicate create under concurrency: exactly one job wins the
// idempotency key; every loser observes ErrIdempotencyConflict; the
// winner is retrievable by key.
func TestPostgresVideoStoreDuplicateCreate(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	const racers = 16
	var wg sync.WaitGroup
	wins := make(chan string, racers)
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("job-%d", i)
			if err := s.Create(ctx, pgTestJob(id, "idem-key-1")); err != nil {
				errs <- err
				return
			}
			wins <- id
		}(i)
	}
	wg.Wait()
	close(wins)
	close(errs)

	var winners []string
	for id := range wins {
		winners = append(winners, id)
	}
	if len(winners) != 1 {
		t.Fatalf("want exactly 1 winner, got %d: %v", len(winners), winners)
	}
	nConflict := 0
	for err := range errs {
		if err == ErrIdempotencyConflict {
			nConflict++
		} else {
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if nConflict != racers-1 {
		t.Fatalf("want %d idempotency conflicts, got %d", racers-1, nConflict)
	}
	got, err := s.GetByIdempotency(ctx, "idem-key-1")
	if err != nil {
		t.Fatalf("GetByIdempotency: %v", err)
	}
	if got.JobID != winners[0] {
		t.Fatalf("winner = %q, GetByIdempotency = %q", winners[0], got.JobID)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want exactly 1 stored job, got %d", len(list))
	}
}

// Duplicate job_id (not just key) is the plain "already exists" error,
// matching the memory store's check order.
func TestPostgresVideoStoreDuplicateJobID(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	if err := s.Create(ctx, pgTestJob("j1", "k1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, pgTestJob("j1", "k2")); err == nil {
		t.Fatal("duplicate job_id should fail")
	}
}

// Concurrent lease claim: exactly one claimant wins; a live foreign
// lease refuses; the same owner renews; an expired lease is claimable.
func TestPostgresVideoStoreConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	if err := s.Create(ctx, pgTestJob("j1", "")); err != nil {
		t.Fatal(err)
	}
	const racers = 16
	var wg sync.WaitGroup
	won := make(chan string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := fmt.Sprintf("owner-%d", i)
			if _, ok, err := s.TryClaim(ctx, "j1", owner, 5*time.Minute, time.Now()); err != nil {
				t.Errorf("claim: %v", err)
			} else if ok {
				won <- owner
			}
		}(i)
	}
	wg.Wait()
	close(won)
	var winners []string
	for o := range won {
		winners = append(winners, o)
	}
	if len(winners) != 1 {
		t.Fatalf("want exactly 1 claim winner, got %d: %v", len(winners), winners)
	}
	// Live foreign lease refuses.
	if _, ok, err := s.TryClaim(ctx, "j1", "owner-other", time.Minute, time.Now()); err != nil || ok {
		t.Fatalf("live foreign lease must refuse: ok=%v err=%v", ok, err)
	}
	// Same owner renews.
	got, ok, err := s.TryClaim(ctx, "j1", winners[0], time.Minute, time.Now())
	if err != nil || !ok {
		t.Fatalf("same-owner renewal: ok=%v err=%v", ok, err)
	}
	if got.LeaseOwner != winners[0] {
		t.Fatalf("renewed owner = %q", got.LeaseOwner)
	}
	// Missing job.
	if _, _, err := s.TryClaim(ctx, "missing", "o", time.Minute, time.Now()); err != ErrNotFound {
		t.Fatalf("missing job err = %v, want ErrNotFound", err)
	}
}

// An expired lease is claimable by another owner (server-clock
// expiry, not caller-clock).
func TestPostgresVideoStoreExpiredLeaseClaimable(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	if err := s.Create(ctx, pgTestJob("j1", "")); err != nil {
		t.Fatal(err)
	}
	// Claim with a 1ms TTL, then wait for server-side expiry.
	if _, ok, err := s.TryClaim(ctx, "j1", "owner-a", time.Millisecond, time.Now()); err != nil || !ok {
		t.Fatalf("initial claim: ok=%v err=%v", ok, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, ok, err := s.TryClaim(ctx, "j1", "owner-b", 5*time.Minute, time.Now())
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("expired lease never became claimable")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Restart/recovery: jobs, idempotency mappings and leases survive a
// close/reopen of the store.
func TestPostgresVideoStoreRestartRecovery(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	job := pgTestJob("j1", "k1")
	job.State = video.StateProcessing
	if err := s.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.TryClaim(ctx, "j1", "owner-a", 5*time.Minute, time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openVideoPGStore(t)
	got, err := s2.Get(ctx, "j1")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.State != video.StateProcessing {
		t.Fatalf("state = %q, want processing", got.State)
	}
	if got.LeaseOwner != "owner-a" {
		t.Fatalf("lease owner = %q, want owner-a", got.LeaseOwner)
	}
	if got.LeaseUntil.IsZero() || time.Until(got.LeaseUntil) < 4*time.Minute {
		t.Fatalf("lease_until not persisted: %v", got.LeaseUntil)
	}
	byKey, err := s2.GetByIdempotency(ctx, "k1")
	if err != nil || byKey.JobID != "j1" {
		t.Fatalf("idempotency mapping lost: %v %+v", err, byKey)
	}
	// A duplicate create after restart still conflicts.
	if err := s2.Create(ctx, pgTestJob("j2", "k1")); err != ErrIdempotencyConflict {
		t.Fatalf("duplicate after restart err = %v, want ErrIdempotencyConflict", err)
	}
	// A live foreign lease still refuses after restart.
	if _, ok, err := s2.TryClaim(ctx, "j1", "owner-b", time.Minute, time.Now()); err != nil || ok {
		t.Fatalf("live foreign lease must refuse after restart: ok=%v err=%v", ok, err)
	}
}

// Invalid transitions: a terminal job cannot be overwritten with a
// different record (ErrTerminalJob); a byte-identical replay is
// allowed (idempotent completion).
func TestPostgresVideoStoreTerminalGuard(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	job := pgTestJob("j1", "")
	if err := s.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	done := job
	done.State = video.StateCompleted
	if err := s.Update(ctx, done); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Byte-identical replay is allowed.
	if err := s.Update(ctx, done); err != nil {
		t.Fatalf("identical replay should be allowed: %v", err)
	}
	// A different record over a terminal job is blocked.
	changed := done
	changed.State = video.StateProcessing
	if err := s.Update(ctx, changed); err != ErrTerminalJob {
		t.Fatalf("terminal overwrite err = %v, want ErrTerminalJob", err)
	}
	// Missing job.
	if err := s.Update(ctx, pgTestJob("missing", "")); err != ErrNotFound {
		t.Fatalf("update missing err = %v, want ErrNotFound", err)
	}
}

// Empty idempotency keys are never indexed: many jobs may share the
// empty key, and lookup by empty key is ErrNotFound.
func TestPostgresVideoStoreEmptyIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	for _, id := range []string{"j1", "j2", "j3"} {
		if err := s.Create(ctx, pgTestJob(id, "")); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := s.GetByIdempotency(ctx, ""); err != ErrNotFound {
		t.Fatalf("empty key lookup err = %v, want ErrNotFound", err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %d, err = %v", len(list), err)
	}
}

// Delete frees the idempotency key: the same key may create a new job
// afterwards (retention pruning semantic).
func TestPostgresVideoStoreDeleteFreesIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	s := openVideoPGStore(t)
	resetVideoJobsTable(t, s)

	if err := s.Create(ctx, pgTestJob("j1", "k1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "j1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "j1"); err != nil {
		t.Fatal("deleting a missing job should be a no-op")
	}
	if err := s.Create(ctx, pgTestJob("j2", "k1")); err != nil {
		t.Fatalf("key should be reusable after delete: %v", err)
	}
	got, err := s.GetByIdempotency(ctx, "k1")
	if err != nil || got.JobID != "j2" {
		t.Fatalf("GetByIdempotency after re-create: %v %+v", err, got)
	}
}

// Sanity: the integration DSN helper actually reaches a server.
func TestPostgresVideoStoreDSNReachable(t *testing.T) {
	db, err := sql.Open("pgx", videoPGDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}
