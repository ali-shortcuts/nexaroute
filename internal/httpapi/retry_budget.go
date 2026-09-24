package httpapi

import "sync"

// retryBudget is a Finagle-style token bucket that caps failover retries
// (and hedged backup attempts) as a fraction of served data-plane traffic.
// Every admitted request deposits one token; every retry beyond the first
// attempt withdraws 1/ratio tokens. When the bucket is empty the gateway
// fails fast instead of hammering already-failing providers, which keeps a
// total outage from turning into a retry storm with slow 502s.
//
// A ratio <= 0 disables the budget (unlimited retries, historical
// behaviour). The bucket starts full so a quiet gateway still fails over.
const retryBudgetCapacity = 100

type retryBudget struct {
	mu     sync.Mutex
	tokens float64
	ratio  float64
}

func newRetryBudget(ratio float64) *retryBudget {
	b := &retryBudget{tokens: retryBudgetCapacity}
	b.setRatio(ratio)
	return b
}

func (b *retryBudget) setRatio(ratio float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ratio < 0 || ratio > 1 {
		ratio = 0
	}
	b.ratio = ratio
	if b.tokens > retryBudgetCapacity {
		b.tokens = retryBudgetCapacity
	}
}

// deposit records one admitted data-plane request.
func (b *retryBudget) deposit() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ratio <= 0 {
		return
	}
	b.tokens++
	if b.tokens > retryBudgetCapacity {
		b.tokens = retryBudgetCapacity
	}
}

// allowRetry reports whether one more upstream attempt (failover or hedge)
// may be spent. The first attempt of a request never consults the budget.
func (b *retryBudget) allowRetry() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ratio <= 0 {
		return true
	}
	cost := 1 / b.ratio
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}
