//go:build integration

// Real-PostgreSQL integration tests for the durable video cost ledger.
// They run only with the `integration` build tag and a disposable
// database in NEXAROUTE_TEST_POSTGRES_DSN. Ordinary unit runs never
// touch a real server.
package cost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func openTestLedger(t *testing.T) (*PostgresLedger, *sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	ctx := context.Background()
	ledger, err := OpenPostgresLedger(ctx, dsn)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Aggregates are table-global by design, and package tests run
	// sequentially against a disposable database: start each test from
	// an empty table so sums are exact.
	if _, err := raw.ExecContext(context.Background(), `DELETE FROM `+videoCostTable); err != nil {
		t.Fatalf("clean cost table: %v", err)
	}
	prefix := fmt.Sprintf("it-%d-", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = raw.ExecContext(context.Background(), `DELETE FROM `+videoCostTable+` WHERE job_id LIKE $1`, prefix+"%")
		_ = raw.Close()
		_ = ledger.Close()
	})
	return ledger, raw, prefix
}

func estimate(usd float64) video.CostEstimate {
	return video.CostEstimate{Provider: "p", Model: "m", Currency: "USD",
		EstimatedUSD: usd, UpperBoundUSD: usd, PriceKnown: true, BillableSeconds: 10}
}

func TestPostgresLedgerConcurrentDuplicateReserves(t *testing.T) {
	ledger, _, prefix := openTestLedger(t)
	job := prefix + "dup"
	const workers = 16
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = ledger.ReserveForJob(job, estimate(2.50), 0)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	if got := ledger.Reserved(); got != 2.50 {
		t.Fatalf("reserved = %v, want the bound counted exactly once (2.50)", got)
	}
	if n := len(ledger.Entries()); n != 1 {
		t.Fatalf("entries = %d, want 1", n)
	}
}

func TestPostgresLedgerDuplicateAndConflictingSettlement(t *testing.T) {
	ledger, _, prefix := openTestLedger(t)
	job := prefix + "settle"
	if err := ledger.ReserveForJob(job, estimate(3.00), 0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SettleActual(job, 2.75, 3.00); err != nil {
		t.Fatal(err)
	}
	// Same settlement again: no-op, charged once.
	if err := ledger.SettleActual(job, 2.75, 3.00); err != nil {
		t.Fatalf("duplicate settlement: %v", err)
	}
	if got := ledger.Spent(); got != 2.75 {
		t.Fatalf("spent = %v, want exactly one charge of 2.75", got)
	}
	// A different actual after settlement is a conflict, not a charge.
	if err := ledger.SettleActual(job, 9.99, 3.00); err == nil {
		t.Fatal("conflicting settlement accepted")
	}
	if got := ledger.Spent(); got != 2.75 {
		t.Fatalf("spent after conflict = %v, want 2.75", got)
	}
	// Reserving a finalized job is an error; reconciliation reports the
	// estimate-vs-actual delta.
	if err := ledger.ReserveForJob(job, estimate(3.00), 0); err == nil {
		t.Fatal("reserve of a finalized job accepted")
	}
	est, act, delta, reconciled, ok := ledger.Reconciliation(job)
	if !ok || !reconciled || est != 3.00 || act != 2.75 || delta != -0.25 {
		t.Fatalf("reconciliation = (%v, %v, %v, %v, %v)", est, act, delta, reconciled, ok)
	}
}

func TestPostgresLedgerEstimateThenActualUpgradeAndRelease(t *testing.T) {
	ledger, _, prefix := openTestLedger(t)
	jobA := prefix + "upgrade"
	if err := ledger.SettleEstimate(jobA, 1.25); err != nil {
		t.Fatal(err)
	}
	if got := ledger.EstimatedSpent(); got != 1.25 {
		t.Fatalf("estimated spent = %v, want 1.25", got)
	}
	if err := ledger.SettleActual(jobA, 1.50, 1.25); err != nil {
		t.Fatalf("upgrade to actual: %v", err)
	}
	if got := ledger.EstimatedSpent(); got != 0 {
		t.Fatalf("estimated spent after upgrade = %v, want 0", got)
	}
	if got := ledger.Spent(); got != 1.50 {
		t.Fatalf("spent after upgrade = %v, want 1.50", got)
	}
	jobB := prefix + "release"
	if err := ledger.ReserveForJob(jobB, estimate(4.00), 0); err != nil {
		t.Fatal(err)
	}
	ledger.ReleaseForJob(jobB)
	if got := ledger.Reserved(); got != 0 {
		t.Fatalf("reserved after release = %v, want 0", got)
	}
	// Settling a released job at its estimate matches the in-memory
	// state machine (release only frees an outstanding reservation).
	if err := ledger.SettleEstimate(jobB, 4.00); err != nil {
		t.Fatalf("settle released job: %v", err)
	}
}

func TestPostgresLedgerSurvivesRestart(t *testing.T) {
	ledger, _, prefix := openTestLedger(t)
	job := prefix + "restart"
	if err := ledger.ReserveForJob(job, estimate(5.00), 0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SettleActual(job, 4.50, 5.00); err != nil {
		t.Fatal(err)
	}
	pending := prefix + "restart-pending"
	if err := ledger.ReserveForJob(pending, estimate(7.00), 0); err != nil {
		t.Fatal(err)
	}
	// A new ledger over the same database (process restart) sees the
	// durable state exactly.
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	reopened, err := OpenPostgresLedger(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Spent(); got != 4.50 {
		t.Fatalf("spent after restart = %v, want 4.50", got)
	}
	if got := reopened.Reserved(); got != 7.00 {
		t.Fatalf("reserved after restart = %v, want 7.00", got)
	}
	if _, _, _, reconciled, ok := reopened.Reconciliation(job); !ok || !reconciled {
		t.Fatal("settled job not reconciled after restart")
	}
	// And the restarted instance can still settle the pending job
	// exactly once.
	if err := reopened.SettleActual(pending, 6.00, 7.00); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SettleActual(pending, 6.00, 7.00); err != nil {
		t.Fatalf("repeat settlement after restart: %v", err)
	}
	if got := reopened.Spent(); got != 10.50 {
		t.Fatalf("spent = %v, want 10.50", got)
	}
}

func TestPostgresLedgerRejectsInvalidAmounts(t *testing.T) {
	ledger, _, prefix := openTestLedger(t)
	job := prefix + "invalid"
	if err := ledger.ReserveForJob(job, estimate(1.00), 0); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err := ledger.SettleActual(job, bad, 1.00); !errors.Is(err, ErrInvalidEstimate) {
			t.Fatalf("settle actual %v: error = %v, want ErrInvalidEstimate", bad, err)
		}
		if err := ledger.SettleEstimate(job, bad); !errors.Is(err, ErrInvalidEstimate) {
			t.Fatalf("settle estimate %v: error = %v, want ErrInvalidEstimate", bad, err)
		}
	}
	// The reservation is untouched and still releasable/settleable.
	if got := ledger.Reserved(); got != 1.00 {
		t.Fatalf("reserved after invalid attempts = %v, want 1.00", got)
	}
	if err := ledger.SettleActual(job, 0.90, 1.00); err != nil {
		t.Fatalf("valid settlement after invalid attempts: %v", err)
	}
	// Over-budget reserve is rejected before any row is written.
	over := prefix + "over"
	if err := ledger.ReserveForJob(over, estimate(50.00), 10.00); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over-budget reserve error = %v, want ErrBudgetExceeded", err)
	}
	if _, _, _, _, ok := ledger.Reconciliation(over); ok {
		t.Fatal("over-budget reserve left a row behind")
	}
}

func TestPostgresLedgerStoreFailureIsLoud(t *testing.T) {
	ledger, _, prefix := openTestLedger(t)
	job := prefix + "outage"
	if err := ledger.ReserveForJob(job, estimate(1.00), 0); err != nil {
		t.Fatal(err)
	}
	// Closing the pool simulates a lost database: mutations must fail
	// loudly, never report success against volatile state.
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SettleActual(job, 1.00, 1.00); err == nil {
		t.Fatal("settlement against a closed database reported success")
	}
	if err := ledger.ReserveForJob(prefix+"other", estimate(1.00), 0); err == nil {
		t.Fatal("reserve against a closed database reported success")
	}
}
