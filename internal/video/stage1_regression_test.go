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
	st := queue.NewMemoryStore()
	q := queue.New(2)
	p := &estimateFailureProvider{Fake: providers.NewFake(1)}
	l := &cost.Ledger{}
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}, Ledger: l}
	_, err := o.Create(ctx, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo, ProviderPreference: "fake", MaxCostUSD: 1})
	if err == nil || !strings.Contains(err.Error(), "cannot safely estimate video cost") {
		t.Fatalf("expected fail-closed cost estimate, got %v", err)
	}
	jobs, listErr := st.List(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(jobs) != 0 || q.Len() != 0 || l.Reserved() != 0 {
		t.Fatalf("failed estimate admitted work: jobs=%d queue=%d reserved=%.2f", len(jobs), q.Len(), l.Reserved())
	}
}

func TestCreateRejectsDurationBeyondAdvertisedProviderLimit(t *testing.T) {
	ctx := context.Background()
	st := queue.NewMemoryStore()
	q := queue.New(2)
	p := providers.NewFake(1)
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}, Ledger: &cost.Ledger{}}
	_, err := o.Create(ctx, video.VideoRequest{ProjectID: "p", Prompt: "ten minute story", DurationSeconds: 600, Mode: video.ModeTextToVideo, ProviderPreference: "fake"})
	if err == nil || !strings.Contains(err.Error(), "exceeds provider maximum") {
		t.Fatalf("expected unsupported duration to fail, got %v", err)
	}
	if q.Len() != 0 {
		t.Fatalf("unsupported duration entered queue: %d", q.Len())
	}
}

func TestRecoverAvoidsResubmittingAmbiguousAdmission(t *testing.T) {
	ctx := context.Background()
	st := queue.NewMemoryStore()
	q := queue.New(4)
	p := providers.NewFake(1)
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}}

	job := video.NewJob("ambiguous", video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	job.Provider = "fake"
	job.Model = "fake-video-1"
	job.State = video.StateAdmitted
	if err := st.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := o.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateNeedsManualAction {
		t.Fatalf("ambiguous submission state=%s, want needs_manual_action", got.State)
	}
	if !strings.Contains(got.LastError, "duplicate provider charges") {
		t.Fatalf("unexpected recovery explanation: %q", got.LastError)
	}
	if p.NextID != 0 || q.Len() != 0 {
		t.Fatalf("recovery blindly submitted work: provider creates=%d queue=%d", p.NextID, q.Len())
	}
}

func TestRecoverResumesKnownProviderJobWithoutCreatingAnother(t *testing.T) {
	ctx := context.Background()
	st := queue.NewMemoryStore()
	q := queue.New(4)
	p := providers.NewFake(2)
	providerJob, err := p.CreateJob(ctx, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	if err != nil {
		t.Fatal(err)
	}
	job := video.NewJob("known", video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	job.Provider, job.Model = "fake", "fake-video-1"
	job.ProviderJobID = providerJob.ProviderJobID
	job.State = video.StateProcessing
	if err := st.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}}
	if err := o.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 1 {
		t.Fatalf("recovered queue size=%d, want 1", q.Len())
	}
	if p.NextID != 1 {
		t.Fatalf("recovery created another provider job: creates=%d", p.NextID)
	}
}

func TestLedgerJobReservationAndEstimateSettlementAreIdempotent(t *testing.T) {
	l := &cost.Ledger{}
	e := video.CostEstimate{Provider: "p", Model: "m", Currency: "USD", EstimatedUSD: 3, UpperBoundUSD: 4, PriceKnown: true}
	if err := l.ReserveForJob("job-1", e, 4); err != nil {
		t.Fatal(err)
	}
	if err := l.ReserveForJob("job-1", e, 4); err != nil {
		t.Fatal(err)
	}
	if l.Reserved() != 4 {
		t.Fatalf("duplicate reservation inflated reserved amount: %.2f", l.Reserved())
	}
	if err := l.SettleEstimate("job-1", 3); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleEstimate("job-1", 3); err != nil {
		t.Fatal(err)
	}
	if l.Reserved() != 0 || l.Spent() != 0 || l.EstimatedSpent() != 3 {
		t.Fatalf("wrong settlement reserved=%.2f actual=%.2f estimated=%.2f", l.Reserved(), l.Spent(), l.EstimatedSpent())
	}
}

func TestRunJobDownloadsAssetsBeforeMarkingComplete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sink, err := storage.NewLocal(dir, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	st := queue.NewMemoryStore()
	q := queue.New(2)
	p := providers.NewFake(1)
	l := &cost.Ledger{}
	o := &orchestrator.Orchestrator{Store: st, Queue: q, Providers: orchestrator.Registry{"fake": p}, Ledger: l, Assets: sink, PollInterval: time.Millisecond, MaxPolls: 4}
	created, err := o.Create(ctx, video.VideoRequest{ProjectID: "p", Prompt: "scene", DurationSeconds: 1, Mode: video.ModeTextToVideo, ProviderPreference: "fake", IdempotencyKey: "artifact-test"})
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
		t.Fatalf("job completed without persisted output: state=%s assets=%d err=%s", got.State, len(got.OutputAssets), got.LastError)
	}
	bytes, err := sink.(*storage.Local).Read(got.OutputAssets[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bytes), "not a playable video") {
		t.Fatalf("fake provider artifact wasn't transparently marked: %q", bytes)
	}
	if l.Reserved() != 0 || l.EstimatedSpent() != got.EstimatedCostUSD {
		t.Fatalf("cost reservation not settled: reserved=%.2f estimate_spent=%.2f expected=%.2f", l.Reserved(), l.EstimatedSpent(), got.EstimatedCostUSD)
	}
	time.Sleep(time.Millisecond)
}


func TestFileStoreRollsBackMemoryOnPersistenceFailure(t *testing.T) {
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

	// Replace the parent directory with a regular file so persistence fails
	// after the in-memory map is tentatively updated.
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
		t.Fatalf("failed update leaked into memory: state=%s want=%s", got.State, first.State)
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
