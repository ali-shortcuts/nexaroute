package queue

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// requireStress skips unless the bounded stress harness is running, matching
// the TestStress convention used by the other stress suites (see
// scripts/stress.sh). All claim outcomes here are deterministic: no sleeps,
// no wall-clock dependence — lease expiry is exercised by advancing the
// explicit `now` argument past the TTL.
func requireStress(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
}

// TestStressMemoryStoreSingleClaimWinner proves that when many goroutines
// race to lease the same job, exactly one wins, losers observe claimed=false
// without an error, the winner can renew, a foreign owner is blocked while
// the lease is live, and a new owner wins after the lease expires.
func TestStressMemoryStoreSingleClaimWinner(t *testing.T) {
	requireStress(t)
	assertSingleClaimWinner(t, NewMemoryStore())
}

// TestStressFileStoreSingleClaimWinner is the same single-winner property
// against the durable file store. Scope note, stated honestly: the file
// store's check-and-set is atomic within one process (mutex-guarded);
// this test proves the in-process single-winner property only, not
// cross-process file locking.
func TestStressFileStoreSingleClaimWinner(t *testing.T) {
	requireStress(t)
	store, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertSingleClaimWinner(t, store)
}

func assertSingleClaimWinner(t *testing.T, store JobStore) {
	t.Helper()
	ctx := context.Background()
	if err := store.Create(ctx, video.VideoJob{JobID: "job-1"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	const racers = 32
	var winners atomic.Int32
	var winnerOwner atomic.Value
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := "worker-" + itoa(i)
			_, claimed, err := store.TryClaim(ctx, "job-1", owner, time.Minute, now)
			if err != nil {
				t.Errorf("claim by %s: %v", owner, err)
				return
			}
			if claimed {
				winners.Add(1)
				winnerOwner.Store(owner)
			}
		}(i)
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", got)
	}
	winner := winnerOwner.Load().(string)

	// The winner renews; a foreign owner is blocked while the lease is live.
	if _, claimed, err := store.TryClaim(ctx, "job-1", winner, time.Minute, now); err != nil || !claimed {
		t.Fatalf("winner renewal: claimed=%v err=%v, want claimed=true err=nil", claimed, err)
	}
	if _, claimed, err := store.TryClaim(ctx, "job-1", "intruder", time.Minute, now); err != nil || claimed {
		t.Fatalf("foreign claim during live lease: claimed=%v err=%v, want claimed=false err=nil", claimed, err)
	}
	// Past the TTL the lease is expired and a new owner wins.
	if _, claimed, err := store.TryClaim(ctx, "job-1", "successor", time.Minute, now.Add(2*time.Minute)); err != nil || !claimed {
		t.Fatalf("post-expiry claim: claimed=%v err=%v, want claimed=true err=nil", claimed, err)
	}
}

// TestStressMemoryStoreConcurrentCreateAndClaim proves that concurrent
// creation and self-claim of distinct jobs all succeed and the final list
// holds exactly the created jobs.
func TestStressMemoryStoreConcurrentCreateAndClaim(t *testing.T) {
	requireStress(t)
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	const workers = 16
	const jobsPerWorker = 25
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < jobsPerWorker; j++ {
				id := "job-" + itoa(w) + "-" + itoa(j)
				if err := store.Create(ctx, video.VideoJob{JobID: id}); err != nil {
					t.Errorf("create %s: %v", id, err)
					return
				}
				if _, claimed, err := store.TryClaim(ctx, id, "worker-"+itoa(w), time.Minute, now); err != nil || !claimed {
					t.Errorf("claim %s: claimed=%v err=%v", id, claimed, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	jobs, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(jobs); got != workers*jobsPerWorker {
		t.Fatalf("listed jobs = %d, want %d", got, workers*jobsPerWorker)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
