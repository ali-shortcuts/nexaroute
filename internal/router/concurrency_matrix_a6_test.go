package router

// v0.12.0 A6 (issue #78, parent #49): concurrency matrix for 1/2/4/8/16
// healthy deployments.
//
// What this test proves, using only production routing code
// (Router.Candidates with strategy "round_robin"):
//   - Distribution: concurrent top-1 selections spread evenly across every
//     healthy deployment (exact round-robin rotation).
//   - No starvation: every healthy deployment serves a positive, exact share.
//   - No data races: the whole matrix runs under `-race`; all test-side
//     aggregation uses per-worker local maps merged under a mutex.
//   - Bounded goroutines: worker count is fixed (8) and NumGoroutine returns
//     to baseline after the matrix (no per-request goroutine leaks; the
//     Router itself spawns no goroutines on the Candidates path).
//
// Determinism contract (acceptance: fixed seed and explicit tolerance):
//   - Fixed seed: a6Seed = 42 (math/rand). It deterministically shuffles the
//     input provider order before building each Router, proving distribution
//     is independent of registration order. The round-robin rotation itself
//     needs no RNG: a fresh Router starts its atomic counter at zero, so a
//     total that is a multiple of N yields exactly total/N top-1 hits per
//     deployment regardless of goroutine interleaving.
//   - Explicit tolerance: a6Tolerance = 0 selections. Observed per-deployment
//     counts must equal expected exactly; any deviation fails. This is sound
//     because the atomic rotation partitions the total exactly when
//     total%N == 0 (guaranteed by construction below).
//
// CI suitability (acceptance: soak stays separate):
//   - Total selections = 5 sizes x 800 = 4000 Candidates calls, each
//     in-process with no sleeps, no network, no httptest servers.
//   - Overall timeout bound a6TestBound = 30s fails fast on regression.

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

const (
	a6Seed       = 42
	a6Tolerance  = 0
	a6Workers    = 8
	a6TotalPerN  = 800
	a6TestBound  = 30 * time.Second
	a6GoroutineS = 5
)

func a6Config(n int, order []int) config.Config {
	cfg := config.Default()
	cfg.Routing.Strategy = "round_robin"
	cfg.Routing.SessionAffinity = false
	cfg.Routing.MaxAttempts = n
	if cfg.Routing.MaxAttempts < 1 {
		cfg.Routing.MaxAttempts = 1
	}
	cfg.Routing.FailureThreshold = 5
	cfg.Providers = nil
	for _, idx := range order {
		id := fmt.Sprintf("p%02d", idx)
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: id, Name: id, Type: "openai_compatible",
			BaseURL: "http://example.invalid", AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{
				ID: "m", Model: "shared-model", Aliases: []string{"coding"},
				Enabled: true, Priority: 0, Weight: 1,
			}},
		})
	}
	cfg.ApplyDefaults()
	// ApplyDefaults must not clobber the strategy under test.
	cfg.Routing.Strategy = "round_robin"
	cfg.Routing.SessionAffinity = false
	return cfg
}

func a6WaitForGoroutines(baseline int, slop int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+slop {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= baseline+slop
}

func TestA6ConcurrencyMatrix(t *testing.T) {
	start := time.Now()
	checkBound := func(phase string) {
		t.Helper()
		if elapsed := time.Since(start); elapsed > a6TestBound {
			t.Fatalf("phase %s exceeded timeout bound %s (elapsed %s)", phase, a6TestBound, elapsed)
		}
	}

	// Warm up the scheduler so the baseline goroutine count is stable.
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for _, n := range []int{1, 2, 4, 8, 16} {
		checkBound(fmt.Sprintf("size-%d", n))
		if a6TotalPerN%n != 0 {
			t.Fatalf("total %d not divisible by N=%d; exact-distribution contract broken", a6TotalPerN, n)
		}
		if a6TotalPerN%a6Workers != 0 {
			t.Fatalf("total %d not divisible by workers=%d", a6TotalPerN, a6Workers)
		}
		expected := a6TotalPerN / n
		perWorker := a6TotalPerN / a6Workers

		// Deterministic input order: seeded shuffle proves rotation does not
		// depend on registration order.
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}
		rng := rand.New(rand.NewSource(a6Seed))
		rng.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })

		cfg := a6Config(n, order)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("N=%d config invalid: %v", n, err)
		}
		hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
		r := New(cfg, hm)
		all := r.All()
		if len(all) != n {
			t.Fatalf("N=%d deployments=%d", n, len(all))
		}
		for _, d := range all {
			hm.RecordSuccess(d.ID, time.Millisecond)
		}
		// All deployments must be simultaneously eligible before the hammer.
		got := r.Candidates(Requirement{Model: "coding"})
		if len(got) != n {
			t.Fatalf("N=%d eligible=%d want %d (all healthy must be routable)", n, len(got), n)
		}

		sizeStart := time.Now()
		counts := make(map[string]int, n)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for w := 0; w < a6Workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := make(map[string]int, n)
				for i := 0; i < perWorker; i++ {
					cands := r.Candidates(Requirement{Model: "coding"})
					if len(cands) != n {
						// Record under mutex via panic-safe path: stash a
						// sentinel and fail after join to keep -race clean.
						mu.Lock()
						counts[fmt.Sprintf("__eligible_%d_vs_%d", len(cands), n)]++
						mu.Unlock()
						continue
					}
					local[cands[0].Deployment.ID]++
				}
				mu.Lock()
				for k, v := range local {
					counts[k] += v
				}
				mu.Unlock()
			}()
		}
		wg.Wait()

		// No starvation + exact distribution within explicit tolerance.
		total := 0
		for k, v := range counts {
			if len(k) > 10 && k[:11] == "__eligible_" {
				t.Fatalf("N=%d candidate-set shrank under concurrency: %v", n, counts)
			}
			total += v
		}
		if total != a6TotalPerN {
			t.Fatalf("N=%d total selections=%d want %d (counts=%v)", n, total, a6TotalPerN, counts)
		}
		if len(counts) != n {
			t.Fatalf("N=%d starvation: only %d of %d deployments served (counts=%v)", n, len(counts), n, counts)
		}
		for _, d := range all {
			c := counts[d.ID]
			if c == 0 {
				t.Fatalf("N=%d starvation: %s received zero of %d selections", n, d.ID, a6TotalPerN)
			}
			diff := c - expected
			if diff < 0 {
				diff = -diff
			}
			if diff > a6Tolerance {
				t.Fatalf("N=%d distribution out of tolerance: %s got %d want %d diff=%d tol=%d (seed=%d workers=%d total=%d)",
					n, d.ID, c, expected, diff, a6Tolerance, a6Seed, a6Workers, a6TotalPerN)
			}
		}
		if !a6WaitForGoroutines(baseline, a6GoroutineS) {
			t.Fatalf("N=%d goroutine leak: baseline=%d now=%d slop=%d", n, baseline, runtime.NumGoroutine(), a6GoroutineS)
		}
		t.Logf("A6 N=%2d workers=%d total=%d expected_each=%d seed=%d tol=%d elapsed=%s goroutines=%d",
			n, a6Workers, a6TotalPerN, expected, a6Seed, a6Tolerance, time.Since(sizeStart).Round(time.Millisecond), runtime.NumGoroutine())
	}

	if elapsed := time.Since(start); elapsed > a6TestBound {
		t.Fatalf("A6 matrix exceeded timeout bound %s (elapsed %s)", a6TestBound, elapsed)
	}
	if !a6WaitForGoroutines(baseline, a6GoroutineS) {
		t.Fatalf("A6 final goroutine leak: baseline=%d now=%d slop=%d", baseline, runtime.NumGoroutine(), a6GoroutineS)
	}
	t.Logf("A6 matrix complete: sizes=[1 2 4 8 16] seed=%d tol=%d workers=%d total_per_size=%d elapsed=%s baseline_goroutines=%d",
		a6Seed, a6Tolerance, a6Workers, a6TotalPerN, time.Since(start).Round(time.Millisecond), baseline)
}
