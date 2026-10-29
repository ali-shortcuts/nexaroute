package cost

import (
	"sync"
	"testing"
)

// Reconciliation of an estimate-settled job reports the estimate with no
// actual and no reconciliation: the job has a ledger record, but nothing
// has actually been spent yet.
func TestLedgerReconciliationEstimateSettled(t *testing.T) {
	l := &Ledger{}
	if err := l.SettleEstimate("j1", 2.5); err != nil {
		t.Fatalf("SettleEstimate: %v", err)
	}
	estimate, actual, delta, reconciled, ok := l.Reconciliation("j1")
	if !ok || reconciled || estimate != 2.5 || actual != 0 || delta != 0 {
		t.Fatalf("Reconciliation = (%v,%v,%v,%v,%v), want (2.5,0,0,false,true)",
			estimate, actual, delta, reconciled, ok)
	}
}

// The clamp branches only fire on corrupted in-memory state: the public
// API subtracts exactly the amount it previously added, so totals cannot
// go negative through Reserve/Settle/Release alone. These white-box
// cases pin the clamp contract itself: a total that would go negative
// is floored at zero instead of reporting a negative balance.
func TestLedgerDefensiveClamps(t *testing.T) {
	t.Run("release floors reserved at zero", func(t *testing.T) {
		l := &Ledger{
			reserved: 5,
			byJob: map[string]reservation{
				"j1": {estimate: knownEstimate(10), state: reservedState},
			},
		}
		l.ReleaseForJob("j1")
		if got := l.Reserved(); got != 0 {
			t.Fatalf("Reserved = %v, want 0", got)
		}
		// A second release is a no-op, not a second subtraction.
		l.ReleaseForJob("j1")
		if got := l.Reserved(); got != 0 {
			t.Fatalf("Reserved after second release = %v, want 0", got)
		}
	})

	t.Run("settle actual from reserved floors reserved at zero", func(t *testing.T) {
		l := &Ledger{
			reserved: 3,
			byJob: map[string]reservation{
				"j1": {estimate: knownEstimate(10), state: reservedState},
			},
		}
		if err := l.SettleActual("j1", 4, 0); err != nil {
			t.Fatalf("SettleActual: %v", err)
		}
		if got := l.Reserved(); got != 0 {
			t.Fatalf("Reserved = %v, want 0", got)
		}
		if got := l.Spent(); got != 4 {
			t.Fatalf("Spent = %v, want 4", got)
		}
	})

	t.Run("settle actual from estimate settled floors estimated spent at zero", func(t *testing.T) {
		l := &Ledger{
			estimatedSpent: 1,
			byJob: map[string]reservation{
				"j1": {estimate: knownEstimate(10), state: estimateSettledState},
			},
		}
		if err := l.SettleActual("j1", 4, 0); err != nil {
			t.Fatalf("SettleActual: %v", err)
		}
		if got := l.EstimatedSpent(); got != 0 {
			t.Fatalf("EstimatedSpent = %v, want 0", got)
		}
		if got := l.Spent(); got != 4 {
			t.Fatalf("Spent = %v, want 4", got)
		}
	})

	t.Run("settle estimate from reserved floors reserved at zero", func(t *testing.T) {
		l := &Ledger{
			reserved: 2,
			byJob: map[string]reservation{
				"j1": {estimate: knownEstimate(10), state: reservedState},
			},
		}
		if err := l.SettleEstimate("j1", 7); err != nil {
			t.Fatalf("SettleEstimate: %v", err)
		}
		if got := l.Reserved(); got != 0 {
			t.Fatalf("Reserved = %v, want 0", got)
		}
		if got := l.EstimatedSpent(); got != 7 {
			t.Fatalf("EstimatedSpent = %v, want 7", got)
		}
	})
}

// Concurrent idempotent operations must account exactly once: the
// ledger lock serializes the check-and-set, so racing callers cannot
// double-count a reservation, double-spend a settlement, or drive a
// total negative with repeated releases.
func TestLedgerConcurrentIdempotentAccounting(t *testing.T) {
	const workers = 32

	t.Run("concurrent reserve counts once", func(t *testing.T) {
		l := &Ledger{}
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = l.ReserveForJob("j1", knownEstimate(5), 0)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("reserve %d: %v", i, err)
			}
		}
		if got := l.Reserved(); got != 5 {
			t.Fatalf("Reserved = %v, want 5", got)
		}
		if got := len(l.Entries()); got != 1 {
			t.Fatalf("Entries = %d, want 1", got)
		}
	})

	t.Run("concurrent settle actual spends once", func(t *testing.T) {
		l := &Ledger{}
		if err := l.ReserveForJob("j1", knownEstimate(5), 0); err != nil {
			t.Fatalf("ReserveForJob: %v", err)
		}
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = l.SettleActual("j1", 7, 5)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("settle %d: %v", i, err)
			}
		}
		if got := l.Spent(); got != 7 {
			t.Fatalf("Spent = %v, want 7", got)
		}
		if got := l.Reserved(); got != 0 {
			t.Fatalf("Reserved = %v, want 0", got)
		}
	})

	t.Run("concurrent release never goes negative", func(t *testing.T) {
		l := &Ledger{}
		if err := l.ReserveForJob("j1", knownEstimate(5), 0); err != nil {
			t.Fatalf("ReserveForJob: %v", err)
		}
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.ReleaseForJob("j1")
			}()
		}
		wg.Wait()
		if got := l.Reserved(); got != 0 {
			t.Fatalf("Reserved = %v, want 0", got)
		}
		if _, _, _, _, ok := l.Reconciliation("j1"); !ok {
			t.Fatalf("Reconciliation missing after concurrent release")
		}
	})
}
