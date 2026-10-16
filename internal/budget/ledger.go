package budget

import (
	"errors"
	"math"
	"sort"
	"sync"
)

var (
	ErrBudgetExceeded      = errors.New("budget exceeded")
	ErrReservationNotFound = errors.New("budget reservation not found")
	ErrReservationClosed   = errors.New("budget reservation already closed")
)

type Limit struct {
	Key    string
	Amount int64
}
type Reservation struct {
	ID, Key  string
	Reserved int64
	Settled  int64
	Closed   bool
}
type Alert struct {
	Key       string
	Threshold int
	Used      int64
	Limit     int64
}

type Ledger struct {
	mu           sync.Mutex
	limits       map[string]Limit
	used         map[string]int64
	reserved     map[string]int64
	reservations map[string]Reservation
	alerted      map[string]map[int]bool
}

func NewLedger(limits []Limit) (*Ledger, error) {
	l := &Ledger{limits: map[string]Limit{}, used: map[string]int64{}, reserved: map[string]int64{}, reservations: map[string]Reservation{}, alerted: map[string]map[int]bool{}}
	for _, limit := range limits {
		if limit.Key == "" || limit.Amount < 0 {
			return nil, errors.New("invalid budget limit")
		}
		l.limits[limit.Key] = limit
	}
	return l, nil
}

func (l *Ledger) Reserve(id, key string, amount int64) (Reservation, error) {
	if id == "" || key == "" || amount <= 0 {
		return Reservation{}, ErrBudgetExceeded
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.reservations[id]; ok {
		return Reservation{}, ErrReservationClosed
	}
	limit, ok := l.limits[key]
	if !ok {
		return Reservation{}, ErrBudgetExceeded
	}
	if amount > remainingBudget(limit.Amount, l.used[key], l.reserved[key]) {
		return Reservation{}, ErrBudgetExceeded
	}
	r := Reservation{ID: id, Key: key, Reserved: amount}
	l.reservations[id] = r
	l.reserved[key] += amount
	return r, nil
}

func (l *Ledger) Settle(id string, actual int64) ([]Alert, error) {
	if actual < 0 {
		actual = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.reservations[id]
	if !ok {
		return nil, ErrReservationNotFound
	}
	if r.Closed {
		return nil, ErrReservationClosed
	}
	l.reserved[r.Key] -= r.Reserved
	l.used[r.Key] = saturatingAdd(l.used[r.Key], actual)
	r.Settled = actual
	r.Closed = true
	l.reservations[id] = r
	return l.alertsLocked(r.Key), nil
}

func (l *Ledger) Release(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.reservations[id]
	if !ok {
		return ErrReservationNotFound
	}
	if r.Closed {
		return ErrReservationClosed
	}
	l.reserved[r.Key] -= r.Reserved
	r.Closed = true
	l.reservations[id] = r
	return nil
}

func (l *Ledger) Usage(key string) (used, reserved int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok = l.limits[key]
	return l.used[key], l.reserved[key], ok
}
func (l *Ledger) alertsLocked(key string) []Alert {
	limit := l.limits[key].Amount
	if limit <= 0 {
		return nil
	}
	used := l.used[key]
	out := []Alert{}
	for _, threshold := range []int{50, 80, 95, 100} {
		if thresholdReached(used, limit, threshold) && !l.alerted[key][threshold] {
			if l.alerted[key] == nil {
				l.alerted[key] = map[int]bool{}
			}
			l.alerted[key][threshold] = true
			out = append(out, Alert{Key: key, Threshold: threshold, Used: used, Limit: limit})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Threshold < out[j].Threshold })
	return out
}

// remainingBudget returns limit-used-reserved, or -1 when used+reserved
// already exceeds limit. All inputs are non-negative (ledger invariant),
// so limit-used and the final subtraction cannot overflow; the two
// comparisons detect the overflow cases without wrapping.
func remainingBudget(limit, used, reserved int64) int64 {
	if used > limit {
		return -1
	}
	if reserved > limit-used {
		return -1
	}
	return limit - used - reserved
}

// saturatingAdd adds two non-negative amounts, pinning at MaxInt64 instead
// of wrapping negative. The ledger's "never negative" invariant depends on
// it: a huge settle must not wrap used below zero.
func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// thresholdReached reports used*100 >= limit*threshold without the
// multiplication ever overflowing: used >= ceil(limit*threshold/100),
// computed as q*threshold + ceil(r*threshold/100) with limit = 100*q + r.
// q <= MaxInt64/100 so q*threshold cannot overflow for threshold <= 100.
func thresholdReached(used, limit int64, threshold int) bool {
	q, r := limit/100, limit%100
	need := q*int64(threshold) + r*int64(threshold)/100
	if r*int64(threshold)%100 != 0 {
		need++
	}
	return used >= need
}
