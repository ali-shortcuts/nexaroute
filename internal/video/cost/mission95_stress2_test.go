package cost

import (
	"math"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Second wave of bounded, deterministic stress checks for issue #664.
// These extend mission95_stress_test.go (totals + duplicate/conflicting
// settle) with the cases that wave did not cover: same-job settle races,
// double reserve, fixed-seed workloads, invalid amounts, release ordering,
// and accounted (unpruned) growth. All coordination is via start gates
// and mutexes; no sleeps, no wall-clock dependence. Integer-valued
// amounts keep float64 sums exact. requireStress and itoa are defined
// in mission95_stress_test.go in this package.

// stressSeed reads a fixed seed for the deterministic workload test.
// The default is fixed, so a bare stress run is reproducible; the
// operator may vary it explicitly to widen the workload.
func stressSeed(t *testing.T) int64 {
	t.Helper()
	raw := os.Getenv("NEXAROUTE_STRESS_SEED")
	if raw == "" {
		return 42
	}
	seed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("NEXAROUTE_STRESS_SEED %q is not an integer: %v", raw, err)
	}
	return seed
}

func stressEstimate(est, upper float64) video.CostEstimate {
	return video.CostEstimate{PriceKnown: true, EstimatedUSD: est, UpperBoundUSD: upper}
}

// TestStressLedgerSameJobSettleRace proves a double-settlement race
// cannot double-count: many goroutines settle the same reserved job
// with distinct values behind one start gate. Exactly one wins, the
// rest get a conflict error, spent equals the winner's value, and a
// later same-value settle is still idempotent.
func TestStressLedgerSameJobSettleRace(t *testing.T) {
	requireStress(t)
	var ledger Ledger
	if err := ledger.ReserveForJob("race-job", stressEstimate(1, 100), 0); err != nil {
		t.Fatal(err)
	}
	const racers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, racers)
	won := make([]float64, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			value := float64(i + 1)
			if err := ledger.SettleActual("race-job", value, 1); err == nil {
				won[i] = value
			} else {
				errs[i] = err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	var spent float64
	for i := 0; i < racers; i++ {
		if errs[i] == nil {
			winners++
			spent = won[i]
		}
	}
	if winners != 1 {
		t.Fatalf("settle winners = %d, want exactly 1", winners)
	}
	if got := ledger.Spent(); got != spent || spent < 1 || spent > racers {
		t.Fatalf("spent = %v, winner value = %v, want spent == winner in [1,%d]", got, spent, racers)
	}
	if err := ledger.SettleActual("race-job", spent, 1); err != nil {
		t.Fatalf("same-value re-settle after race: %v", err)
	}
	if got := ledger.Spent(); got != spent {
		t.Fatalf("spent after idempotent re-settle = %v, want %v", got, spent)
	}
}

// TestStressLedgerDoubleReserveCountsOnce proves concurrent duplicate
// reservations of one job are idempotent: every caller succeeds, but
// the reservation is counted exactly once and produces one entry.
func TestStressLedgerDoubleReserveCountsOnce(t *testing.T) {
	requireStress(t)
	var ledger Ledger
	const racers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = ledger.ReserveForJob("dup-job", stressEstimate(3, 5), 0)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reserve racer %d: %v", i, err)
		}
	}
	if got := ledger.Reserved(); got != 5 {
		t.Fatalf("reserved = %v, want 5 (counted once)", got)
	}
	if got := len(ledger.Entries()); got != 1 {
		t.Fatalf("entries = %d, want 1", got)
	}
	if err := ledger.SettleActual("dup-job", 3, 3); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Reserved(); got != 0 {
		t.Fatalf("reserved after settle = %v, want 0", got)
	}
	if got := ledger.Spent(); got != 3 {
		t.Fatalf("spent = %v, want 3", got)
	}
}

// TestStressLedgerFixedSeedWorkload runs a seeded, per-job operation
// chain workload concurrently (disjoint jobs, so totals are exact) and
// replays the same chains sequentially, proving both executions agree
// with a model computed from the seed. The seed fixes the workload,
// not the goroutine schedule; the assertions are schedule-invariant.
func TestStressLedgerFixedSeedWorkload(t *testing.T) {
	requireStress(t)
	seed := stressSeed(t)
	const jobs = 50
	rng := rand.New(rand.NewSource(seed))
	chains := make([]stressChain, jobs)
	var wantSpent, wantEstimated, wantReserved float64
	for j := 0; j < jobs; j++ {
		est := float64(rng.Intn(5) + 1)
		upper := est + 1
		c := stressChain{id: "seed-" + itoa(j), est: est, upper: upper}
		switch rng.Intn(4) {
		case 0:
			c.action = "settle"
			c.actual = est
			wantSpent += est
		case 1:
			c.action = "release"
		case 2:
			c.action = "estimate"
			wantEstimated += est
		default:
			c.action = "reserved"
			wantReserved += upper
		}
		chains[j] = c
	}
	run := func(l *Ledger, concurrent bool) {
		if !concurrent {
			for _, c := range chains {
				applyChain(t, l, c)
			}
			return
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, c := range chains {
			wg.Add(1)
			go func(c stressChain) {
				defer wg.Done()
				<-start
				applyChain(t, l, c)
			}(c)
		}
		close(start)
		wg.Wait()
	}
	var sequential, concurrent Ledger
	run(&sequential, false)
	run(&concurrent, true)
	for name, l := range map[string]*Ledger{"sequential": &sequential, "concurrent": &concurrent} {
		if got := l.Spent(); got != wantSpent {
			t.Fatalf("%s spent = %v, want %v (seed %d)", name, got, wantSpent, seed)
		}
		if got := l.EstimatedSpent(); got != wantEstimated {
			t.Fatalf("%s estimated = %v, want %v (seed %d)", name, got, wantEstimated, seed)
		}
		if got := l.Reserved(); got != wantReserved {
			t.Fatalf("%s reserved = %v, want %v (seed %d)", name, got, wantReserved, seed)
		}
		if got := len(l.Entries()); got != jobs {
			t.Fatalf("%s entries = %d, want %d", name, got, jobs)
		}
	}
}

// stressChain is one job's deterministic operation chain for the
// fixed-seed workload test.
type stressChain struct {
	id     string
	est    float64
	upper  float64
	action string // settle | release | estimate | reserved
	actual float64
}

func applyChain(t *testing.T, l *Ledger, c stressChain) {
	t.Helper()
	if err := l.ReserveForJob(c.id, stressEstimate(c.est, c.upper), 0); err != nil {
		t.Errorf("reserve %s: %v", c.id, err)
		return
	}
	switch c.action {
	case "settle":
		if err := l.SettleActual(c.id, c.actual, c.est); err != nil {
			t.Errorf("settle %s: %v", c.id, err)
		}
	case "release":
		l.ReleaseForJob(c.id)
	case "estimate":
		if err := l.SettleEstimate(c.id, c.est); err != nil {
			t.Errorf("settle-estimate %s: %v", c.id, err)
		}
	case "reserved":
	}
}

// TestStressLedgerInvalidAmountsLeaveTotalsUntouched proves invalid
// inputs are rejected without moving any total, under concurrency too.
func TestStressLedgerInvalidAmountsLeaveTotalsUntouched(t *testing.T) {
	requireStress(t)
	var ledger Ledger
	bad := []video.CostEstimate{
		{PriceKnown: false, EstimatedUSD: 1, UpperBoundUSD: 2},
		{PriceKnown: true, EstimatedUSD: -1, UpperBoundUSD: 2},
		{PriceKnown: true, EstimatedUSD: math.NaN(), UpperBoundUSD: 2},
		{PriceKnown: true, EstimatedUSD: 1, UpperBoundUSD: math.Inf(1)},
		{PriceKnown: true, EstimatedUSD: 5, UpperBoundUSD: 2},
	}
	for i, e := range bad {
		if err := ledger.ReserveForJob("bad-"+itoa(i), e, 0); err == nil {
			t.Fatalf("invalid reserve %d accepted", i)
		}
	}
	if err := ledger.ReserveForJob("good", stressEstimate(1, 2), 0); err != nil {
		t.Fatal(err)
	}
	invalidSettles := []float64{-1, math.NaN(), math.Inf(1)}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, v := range invalidSettles {
		wg.Add(1)
		go func(v float64) {
			defer wg.Done()
			<-start
			if err := ledger.SettleActual("good", v, 1); err == nil {
				t.Errorf("invalid settle %v accepted", v)
			}
		}(v)
	}
	close(start)
	wg.Wait()
	if got := ledger.Spent(); got != 0 {
		t.Fatalf("spent = %v, want 0", got)
	}
	if got := ledger.Reserved(); got != 2 {
		t.Fatalf("reserved = %v, want 2 (only the good reservation)", got)
	}
	if got := len(ledger.Entries()); got != 1 {
		t.Fatalf("entries = %d, want 1", got)
	}
}

// TestStressLedgerReleaseThenSettle pins the release/settle ordering:
// a release frees the reservation, a later settle still accounts the
// actual, and a second release is a no-op that never drives reserved
// negative.
func TestStressLedgerReleaseThenSettle(t *testing.T) {
	requireStress(t)
	var ledger Ledger
	if err := ledger.ReserveForJob("rel-job", stressEstimate(2, 4), 0); err != nil {
		t.Fatal(err)
	}
	ledger.ReleaseForJob("rel-job")
	ledger.ReleaseForJob("rel-job")
	if got := ledger.Reserved(); got != 0 {
		t.Fatalf("reserved after double release = %v, want 0", got)
	}
	if err := ledger.SettleActual("rel-job", 2, 2); err != nil {
		t.Fatalf("settle after release: %v", err)
	}
	if got := ledger.Spent(); got != 2 {
		t.Fatalf("spent = %v, want 2", got)
	}
	ledger.ReleaseForJob("rel-job")
	if got := ledger.Reserved(); got != 0 {
		t.Fatalf("reserved after post-settle release = %v, want 0", got)
	}
}

// TestStressLedgerGrowthIsAccounted pins the retention behavior the
// soak observes: settled jobs are NOT pruned, so entries grow exactly
// one per reservation. Any deviation (lost or duplicated entries)
// fails; the absence of pruning itself is documented, not claimed
// away — long-lived ledgers grow linearly with jobs served.
func TestStressLedgerGrowthIsAccounted(t *testing.T) {
	requireStress(t)
	var ledger Ledger
	const jobs = 2000
	for j := 0; j < jobs; j++ {
		id := "growth-" + itoa(j)
		if err := ledger.ReserveForJob(id, stressEstimate(1, 2), 0); err != nil {
			t.Fatal(err)
		}
		if err := ledger.SettleActual(id, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(ledger.Entries()); got != jobs {
		t.Fatalf("entries after %d settled jobs = %d, want exactly %d (no pruning, no loss)", jobs, got, jobs)
	}
	if got := ledger.Spent(); got != jobs {
		t.Fatalf("spent = %v, want %d", got, jobs)
	}
	if got := ledger.Reserved(); got != 0 {
		t.Fatalf("reserved = %v, want 0", got)
	}
}
