package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// businessErrorLedger stands in for the cost ledger returning one of its
// documented business rejections (or a backend outage) from ReserveForJob.
// It implements orchestrator.CostLedger so the tests drive the real
// Orchestrator.Create through the real HTTP handler.
type businessErrorLedger struct{ err error }

func (b businessErrorLedger) ReserveForJob(string, video.CostEstimate, float64) error {
	return b.err
}
func (businessErrorLedger) ReleaseForJob(string)                        {}
func (businessErrorLedger) SettleActual(string, float64, float64) error { return nil }
func (businessErrorLedger) SettleEstimate(string, float64) error        { return nil }
func (businessErrorLedger) Entries() []video.CostEstimate               { return nil }
func (businessErrorLedger) Reserved() float64                           { return 0 }
func (businessErrorLedger) Spent() float64                              { return 0 }

func handlerWithLedger(t *testing.T, ledger orchestrator.CostLedger) *Handler {
	t.Helper()
	store := queue.NewMemoryStore()
	provider := providers.NewFake(1)
	orch := &orchestrator.Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers: orchestrator.Registry{"fake": provider},
		Ledger:    ledger,
	}
	return &Handler{Orch: orch, Store: store, Providers: orchestrator.Registry{"fake": provider}, RequireAuth: false}
}

// An unknown-price rejection from the ledger is a client error (400),
// never a retryable 503: on the unfixed #698 head it arrived wrapped as
// store-unavailable and mapped to 503.
func TestHandlerCreateUnknownPriceLedgerIs400Not503(t *testing.T) {
	h := handlerWithLedger(t, businessErrorLedger{err: cost.ErrUnknownPrice})
	w := performJSON(h, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST create unknown price: status=%d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "video_create_failed" || body["retryable"] != false {
		t.Fatalf("POST create unknown price: error body=%v", body)
	}
	if strings.Contains(w.Body.String(), "temporarily unavailable") {
		t.Fatalf("POST create unknown price: mapped to the store-unavailable path: %s", w.Body.String())
	}
}

// An invalid-estimate rejection from the ledger is a client error (400),
// never a retryable 503, for the same reason as unknown price.
func TestHandlerCreateInvalidEstimateLedgerIs400Not503(t *testing.T) {
	h := handlerWithLedger(t, businessErrorLedger{err: cost.ErrInvalidEstimate})
	w := performJSON(h, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST create invalid estimate: status=%d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "video_create_failed" || body["retryable"] != false {
		t.Fatalf("POST create invalid estimate: error body=%v", body)
	}
	if strings.Contains(w.Body.String(), "temporarily unavailable") {
		t.Fatalf("POST create invalid estimate: mapped to the store-unavailable path: %s", w.Body.String())
	}
}

// A genuine ledger outage during creation must stay a generic retryable
// 503: the response must carry the generic store-unavailable message and
// must not leak backend detail (dial errors, DSNs, SQL).
func TestHandlerCreateLedgerOutageIsGeneric503WithoutLeak(t *testing.T) {
	h := handlerWithLedger(t, businessErrorLedger{err: errors.New("video cost ledger: begin reserve: dial tcp: connection refused")})
	w := performJSON(h, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST create ledger outage: status=%d, want %d (body=%s)", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "video_store_unavailable" || body["retryable"] != true {
		t.Fatalf("POST create ledger outage: error body=%v", body)
	}
	if strings.Contains(w.Body.String(), "dial tcp") || strings.Contains(w.Body.String(), "connection refused") {
		t.Fatalf("POST create ledger outage: backend detail leaked: %s", w.Body.String())
	}
}
