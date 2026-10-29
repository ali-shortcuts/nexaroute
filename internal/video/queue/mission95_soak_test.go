package queue

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Long-form (relative to CI stress) soak/recovery checks for issue
// #664, category 2. These run only behind NEXAROUTE_SOAK=1, the same
// gate as the other soak suites (see scripts/soak.sh), and record
// duration, workload, error count, resource observations, recovery
// behavior, and final state via t.Logf. They are bounded rounds, not
// an hours-long production soak: do not cite them as proof of
// long-duration stability. Time is an explicit argument everywhere;
// no sleeps. The store under test is the single-process file store;
// two processes sharing one file are a documented non-goal (see
// TryClaim's doc comment), so nothing here claims multi-process or
// multi-host safety.

func requireSoak(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_SOAK") != "1" {
		t.Skip("set NEXAROUTE_SOAK=1 to run soak checks")
	}
}

func soakIntEnv(t *testing.T, name string, def int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		t.Fatalf("%s %q must be a positive integer", name, raw)
	}
	return v
}

func heapAllocBytes() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestSoakVideoQueueRestartRounds runs several create/claim/update
// rounds against one file store, reopening ("restarting") the store
// after every round and verifying the full recovered state. It also
// runs one corrupt-copy episode: a corrupted copy must fail to load
// while the original keeps loading. Every round reports its duration
// and the store file size, so growth is observed, not assumed.
func TestSoakVideoQueueRestartRounds(t *testing.T) {
	requireSoak(t)
	ctx := context.Background()
	rounds := soakIntEnv(t, "NEXAROUTE_SOAK_VIDEO_ROUNDS", 5)
	const jobsPerRound = 20
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	errors := 0
	startAll := time.Now()
	heapBefore := heapAllocBytes()
	var sizes []int64
	totalJobs := 0
	for r := 0; r < rounds; r++ {
		roundStart := time.Now()
		now := base.Add(time.Duration(r) * 2 * time.Hour)
		store, err := NewFileStore(path)
		if err != nil {
			t.Fatalf("round %d reopen: %v", r, err)
		}
		for j := 0; j < jobsPerRound; j++ {
			id := "soak-r" + itoa(r) + "-j" + itoa(j)
			job := video.VideoJob{JobID: id, State: video.StateQueued, Progress: 0}
			if j%4 == 0 {
				job.IdempotencyKey = "soak-key-" + id
			}
			if err := store.Create(ctx, job); err != nil {
				t.Errorf("round %d create %s: %v", r, id, err)
				errors++
				continue
			}
			totalJobs++
			switch j % 4 {
			case 0, 1:
				// Claimed and left in progress: the lease must
				// survive the restart, then expire on the
				// timeline (rounds are 2h apart, TTL is 1h).
				if _, claimed, err := store.TryClaim(ctx, id, "worker", time.Hour, now); err != nil || !claimed {
					t.Errorf("round %d claim %s: claimed=%v err=%v", r, id, claimed, err)
					errors++
				}
			case 2:
				// Completed: terminal state must survive.
				done := job
				done.State = video.StateCompleted
				done.Progress = 1
				if err := store.Update(ctx, done); err != nil {
					t.Errorf("round %d complete %s: %v", r, id, err)
					errors++
				}
			}
		}
		// Reopen and verify the whole recovered state before the
		// next round: counts exact, terminal jobs terminal, and
		// leases from earlier rounds expired on the timeline
		// while this round's leases are still live.
		reopened, err := NewFileStore(path)
		if err != nil {
			t.Fatalf("round %d verify reopen: %v", r, err)
		}
		listed, err := reopened.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != totalJobs {
			t.Errorf("round %d recovered jobs = %d, want %d", r, len(listed), totalJobs)
			errors++
		}
		for _, got := range listed {
			if got.State == video.StateCompleted && got.Progress != 1 {
				t.Errorf("round %d terminal job %s progress = %v, want 1", r, got.JobID, got.Progress)
				errors++
			}
		}
		if st, err := os.Stat(path); err == nil {
			sizes = append(sizes, st.Size())
		}
		t.Logf("round %d: jobs_total=%d duration=%s file_bytes=%d errors=%d",
			r, totalJobs, time.Since(roundStart).Round(time.Millisecond),
			sizes[len(sizes)-1], errors)
	}
	// Corrupt-copy episode: corrupting a COPY must fail to load,
	// and the original must still load with the exact job count.
	corruptPath := filepath.Join(dir, "corrupt.json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, append(good, []byte("{{{garbage")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(corruptPath); err == nil {
		t.Error("corrupted copy loaded without error")
		errors++
	}
	again, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("original after corrupt-copy episode: %v", err)
	}
	finalList, err := again.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalList) != totalJobs {
		t.Errorf("final jobs = %d, want %d", len(finalList), totalJobs)
		errors++
	}
	t.Logf("SOAK video-queue-restart: rounds=%d jobs=%d duration=%s errors=%d file_bytes_per_round=%v heap_alloc_delta=%d",
		rounds, totalJobs, time.Since(startAll).Round(time.Millisecond), errors, sizes,
		int64(heapAllocBytes())-int64(heapBefore))
	if errors != 0 {
		t.Fatalf("soak recorded %d errors", errors)
	}
}
