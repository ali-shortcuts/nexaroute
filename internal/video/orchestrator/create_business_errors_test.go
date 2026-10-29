package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// The Reserve-site business rejections other than budget must also reach
// the caller unwrapped: the API maps ErrStoreUnavailable to a retryable
// 503, but unknown-price and invalid-estimate rejections are client
// errors (400). failingCostLedger is defined in
// create_budget_mapping_test.go in this package.
func TestCreateUnknownPriceLedgerIsNotStoreUnavailable(t *testing.T) {
	orch := &Orchestrator{
		Store: queue.NewMemoryStore(), Queue: queue.New(4),
		Providers: Registry{"fake": providerWithDefaults()},
		Ledger:    failingCostLedger{err: cost.ErrUnknownPrice},
	}
	_, err := orch.Create(context.Background(), requestWithDefaults())
	if err == nil || !errors.Is(err, cost.ErrUnknownPrice) {
		t.Fatalf("unknown price: err=%v, want cost.ErrUnknownPrice", err)
	}
	if errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("unknown price wrapped as store-unavailable: %v", err)
	}
}

func TestCreateInvalidEstimateLedgerIsNotStoreUnavailable(t *testing.T) {
	orch := &Orchestrator{
		Store: queue.NewMemoryStore(), Queue: queue.New(4),
		Providers: Registry{"fake": providerWithDefaults()},
		Ledger:    failingCostLedger{err: cost.ErrInvalidEstimate},
	}
	_, err := orch.Create(context.Background(), requestWithDefaults())
	if err == nil || !errors.Is(err, cost.ErrInvalidEstimate) {
		t.Fatalf("invalid estimate: err=%v, want cost.ErrInvalidEstimate", err)
	}
	if errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("invalid estimate wrapped as store-unavailable: %v", err)
	}
}
