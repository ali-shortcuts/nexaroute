package usage

import (
	"math"
	"testing"
	"time"
)

func TestIssue663CoreBudgetLedgerStats(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	l := NewBudgetLedger()
	rows, reservations, reserved := l.Stats()
	if rows != 0 || reservations != 0 || reserved != 0 {
		t.Fatalf("empty ledger stats = (%d, %d, %v)", rows, reservations, reserved)
	}
	id, ok := l.Reserve("key-1", 10, 3, now)
	if !ok || id == 0 {
		t.Fatalf("reserve = (%d, %v)", id, ok)
	}
	rows, reservations, reserved = l.Stats()
	if rows != 1 || reservations != 1 || reserved != 3 {
		t.Fatalf("open-reservation stats = (%d, %d, %v)", rows, reservations, reserved)
	}
	l.Settle("key-1", id, 2.5)
	rows, reservations, reserved = l.Stats()
	if rows != 1 || reservations != 0 || reserved != 0 {
		t.Fatalf("settled stats = (%d, %d, %v): the row must survive because spend is committed", rows, reservations, reserved)
	}
	if committed, reservedNow := l.Snapshot("key-1", now); committed != 2.5 || reservedNow != 0 {
		t.Fatalf("snapshot after settle = (%v, %v)", committed, reservedNow)
	}
	// A released-only reservation leaves nothing behind: the empty
	// row is evicted, so Stats returns to zero.
	id2, ok := l.Reserve("key-2", 10, 1, now)
	if !ok {
		t.Fatal("second reserve must succeed")
	}
	l.Release("key-2", id2)
	rows, reservations, reserved = l.Stats()
	if rows != 1 || reservations != 0 || reserved != 0 {
		t.Fatalf("stats after release = (%d, %d, %v): key-2's empty row must be evicted", rows, reservations, reserved)
	}
	// A failed over-limit reserve on a fresh key also leaves no row.
	if _, ok := l.Reserve("key-3", 1, 5, now); ok {
		t.Fatal("over-limit reserve must be rejected")
	}
	rows, _, _ = l.Stats()
	if rows != 1 {
		t.Fatalf("failed reserve must not leave a row: rows=%d", rows)
	}
}

func TestIssue663CoreBudgetLedgerSettleReleaseBranches(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	l := NewBudgetLedger()
	// No-op ids and unknown keys/ids never panic and never create rows.
	l.Settle("missing", 0, 5)
	l.Release("missing", 0)
	l.Settle("missing", 99, 5)
	l.Release("missing", 99)
	if rows, _, _ := l.Stats(); rows != 0 {
		t.Fatalf("no-op settle/release created rows: %d", rows)
	}
	// Negative, NaN and infinite actuals settle to zero, and the
	// first settle wins over any later settle of the same id.
	id, _ := l.Reserve("key-1", 100, 10, now)
	l.Settle("key-1", id, -3)
	if committed, _ := l.Snapshot("key-1", now); committed != 0 {
		t.Fatalf("negative settle must commit zero, got %v", committed)
	}
	id, _ = l.Reserve("key-1", 100, 10, now)
	l.Settle("key-1", id, math.NaN())
	if committed, _ := l.Snapshot("key-1", now); committed != 0 {
		t.Fatalf("NaN settle must commit zero, got %v", committed)
	}
	id, _ = l.Reserve("key-1", 100, 10, now)
	l.Settle("key-1", id, math.Inf(1))
	if committed, _ := l.Snapshot("key-1", now); committed != 0 {
		t.Fatalf("infinite settle must commit zero, got %v", committed)
	}
	id, _ = l.Reserve("key-1", 100, 10, now)
	l.Settle("key-1", id, 4)
	l.Settle("key-1", id, 9)
	if committed, _ := l.Snapshot("key-1", now); committed != 4 {
		t.Fatalf("first settle must win, committed = %v", committed)
	}
	// Release of an unknown id on a known key is a no-op; release of
	// a live id drops the hold without committing.
	id, _ = l.Reserve("key-1", 100, 10, now)
	l.Release("key-1", 9999)
	if _, reserved := l.Snapshot("key-1", now); reserved != 10 {
		t.Fatalf("unknown-id release must not touch the hold, reserved = %v", reserved)
	}
	l.Release("key-1", id)
	if _, reserved := l.Snapshot("key-1", now); reserved != 0 {
		t.Fatalf("release must drop the hold, reserved = %v", reserved)
	}
	// Admission no-ops: zero/negative amounts and limits admit
	// without creating a reservation.
	if id, ok := l.Reserve("key-9", 10, 0, now); !ok || id != 0 {
		t.Fatalf("zero-amount reserve = (%d, %v)", id, ok)
	}
	if id, ok := l.Reserve("key-9", 0, 5, now); !ok || id != 0 {
		t.Fatalf("zero-limit reserve = (%d, %v)", id, ok)
	}
	if _, ok := l.Reserve("key-9", 10, math.NaN(), now); ok {
		t.Error("NaN reserve must be rejected")
	}
	if _, ok := l.Reserve("key-9", math.Inf(1), 5, now); ok {
		t.Error("infinite-limit reserve must be rejected")
	}
	if rows, _, _ := l.Stats(); rows != 1 {
		t.Fatalf("admission no-ops must not create rows: rows=%d", rows)
	}
}
