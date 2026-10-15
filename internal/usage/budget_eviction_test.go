package usage

import (
	"fmt"
	"testing"
	"time"
)

// The rows map must not grow without bound across rotated or
// ephemeral key IDs: a row that ends with no committed spend, no
// hold and no live reservations is evicted. A row with committed
// spend is the budget record itself and is never evicted.
func TestBudgetLedgerEvictsFullyReleasedRows(t *testing.T) {
	l := NewBudgetLedger()
	now := time.Now()
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("key-%d", i)
		id, ok := l.Reserve(key, 10, 1, now)
		if !ok || id == 0 {
			t.Fatalf("reserve %s: id=%d ok=%v", key, id, ok)
		}
		l.Release(key, id)
	}
	if len(l.rows) != 0 {
		t.Fatalf("rows after reserve+release across 50 keys = %d, want 0", len(l.rows))
	}
	// Settle at zero actual also leaves an evictable row.
	id, _ := l.Reserve("z", 10, 1, now)
	l.Settle("z", id, 0)
	if len(l.rows) != 0 {
		t.Fatalf("rows after zero-settle = %d, want 0", len(l.rows))
	}
	// Committed spend keeps the row.
	id, _ = l.Reserve("keep", 10, 1, now)
	l.Settle("keep", id, 0.5)
	if len(l.rows) != 1 {
		t.Fatalf("rows after committed settle = %d, want 1", len(l.rows))
	}
	if c, r := l.Snapshot("keep", now); c != 0.5 || r != 0 {
		t.Fatalf("snapshot keep = (%v,%v), want (0.5,0)", c, r)
	}
	// A rejected admission must not leave a row behind either.
	if _, ok := l.Reserve("tight", 1, 5, now); ok {
		t.Fatal("over-limit reserve admitted")
	}
	if _, exists := l.rows["tight"]; exists {
		t.Fatal("rejected admission left a row behind")
	}
}
