package cost

import (
	"os"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// requireStress skips unless the bounded stress harness is running, matching
// the TestStress convention used by the other stress suites (see
// scripts/stress.sh). The test itself is deterministic: all amounts are
// integer-valued, so float64 sums are exact.
func requireStress(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
}

// TestStressLedgerConcurrentSettleTotals proves the in-memory ledger keeps
// exact totals when many goroutines reserve and settle distinct jobs at
// once, and that duplicate settlement is idempotent while a conflicting
// settlement is rejected without moving the total.
func TestStressLedgerConcurrentSettleTotals(t *testing.T) {
	requireStress(t)

	var ledger Ledger
	const workers = 32
	const jobsPerWorker = 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			actual := float64(w + 1)
			est := video.CostEstimate{PriceKnown: true, EstimatedUSD: 1, UpperBoundUSD: 2}
			for j := 0; j < jobsPerWorker; j++ {
				id := videoJobIDForStress(w, j)
				if err := ledger.ReserveForJob(id, est, 0); err != nil {
					t.Errorf("reserve %s: %v", id, err)
					return
				}
				if err := ledger.SettleActual(id, actual, 1); err != nil {
					t.Errorf("settle %s: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Expected total: sum over workers of jobsPerWorker*(w+1)
	// = 50 * (1+...+32) = 50 * 528 = 26400.
	const wantSpent = 26400.0
	if got := ledger.Spent(); got != wantSpent {
		t.Fatalf("spent = %v, want %v", got, wantSpent)
	}
	if got := ledger.Reserved(); got != 0 {
		t.Fatalf("reserved = %v, want 0 after all settlements", got)
	}
	if got := len(ledger.Entries()); got != workers*jobsPerWorker {
		t.Fatalf("entries = %d, want %d", got, workers*jobsPerWorker)
	}
	estimate, actual, delta, reconciled, ok := ledger.Reconciliation(videoJobIDForStress(0, 0))
	if !ok || !reconciled || estimate != 1 || actual != 1 || delta != 0 {
		t.Fatalf("reconciliation(worker0,job0) = (%v,%v,%v,%v,%v), want (1,1,0,true,true)",
			estimate, actual, delta, reconciled, ok)
	}

	// Duplicate settlement with the same value is idempotent; a conflicting
	// value is rejected and must not move the total.
	id := videoJobIDForStress(0, 0)
	if err := ledger.SettleActual(id, 1, 1); err != nil {
		t.Fatalf("duplicate settle with same value: %v", err)
	}
	if err := ledger.SettleActual(id, 2, 1); err == nil {
		t.Fatal("conflicting settle accepted, want an error")
	}
	if got := ledger.Spent(); got != wantSpent {
		t.Fatalf("spent after duplicate/conflicting settles = %v, want %v", got, wantSpent)
	}
}

func videoJobIDForStress(worker, job int) string {
	return "stress-" + itoa(worker) + "-" + itoa(job)
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
