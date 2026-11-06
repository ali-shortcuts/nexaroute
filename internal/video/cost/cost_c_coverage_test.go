package cost

import (
	"context"
	"math"
	"testing"
)

// Agent C coverage: the Postgres ledger open paths and the reconciliation
// arithmetic that the earlier suites never reached.

// A malformed DSN is only parsed lazily by the pgx driver, so it surfaces
// at Ping time as "postgres is unreachable", not as an Open error. The
// unreachable branch is already covered by the existing read tests.
func TestOpenPostgresLedgerDBWithFakeDriver(t *testing.T) {
	// Healthy fake: ping and migration succeed, so Open returns a ledger.
	l, err := openPostgresLedgerDB(context.Background(), openFakeCostDB(t, &fakeCostDB{}))
	if err != nil {
		t.Fatalf("open with fake DB: %v", err)
	}
	if l == nil {
		t.Fatal("open with fake DB returned a nil ledger")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Migration failure (begin fails): Open must return the error and
	// close the database instead of leaking it.
	if _, err := openPostgresLedgerDB(context.Background(), openFakeCostDB(t, &fakeCostDB{failBegin: true})); err == nil {
		t.Fatal("migration failure must make Open return an error")
	}
}

func TestReconciliationDeltaCannotBeNaNOrInf(t *testing.T) {
	// Both amounts are sanitized to finite values before the subtraction,
	// so the largest representable estimate against a zero actual still
	// yields a finite delta, not NaN or Inf.
	ledger := Ledger{byJob: map[string]reservation{
		"job": {estimate: knownEstimate(math.MaxFloat64), state: actualSettledState, actual: 0},
	}}
	gotEst, gotAct, d, reconciled, ok := ledger.Reconciliation("job")
	if !ok || !reconciled || gotEst != math.MaxFloat64 || gotAct != 0 || d != -math.MaxFloat64 {
		t.Fatalf("reconciliation = (%v, %v, %v, %v, %v), want (%v, 0, %v, true, true)", gotEst, gotAct, d, reconciled, ok, math.MaxFloat64, -math.MaxFloat64)
	}
}
