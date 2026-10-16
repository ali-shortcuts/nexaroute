package budget

// Wave C-2 (#403) supervisor-takeover fuzz target for the budget ledger.
//
// Randomized reserve/settle/release sequences must preserve:
//   - balances never negative (used >= 0, reserved >= 0);
//   - hold conservation: reserved[key] == sum of open holds for key;
//   - admission soundness: a successful Reserve implies
//     used+reserved+amount <= limit (no int64 overflow bypass);
//   - failed operations change nothing (fail-closed accounting).

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"
)

type fuzzHold struct {
	key    string
	amount int64
}

type fuzzLedgerModel struct {
	used     map[string]int64
	reserved map[string]int64
	holds    map[string]fuzzHold // open holds by reservation id
	openSum  map[string]int64    // incremental sum of open holds per key
	limits   map[string]int64
}

var fuzzLedgerKeys = []string{"a", "b", "c"}

func newFuzzLedgerModel() *fuzzLedgerModel {
	return &fuzzLedgerModel{
		used:     map[string]int64{},
		reserved: map[string]int64{},
		holds:    map[string]fuzzHold{},
		openSum:  map[string]int64{},
		limits:   map[string]int64{"a": 1000000, "b": 500, "c": 0},
	}
}

// TestLedgerAdmissionOverflow is the minimized regression for the int64
// overflow the fuzzer hunts in Reserve/Settle (#403): used+reserved+amount
// can wrap negative and bypass the budget limit, and used += actual can
// wrap negative on huge settles. Both must hold the ledger's invariants.
func TestLedgerAdmissionOverflow(t *testing.T) {
	l, err := NewLedger([]Limit{{Key: "a", Amount: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve("id1", "a", 600); err != nil {
		t.Fatalf("legit reserve: %v", err)
	}
	// 0 + 600 + MaxInt64 wraps negative and passes the naive
	// used+reserved+amount > limit check.
	if _, err := l.Reserve("id2", "a", math.MaxInt64); err != ErrBudgetExceeded {
		t.Fatalf("RED: overflowing reserve bypassed admission: err=%v", err)
	}
	if _, err := l.Reserve("id3", "a", 400); err != nil {
		t.Fatalf("legit reserve after rejected bypass: %v", err)
	}
	used, reserved, _ := l.Usage("a")
	if reserved != 1000 || used != 0 {
		t.Fatalf("balances wrong after bypass attempt: used=%d reserved=%d", used, reserved)
	}

	l2, err := NewLedger([]Limit{{Key: "a", Amount: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l2.Reserve("s1", "a", 600); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.Reserve("s2", "a", 400); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.Settle("s1", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.Settle("s2", math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	used2, _, _ := l2.Usage("a")
	if used2 < 0 {
		t.Fatalf("RED: used wrapped negative after huge settles: %d", used2)
	}
	if used2 != math.MaxInt64 {
		t.Fatalf("used should saturate at MaxInt64, got %d", used2)
	}
}

// op layout: [op:1][key:1][id:7][amount:8]
func FuzzLedgerOps(f *testing.F) {
	f.Add([]byte{
		0, 0, 1, 2, 3, 4, 5, 6, 7, 0, 0, 0, 0, 0, 0, 3, 232, // reserve a id=01020304050607 amount=1000
		1, 0, 1, 2, 3, 4, 5, 6, 7, 0, 0, 0, 0, 0, 0, 3, 0, // settle same id actual=768
		0, 1, 9, 9, 9, 9, 9, 9, 9, 0, 0, 0, 0, 0, 0, 0, 100, // reserve b amount=100
		2, 1, 9, 9, 9, 9, 9, 9, 9, 0, 0, 0, 0, 0, 0, 0, 0, // release it
	})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 64<<10 {
			t.Skip("input too large for a bounded fuzz iteration")
		}
		limits := []Limit{{Key: "a", Amount: 1000000}, {Key: "b", Amount: 500}, {Key: "c", Amount: 0}}
		l, err := NewLedger(limits)
		if err != nil {
			t.Fatal(err)
		}
		m := newFuzzLedgerModel()
		for off := 0; off+17 <= len(ops); off += 17 {
			op := ops[off] % 3
			key := fuzzLedgerKeys[ops[off+1]%3]
			id := "r" + hex.EncodeToString(ops[off+2:off+9])
			amount := int64(binary.BigEndian.Uint64(ops[off+9 : off+17]))
			switch op {
			case 0: // Reserve
				usedBefore, reservedBefore, _ := l.Usage(key)
				r, rerr := l.Reserve(id, key, amount)
				if rerr != nil {
					checkLedgerUnchanged(t, l, m, key)
					continue
				}
				m.holds[id] = fuzzHold{key: key, amount: r.Reserved}
				m.reserved[key] += r.Reserved
				m.openSum[key] += r.Reserved
				// Admission soundness: success must imply the pre-state
				// had room (overflow-safe, mirroring remainingBudget).
				if amount <= 0 || amount > remainingBudget(m.limits[key], usedBefore, reservedBefore) {
					t.Fatalf("admission bypass: reserve(%q,%q,%d) succeeded with used=%d reserved=%d limit=%d",
						id, key, amount, usedBefore, reservedBefore, m.limits[key])
				}
			case 1: // Settle
				actual := amount
				if actual < 0 {
					actual = 0 // mirror the ledger's clamp exactly
				}
				_, serr := l.Settle(id, actual)
				if serr != nil {
					checkLedgerUnchanged(t, l, m, key)
					continue
				}
				h := m.holds[id]
				delete(m.holds, id)
				m.reserved[h.key] -= h.amount
				m.openSum[h.key] -= h.amount
				m.used[h.key] = saturatingAdd(m.used[h.key], actual)
			case 2: // Release
				rerr := l.Release(id)
				if rerr != nil {
					checkLedgerUnchanged(t, l, m, key)
					continue
				}
				h := m.holds[id]
				delete(m.holds, id)
				m.reserved[h.key] -= h.amount
				m.openSum[h.key] -= h.amount
			}
			checkLedgerInvariants(t, l, m)
		}
	})
}

func checkLedgerUnchanged(t *testing.T, l *Ledger, m *fuzzLedgerModel, key string) {
	t.Helper()
	for _, k := range fuzzLedgerKeys {
		used, reserved, ok := l.Usage(k)
		if !ok || used != m.used[k] || reserved != m.reserved[k] {
			t.Fatalf("failed op changed balances for %q: ledger=(%d,%d) model=(%d,%d)",
				k, used, reserved, m.used[k], m.reserved[k])
		}
	}
}

func checkLedgerInvariants(t *testing.T, l *Ledger, m *fuzzLedgerModel) {
	t.Helper()
	for _, k := range fuzzLedgerKeys {
		used, reserved, ok := l.Usage(k)
		if !ok {
			t.Fatalf("known key %q vanished", k)
		}
		if used < 0 || reserved < 0 {
			t.Fatalf("negative balance for %q: used=%d reserved=%d", k, used, reserved)
		}
		if used != m.used[k] || reserved != m.reserved[k] {
			t.Fatalf("model diverged for %q: ledger=(%d,%d) model=(%d,%d)",
				k, used, reserved, m.used[k], m.reserved[k])
		}
		if reserved != m.openSum[k] {
			t.Fatalf("hold conservation violated for %q: reserved=%d sum(open)=%d", k, reserved, m.openSum[k])
		}
	}
}
