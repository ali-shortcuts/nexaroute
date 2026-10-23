package cost

import (
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
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
