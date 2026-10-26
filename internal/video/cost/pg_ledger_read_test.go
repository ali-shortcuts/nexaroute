package cost

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// TestPostgresLedgerAggregateReadsDegradeToZeroOnStoreError pins the
// documented reporting contract: when the store is unreachable, the
// aggregate reads report zero (and log the failure) instead of
// panicking or fabricating a durable figure; mutations stay
// fail-closed and are covered by the integration suite.
func TestPostgresLedgerAggregateReadsDegradeToZeroOnStoreError(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://127.0.0.1:1/nexaroute")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	l := &PostgresLedger{db: db}
	if got := l.Spent(); got != 0 {
		t.Fatalf("Spent on closed store = %v, want 0", got)
	}
	if got := l.Reserved(); got != 0 {
		t.Fatalf("Reserved on closed store = %v, want 0", got)
	}
	if got := l.EstimatedSpent(); got != 0 {
		t.Fatalf("EstimatedSpent on closed store = %v, want 0", got)
	}
	if entries := l.Entries(); len(entries) != 0 {
		t.Fatalf("Entries on closed store = %v, want empty", entries)
	}
}

// TestOpenPostgresLedgerRejectsInvalidConfiguration exercises fail-closed
// constructor validation without requiring a running PostgreSQL service.
func TestOpenPostgresLedgerRejectsInvalidConfiguration(t *testing.T) {
	if _, err := OpenPostgresLedger(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "DSN is required") {
		t.Fatalf("empty DSN error = %v, want required-DSN error", err)
	}

	if _, err := OpenPostgresLedger(context.Background(), "://not a dsn"); err == nil {
		t.Fatal("malformed DSN unexpectedly opened a cost ledger")
	} else if strings.Contains(err.Error(), "not a dsn") {
		t.Fatalf("malformed DSN leaked into error: %v", err)
	}
}

// These mutation guards are intentionally usable before a database handle is
// needed. Pinning them here protects the durable adapter's public contract
// without treating a fake SQL server as evidence of PostgreSQL semantics.
func TestPostgresLedgerRejectsInvalidMutationsBeforeStoreAccess(t *testing.T) {
	ledger := &PostgresLedger{}
	e := knownEstimate(1)

	reserveCases := []struct {
		name     string
		job      string
		estimate video.CostEstimate
		max      float64
		want     error
	}{
		{name: "empty job id", job: "", estimate: e, want: errors.New("job ID is required")},
		{name: "unknown price", job: "unknown", estimate: video.CostEstimate{}, want: ErrUnknownPrice},
		{name: "invalid estimate", job: "invalid", estimate: knownEstimate(-1), want: ErrInvalidEstimate},
		{name: "invalid upper bound", job: "bound", estimate: video.CostEstimate{PriceKnown: true, EstimatedUSD: 2, UpperBoundUSD: 1}, want: ErrInvalidEstimate},
		{name: "negative max", job: "negative-max", estimate: e, max: -1, want: ErrInvalidEstimate},
		{name: "NaN max", job: "nan-max", estimate: e, max: math.NaN(), want: ErrInvalidEstimate},
		{name: "infinite max", job: "infinite-max", estimate: e, max: math.Inf(1), want: ErrInvalidEstimate},
		{name: "budget exceeded", job: "budget", estimate: e, max: 0.5, want: ErrBudgetExceeded},
	}
	for _, tc := range reserveCases {
		t.Run("reserve/"+tc.name, func(t *testing.T) {
			err := ledger.ReserveForJob(tc.job, tc.estimate, tc.max)
			if tc.name == "empty job id" {
				if err == nil || !strings.Contains(err.Error(), tc.want.Error()) {
					t.Fatalf("error = %v, want job-ID validation error", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	actualCases := []struct {
		name             string
		job              string
		actual, fallback float64
	}{
		{name: "empty job id", job: "", actual: 1, fallback: 1},
		{name: "negative actual", job: "negative", actual: -1, fallback: 1},
		{name: "NaN actual", job: "nan", actual: math.NaN(), fallback: 1},
		{name: "infinite actual", job: "infinite", actual: math.Inf(1), fallback: 1},
		{name: "negative fallback", job: "fallback-negative", actual: 1, fallback: -1},
		{name: "NaN fallback", job: "fallback-nan", actual: 1, fallback: math.NaN()},
		{name: "infinite fallback", job: "fallback-infinite", actual: 1, fallback: math.Inf(1)},
	}
	for _, tc := range actualCases {
		t.Run("settle-actual/"+tc.name, func(t *testing.T) {
			if err := ledger.SettleActual(tc.job, tc.actual, tc.fallback); !errors.Is(err, ErrInvalidEstimate) {
				t.Fatalf("error = %v, want ErrInvalidEstimate", err)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		job    string
		amount float64
	}{
		{name: "empty job id", job: "", amount: 1},
		{name: "negative amount", job: "negative", amount: -1},
		{name: "NaN amount", job: "nan", amount: math.NaN()},
		{name: "infinite amount", job: "infinite", amount: math.Inf(1)},
	} {
		t.Run("settle-estimate/"+tc.name, func(t *testing.T) {
			if err := ledger.SettleEstimate(tc.job, tc.amount); !errors.Is(err, ErrInvalidEstimate) {
				t.Fatalf("error = %v, want ErrInvalidEstimate", err)
			}
		})
	}
}

// Reconciliation must report "not available" when the durable store itself
// is unavailable; an SQL error must not be mistaken for a valid zero-cost row.
func TestPostgresLedgerReconciliationFailsClosedOnStoreError(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://127.0.0.1:1/nexaroute?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ledger := &PostgresLedger{db: db}
	est, actual, delta, reconciled, ok := ledger.Reconciliation("job")
	if ok || reconciled || est != 0 || actual != 0 || delta != 0 {
		t.Fatalf("Reconciliation on a closed store = (%v,%v,%v,%v,%v), want unavailable zeros", est, actual, delta, reconciled, ok)
	}
}

// The constructor must fail quickly and redact credentials if the database is
// unreachable. This is a local negative test, not a live-service acceptance.
func TestOpenPostgresLedgerFailsClosedWhenUnreachable(t *testing.T) {
	dsn := "postgres://nexaroute:secret@127.0.0.1:1/nexaroute_test?sslmode=disable"
	start := time.Now()
	_, err := OpenPostgresLedger(context.Background(), dsn)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable postgres error = %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("DSN leaked into error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("unreachable open took too long: %v", elapsed)
	}
}
