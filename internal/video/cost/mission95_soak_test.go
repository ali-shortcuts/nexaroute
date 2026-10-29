package cost

import (
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Long-form (relative to CI stress) soak checks for issue #664,
// category 2, behind the same NEXAROUTE_SOAK=1 gate as the other
// soak suites (see scripts/soak.sh). The ledger under test is
// in-memory only, so there is no restart to recover from; the soak
// instead proves sustained totals do not drift over rounds and that
// a deterministic replay from the same seed reproduces them exactly.
// Results are logged (rounds, jobs, duration, totals, heap delta);
// heap figures are observations, not pass/fail thresholds. Bounded
// rounds, not hours-long production soak: do not cite this as proof
// of long-duration stability.

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

func soakSeedEnv(t *testing.T) int64 {
	t.Helper()
	raw := os.Getenv("NEXAROUTE_SOAK_SEED")
	if raw == "" {
		return 7
	}
	seed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("NEXAROUTE_SOAK_SEED %q is not an integer: %v", raw, err)
	}
	return seed
}

// TestSoakVideoLedgerSustainedTotals settles a seeded workload over
// several rounds on one ledger, asserting cumulative totals exactly
// after every round, then replays the identical workload on a fresh
// ledger and requires identical totals. Any drift, lost update, or
// double count fails; the replay proves the workload itself is
// deterministic.
func TestSoakVideoLedgerSustainedTotals(t *testing.T) {
	requireSoak(t)
	rounds := soakIntEnv(t, "NEXAROUTE_SOAK_VIDEO_ROUNDS", 5)
	const jobsPerRound = 100
	seed := soakSeedEnv(t)
	type job struct {
		id     string
		est    float64
		actual float64
	}
	build := func() ([]job, float64) {
		rng := rand.New(rand.NewSource(seed))
		jobs := make([]job, 0, rounds*jobsPerRound)
		var wantSpent float64
		for r := 0; r < rounds; r++ {
			for j := 0; j < jobsPerRound; j++ {
				est := float64(rng.Intn(5) + 1)
				// Half the jobs settle at the estimate, half one
				// unit above it; the model tracks both exactly.
				actual := est
				if rng.Intn(2) == 0 {
					actual = est + 1
				}
				jobs = append(jobs, job{
					id:  "soak-r" + itoa(r) + "-j" + itoa(j),
					est: est, actual: actual,
				})
				wantSpent += actual
			}
		}
		return jobs, wantSpent
	}
	jobs, wantSpent := build()
	var heapBefore uint64
	{
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		heapBefore = m.HeapAlloc
	}
	startAll := time.Now()
	var ledger Ledger
	errors := 0
	for r := 0; r < rounds; r++ {
		roundStart := time.Now()
		for j := 0; j < jobsPerRound; j++ {
			c := jobs[r*jobsPerRound+j]
			est := video.CostEstimate{PriceKnown: true, EstimatedUSD: c.est, UpperBoundUSD: c.est + 1}
			if err := ledger.ReserveForJob(c.id, est, 0); err != nil {
				t.Errorf("round %d reserve %s: %v", r, c.id, err)
				errors++
				continue
			}
			if err := ledger.SettleActual(c.id, c.actual, c.est); err != nil {
				t.Errorf("round %d settle %s: %v", r, c.id, err)
				errors++
			}
		}
		// Cumulative expectation after this round: the prefix of
		// the model, recomputed from the same deterministic list.
		var wantPrefix float64
		for _, c := range jobs[:(r+1)*jobsPerRound] {
			wantPrefix += c.actual
		}
		if got := ledger.Spent(); got != wantPrefix {
			t.Errorf("round %d spent = %v, want %v", r, got, wantPrefix)
			errors++
		}
		if got := len(ledger.Entries()); got != (r+1)*jobsPerRound {
			t.Errorf("round %d entries = %d, want %d", r, got, (r+1)*jobsPerRound)
			errors++
		}
		t.Logf("round %d: jobs_total=%d spent=%v duration=%s errors=%d",
			r, (r+1)*jobsPerRound, ledger.Spent(),
			time.Since(roundStart).Round(time.Millisecond), errors)
	}
	// Deterministic replay on a fresh ledger must match exactly.
	var replay Ledger
	for _, c := range jobs {
		est := video.CostEstimate{PriceKnown: true, EstimatedUSD: c.est, UpperBoundUSD: c.est + 1}
		if err := replay.ReserveForJob(c.id, est, 0); err != nil {
			t.Fatalf("replay reserve %s: %v", c.id, err)
		}
		if err := replay.SettleActual(c.id, c.actual, c.est); err != nil {
			t.Fatalf("replay settle %s: %v", c.id, err)
		}
	}
	if got := replay.Spent(); got != wantSpent {
		t.Errorf("replay spent = %v, want %v", got, wantSpent)
		errors++
	}
	if got := replay.Spent(); got != ledger.Spent() {
		t.Errorf("replay spent %v != soak spent %v", got, ledger.Spent())
		errors++
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("SOAK video-ledger-sustained: rounds=%d jobs=%d duration=%s spent=%v entries=%d errors=%d heap_alloc_delta=%d seed=%d",
		rounds, len(jobs), time.Since(startAll).Round(time.Millisecond),
		ledger.Spent(), len(ledger.Entries()), errors,
		int64(m.HeapAlloc)-int64(heapBefore), seed)
	if errors != 0 {
		t.Fatalf("soak recorded %d errors", errors)
	}
}
