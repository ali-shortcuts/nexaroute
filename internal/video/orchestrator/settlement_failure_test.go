package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type injectedSettlementLedger struct {
	*cost.Ledger
	actualErr   error
	estimateErr error
}

func (l *injectedSettlementLedger) SettleActual(jobID string, actual, estimate float64) error {
	if l.actualErr != nil {
		return l.actualErr
	}
	return l.Ledger.SettleActual(jobID, actual, estimate)
}

func (l *injectedSettlementLedger) SettleEstimate(jobID string, estimate float64) error {
	if l.estimateErr != nil {
		return l.estimateErr
	}
	return l.Ledger.SettleEstimate(jobID, estimate)
}

func TestRegularCompletionSettlementFailureStaysUnreconciledAndResumable(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
	actual := 1.5
	p.statuses = []video.ProviderJobStatus{{
		ProviderJobID: "remote", State: video.StateCompleted, Progress: 1,
		ActualCostUSD: &actual, Outputs: []video.Asset{{ID: "out", Filename: "out.mp4"}},
	}}
	p.downloaded = []video.Asset{validStoredAsset()}
	store, q := queue.NewMemoryStore(), queue.New(2)
	baseLedger := &cost.Ledger{}
	ledger := &injectedSettlementLedger{Ledger: baseLedger, actualErr: errors.New("injected actual settlement failure")}
	orch := &Orchestrator{
		Store: store, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: ledger,
		PollInterval: time.Nanosecond, MaxPolls: 1,
	}
	job, err := orch.Create(ctx, requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := orch.RunJob(ctx, queued); err == nil || !strings.Contains(err.Error(), "settlement failed") {
		t.Fatalf("RunJob error=%v, want settlement failure", err)
	}
	got, err := store.Get(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateUploading || got.CostReconciled || got.CostBasis != "estimate" {
		t.Fatalf("failed regular settlement was persisted as complete/reconciled: state=%s reconciled=%v basis=%q", got.State, got.CostReconciled, got.CostBasis)
	}
	if !strings.Contains(got.LastError, "settlement failed") || baseLedger.Reserved() <= 0 || baseLedger.Spent() != 0 {
		t.Fatalf("regular retry/accounting state: error=%q reserved=%v spent=%v", got.LastError, baseLedger.Reserved(), baseLedger.Spent())
	}
}

func TestEpisodeCompletionSettlementFailureStaysUnreconciledAndResumable(t *testing.T) {
	ctx := context.Background()
	base := providerWithDefaults()
	firstActual, secondActual := 0.75, 1.25
	base.statuses = []video.ProviderJobStatus{
		{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, ActualCostUSD: &firstActual, Outputs: []video.Asset{{ID: "out-1", Filename: "one.mp4", ContentType: "video/mp4"}}},
		{ProviderJobID: "remote-2", State: video.StateCompleted, Progress: 1, ActualCostUSD: &secondActual, Outputs: []video.Asset{{ID: "out-2", Filename: "two.mp4", ContentType: "video/mp4"}}},
	}
	provider := &episodeTestProvider{fakeProvider: base}
	store, q := queue.NewMemoryStore(), queue.New(4)
	baseLedger := &cost.Ledger{}
	ledger := &injectedSettlementLedger{Ledger: baseLedger, actualErr: errors.New("injected episode actual settlement failure")}
	orch := newEpisodeOrchestrator(t, store, q, provider, &episodeTestSink{root: t.TempDir()}, &episodeTestComposer{})
	orch.Ledger = ledger
	orch.PollInterval = time.Nanosecond
	job, err := orch.Create(ctx, episodeRequest())
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := orch.RunJob(ctx, queued); err == nil || !strings.Contains(err.Error(), "settlement failed") {
		t.Fatalf("RunJob error=%v, want episode settlement failure", err)
	}
	got, err := store.Get(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateComposing || got.CostReconciled || got.CostBasis != "estimate" || len(got.OutputAssets) != 0 {
		t.Fatalf("failed episode settlement was persisted as complete/reconciled: state=%s reconciled=%v basis=%q outputs=%d", got.State, got.CostReconciled, got.CostBasis, len(got.OutputAssets))
	}
	if !strings.Contains(got.LastError, "settlement failed") || baseLedger.Reserved() <= 0 || baseLedger.Spent() != 0 {
		t.Fatalf("episode retry/accounting state: error=%q reserved=%v spent=%v", got.LastError, baseLedger.Reserved(), baseLedger.Spent())
	}
}
