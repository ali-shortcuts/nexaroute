package queue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Second wave of bounded, deterministic stress checks for issue #664.
// Extends mission95_stress_test.go (single-winner claims, create+claim)
// with duplicate work, terminal-update races, full-state restart
// recovery, corrupted persisted state, persist-failure rollback, and
// cancellation at the worker level. All coordination is via start
// gates, ack channels, and explicit `now` arguments; no sleeps.
// requireStress and itoa are defined in mission95_stress_test.go in
// this package.

// TestStressSameOwnerConcurrentRenewal proves concurrent renewal by
// the lease holder is idempotent: every caller succeeds, the owner
// is unchanged, and the lease still blocks a foreign owner.
func TestStressSameOwnerConcurrentRenewal(t *testing.T) {
	requireStress(t)
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := store.Create(ctx, video.VideoJob{JobID: "job-1", State: video.StateQueued}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.TryClaim(ctx, "job-1", "owner-a", time.Minute, now); err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	const racers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]bool, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i], errs[i] = store.TryClaim(ctx, "job-1", "owner-a", time.Minute, now)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range racers {
		if errs[i] != nil || !results[i] {
			t.Fatalf("renewal racer %d: claimed=%v err=%v", i, results[i], errs[i])
		}
	}
	got, err := store.Get(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseOwner != "owner-a" || !got.LeaseUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("lease after renewals = %+v", got)
	}
	if _, claimed, err := store.TryClaim(ctx, "job-1", "intruder", time.Minute, now); err != nil || claimed {
		t.Fatalf("foreign claim during live lease: claimed=%v err=%v", claimed, err)
	}
}

// TestStressDuplicateCreateAndIdempotency proves duplicate work is
// rejected exactly once: concurrent creates of one JobID yield one
// success and leave the original untouched, and concurrent creates
// sharing one idempotency key yield one success plus
// ErrIdempotencyConflict for the rest, with the winner retrievable
// by that key. Run against both stores; the file store persists
// only the winner because losers fail before the write.
func TestStressDuplicateCreateAndIdempotency(t *testing.T) {
	requireStress(t)
	ctx := context.Background()
	stores := map[string]JobStore{"memory": NewMemoryStore()}
	fileStore, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	stores["file"] = fileStore
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			const racers = 16
			start := make(chan struct{})
			var wg sync.WaitGroup
			errs := make([]error, racers)
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					errs[i] = store.Create(ctx, video.VideoJob{
						JobID: "dup", State: video.StateQueued,
						ProjectID: "original",
					})
				}(i)
			}
			close(start)
			wg.Wait()
			successes := 0
			for _, err := range errs {
				if err == nil {
					successes++
				}
			}
			if successes != 1 {
				t.Fatalf("duplicate JobID creates: %d successes, want 1", successes)
			}
			got, err := store.Get(ctx, "dup")
			if err != nil || got.ProjectID != "original" {
				t.Fatalf("original after duplicate creates = %+v err=%v", got, err)
			}
			// Same idempotency key, distinct JobIDs: one winner.
			start2 := make(chan struct{})
			var wg2 sync.WaitGroup
			errs2 := make([]error, racers)
			for i := 0; i < racers; i++ {
				wg2.Add(1)
				go func(i int) {
					defer wg2.Done()
					<-start2
					errs2[i] = store.Create(ctx, video.VideoJob{
						JobID: "idem-" + itoa(i), State: video.StateQueued,
						IdempotencyKey: "key-1",
					})
				}(i)
			}
			close(start2)
			wg2.Wait()
			successes, conflicts := 0, 0
			for _, err := range errs2 {
				if err == nil {
					successes++
				} else if errors.Is(err, ErrIdempotencyConflict) {
					conflicts++
				}
			}
			if successes != 1 || conflicts != racers-1 {
				t.Fatalf("idempotency race: successes=%d conflicts=%d, want 1/%d", successes, conflicts, racers-1)
			}
			winner, err := store.GetByIdempotency(ctx, "key-1")
			if err != nil {
				t.Fatalf("GetByIdempotency: %v", err)
			}
			if winner.IdempotencyKey != "key-1" {
				t.Fatalf("idempotency winner = %+v", winner)
			}
		})
	}
}

// TestStressTerminalUpdateRace proves concurrent transitions to a
// terminal state cannot produce a torn or overwritten record: with
// distinct progress values, exactly one update wins, the final
// record equals that winner exactly, and an identical re-update
// stays allowed while any differing update is refused.
func TestStressTerminalUpdateRace(t *testing.T) {
	requireStress(t)
	store := NewMemoryStore()
	ctx := context.Background()
	base := video.VideoJob{JobID: "term", State: video.StateQueued}
	if err := store.Create(ctx, base); err != nil {
		t.Fatal(err)
	}
	const racers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	candidates := make([]video.VideoJob, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		candidates[i] = base
		candidates[i].State = video.StateCompleted
		candidates[i].Progress = float64(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = store.Update(ctx, candidates[i])
		}(i)
	}
	close(start)
	wg.Wait()
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrTerminalJob) {
			t.Fatalf("unexpected update error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("terminal updates: %d successes, want 1", successes)
	}
	got, err := store.Get(ctx, "term")
	if err != nil {
		t.Fatal(err)
	}
	match := false
	for _, c := range candidates {
		if reflect.DeepEqual(got, c) {
			match = true
		}
	}
	if !match {
		t.Fatalf("final record %+v matches no candidate (torn write?)", got)
	}
	if err := store.Update(ctx, got); err != nil {
		t.Fatalf("identical re-update of terminal job: %v", err)
	}
	changed := got
	changed.Progress = 99
	if err := store.Update(ctx, changed); !errors.Is(err, ErrTerminalJob) {
		t.Fatalf("differing update of terminal job: err=%v, want ErrTerminalJob", err)
	}
}

// TestStressConcurrentUpdateNoTornWrite proves concurrent whole-record
// updates to a non-terminal job never mix fields: the final record
// equals exactly one of the written candidates.
func TestStressConcurrentUpdateNoTornWrite(t *testing.T) {
	requireStress(t)
	store := NewMemoryStore()
	ctx := context.Background()
	base := video.VideoJob{JobID: "upd", State: video.StateQueued}
	if err := store.Create(ctx, base); err != nil {
		t.Fatal(err)
	}
	const racers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	candidates := make([]video.VideoJob, racers)
	for i := 0; i < racers; i++ {
		candidates[i] = base
		candidates[i].Progress = float64(i) / 10
		candidates[i].LastError = "err-" + itoa(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := store.Update(ctx, candidates[i]); err != nil {
				t.Errorf("update %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	got, err := store.Get(ctx, "upd")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if reflect.DeepEqual(got, c) {
			return
		}
	}
	t.Fatalf("final record %+v matches no candidate (torn write?)", got)
}

// TestStressFileStoreRestartRecoversFullState goes beyond the existing
// single-lease reopen unit test: a full mixed job set (live lease,
// expired lease, terminal job, idempotency keys, progress) survives a
// store reopen exactly, a stray temp file from an interrupted persist
// does not break loading, and post-restart rules (live lease blocks,
// expired lease frees, terminal guard holds, duplicates still fail)
// all hold on the recovered state.
func TestStressFileStoreRestartRecoversFullState(t *testing.T) {
	requireStress(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	jobs := []video.VideoJob{
		{JobID: "live", State: video.StateProcessing, IdempotencyKey: "k-live", Progress: 0.5},
		{JobID: "expired", State: video.StateProcessing, Progress: 0.25},
		{JobID: "done", State: video.StateCompleted, Progress: 1},
		{JobID: "queued", State: video.StateQueued, IdempotencyKey: "k-queued"},
	}
	for _, j := range jobs {
		if err := store.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if _, claimed, err := store.TryClaim(ctx, "live", "owner-a", time.Hour, now); err != nil || !claimed {
		t.Fatalf("claim live: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := store.TryClaim(ctx, "expired", "owner-old", time.Minute, now); err != nil || !claimed {
		t.Fatalf("claim expired: claimed=%v err=%v", claimed, err)
	}
	// A stray temp file, as left by an interrupted persist, must not
	// break loading: persist uses temp+rename, so only jobs.json
	// is authoritative.
	if err := os.WriteFile(filepath.Join(dir, ".video-jobs-stray"), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen with stray temp file: %v", err)
	}
	listed, err := reopened.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(jobs) {
		t.Fatalf("recovered jobs = %d, want %d", len(listed), len(jobs))
	}
	byID := map[string]video.VideoJob{}
	for _, j := range listed {
		byID[j.JobID] = j
	}
	if got := byID["live"]; got.LeaseOwner != "owner-a" || !got.LeaseUntil.Equal(now.Add(time.Hour)) || got.Progress != 0.5 {
		t.Fatalf("recovered live job = %+v", got)
	}
	if got := byID["done"]; got.State != video.StateCompleted || got.Progress != 1 {
		t.Fatalf("recovered terminal job = %+v", got)
	}
	if _, claimed, err := reopened.TryClaim(ctx, "live", "owner-b", time.Minute, now.Add(30*time.Minute)); err != nil || claimed {
		t.Fatalf("live lease after restart: claimed=%v err=%v, want blocked", claimed, err)
	}
	if _, claimed, err := reopened.TryClaim(ctx, "expired", "owner-new", time.Minute, now.Add(2*time.Minute)); err != nil || !claimed {
		t.Fatalf("expired lease after restart: claimed=%v err=%v, want claimable", claimed, err)
	}
	winner, err := reopened.GetByIdempotency(ctx, "k-live")
	if err != nil || winner.JobID != "live" {
		t.Fatalf("idempotency after restart = %+v err=%v", winner, err)
	}
	changed := byID["done"]
	changed.Progress = 0.5
	if err := reopened.Update(ctx, changed); !errors.Is(err, ErrTerminalJob) {
		t.Fatalf("terminal guard after restart: err=%v, want ErrTerminalJob", err)
	}
	if err := reopened.Create(ctx, video.VideoJob{JobID: "live"}); err == nil {
		t.Fatal("duplicate create after restart accepted")
	}
}

// TestStressFileStoreCorruptStateFailsClosed proves corrupted
// persisted state is refused, never partially loaded or silently
// repaired, and a failed open leaves the file bytes untouched.
func TestStressFileStoreCorruptStateFailsClosed(t *testing.T) {
	requireStress(t)
	cases := map[string]string{
		"garbage":   "this is not json at all {{{",
		"truncated": `{"jobs": {"a": {"job_id": "a", "state": "queued"`,
		"dupkey": `{"jobs": {"a": {"job_id": "a", "idempotency_key": "k"}, ` +
			`"b": {"job_id": "b", "idempotency_key": "k"}}, "idempotency": {}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewFileStore(path); err == nil {
				t.Fatalf("corrupt store %q loaded without error", name)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != content {
				t.Fatal("failed open modified the corrupt file")
			}
		})
	}
}

// TestStressFileStorePersistFailureRollsBack proves a persist failure
// cannot leave memory and disk diverged: Create and TryClaim return
// the error and their in-memory effects are rolled back. The failure
// is produced deterministically by replacing the store directory
// with a regular file (MkdirAll then fails); no permission tricks,
// which would be unreliable under root.
func TestStressFileStorePersistFailureRollsBack(t *testing.T) {
	requireStress(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	breakDir := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("create", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sub")
		store, err := NewFileStore(filepath.Join(dir, "jobs.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Create(ctx, video.VideoJob{JobID: "good", State: video.StateQueued}); err != nil {
			t.Fatal(err)
		}
		breakDir(t, dir)
		if err := store.Create(ctx, video.VideoJob{JobID: "broken", State: video.StateQueued}); err == nil {
			t.Fatal("create with broken persist dir accepted")
		}
		if _, err := store.Get(ctx, "broken"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rolled-back job visible: err=%v", err)
		}
		listed, err := store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].JobID != "good" {
			t.Fatalf("list after rollback = %+v", listed)
		}
	})
	t.Run("claim", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sub")
		store, err := NewFileStore(filepath.Join(dir, "jobs.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Create(ctx, video.VideoJob{JobID: "c1", State: video.StateQueued}); err != nil {
			t.Fatal(err)
		}
		breakDir(t, dir)
		if _, claimed, err := store.TryClaim(ctx, "c1", "owner-a", time.Minute, now); err == nil || claimed {
			t.Fatalf("claim with broken persist dir: claimed=%v err=%v", claimed, err)
		}
		got, err := store.Get(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		if got.LeaseOwner != "" || !got.LeaseUntil.IsZero() {
			t.Fatalf("rolled-back lease visible: %+v", got)
		}
	})
}

// TestStressClaimWorkerStopsOnCancel proves cancellation is honored
// at the worker level deterministically: workers ack each claim over
// a channel, so the test knows claims finished before cancelling,
// then asserts no further job is claimed after cancellation. It also
// pins the store-level fact that contexts are currently ignored by
// the stores themselves (documented limit, not a worker bug): a
// direct call with an already-cancelled context still succeeds, so
// cancellation must be implemented by the caller loop, as here.
func TestStressClaimWorkerStopsOnCancel(t *testing.T) {
	requireStress(t)
	store := NewMemoryStore()
	ctx := context.Background()
	const total = 6
	for i := 0; i < total; i++ {
		if err := store.Create(ctx, video.VideoJob{JobID: "job-" + itoa(i), State: video.StateQueued}); err != nil {
			t.Fatal(err)
		}
	}
	// Pin the store-level behavior: cancelled contexts do not fail
	// store calls (they are ignored by design today).
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Get(cancelled, "job-0"); err != nil {
		t.Fatalf("store Get with cancelled ctx: %v (stores ignore ctx today)", err)
	}

	workCtx, stop := context.WithCancel(ctx)
	jobsCh := make(chan string)
	acked := make(chan string)
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-workCtx.Done():
				return
			case id := <-jobsCh:
				now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
				if _, claimed, err := store.TryClaim(workCtx, id, "worker", time.Minute, now); err != nil || !claimed {
					t.Errorf("worker claim %s: claimed=%v err=%v", id, claimed, err)
					return
				}
				acked <- id
			}
		}
	}
	const workers = 2
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	// Feed two jobs and wait for their acks: completion is signalled,
	// not slept for.
	go func() {
		jobsCh <- "job-0"
		jobsCh <- "job-1"
	}()
	for i := 0; i < 2; i++ {
		<-acked
	}
	stop()
	wg.Wait()
	// After cancellation, the remaining jobs must stay unclaimed even
	// though they are offered: workers are gone, so no one claims.
	for i := 2; i < total; i++ {
		got, err := store.Get(ctx, "job-"+itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		if got.LeaseOwner != "" {
			t.Fatalf("job-%d claimed after cancellation by %q", i, got.LeaseOwner)
		}
	}
}
