package budget

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func mustLedger(t *testing.T, limits []Limit) *Ledger {
	t.Helper()
	l, err := NewLedger(limits)
	if err != nil {
		t.Fatalf("NewLedger(%v): %v", limits, err)
	}
	return l
}

func TestNewLedgerRejectsInvalidLimits(t *testing.T) {
	for _, limits := range [][]Limit{
		{{Key: "", Amount: 100}},
		{{Key: "k", Amount: -1}},
		{{Key: "ok", Amount: 10}, {Key: "", Amount: 1}},
	} {
		if _, err := NewLedger(limits); err == nil {
			t.Fatalf("NewLedger(%v) should reject invalid limit", limits)
		}
	}
	// A zero limit is accepted: it is a valid "deny all" budget.
	l := mustLedger(t, []Limit{{Key: "zero", Amount: 0}})
	if _, err := l.Reserve("r1", "zero", 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("zero-limit reserve err=%v, want ErrBudgetExceeded", err)
	}
	used, reserved, ok := l.Usage("zero")
	if !ok || used != 0 || reserved != 0 {
		t.Fatalf("zero-limit usage=%d/%d ok=%v", used, reserved, ok)
	}
}

func TestReserveRejectsInvalidArguments(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	for _, tc := range []struct {
		id, key string
		amount  int64
	}{
		{"", "k", 10},
		{"r", "", 10},
		{"r", "k", 0},
		{"r", "k", -5},
	} {
		if _, err := l.Reserve(tc.id, tc.key, tc.amount); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("Reserve(%q,%q,%d) err=%v, want ErrBudgetExceeded", tc.id, tc.key, tc.amount, err)
		}
	}
	// Rejected reservations leave no trace: the same id stays usable.
	if _, err := l.Reserve("r", "k", 10); err != nil {
		t.Fatalf("Reserve after rejections: %v", err)
	}
}

func TestReserveDuplicateIDReturnsClosed(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Reserve("dup", "k", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("dup", "k", 10); !errors.Is(err, ErrReservationClosed) {
		t.Fatalf("duplicate Reserve err=%v, want ErrReservationClosed", err)
	}
	// The duplicate did not double-count the reservation.
	if _, reserved, _ := l.Usage("k"); reserved != 30 {
		t.Fatalf("reserved=%d, want 30", reserved)
	}
}

func TestReserveUnknownKeyAndOverLimit(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Reserve("a", "missing", 10); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("unknown key err=%v, want ErrBudgetExceeded", err)
	}
	if _, err := l.Reserve("b", "k", 101); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over-limit err=%v, want ErrBudgetExceeded", err)
	}
	if _, err := l.Reserve("c", "k", 100); err != nil {
		t.Fatalf("exact-limit reserve should succeed: %v", err)
	}
	// Fully reserved: any further amount exceeds the remainder.
	if _, err := l.Reserve("d", "k", 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("fully-reserved err=%v, want ErrBudgetExceeded", err)
	}
}

// TestRemainingBudgetReservedExceedsRemainder drives remainingBudget's
// second overflow guard: reserved already exceeds limit-used, so the
// remainder is negative even though used alone is under the limit.
func TestRemainingBudgetReservedExceedsRemainder(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Reserve("a", "k", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("b", "k", 30); err != nil {
		t.Fatal(err)
	}
	// Settle more than reserved: used jumps past the reserved amount,
	// leaving used=80 with reserved=30 still outstanding.
	if _, err := l.Settle("a", 80); err != nil {
		t.Fatal(err)
	}
	// used=80, reserved=30, limit=100: remaining is -1, not 20-30.
	if _, err := l.Reserve("c", "k", 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("reserve past remainder err=%v, want ErrBudgetExceeded", err)
	}
	used, reserved, _ := l.Usage("k")
	if used != 80 || reserved != 30 {
		t.Fatalf("used=%d reserved=%d, want 80/30", used, reserved)
	}
}

// TestSettleOverLimitMakesUsedExceedLimit documents that Settle does not
// enforce the limit (actual spend may exceed the estimate); the next
// Reserve then fails via remainingBudget's used>limit guard.
func TestSettleOverLimitMakesUsedExceedLimit(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Reserve("a", "k", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle("a", 150); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("b", "k", 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("reserve after over-limit settle err=%v, want ErrBudgetExceeded", err)
	}
}

func TestSettleClampsNegativeActualToZero(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Reserve("a", "k", 10); err != nil {
		t.Fatal(err)
	}
	alerts, err := l.Settle("a", -5)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("Settle(-5) alerts=%v err=%v", alerts, err)
	}
	used, reserved, _ := l.Usage("k")
	if used != 0 || reserved != 0 {
		t.Fatalf("used=%d reserved=%d after negative settle, want 0/0", used, reserved)
	}
}

func TestSettleAndReleaseUnknownReservation(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Settle("ghost", 10); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("Settle unknown err=%v, want ErrReservationNotFound", err)
	}
	if err := l.Release("ghost"); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("Release unknown err=%v, want ErrReservationNotFound", err)
	}
}

func TestReleaseClosedReservation(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Reserve("a", "k", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle("a", 10); err != nil {
		t.Fatal(err)
	}
	if err := l.Release("a"); !errors.Is(err, ErrReservationClosed) {
		t.Fatalf("Release settled err=%v, want ErrReservationClosed", err)
	}
	// Releasing twice also reports closed, and frees nothing twice.
	if err := l.Release("a"); !errors.Is(err, ErrReservationClosed) {
		t.Fatalf("Release settled again err=%v, want ErrReservationClosed", err)
	}
	if used, reserved, _ := l.Usage("k"); used != 10 || reserved != 0 {
		t.Fatalf("used=%d reserved=%d, want 10/0", used, reserved)
	}
}

func TestUsageUnknownKey(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	used, reserved, ok := l.Usage("missing")
	if ok || used != 0 || reserved != 0 {
		t.Fatalf("Usage(missing)=%d/%d ok=%v, want 0/0 false", used, reserved, ok)
	}
}

// TestThresholdReachedRoundingBoundary pins the exact ceil semantics of
// thresholdReached: with limit 101 the 50% threshold needs 51, not 50.
func TestThresholdReachedRoundingBoundary(t *testing.T) {
	// Just below the boundary: 50 of 101 is below ceil(101*50/100)=51.
	l := mustLedger(t, []Limit{{Key: "k", Amount: 101}})
	if _, err := l.Reserve("a", "k", 50); err != nil {
		t.Fatal(err)
	}
	alerts, err := l.Settle("a", 50)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("Settle(50/101) alerts=%v err=%v, want none", alerts, err)
	}
	// Exactly at the boundary: 51 fires the 50% alert and nothing else.
	l = mustLedger(t, []Limit{{Key: "k", Amount: 101}})
	if _, err := l.Reserve("b", "k", 51); err != nil {
		t.Fatal(err)
	}
	alerts, err = l.Settle("b", 51)
	if err != nil || len(alerts) != 1 || alerts[0].Threshold != 50 {
		t.Fatalf("Settle(51/101) alerts=%v err=%v, want [50]", alerts, err)
	}
	// 100 of 101 fires 50/80/95 but not 100 (needs 101).
	l = mustLedger(t, []Limit{{Key: "k", Amount: 101}})
	if _, err := l.Reserve("c", "k", 100); err != nil {
		t.Fatal(err)
	}
	alerts, err = l.Settle("c", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := []int{}
	for _, a := range alerts {
		got = append(got, a.Threshold)
		if a.Key != "k" || a.Limit != 101 || a.Used != 100 {
			t.Fatalf("alert fields wrong: %+v", a)
		}
	}
	if len(got) != 3 || got[0] != 50 || got[1] != 80 || got[2] != 95 {
		t.Fatalf("Settle(100/101) thresholds=%v, want [50 80 95]", got)
	}
}

// TestAlertsFireOncePerThreshold proves alerts deduplicate across settles:
// crossing 50% alerts once, and a later settle crossing 80% alerts only
// for 80%, never re-firing 50%.
func TestAlertsFireOncePerThreshold(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 100}})
	if _, err := l.Reserve("a", "k", 60); err != nil {
		t.Fatal(err)
	}
	alerts, err := l.Settle("a", 60)
	if err != nil || len(alerts) != 1 || alerts[0].Threshold != 50 {
		t.Fatalf("first settle alerts=%v err=%v, want [50]", alerts, err)
	}
	if _, err := l.Reserve("b", "k", 30); err != nil {
		t.Fatal(err)
	}
	alerts, err = l.Settle("b", 25)
	if err != nil || len(alerts) != 1 || alerts[0].Threshold != 80 {
		t.Fatalf("second settle alerts=%v err=%v, want [80]", alerts, err)
	}
}

// TestAlertsLockedZeroLimitNeverAlerts is a white-box test of the defensive
// guard in alertsLocked: a zero limit can never own a reservation through
// the public API (Reserve rejects it), so the guard is only reachable with
// a hand-built ledger. It must still return nil rather than firing every
// threshold (thresholdReached with limit 0 needs 0).
func TestAlertsLockedZeroLimitNeverAlerts(t *testing.T) {
	l := &Ledger{
		limits:       map[string]Limit{"k": {Key: "k", Amount: 0}},
		used:         map[string]int64{"k": 0},
		reserved:     map[string]int64{},
		reservations: map[string]Reservation{},
		alerted:      map[string]map[int]bool{},
	}
	if alerts := l.alertsLocked("k"); len(alerts) != 0 {
		t.Fatalf("zero-limit alerts=%v, want none", alerts)
	}
}

func TestSaturatingAddPinsAtMaxInt64(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: math.MaxInt64}})
	// Reserve both before settling: once used hits MaxInt64, remaining
	// budget is 0 and no further reservation is possible.
	if _, err := l.Reserve("a", "k", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("b", "k", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle("a", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	// A second huge settle must pin used at MaxInt64, never wrap negative.
	if _, err := l.Settle("b", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	used, reserved, _ := l.Usage("k")
	if used != math.MaxInt64 || reserved != 0 {
		t.Fatalf("used=%d reserved=%d, want MaxInt64/0 (must not wrap)", used, reserved)
	}
}

// TestConcurrentSettleAndReleaseKeepsAccountingExact settles and releases
// distinct reservations from many goroutines and requires the final
// accounting to be exact: no lost updates, no double frees.
func TestConcurrentSettleAndReleaseKeepsAccountingExact(t *testing.T) {
	l := mustLedger(t, []Limit{{Key: "k", Amount: 1000}})
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := l.Reserve(string(rune('a'+i%26))+string(rune('0'+i/26)), "k", 10); err != nil {
			t.Fatalf("setup Reserve %d: %v", i, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a'+i%26)) + string(rune('0'+i/26))
			if i%2 == 0 {
				if _, err := l.Settle(id, 10); err != nil {
					t.Errorf("Settle %s: %v", id, err)
				}
			} else {
				if err := l.Release(id); err != nil {
					t.Errorf("Release %s: %v", id, err)
				}
			}
		}(i)
	}
	wg.Wait()
	used, reserved, _ := l.Usage("k")
	if used != 250 || reserved != 0 {
		t.Fatalf("used=%d reserved=%d, want 250/0", used, reserved)
	}
}
