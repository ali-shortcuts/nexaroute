package orchestrator

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

func floatPtr(v float64) *float64 { return &v }

func TestReconcileOnCompletionWithDeltaMath(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
	actual := 1.25
	p.statuses = []video.ProviderJobStatus{{
		ProviderJobID: "remote", State: video.StateCompleted, Progress: 1,
		ActualCostUSD: floatPtr(actual),
		Outputs:       []video.Asset{{ID: "out", Filename: "out.mp4"}},
	}}
	p.downloaded = []video.Asset{validStoredAsset()}
	st, q := queue.NewMemoryStore(), queue.New(1)
	ledger := &cost.Ledger{}
	o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1}
	created, err := o.Create(ctx, requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CostReconciled || got.CostBasis != "actual" {
		t.Fatalf("job not marked reconciled: reconciled=%v basis=%q", got.CostReconciled, got.CostBasis)
	}
	if got.ActualCostUSD != actual {
		t.Fatalf("actual=%v want %v", got.ActualCostUSD, actual)
	}
	wantDelta := actual - got.EstimatedCostUSD
	if got.CostDeltaUSD == nil || math.Abs(*got.CostDeltaUSD-wantDelta) > 1e-9 {
		t.Fatalf("delta=%v want %v (estimate=%v)", got.CostDeltaUSD, wantDelta, got.EstimatedCostUSD)
	}
	if ledger.Spent() != actual || ledger.Reserved() != 0 {
		t.Fatalf("ledger spent=%v reserved=%v", ledger.Spent(), ledger.Reserved())
	}
	est, act, delta, reconciled, ok := ledger.Reconciliation(created.JobID)
	if !ok || !reconciled || act != actual || math.Abs(delta-wantDelta) > 1e-9 || est != got.EstimatedCostUSD {
		t.Fatalf("ledger reconciliation est=%v act=%v delta=%v reconciled=%v ok=%v", est, act, delta, reconciled, ok)
	}
}

func TestReconcileLegacyUsageFallback(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
	p.statuses = []video.ProviderJobStatus{{
		ProviderJobID: "remote", State: video.StateCompleted, Progress: 1,
		Usage:   map[string]string{"actual_cost_usd": "1.5"},
		Outputs: []video.Asset{{ID: "out", Filename: "out.mp4"}},
	}}
	p.downloaded = []video.Asset{validStoredAsset()}
	st, q := queue.NewMemoryStore(), queue.New(1)
	ledger := &cost.Ledger{}
	o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1}
	created, err := o.Create(ctx, requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	queued, _ := q.Next(ctx)
	if err := o.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, created.JobID)
	if !got.CostReconciled || got.ActualCostUSD != 1.5 || got.CostDeltaUSD == nil {
		t.Fatalf("legacy usage fallback not reconciled: %+v", got)
	}
}

func TestNoActualJobStaysEstimateUnreconciled(t *testing.T) {
	ctx := context.Background()
	for name, status := range map[string]video.ProviderJobStatus{
		"no usage": {ProviderJobID: "remote", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{Filename: "out.mp4"}}},
		"invalid usage": {ProviderJobID: "remote", State: video.StateCompleted, Progress: 1,
			Usage:   map[string]string{"actual_cost_usd": "NaN"},
			Outputs: []video.Asset{{Filename: "out.mp4"}}},
		"negative explicit": {ProviderJobID: "remote", State: video.StateCompleted, Progress: 1,
			ActualCostUSD: floatPtr(-1),
			Outputs:       []video.Asset{{Filename: "out.mp4"}}},
	} {
		t.Run(name, func(t *testing.T) {
			p := providerWithDefaults()
			p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
			p.statuses = []video.ProviderJobStatus{status}
			p.downloaded = []video.Asset{validStoredAsset()}
			st, q := queue.NewMemoryStore(), queue.New(1)
			ledger := &cost.Ledger{}
			o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1}
			created, err := o.Create(ctx, requestWithDefaults())
			if err != nil {
				t.Fatal(err)
			}
			queued, _ := q.Next(ctx)
			if err := o.RunJob(ctx, queued); err != nil {
				t.Fatal(err)
			}
			got, _ := st.Get(ctx, created.JobID)
			if got.CostReconciled {
				t.Fatalf("job silently presented as actual: %+v", got)
			}
			if got.CostBasis != "estimate" || got.CostDeltaUSD != nil || got.ActualCostUSD != 0 {
				t.Fatalf("unreconciled fields wrong: basis=%q delta=%v actual=%v", got.CostBasis, got.CostDeltaUSD, got.ActualCostUSD)
			}
			if ledger.EstimatedSpent() != got.EstimatedCostUSD || ledger.Spent() != 0 {
				t.Fatalf("ledger estimated=%v spent=%v", ledger.EstimatedSpent(), ledger.Spent())
			}
		})
	}
}

func TestDuplicateCompletionDoesNotDoubleApply(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
	actual := 2.0
	p.statuses = []video.ProviderJobStatus{{
		ProviderJobID: "remote", State: video.StateCompleted, Progress: 1,
		ActualCostUSD: floatPtr(actual),
		Outputs:       []video.Asset{{ID: "out", Filename: "out.mp4"}},
	}}
	p.downloaded = []video.Asset{validStoredAsset()}
	st, q := queue.NewMemoryStore(), queue.New(2)
	ledger := &cost.Ledger{}
	o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1}
	created, err := o.Create(ctx, requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	queued, _ := q.Next(ctx)
	if err := o.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	first, _ := st.Get(ctx, created.JobID)
	spentAfterFirst := ledger.Spent()
	// Replay the terminal job through RunJob: must be a safe no-op.
	if err := o.RunJob(ctx, first); err != nil {
		t.Fatalf("replay of completed job error=%v", err)
	}
	second, _ := st.Get(ctx, created.JobID)
	if ledger.Spent() != spentAfterFirst || second.ActualCostUSD != first.ActualCostUSD || !second.CostReconciled {
		t.Fatalf("replay double-applied: spent=%v first=%+v second=%+v", ledger.Spent(), first, second)
	}
	if second.CostDeltaUSD == nil || *second.CostDeltaUSD != *first.CostDeltaUSD {
		t.Fatalf("replay changed delta: %v vs %v", second.CostDeltaUSD, first.CostDeltaUSD)
	}
}

func TestRecoveryPreservesReconciledState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.json")
	store, err := queue.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p := providers.NewFake(1)
	deterministic := 0.015
	p.ActualCostUSD = floatPtr(deterministic)
	sink, err := storage.NewLocal(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &cost.Ledger{}
	o := &Orchestrator{Store: store, Queue: queue.New(2), Providers: Registry{"fake": p}, Assets: sink, Ledger: ledger, PollInterval: time.Millisecond, MaxPolls: 3}
	created, err := o.Create(ctx, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo, ProviderPreference: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := o.Queue.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.RunJob(ctx, queued); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.CostReconciled || completed.ActualCostUSD != deterministic || completed.CostDeltaUSD == nil {
		t.Fatalf("fake deterministic actual not reconciled: %+v", completed)
	}
	wantDelta := deterministic - completed.EstimatedCostUSD
	if math.Abs(*completed.CostDeltaUSD-wantDelta) > 1e-9 {
		t.Fatalf("delta=%v want %v", *completed.CostDeltaUSD, wantDelta)
	}
	// Simulate a restart: reopen the file store and recover with a fresh
	// in-memory ledger. The completed job must keep its reconciled fields
	// and must not be re-queued or double-settled.
	reopened, err := queue.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	freshLedger := &cost.Ledger{}
	recoveredQueue := queue.New(4)
	restarted := &Orchestrator{Store: reopened, Queue: recoveredQueue, Providers: Registry{"fake": providers.NewFake(1)}, Ledger: freshLedger}
	if err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if recoveredQueue.Len() != 0 {
		t.Fatalf("recovery re-queued completed job: queue=%d", recoveredQueue.Len())
	}
	after, err := reopened.Get(ctx, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.CostReconciled || after.ActualCostUSD != deterministic || after.CostDeltaUSD == nil || *after.CostDeltaUSD != *completed.CostDeltaUSD || after.CostBasis != "actual" {
		t.Fatalf("recovery lost reconciled state: %+v", after)
	}
	if freshLedger.Spent() != 0 {
		t.Fatalf("recovery double-settled fresh ledger: spent=%v", freshLedger.Spent())
	}
}

func TestFakeProviderDeterministicActualHook(t *testing.T) {
	p := providers.NewFake(1)
	if _, err := p.CreateJob(context.Background(), video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo}); err != nil {
		t.Fatal(err)
	}
	// Default fake reports no actual.
	st, err := p.GetJob(context.Background(), "fake-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.ActualCostUSD != nil {
		t.Fatalf("default fake unexpectedly reported actual: %v", *st.ActualCostUSD)
	}
	p2 := providers.NewFake(1)
	want := 0.037
	p2.ActualCostUSD = floatPtr(want)
	if _, err := p2.CreateJob(context.Background(), video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo}); err != nil {
		t.Fatal(err)
	}
	got, err := p2.GetJob(context.Background(), "fake-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ActualCostUSD == nil || *got.ActualCostUSD != want {
		t.Fatalf("deterministic actual=%v want %v", got.ActualCostUSD, want)
	}
	if got.Usage["actual_cost_usd"] == "" {
		t.Fatal("deterministic actual missing from legacy usage entry")
	}
}

func TestEpisodeReconciliationSumsShotActuals(t *testing.T) {
	j := video.NewJob("episode-1", video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 2, Mode: video.ModeTextToVideo})
	j.EstimatedCostUSD = 2.0
	j.ShotExecutions = []video.ShotExecution{
		{ShotID: "s1", ActualCostUSD: floatPtr(0.75)},
		{ShotID: "s2", UsageMetadata: map[string]string{"actual_cost_usd": "1.5"}},
	}
	applyEpisodeCostReconciliation(&j)
	if !j.CostReconciled || j.ActualCostUSD != 2.25 || j.CostDeltaUSD == nil || *j.CostDeltaUSD != 0.25 {
		t.Fatalf("episode reconciliation wrong: %+v", j)
	}
	partial := video.NewJob("episode-2", video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 2, Mode: video.ModeTextToVideo})
	partial.EstimatedCostUSD = 2.0
	partial.ShotExecutions = []video.ShotExecution{
		{ShotID: "s1", ActualCostUSD: floatPtr(0.75)},
		{ShotID: "s2"},
	}
	applyEpisodeCostReconciliation(&partial)
	if partial.CostReconciled || partial.CostBasis != "estimate" || partial.CostDeltaUSD != nil {
		t.Fatalf("partial episode must stay unreconciled: %+v", partial)
	}
}

func TestWebhookCompletionReconcilesIdempotently(t *testing.T) {
	ctx := context.Background()
	st := queue.NewMemoryStore()
	j := queueJobForTest("webhook-1")
	j.Provider, j.ProviderJobID, j.State, j.EstimatedCostUSD = "fake", "remote-1", video.StateProcessing, 2
	if err := st.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	ledger := &cost.Ledger{}
	wp := &WebhookProcessor{Store: st, Ledger: ledger}
	actual := 2.5
	if err := wp.Apply(ctx, "evt-1", video.ProviderJobStatus{ProviderJobID: "remote-1", State: video.StateCompleted, ActualCostUSD: floatPtr(actual)}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, "webhook-1")
	if !got.CostReconciled || got.ActualCostUSD != actual || got.CostDeltaUSD == nil || *got.CostDeltaUSD != 0.5 {
		t.Fatalf("webhook reconciliation wrong: %+v", got)
	}
	if ledger.Spent() != actual {
		t.Fatalf("webhook ledger spent=%v", ledger.Spent())
	}
	if err := wp.Apply(ctx, "evt-2", video.ProviderJobStatus{ProviderJobID: "remote-1", State: video.StateCompleted, ActualCostUSD: floatPtr(actual)}); err != nil {
		t.Fatal(err)
	}
	if ledger.Spent() != actual {
		t.Fatalf("webhook replay double-settled: spent=%v", ledger.Spent())
	}
}
