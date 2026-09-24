package httpapi

import "testing"

func drainBudget(b *retryBudget) int {
	allowed := 0
	for b.allowRetry() {
		allowed++
	}
	return allowed
}

func TestRetryBudgetStartsFullAndDrains(t *testing.T) {
	b := newRetryBudget(0.2)
	// 100 tokens at 5 per retry: the first 20 retries of a quiet gateway
	// always succeed, then failover fails fast.
	if got := drainBudget(b); got != 20 {
		t.Fatalf("full budget at ratio 0.2 must allow 20 retries, got %d", got)
	}
}

func TestRetryBudgetRefillsWithTraffic(t *testing.T) {
	b := newRetryBudget(0.2)
	drainBudget(b)
	for i := 0; i < 5; i++ {
		b.deposit()
	}
	if !b.allowRetry() {
		t.Fatal("5 deposits must fund 1 retry at ratio 0.2")
	}
	if b.allowRetry() {
		t.Fatal("budget must be exhausted again after the funded retry")
	}
	// Deposits are capped: a healthy flood cannot bank unbounded retries.
	for i := 0; i < 1000; i++ {
		b.deposit()
	}
	if got := drainBudget(b); got != 20 {
		t.Fatalf("capped budget must allow exactly 20 retries, got %d", got)
	}
}

func TestRetryBudgetDisabledIsUnlimited(t *testing.T) {
	b := newRetryBudget(0)
	for i := 0; i < 1000; i++ {
		if !b.allowRetry() {
			t.Fatalf("disabled budget denied retry %d", i)
		}
	}
}

func TestRetryBudgetRatioReload(t *testing.T) {
	b := newRetryBudget(0.2)
	b.setRatio(1)
	if got := drainBudget(b); got != 100 {
		t.Fatalf("ratio 1 must allow 100 retries from full, got %d", got)
	}
	b.setRatio(0)
	for i := 0; i < 50; i++ {
		if !b.allowRetry() {
			t.Fatal("reloading ratio 0 must disable the budget")
		}
	}
}
