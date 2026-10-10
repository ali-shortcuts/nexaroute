package video_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

type estimateFailureProvider struct{ *providers.Fake }

func (p *estimateFailureProvider) EstimateCost(video.VideoRequest) (video.CostEstimate, error) {
	return video.CostEstimate{}, errors.New("pricing unavailable")
}

func TestCreateFailsClosedWhenCostEstimateFails(t *testing.T) {
	ctx := context.Background()
	st, q, ledger := queue.NewMemoryStore(), queue.New(2), &cost.Ledger{}
	p := &estimateFailureProvider{Fake: providers.NewFake(1)}
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}, Ledger: ledger}
	_, err := o.Create(ctx, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo, ProviderPreference: "fake", MaxCostUSD: 1})
	if err == nil || !strings.Contains(err.Error(), "cannot safely estimate video cost") {
		t.Fatalf("expected fail-closed estimate, got %v", err)
	}
	jobs, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 || q.Len() != 0 || ledger.Reserved() != 0 {
		t.Fatalf("failed estimate admitted work: jobs=%d queued=%d reserved=%.2f", len(jobs), q.Len(), ledger.Reserved())
	}
}

func TestCreateRejectsDurationBeyondProviderLimit(t *testing.T) {
	ctx := context.Background()
	st, q := queue.NewMemoryStore(), queue.New(2)
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": providers.NewFake(1)}, Ledger: &cost.Ledger{}}
	_, err := o.Create(ctx, video.VideoRequest{ProjectID: "p", Prompt: "ten minute story", DurationSeconds: 600, Mode: video.ModeTextToVideo, ProviderPreference: "fake"})
	if err == nil || !strings.Contains(err.Error(), "exceeds provider maximum") {
		t.Fatalf("expected unsupported duration to fail, got %v", err)
	}
	if q.Len() != 0 {
		t.Fatalf("unsupported duration entered queue: %d", q.Len())
	}
}

func TestRecoverNeverResubmitsAmbiguousOrDuplicatesKnownJobs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.json")
	st, err := queue.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	q := queue.New(4)
	p := providers.NewFake(2)
	providerJob, err := p.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	if err != nil {
		t.Fatal(err)
	}

	known := video.NewJob("known", video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	known.Provider, known.Model, known.ProviderJobID, known.State = "fake", "fake-video-1", providerJob.ProviderJobID, video.StateProcessing
	if err := st.Create(ctx, known); err != nil {
		t.Fatal(err)
	}
	ambiguous := video.NewJob("ambiguous", video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	ambiguous.Provider, ambiguous.Model, ambiguous.State = "fake", "fake-video-1", video.StateAdmitted
	if err := st.Create(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	reopened, err := queue.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	o := &orchestrator.Orchestrator{Store: reopened, Queue: q, Providers: orchestrator.Registry{"fake": p}}
	if err := o.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(ctx, "ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateNeedsManualAction || !strings.Contains(got.LastError, "duplicate provider charges") {
		t.Fatalf("ambiguous job not fenced: state=%s error=%q", got.State, got.LastError)
	}
	if q.Len() != 1 || p.NextID != 1 {
		t.Fatalf("recovery queue=%d provider creates=%d, want queue=1 creates=1", q.Len(), p.NextID)
	}
}

func TestRunJobPersistsOutputBeforeCompletionAndSettlesEstimate(t *testing.T) {
	ctx := context.Background()
	sink, err := storage.NewLocal(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	st, q, p, ledger := queue.NewMemoryStore(), queue.New(2), providers.NewFake(1), &cost.Ledger{}
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}, Ledger: ledger, Assets: sink, PollInterval: time.Millisecond, MaxPolls: 3}
	created, err := o.Create(ctx, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo, ProviderPreference: "fake", IdempotencyKey: "artifact"})
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
	if got.State != video.StateCompleted || len(got.OutputAssets) != 1 {
		t.Fatalf("job completion without output: state=%s assets=%d error=%s", got.State, len(got.OutputAssets), got.LastError)
	}
	data, err := sink.Read(got.OutputAssets[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "not a playable video") || got.OutputAssets[0].Kind != "development-placeholder" {
		t.Fatal("fake provider output was misrepresented as a real video")
	}
	if ledger.Reserved() != 0 || ledger.EstimatedSpent() != got.EstimatedCostUSD {
		t.Fatalf("reservation not settled: reserved=%.2f estimated=%.2f", ledger.Reserved(), ledger.EstimatedSpent())
	}
	time.Sleep(time.Millisecond)
}

func TestFileStoreRollsBackMemoryAfterDurabilityFailure(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "jobs")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := queue.NewFileStore(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := video.NewJob("first", video.VideoRequest{ProjectID: "p", Prompt: "one", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("block directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	updated := first
	updated.State = video.StateProcessing
	if err := store.Update(ctx, updated); err == nil {
		t.Fatal("expected update persistence failure")
	}
	got, err := store.Get(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != first.State {
		t.Fatalf("failed update leaked into memory: %s want %s", got.State, first.State)
	}

	second := video.NewJob("second", video.VideoRequest{ProjectID: "p", Prompt: "two", DurationSeconds: 1, Mode: video.ModeTextToVideo, IdempotencyKey: "second-key"})
	if err := store.Create(ctx, second); err == nil {
		t.Fatal("expected create persistence failure")
	}
	if _, err := store.Get(ctx, second.JobID); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("failed create leaked into memory: %v", err)
	}
	if _, err := store.GetByIdempotency(ctx, "second-key"); !errors.Is(err, queue.ErrNotFound) {
		t.Fatalf("failed create leaked idempotency index: %v", err)
	}
}

func TestCancelledContextDoesNotSubmitQueuedProviderJob(t *testing.T) {
	bg := context.Background()
	st, q, p := queue.NewMemoryStore(), queue.New(2), providers.NewFake(1)
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}, Ledger: &cost.Ledger{}}
	created, err := o.Create(bg, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo, ProviderPreference: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := q.Next(bg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := o.RunJob(ctx, queued); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunJob error=%v want context.Canceled", err)
	}
	got, err := st.Get(bg, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateQueued || p.NextID != 0 {
		t.Fatalf("cancelled queued work was submitted or mutated: state=%s creates=%d", got.State, p.NextID)
	}
}
