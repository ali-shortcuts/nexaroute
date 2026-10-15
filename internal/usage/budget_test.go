package usage

import (
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// M5-6 budget ledger properties under concurrency: for admitted
// reservations, committed+reserved never exceeds the limit; every
// reservation settles or expires exactly once (conservation).

func TestBudgetLedgerConcurrentInvariants(t *testing.T) {
	ledger := NewBudgetLedger()
	const limit = 100.0
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	settled := 0.0
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 200; i++ {
				amount := rng.Float64() * 2
				id, ok := ledger.Reserve("k", limit, amount, now)
				if !ok {
					continue
				}
				committed, reserved := ledger.Snapshot("k", now)
				if committed+reserved > limit+1e-6 {
					t.Errorf("budget exceeded: committed=%v reserved=%v limit=%v", committed, reserved, limit)
					return
				}
				actual := amount * rng.Float64()
				ledger.Settle("k", id, actual)
				mu.Lock()
				settled += actual
				mu.Unlock()
			}
		}(int64(g + 1))
	}
	wg.Wait()
	committed, reserved := ledger.Snapshot("k", now)
	if reserved != 0 {
		t.Fatalf("reserved leaked: %v", reserved)
	}
	// committed is summed inside the ledger in lock-acquisition order while the
	// test accumulates settled in goroutine-completion order; float64 addition
	// is order-sensitive at the last bit (~1e-14 observed). Conservation is
	// the invariant under test, so compare with a tolerance well below any
	// meaningful money epsilon.
	if diff := math.Abs(committed - settled); diff > 1e-9 {
		t.Fatalf("committed %v != settled sum %v (diff %v)", committed, settled, diff)
	}
}

func TestBudgetLedgerExpiryCommitsEstimate(t *testing.T) {
	ledger := NewBudgetLedger()
	now := time.Now()
	id, ok := ledger.Reserve("k", 10, 3, now)
	if !ok || id == 0 {
		t.Fatalf("reserve failed: %v %v", id, ok)
	}
	// Before TTL: still reserved, not committed.
	if c, r := ledger.Snapshot("k", now.Add(time.Minute)); c != 0 || r != 3 {
		t.Fatalf("pre-expiry = (%v,%v), want (0,3)", c, r)
	}
	// After TTL: committed at the estimate, hold released.
	if c, r := ledger.Snapshot("k", now.Add(BudgetReservationTTL+time.Minute)); c != 3 || r != 0 {
		t.Fatalf("post-expiry = (%v,%v), want (3,0)", c, r)
	}
	// A late settle is a no-op (expiry won).
	ledger.Settle("k", id, 1)
	if c, _ := ledger.Snapshot("k", now.Add(BudgetReservationTTL+2*time.Minute)); c != 3 {
		t.Fatalf("late settle changed committed: %v", c)
	}
}

func TestBudgetLedgerAdmissionAndRelease(t *testing.T) {
	ledger := NewBudgetLedger()
	now := time.Now()
	if _, ok := ledger.Reserve("k", 5, 6, now); ok {
		t.Fatal("over-limit reservation admitted")
	}
	id, ok := ledger.Reserve("k", 5, 5, now)
	if !ok {
		t.Fatal("at-limit reservation rejected")
	}
	if _, ok := ledger.Reserve("k", 5, 0.01, now); ok {
		t.Fatal("reservation beyond a full budget admitted")
	}
	ledger.Release("k", id)
	if _, ok := ledger.Reserve("k", 5, 5, now); !ok {
		t.Fatal("reservation after release rejected")
	}
}

// Tracker property: concurrent RecordEx sums reconcile exactly —
// totals equal row sums, per-version costs sum to recorded cost.
func TestTrackerConcurrentRecordExReconciles(t *testing.T) {
	tracker := New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g + 7)))
			for i := 0; i < 250; i++ {
				version := "v1"
				if i%3 == 0 {
					version = "v2"
				}
				tracker.RecordEx("p/m", Counts{Input: 10, Output: 5, CacheRead: 3, CacheWrite: 1}, 0.25, version)
				_ = rng.Intn(2)
			}
		}(g)
	}
	wg.Wait()
	snap := tracker.Snapshot(nil)
	if snap.TotalRequests != 2000 || snap.TotalPromptTokens != 20000 || snap.TotalCompletionTokens != 10000 {
		t.Fatalf("totals wrong: %+v", snap)
	}
	if snap.TotalCacheReadTokens != 6000 || snap.TotalCacheWriteTokens != 2000 {
		t.Fatalf("cache totals wrong: read=%d write=%d", snap.TotalCacheReadTokens, snap.TotalCacheWriteTokens)
	}
	if len(snap.ByDeployment) != 1 {
		t.Fatalf("rows: %d", len(snap.ByDeployment))
	}
	row := snap.ByDeployment[0]
	if row.RecordedCostUSD != 500 {
		t.Fatalf("recorded cost = %v, want 500", row.RecordedCostUSD)
	}
	sum := 0.0
	for _, v := range row.CostByVersion {
		sum += v
	}
	if sum != row.RecordedCostUSD || snap.TotalRecordedCostUSD != 500 {
		t.Fatalf("version split does not reconcile: sum=%v row=%v total=%v", sum, row.RecordedCostUSD, snap.TotalRecordedCostUSD)
	}
	if row.EstimatedCost != 500 {
		t.Fatalf("estimated cost should equal recorded when fully priced: %v", row.EstimatedCost)
	}
}
