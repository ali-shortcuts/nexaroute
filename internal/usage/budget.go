package usage

import (
	"math"
	"sync"
	"time"
)

// BudgetLedger enforces per-key USD spend budgets atomically
// (M5-6). Admission reserves a conservative estimate; the first
// usage report settles the reservation to the actual cost.
// Reservations that never settle expire into committed-at-estimate
// after BudgetReservationTTL — mirroring the gateway's TPM rule
// that requests without a usage report keep their admission
// estimate — so a transport failure cannot make spend vanish, and
// a leaked reservation cannot block a key forever.
//
// The ledger is runtime state with the same lifetime as the usage
// tracker: it resets on gateway restart.
type BudgetLedger struct {
	mu   sync.Mutex
	seq  uint64
	rows map[string]*budgetRow
}

type budgetRow struct {
	committed    float64
	reserved     float64
	reservations map[uint64]budgetReservation
}

type budgetReservation struct {
	amount float64
	at     time.Time
}

// BudgetReservationTTL bounds how long an unsettled reservation
// holds budget before it is committed at its estimate.
const BudgetReservationTTL = 15 * time.Minute

const budgetEpsilon = 1e-9

// NewBudgetLedger returns an empty ledger.
func NewBudgetLedger() *BudgetLedger {
	return &BudgetLedger{rows: map[string]*budgetRow{}}
}

func (l *BudgetLedger) rowLocked(key string) *budgetRow {
	row := l.rows[key]
	if row == nil {
		row = &budgetRow{reservations: map[uint64]budgetReservation{}}
		l.rows[key] = row
	}
	return row
}

// expireLocked commits reservations older than the TTL at their
// estimate and releases their hold.
func (l *BudgetLedger) expireLocked(row *budgetRow, now time.Time) {
	for id, res := range row.reservations {
		if now.Sub(res.at) >= BudgetReservationTTL {
			row.reserved -= res.amount
			row.committed += res.amount
			delete(row.reservations, id)
		}
	}
	snapReserved(row)
}

// snapReserved zeroes floating-point dust around an empty hold so
// a fully settled ledger reports exactly 0 reserved.
func snapReserved(row *budgetRow) {
	if row.reserved < budgetEpsilon && row.reserved > -budgetEpsilon {
		row.reserved = 0
	}
}

// Reserve atomically holds `amount` against the key's budget when
// committed+reserved+amount stays within `limit`. amount <= 0 or
// limit <= 0 is an admitted no-op (id 0). Expiry for the key is
// applied first, so stale holds never block fresh admissions.
func (l *BudgetLedger) Reserve(key string, limit, amount float64, now time.Time) (uint64, bool) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || math.IsNaN(limit) || math.IsInf(limit, 0) {
		return 0, false
	}
	if amount <= 0 || limit <= 0 {
		return 0, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	row := l.rowLocked(key)
	l.expireLocked(row, now)
	if row.committed+row.reserved+amount > limit+budgetEpsilon {
		l.evictIfEmptyLocked(key, row)
		return 0, false
	}
	l.seq++
	row.reserved += amount
	row.reservations[l.seq] = budgetReservation{amount: amount, at: now}
	return l.seq, true
}

// Settle converts a reservation to its actual cost: the hold is
// released and `actual` is committed. Settling an unknown or
// already-settled id is a no-op (first settle wins).
func (l *BudgetLedger) Settle(key string, id uint64, actual float64) {
	if id == 0 {
		return
	}
	if actual < 0 || actual != actual || math.IsInf(actual, 0) {
		actual = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	row := l.rows[key]
	if row == nil {
		return
	}
	res, ok := row.reservations[id]
	if !ok {
		return
	}
	row.reserved -= res.amount
	snapReserved(row)
	row.committed += actual
	delete(row.reservations, id)
	l.evictIfEmptyLocked(key, row)
}

// Release drops a reservation without committing anything (used
// when admission is rolled back before any upstream spend).
func (l *BudgetLedger) Release(key string, id uint64) {
	if id == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	row := l.rows[key]
	if row == nil {
		return
	}
	res, ok := row.reservations[id]
	if !ok {
		return
	}
	row.reserved -= res.amount
	snapReserved(row)
	delete(row.reservations, id)
	l.evictIfEmptyLocked(key, row)
}

// evictIfEmptyLocked drops a row that carries no committed spend,
// no hold and no live reservations, so the rows map cannot grow
// without bound across rotated or ephemeral key IDs. A row with any
// committed spend is the budget record itself and is never evicted.
// Callers hold l.mu.
func (l *BudgetLedger) evictIfEmptyLocked(key string, row *budgetRow) {
	if row.committed == 0 && row.reserved == 0 && len(row.reservations) == 0 {
		delete(l.rows, key)
	}
}

// Stats returns the number of key rows, the number of open
// reservations, and the total currently-reserved spend. It is the
// observability seam for leak-invariant soak tests (Wave C-4): a
// healthy gateway shows all three at (or returning to) zero.
func (l *BudgetLedger) Stats() (rows, reservations int, reserved float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, row := range l.rows {
		rows++
		reservations += len(row.reservations)
		reserved += row.reserved
	}
	return rows, reservations, reserved
}

// Snapshot returns the key's committed and currently-reserved
// spend after expiring stale reservations.
func (l *BudgetLedger) Snapshot(key string, now time.Time) (committed, reserved float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row := l.rows[key]
	if row == nil {
		return 0, 0
	}
	l.expireLocked(row, now)
	return row.committed, row.reserved
}
