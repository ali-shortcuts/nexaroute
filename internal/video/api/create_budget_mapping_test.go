package api

import (
	"net/http"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// A budget rejection during creation is a client error (400, not
// retryable), never a retryable 503: the client must lower its budget or
// pick a cheaper option, not retry the same request unchanged.
func TestHandlerCreateBudgetRejectionIs400Not503(t *testing.T) {
	store := queue.NewMemoryStore()
	provider := providers.NewFake(1)
	orch := &orchestrator.Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers: orchestrator.Registry{"fake": provider},
		Ledger:    &cost.Ledger{},
	}
	h := &Handler{Orch: orch, Store: store, Providers: orchestrator.Registry{"fake": provider}, RequireAuth: false}
	req := validAPIRequest()
	req.MaxCostUSD = 0.01 // fake estimate upper bound is DurationSeconds*0.01 = 0.04
	w := performJSON(h, http.MethodPost, "/v1/video/jobs", req, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST create over budget: status=%d, want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if body := errorBody(t, w); body["code"] != "video_create_failed" || body["retryable"] != false {
		t.Fatalf("POST create over budget: error body=%v", body)
	}
}
