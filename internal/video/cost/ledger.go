package cost

import (
	"errors"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var ErrBudgetExceeded = errors.New("video budget exceeded")
var ErrUnknownPrice = errors.New("video price is unknown")

type Price struct {
	PerSecondUSD float64 `json:"per_second_usd"`
	FixedUSD     float64 `json:"fixed_usd"`
	Known        bool    `json:"known"`
}
type Book struct {
	mu     sync.RWMutex
	prices map[string]Price
}

func NewBook() *Book { return &Book{prices: map[string]Price{}} }
func (b *Book) Set(provider, model string, p Price) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prices[provider+"/"+model] = p
}
func (b *Book) Estimate(provider, model string, r video.VideoRequest) (video.CostEstimate, error) {
	b.mu.RLock()
	p, ok := b.prices[provider+"/"+model]
	b.mu.RUnlock()
	if !ok || !p.Known {
		return video.CostEstimate{Provider: provider, Model: model, PriceKnown: false}, ErrUnknownPrice
	}
	v := p.FixedUSD + p.PerSecondUSD*r.DurationSeconds
	return video.CostEstimate{Provider: provider, Model: model, Currency: "USD", EstimatedUSD: v, UpperBoundUSD: v, PriceKnown: true, BillableSeconds: r.DurationSeconds}, nil
}

type Ledger struct {
	mu      sync.Mutex
	spent   float64
	entries []video.CostEstimate
}

func (l *Ledger) Reserve(e video.CostEstimate, max float64) error {
	if !e.PriceKnown {
		return ErrUnknownPrice
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if max > 0 && e.UpperBoundUSD > max {
		return ErrBudgetExceeded
	}
	l.entries = append(l.entries, e)
	return nil
}
func (l *Ledger) Spent() float64 { l.mu.Lock(); defer l.mu.Unlock(); return l.spent }
func (l *Ledger) Entries() []video.CostEstimate {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]video.CostEstimate(nil), l.entries...)
}
