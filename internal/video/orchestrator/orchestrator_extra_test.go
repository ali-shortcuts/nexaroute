package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type failingJobStore struct {
	*queue.MemoryStore
	listErr   error
	updateErr error
}

func (s *failingJobStore) List(context.Context) ([]video.VideoJob, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemoryStore.List(context.Background())
}
func (s *failingJobStore) Update(context.Context, video.VideoJob) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.MemoryStore.Update(context.Background(), video.VideoJob{})
}

func TestRecoveryAndBackpressureErrorPaths(t *testing.T) {
	ctx := context.Background()
	if err := (&Orchestrator{}).Recover(ctx); err == nil {
		t.Fatal("recovery accepted missing store and queue")
	}
	listErr := errors.New("list unavailable")
	store := &failingJobStore{MemoryStore: queue.NewMemoryStore(), listErr: listErr}
	if err := (&Orchestrator{Store: store, Queue: queue.New(1)}).Recover(ctx); !errors.Is(err, listErr) {
		t.Fatalf("recovery list error=%v", err)
	}
	q := queue.New(1)
	if err := q.Enqueue(ctx, queueJobForTest("occupied")); err != nil {
		t.Fatal(err)
	}
	job := queueJobForTest("backpressure")
	dequeued := make(chan struct{})
	go func() {
		<-dequeued
		_, _ = q.Next(ctx)
	}()
	close(dequeued)
	if err := (&Orchestrator{Queue: q}).enqueueWithBackpressure(ctx, job); err != nil {
		t.Fatalf("backpressure enqueue=%v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("queue length=%d want replacement job", q.Len())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := (&Orchestrator{Queue: q}).enqueueWithBackpressure(cancelled, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backpressure error=%v", err)
	}
	(&Orchestrator{}).scheduleRetry(cancelled, job, time.Nanosecond)
}

func TestRunJobProviderUnavailableAndUnsupportedPersistedStates(t *testing.T) {
	ctx := context.Background()
	store := queue.NewMemoryStore()
	job := queueJobForTest("provider-gone")
	if err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{Store: store, Providers: Registry{}}
	if err := o.RunJob(ctx, job); err == nil {
		t.Fatal("missing provider did not fail")
	}
	got, _ := store.Get(ctx, job.JobID)
	if got.State != video.StateFailed || got.LastError != "provider unavailable" {
		t.Fatalf("missing provider state=%s error=%q", got.State, got.LastError)
	}
	active := queueJobForTest("active-no-id")
	active.State = video.StateProcessing
	if err := store.Create(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: store, Providers: Registry{"fake": providerWithDefaults()}}).RunJob(ctx, active); err == nil {
		t.Fatal("active job without remote ID ran")
	}
	got, _ = store.Get(ctx, active.JobID)
	if got.State != video.StateNeedsManualAction {
		t.Fatalf("active no-id state=%s", got.State)
	}
	unsupported := queueJobForTest("unsupported")
	unsupported.State = video.StateExpired
	if err := store.Create(ctx, unsupported); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: store, Providers: Registry{"fake": providerWithDefaults()}}).RunJob(ctx, unsupported); err == nil {
		t.Fatal("unsupported state ran")
	}
}

func TestPollingDeadlineStatusErrorsAndStaleResponses(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	p.statusErrs = []error{errors.New("temporary provider outage")}
	store := queue.NewMemoryStore()
	j := queueJobForTest("deadline")
	j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateSubmitted
	if err := store.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{Store: store, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 1}
	if err := o.RunJob(ctx, j); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("deadline error=%v", err)
	}
	got, _ := store.Get(ctx, j.JobID)
	if got.RetryCount != 1 || !strings.Contains(got.LastError, "deadline") {
		t.Fatalf("retry state=%#v", got)
	}

	staleProvider := providerWithDefaults()
	staleProvider.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateSubmitted, Progress: .4}}
	stale := queueJobForTest("stale")
	stale.Provider, stale.ProviderJobID, stale.State = "fake", "remote", video.StateProcessing
	staleStore := queue.NewMemoryStore()
	if err := staleStore.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	staleOrch := &Orchestrator{Store: staleStore, Providers: Registry{"fake": staleProvider}, PollInterval: time.Nanosecond, MaxPolls: 1}
	if err := staleOrch.RunJob(ctx, stale); err == nil {
		t.Fatal("stale status unexpectedly completed")
	}
	got, _ = staleStore.Get(ctx, stale.JobID)
	if got.State != video.StateProcessing {
		t.Fatalf("stale response regressed state=%s", got.State)
	}
}

func TestDownloadValidationAndPersistenceFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		outputs     []video.Asset
		downloadErr error
		assets      []video.Asset
		want        string
	}{
		{"no provider outputs", nil, nil, nil, "no output assets"},
		{"empty persisted assets", []video.Asset{{Filename: "remote.mp4"}}, nil, nil, "no persisted output assets"},
		{"download failure", []video.Asset{{Filename: "remote.mp4"}}, errors.New("download failed"), nil, "download or persistence failed"},
		{"missing URI", []video.Asset{{Filename: "remote.mp4"}}, nil, []video.Asset{{ContentType: "video/mp4", SizeBytes: 1, SHA256: strings.Repeat("a", 64)}}, "URI"},
		{"missing content type", []video.Asset{{Filename: "remote.mp4"}}, nil, []video.Asset{{URI: "/tmp/a", SizeBytes: 1, SHA256: strings.Repeat("a", 64)}}, "content type"},
		{"empty size", []video.Asset{{Filename: "remote.mp4"}}, nil, []video.Asset{{URI: "/tmp/a", ContentType: "video/mp4", SHA256: strings.Repeat("a", 64)}}, "non-empty size"},
		{"bad checksum length", []video.Asset{{Filename: "remote.mp4"}}, nil, []video.Asset{{URI: "/tmp/a", ContentType: "video/mp4", SizeBytes: 1, SHA256: "bad"}}, "SHA-256"},
		{"bad checksum encoding", []video.Asset{{Filename: "remote.mp4"}}, nil, []video.Asset{{URI: "/tmp/a", ContentType: "video/mp4", SizeBytes: 1, SHA256: strings.Repeat("z", 64)}}, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := providerWithDefaults()
			p.submit = video.ProviderJob{ProviderJobID: "remote", Provider: "fake", Model: "model-a", Accepted: true}
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCompleted, Progress: 1, Outputs: tc.outputs}}
			p.downloaded, p.downloadErr = tc.assets, tc.downloadErr
			store := queue.NewMemoryStore()
			j := queueJobForTest(tc.name)
			j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateSubmitted
			if err := store.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			o := &Orchestrator{Store: store, Providers: Registry{"fake": p}, Assets: testSink{}, PollInterval: time.Nanosecond, MaxPolls: 1}
			if err := o.RunJob(ctx, j); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want substring %q", err, tc.want)
			}
			got, _ := store.Get(ctx, j.JobID)
			if got.State != video.StateNeedsManualAction && tc.name != "download failure" {
				t.Fatalf("state=%s error=%q", got.State, got.LastError)
			}
		})
	}
	noSinkProvider := providerWithDefaults()
	noSinkProvider.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{Filename: "remote.mp4"}}}}
	noSinkStore := queue.NewMemoryStore()
	noSinkJob := queueJobForTest("no-sink")
	noSinkJob.Provider, noSinkJob.ProviderJobID, noSinkJob.State = "fake", "remote", video.StateSubmitted
	if err := noSinkStore.Create(ctx, noSinkJob); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: noSinkStore, Providers: Registry{"fake": noSinkProvider}, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, noSinkJob); err == nil || !strings.Contains(err.Error(), "asset sink") {
		t.Fatalf("no sink error=%v", err)
	}
}

func TestFailAndManualPersistenceErrors(t *testing.T) {
	ctx := context.Background()
	base := queue.NewMemoryStore()
	j := queueJobForTest("fail")
	if err := base.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	failErr := errors.New("durability failed")
	store := &failingJobStore{MemoryStore: base, updateErr: failErr}
	o := &Orchestrator{Store: store}
	if err := o.fail(ctx, j, "provider failed"); !errors.Is(err, failErr) {
		t.Fatalf("fail persistence error=%v", err)
	}
	if err := o.markNeedsManual(ctx, j, "manual"); !errors.Is(err, failErr) {
		t.Fatalf("manual persistence error=%v", err)
	}
	if err := o.markNeedsManualAndError(ctx, j, "manual"); !errors.Is(err, failErr) {
		t.Fatalf("manual-and-error persistence error=%v", err)
	}
}
