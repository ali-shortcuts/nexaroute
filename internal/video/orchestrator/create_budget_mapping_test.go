package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// failingCostLedger simulates a ledger backend outage: ReserveForJob fails
// with a generic error, the way the PostgreSQL ledger wraps a connection
// failure.
type failingCostLedger struct{ err error }

func (f failingCostLedger) ReserveForJob(string, video.CostEstimate, float64) error {
	return f.err
}
func (failingCostLedger) ReleaseForJob(string)                        {}
func (failingCostLedger) SettleActual(string, float64, float64) error { return nil }
func (failingCostLedger) SettleEstimate(string, float64) error        { return nil }
func (failingCostLedger) Entries() []video.CostEstimate               { return nil }
func (failingCostLedger) Reserved() float64                           { return 0 }
func (failingCostLedger) Spent() float64                              { return 0 }

// A budget rejection during creation must reach the caller as the business
// error itself, not wrapped as store-unavailable: the API maps
// ErrStoreUnavailable to a retryable 503, but an over-budget request must
// be a 400 the client fixes, not a request it retries unchanged.
func TestCreateBudgetRejectionIsNotStoreUnavailable(t *testing.T) {
	orch := &Orchestrator{
		Store: queue.NewMemoryStore(), Queue: queue.New(4),
		Providers: Registry{"fake": providerWithDefaults()},
		Ledger:    &cost.Ledger{},
	}
	r := requestWithDefaults()
	r.MaxCostUSD = 1 // fake estimate upper bound is 3
	_, err := orch.Create(context.Background(), r)
	if err == nil || !errors.Is(err, cost.ErrBudgetExceeded) {
		t.Fatalf("budget rejection: err=%v, want cost.ErrBudgetExceeded", err)
	}
	if errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("budget rejection wrapped as store-unavailable: %v", err)
	}
}

// A genuine ledger outage during creation must still map to
// ErrStoreUnavailable, so the API keeps returning a retryable 503.
func TestCreateLedgerOutageIsStoreUnavailable(t *testing.T) {
	orch := &Orchestrator{
		Store: queue.NewMemoryStore(), Queue: queue.New(4),
		Providers: Registry{"fake": providerWithDefaults()},
		Ledger:    failingCostLedger{err: errors.New("video cost ledger: begin reserve: dial tcp: connection refused")},
	}
	_, err := orch.Create(context.Background(), requestWithDefaults())
	if err == nil || !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("ledger outage: err=%v, want ErrStoreUnavailable", err)
	}
}
