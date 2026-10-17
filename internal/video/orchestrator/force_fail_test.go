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

type blockingOutputsProvider struct {
	*fakeProvider
	started chan struct{}
	resume  <-chan struct{}
}

func (p *blockingOutputsProvider) DownloadOutputs(ctx context.Context, status video.ProviderJobStatus, sink video.AssetSink) ([]video.Asset, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.resume:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return p.fakeProvider.DownloadOutputs(ctx, status, sink)
}

func TestForceFailAfterComfyUIRefusesCancelFencesInFlightCompletion(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.id = "comfyui"
	p.estimate.Provider = "comfyui"
	p.submit = video.ProviderJob{ProviderJobID: "comfyui-remote", Provider: "comfyui", Model: "model-a", Accepted: true}
	p.cancelErr = errors.New("ComfyUI refused cancellation for running job")
	actual := 1.25
	p.statuses = []video.ProviderJobStatus{{
		ProviderJobID: "comfyui-remote", State: video.StateCompleted, Progress: 1,
		ActualCostUSD: &actual, Outputs: []video.Asset{{ID: "output", Filename: "output.mp4"}},
	}}
	p.downloaded = []video.Asset{validStoredAsset()}
	resume := make(chan struct{})
	blocking := &blockingOutputsProvider{fakeProvider: p, started: make(chan struct{}, 1), resume: resume}
	store, q := queue.NewMemoryStore(), queue.New(2)
	ledger := &cost.Ledger{}
	orch := &Orchestrator{
		Store: store, Queue: q, Providers: Registry{"comfyui": blocking},
		Assets: testSink{}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1,
	}
	request := requestWithDefaults()
	request.ProviderPreference = "comfyui"
	job, err := orch.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- orch.RunJob(ctx, queued) }()
	select {
	case <-blocking.started:
	case err := <-done:
		t.Fatalf("job exited before output download began: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("provider output download did not start")
	}

	attempted, confirmed, err := orch.ForceFail(ctx, job.JobID)
	if err != nil {
		t.Fatalf("ForceFail: %v", err)
	}
	if !attempted || confirmed || p.cancelCalls != 1 {
		t.Fatalf("provider cancel attempt/confirmation/calls=%v/%v/%d, want true/false/1", attempted, confirmed, p.cancelCalls)
	}
	if ledger.Reserved() != 0 || ledger.Spent() != 0 || ledger.EstimatedSpent() != 0 {
		t.Fatalf("force-fail ledger reserved/spent/estimated=%v/%v/%v", ledger.Reserved(), ledger.Spent(), ledger.EstimatedSpent())
	}
	close(resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("late in-flight completion returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight completion did not stop after force-fail")
	}

	got, err := store.Get(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateFailed || !got.OperatorForceFailed || got.CostReconciled || len(got.OutputAssets) != 0 {
		t.Fatalf("force-failed job was overwritten or reconciled: state=%s forced=%v reconciled=%v outputs=%d", got.State, got.OperatorForceFailed, got.CostReconciled, len(got.OutputAssets))
	}
	if !strings.Contains(got.LastError, "not confirmed") || !strings.Contains(got.LastError, "may continue") {
		t.Fatalf("force-fail status is not truthful: %q", got.LastError)
	}
	late := got
	late.OperatorForceFailed = false
	late.State = video.StateCompleted
	if err := store.Update(ctx, late); !errors.Is(err, queue.ErrTerminalJob) {
		t.Fatalf("late terminal update error=%v, want ErrTerminalJob", err)
	}
	if _, _, err := orch.ForceFail(ctx, job.JobID); !errors.Is(err, ErrForceFailNotActive) {
		t.Fatalf("repeated force-fail error=%v, want ErrForceFailNotActive", err)
	}
	if p.cancelCalls != 1 || ledger.Reserved() != 0 || ledger.Spent() != 0 {
		t.Fatalf("repeated/late path repeated work: cancel calls=%d reserved=%v spent=%v", p.cancelCalls, ledger.Reserved(), ledger.Spent())
	}
}
