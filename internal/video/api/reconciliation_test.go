package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

// TestAPIExposesReconciliationFields proves the reconciled delta is visible
// through the same GET surface that exposes video job cost today, using
// additive fields only: existing estimated_cost_usd remains, and the new
// cost_reconciled/cost_delta_usd/cost_basis/actual_cost_usd fields appear.
func TestAPIExposesReconciliationFields(t *testing.T) {
	ctx := context.Background()
	store := queue.NewMemoryStore()
	q := queue.New(4)
	actual := 0.015
	fake := providers.NewFake(1)
	fake.ActualCostUSD = &actual
	sink, err := storage.NewLocal(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	orch := &orchestrator.Orchestrator{
		Store: store, Queue: q, Providers: orchestrator.Registry{"fake": fake},
		Assets: sink, Ledger: &cost.Ledger{}, PollInterval: time.Millisecond, MaxPolls: 3,
	}
	h := &Handler{Orch: orch, Store: store, Providers: orchestrator.Registry{"fake": fake}, Assets: sink}
	created := performJSON(h, http.MethodPost, "/v1/video/jobs", validAPIRequest(), "")
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var job video.VideoJob
	if err := json.Unmarshal(created.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	queued, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := orch.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	got := performJSON(h, http.MethodGet, "/v1/video/jobs/"+job.JobID, nil, "")
	if got.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", got.Code, got.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"estimated_cost_usd", "actual_cost_usd", "cost_reconciled", "cost_delta_usd", "cost_basis"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("GET response missing additive cost field %q: %s", key, got.Body.String())
		}
	}
	if raw["cost_reconciled"] != true || raw["cost_basis"] != "actual" {
		t.Fatalf("GET reconciliation flags wrong: %s", got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "estimated_cost_usd") {
		t.Fatal("existing estimated_cost_usd shape broken")
	}
	var fetched video.VideoJob
	if err := json.Unmarshal(got.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if !fetched.CostReconciled || fetched.CostDeltaUSD == nil || fetched.ActualCostUSD != actual {
		t.Fatalf("fetched reconciliation wrong: %+v", fetched)
	}
}
