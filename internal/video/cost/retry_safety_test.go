package cost

import (
	"sync"
	"testing"
)

// A replayed settlement is the durable-plane analogue of a retried
// request: the same settlement delivered many times concurrently must
// count exactly once, and a conflicting replay must not silently
// overwrite the first amount. Sequential idempotent/conflicting
// settlement is already pinned by TestLedgerSettlementPathsAndValidation;
// this test pins the concurrent-replay case a retry storm creates.
func TestLedgerConcurrentReplayedSettlementCountsOnce(t *testing.T) {
	ledger := &Ledger{}
	const goroutines = 32
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = ledger.SettleActual("replayed-job", 2.5, 3.0)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replayed settlement %d error=%v", i, err)
		}
	}
	if got := ledger.Spent(); got != 2.5 {
		t.Fatalf("spent=%v, want 2.5: a replayed settlement must count exactly once", got)
	}
	if _, _, _, reconciled, ok := ledger.Reconciliation("replayed-job"); !ok || !reconciled {
		t.Fatalf("replayed job reconciliation ok=%v reconciled=%v, want both true", ok, reconciled)
	}
	// A conflicting replay after the first settlement is an error,
	// not a silent overwrite.
	if err := ledger.SettleActual("replayed-job", 2.6, 3.0); err == nil {
		t.Fatal("conflicting replayed settlement unexpectedly succeeded")
	}
	if got := ledger.Spent(); got != 2.5 {
		t.Fatalf("spent after conflicting replay=%v, want 2.5", got)
	}
}
