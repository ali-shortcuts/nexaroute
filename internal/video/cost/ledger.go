package cost

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var ErrBudgetExceeded = errors.New("video budget exceeded")
var ErrUnknownPrice = errors.New("video price is unknown")
var ErrInvalidEstimate = errors.New("video cost estimate is invalid")

type Price struct {
	PerSecondUSD float64 `json:"per_second_usd"`
	FixedUSD float64 `json:"fixed_usd"`
	Known bool `json:"known"`
}
type Book struct {
	mu sync.RWMutex
	prices map[string]Price
}
func NewBook() *Book { return &Book{prices: map[string]Price{}} }
func (b *Book) Set(provider, model string, p Price) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.prices == nil { b.prices = map[string]Price{} }
	b.prices[provider+"/"+model] = p
}
func (b *Book) Estimate(provider, model string, r video.VideoRequest) (video.CostEstimate, error) {
	b.mu.RLock()
	p, ok := b.prices[provider+"/"+model]
	b.mu.RUnlock()
	if !ok || !p.Known { return video.CostEstimate{Provider:provider, Model:model, PriceKnown:false}, ErrUnknownPrice }
	if p.PerSecondUSD < 0 || p.FixedUSD < 0 || math.IsNaN(p.PerSecondUSD) || math.IsInf(p.PerSecondUSD,0) || math.IsNaN(p.FixedUSD) || math.IsInf(p.FixedUSD,0) { return video.CostEstimate{}, ErrInvalidEstimate }
	v := p.FixedUSD + p.PerSecondUSD*r.DurationSeconds
	if math.IsNaN(v) || math.IsInf(v,0) { return video.CostEstimate{}, ErrInvalidEstimate }
	return video.CostEstimate{Provider:provider, Model:model, Currency:"USD", EstimatedUSD:v, UpperBoundUSD:v, PriceKnown:true, BillableSeconds:r.DurationSeconds}, nil
}

type reservationState string
const (
	reservedState reservationState = "reserved"
	actualSettledState reservationState = "actual_settled"
	estimateSettledState reservationState = "estimate_settled"
	releasedState reservationState = "released"
)
type reservation struct {
	estimate video.CostEstimate
	state reservationState
	actual float64
}
type Ledger struct {
	mu sync.Mutex
	spent float64
	estimatedSpent float64
	reserved float64
	entries []video.CostEstimate
	byJob map[string]reservation
	legacySeq uint64
}
func validEstimate(e video.CostEstimate) bool {
	return e.PriceKnown && e.EstimatedUSD >= 0 && e.UpperBoundUSD >= e.EstimatedUSD && e.UpperBoundUSD >= 0 &&
		!math.IsNaN(e.EstimatedUSD) && !math.IsInf(e.EstimatedUSD,0) &&
		!math.IsNaN(e.UpperBoundUSD) && !math.IsInf(e.UpperBoundUSD,0)
}

// Reserve preserves the legacy API. New job submissions should use ReserveForJob.
func (l *Ledger) Reserve(e video.CostEstimate, max float64) error {
	l.mu.Lock()
	l.legacySeq++
	id := fmt.Sprintf("legacy-%d", l.legacySeq)
	l.mu.Unlock()
	return l.ReserveForJob(id,e,max)
}
func (l *Ledger) ReserveForJob(jobID string, e video.CostEstimate, max float64) error {
	if jobID == "" { return errors.New("job ID is required for cost reservation") }
	if !validEstimate(e) { if !e.PriceKnown { return ErrUnknownPrice }; return ErrInvalidEstimate }
	if max < 0 || math.IsNaN(max) || math.IsInf(max,0) { return ErrInvalidEstimate }
	if max > 0 && e.UpperBoundUSD > max { return ErrBudgetExceeded }
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byJob == nil { l.byJob = make(map[string]reservation) }
	if old, ok := l.byJob[jobID]; ok {
		if old.state == reservedState { return nil }
		return fmt.Errorf("cost reservation for job %s has already been finalized",jobID)
	}
	l.byJob[jobID] = reservation{estimate:e,state:reservedState}
	l.reserved += e.UpperBoundUSD
	l.entries = append(l.entries,e)
	return nil
}
func (l *Ledger) ReleaseForJob(jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r,ok := l.byJob[jobID]
	if !ok || r.state != reservedState { return }
	l.reserved -= r.estimate.UpperBoundUSD
	if l.reserved < 0 { l.reserved = 0 }
	r.state = releasedState
	l.byJob[jobID] = r
}
func (l *Ledger) SettleActual(jobID string, actual, estimateFallback float64) error {
	if jobID == "" || actual < 0 || math.IsNaN(actual) || math.IsInf(actual,0) || estimateFallback < 0 || math.IsNaN(estimateFallback) || math.IsInf(estimateFallback,0) { return ErrInvalidEstimate }
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byJob == nil { l.byJob = make(map[string]reservation) }
	r,ok := l.byJob[jobID]
	if ok {
		switch r.state {
		case actualSettledState:
			if r.actual == actual { return nil }
			return fmt.Errorf("conflicting actual-cost settlement for job %s",jobID)
		case estimateSettledState:
			l.estimatedSpent -= r.estimate.EstimatedUSD
			if l.estimatedSpent < 0 { l.estimatedSpent = 0 }
		case reservedState:
			l.reserved -= r.estimate.UpperBoundUSD
			if l.reserved < 0 { l.reserved = 0 }
		}
	} else {
		r = reservation{estimate:video.CostEstimate{EstimatedUSD:estimateFallback,UpperBoundUSD:estimateFallback,PriceKnown:true}}
	}
	l.spent += actual
	r.state, r.actual = actualSettledState, actual
	if r.estimate.EstimatedUSD == 0 { r.estimate.EstimatedUSD = estimateFallback }
	l.byJob[jobID] = r
	return nil
}
func (l *Ledger) SettleEstimate(jobID string, estimateUSD float64) error {
	if jobID == "" || estimateUSD < 0 || math.IsNaN(estimateUSD) || math.IsInf(estimateUSD,0) { return ErrInvalidEstimate }
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byJob == nil { l.byJob = make(map[string]reservation) }
	r,ok := l.byJob[jobID]
	if ok {
		switch r.state {
		case estimateSettledState, actualSettledState: return nil
		case reservedState:
			l.reserved -= r.estimate.UpperBoundUSD
			if l.reserved < 0 { l.reserved = 0 }
		}
	} else {
		r = reservation{estimate:video.CostEstimate{EstimatedUSD:estimateUSD,UpperBoundUSD:estimateUSD,PriceKnown:true}}
	}
	r.state = estimateSettledState
	r.estimate.EstimatedUSD = estimateUSD
	l.byJob[jobID] = r
	l.estimatedSpent += estimateUSD
	return nil
}
func (l *Ledger) Spent() float64 { l.mu.Lock(); defer l.mu.Unlock(); return l.spent }
func (l *Ledger) EstimatedSpent() float64 { l.mu.Lock(); defer l.mu.Unlock(); return l.estimatedSpent }
func (l *Ledger) Reserved() float64 { l.mu.Lock(); defer l.mu.Unlock(); return l.reserved }
func (l *Ledger) Entries() []video.CostEstimate { l.mu.Lock(); defer l.mu.Unlock(); return append([]video.CostEstimate(nil),l.entries...) }
