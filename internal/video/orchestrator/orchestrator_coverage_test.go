package orchestrator

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type coverageVideoOrchExtraStore struct {
	*queue.MemoryStore
	getErr, idempotencyErr, createErr, listErr error
	updateErrAt                                int
	updates                                    int
}

func (s *coverageVideoOrchExtraStore) Get(ctx context.Context, id string) (video.VideoJob, error) {
	if s.getErr != nil {
		return video.VideoJob{}, s.getErr
	}
	return s.MemoryStore.Get(ctx, id)
}
func (s *coverageVideoOrchExtraStore) GetByIdempotency(ctx context.Context, key string) (video.VideoJob, error) {
	if s.idempotencyErr != nil {
		return video.VideoJob{}, s.idempotencyErr
	}
	return s.MemoryStore.GetByIdempotency(ctx, key)
}
func (s *coverageVideoOrchExtraStore) Create(ctx context.Context, j video.VideoJob) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.MemoryStore.Create(ctx, j)
}
func (s *coverageVideoOrchExtraStore) List(ctx context.Context) ([]video.VideoJob, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemoryStore.List(ctx)
}
func (s *coverageVideoOrchExtraStore) Update(ctx context.Context, j video.VideoJob) error {
	s.updates++
	if s.updateErrAt == s.updates {
		return errors.New("coverage update failure")
	}
	return s.MemoryStore.Update(ctx, j)
}

func coverageVideoOrchExtraStoreNew() *coverageVideoOrchExtraStore {
	return &coverageVideoOrchExtraStore{MemoryStore: queue.NewMemoryStore()}
}

func TestCoverageVideoOrchExtraCreateFailuresAndValidation(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	badStore := coverageVideoOrchExtraStoreNew()
	badStore.idempotencyErr = errors.New("lookup unavailable")
	r := requestWithDefaults()
	r.IdempotencyKey = "retry-key"
	if _, err := (&Orchestrator{Store: badStore, Queue: queue.New(1), Providers: Registry{"fake": p}}).Create(ctx, r); err == nil || !strings.Contains(err.Error(), "lookup idempotency") {
		t.Fatalf("idempotency lookup failure=%v", err)
	}

	for name, mutate := range map[string]func(*video.VideoRequest, *fakeProvider){
		"known-model-capabilities-required": func(r *video.VideoRequest, p *fakeProvider) { p.caps.Models = nil },
		"nan-maximum-duration":              func(r *video.VideoRequest, p *fakeProvider) { p.caps.MaxDurationSeconds = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			p := providerWithDefaults()
			r := requestWithDefaults()
			mutate(&r, p)
			if _, err := (&Orchestrator{Store: coverageVideoOrchExtraStoreNew(), Queue: queue.New(1), Providers: Registry{"fake": p}}).Create(ctx, r); err == nil {
				t.Fatal("invalid provider capabilities accepted")
			}
		})
	}

	createErr := errors.New("create not durable")
	store := coverageVideoOrchExtraStoreNew()
	store.createErr = createErr
	ledger := &cost.Ledger{}
	if _, err := (&Orchestrator{Store: store, Queue: queue.New(1), Providers: Registry{"fake": providerWithDefaults()}, Ledger: ledger}).Create(ctx, requestWithDefaults()); !errors.Is(err, createErr) {
		t.Fatalf("store create error=%v", err)
	}
	if ledger.Reserved() != 0 {
		t.Fatalf("reservation leaked after store failure: %v", ledger.Reserved())
	}

	budgetReq := requestWithDefaults()
	budgetReq.MaxCostUSD = 1
	if _, err := (&Orchestrator{Store: coverageVideoOrchExtraStoreNew(), Queue: queue.New(1), Providers: Registry{"fake": providerWithDefaults()}, Ledger: &cost.Ledger{}}).Create(ctx, budgetReq); err == nil {
		t.Fatal("over-budget job accepted")
	}
}

func TestCoverageVideoOrchExtraRecoverPersistenceAndUnknownStates(t *testing.T) {
	ctx := context.Background()
	store := coverageVideoOrchExtraStoreNew()
	admitted := queueJobForTest("recover-admitted-error")
	admitted.State, admitted.ProviderJobID = video.StateAdmitted, "remote-admitted"
	if err := store.MemoryStore.Create(ctx, admitted); err != nil {
		t.Fatal(err)
	}
	store.updateErrAt = 1
	if err := (&Orchestrator{Store: store, Queue: queue.New(4)}).Recover(ctx); err == nil || !strings.Contains(err.Error(), "persist recovered") {
		t.Fatalf("recovery persist error=%v", err)
	}

	unknownStore := coverageVideoOrchExtraStoreNew()
	unknown := queueJobForTest("unsupported-persisted-state")
	unknown.State = video.StateExpired
	if err := unknownStore.MemoryStore.Create(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: unknownStore, Queue: queue.New(1)}).Recover(ctx); err == nil || !strings.Contains(err.Error(), "cannot mark") {
		t.Fatalf("unknown persisted state recovery=%v", err)
	}
	got, err := unknownStore.MemoryStore.Get(ctx, unknown.JobID)
	if err != nil || got.State != video.StateExpired {
		t.Fatalf("unknown persisted state=%#v err=%v", got, err)
	}

	manualStore := coverageVideoOrchExtraStoreNew()
	planning := queueJobForTest("planning-manual-save-error")
	planning.State = video.StatePlanning
	if err := manualStore.MemoryStore.Create(ctx, planning); err != nil {
		t.Fatal(err)
	}
	manualStore.updateErrAt = 1
	if err := (&Orchestrator{Store: manualStore, Queue: queue.New(1)}).Recover(ctx); err == nil {
		t.Fatal("manual-action persistence failure suppressed")
	}
}

func TestCoverageVideoOrchExtraSubmissionErrorsAndAdmissionRecovery(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(*fakeProvider){
		"provider-submit-error": func(p *fakeProvider) { p.submitErr = errors.New("submission uncertain") },
		"blank-provider-id":     func(p *fakeProvider) { p.submit = video.ProviderJob{ProviderJobID: "  ", Accepted: true} },
	} {
		t.Run(name, func(t *testing.T) {
			store := coverageVideoOrchExtraStoreNew()
			p := providerWithDefaults()
			setup(p)
			j := queueJobForTest(name)
			j.Provider = "fake"
			if err := store.MemoryStore.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}}).RunJob(ctx, j)
			if err == nil {
				t.Fatal("uncertain or empty-ID submission succeeded")
			}
			got, _ := store.MemoryStore.Get(ctx, j.JobID)
			if got.State != video.StateNeedsManualAction {
				t.Fatalf("state=%s error=%q", got.State, got.LastError)
			}
		})
	}

	store := coverageVideoOrchExtraStoreNew()
	admitted := queueJobForTest("admitted-run")
	admitted.State, admitted.ProviderJobID = video.StateAdmitted, "remote"
	if err := store.MemoryStore.Create(ctx, admitted); err != nil {
		t.Fatal(err)
	}
	p := providerWithDefaults()
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateProcessing, Progress: .1}}
	if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, admitted); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("admitted recovery execution=%v", err)
	}
	got, _ := store.MemoryStore.Get(ctx, admitted.JobID)
	if got.State != video.StateProcessing {
		t.Fatalf("admitted job state=%s", got.State)
	}
}

func TestCoverageVideoOrchExtraPollingStatusTransitionsAndPersistence(t *testing.T) {
	ctx := context.Background()
	t.Run("transient-error-then-active", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		j := queueJobForTest("transient-status")
		j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateSubmitted
		if err := store.MemoryStore.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statusErrs = []error{errors.New("temporary outage")}
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateProcessing, Progress: .3, Usage: map[string]string{"frames": "4"}}}
		err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 2}).RunJob(ctx, j)
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("poll result=%v", err)
		}
		got, _ := store.MemoryStore.Get(ctx, j.JobID)
		if got.State != video.StateProcessing || got.RetryCount != 1 || got.UsageMetadata["frames"] != "4" {
			t.Fatalf("resumable status=%#v", got)
		}
	})

	t.Run("unsupported-transition", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		j := queueJobForTest("invalid-transition")
		j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateSubmitted
		if err := store.MemoryStore.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateUploading, Progress: .5}}
		if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, j); err == nil {
			t.Fatal("unsupported provider transition accepted")
		}
		got, _ := store.MemoryStore.Get(ctx, j.JobID)
		if got.State != video.StateNeedsManualAction {
			t.Fatalf("state=%s", got.State)
		}
	})

	t.Run("progress-persist-error", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		j := queueJobForTest("progress-save-error")
		j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateSubmitted
		if err := store.MemoryStore.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		store.updateErrAt = 1
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateProcessing, Progress: .2}}
		if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, j); err == nil {
			t.Fatal("progress persistence error suppressed")
		}
	})

	t.Run("cancelled-provider-status-releases-reservation", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		j := queueJobForTest("remote-cancelled")
		j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StatePolling
		if err := store.MemoryStore.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		ledger := &cost.Ledger{}
		est := providerWithDefaults().estimate
		if err := ledger.ReserveForJob(j.JobID, est, 0); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCancelled}}
		if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, _ := store.MemoryStore.Get(ctx, j.JobID)
		if got.State != video.StateCancelled || ledger.Reserved() != 0 {
			t.Fatalf("cancel state=%s reserved=%v", got.State, ledger.Reserved())
		}
	})
}

func TestCoverageVideoOrchExtraDownloadEstimateFallbackAndUploadResume(t *testing.T) {
	ctx := context.Background()
	store := coverageVideoOrchExtraStoreNew()
	j := queueJobForTest("estimate-settlement")
	j.Provider, j.ProviderJobID, j.State, j.EstimatedCostUSD = "fake", "remote", video.StateSubmitted, 2
	if err := store.MemoryStore.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	p := providerWithDefaults()
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCompleted, Progress: 1, Usage: map[string]string{"actual_cost_usd": "NaN"}, Outputs: []video.Asset{{Filename: "out.mp4", Metadata: map[string]string{"existing": "yes"}}}}}
	p.downloaded = []video.Asset{validStoredAsset()}
	ledger := &cost.Ledger{}
	if err := ledger.ReserveForJob(j.JobID, p.estimate, 0); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, _ := store.MemoryStore.Get(ctx, j.JobID)
	if got.State != video.StateCompleted || ledger.EstimatedSpent() != 2 || ledger.Reserved() != 0 {
		t.Fatalf("final state=%s estimated spent=%v reserved=%v", got.State, ledger.EstimatedSpent(), ledger.Reserved())
	}
	meta := p.lastDownload.Outputs[0].Metadata
	if meta["existing"] != "yes" || meta["project_id"] != j.ProjectID || meta["episode_id"] != j.EpisodeID {
		t.Fatalf("download metadata not preserved/enriched: %#v", meta)
	}

	// Restarting an upload-stage job polls again without attempting a backward transition.
	resumeStore := coverageVideoOrchExtraStoreNew()
	resume := queueJobForTest("upload-resume")
	resume.Provider, resume.ProviderJobID, resume.State = "fake", "remote-upload", video.StateUploading
	if err := resumeStore.MemoryStore.Create(ctx, resume); err != nil {
		t.Fatal(err)
	}
	resumeProvider := providerWithDefaults()
	resumeProvider.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-upload", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{Filename: "out.mp4"}}}}
	resumeProvider.downloaded = []video.Asset{validStoredAsset()}
	if err := (&Orchestrator{Store: resumeStore, Providers: Registry{"fake": resumeProvider}, Assets: testSink{}, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, resume); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageVideoOrchExtraCancelRequestedAndStoreErrors(t *testing.T) {
	ctx := context.Background()
	store := coverageVideoOrchExtraStoreNew()
	j := queueJobForTest("cancel-requested")
	j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateCancelRequested
	j.EstimatedCostUSD = 2
	if err := store.MemoryStore.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	ledger := &cost.Ledger{}
	if err := ledger.ReserveForJob(j.JobID, providerWithDefaults().estimate, 0); err != nil {
		t.Fatal(err)
	}
	p := providerWithDefaults()
	if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, Ledger: ledger}).RunJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, _ := store.MemoryStore.Get(ctx, j.JobID)
	if got.State != video.StateCancelled || p.cancelCalls != 1 || ledger.Reserved() != 0 || ledger.EstimatedSpent() != 2 {
		t.Fatalf("cancel-requested state=%s calls=%d reserved=%v settled=%v", got.State, p.cancelCalls, ledger.Reserved(), ledger.EstimatedSpent())
	}

	getStore := coverageVideoOrchExtraStoreNew()
	getStore.getErr = errors.New("read failed")
	if err := (&Orchestrator{Store: getStore, Providers: Registry{}}).RunJob(ctx, queueJobForTest("missing")); err == nil || !strings.Contains(err.Error(), "load video job") {
		t.Fatalf("job load error=%v", err)
	}
	if err := (&Orchestrator{Store: coverageVideoOrchExtraStoreNew()}).RunJob(ctx, queueJobForTest("unconfigured")); err == nil {
		t.Fatal("missing provider registry accepted")
	}
}

func TestCoverageVideoOrchExtraCancelPersistenceAndRetryGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := queueJobForTest("retry-cancelled")
	(&Orchestrator{Queue: queue.New(1)}).scheduleRetry(ctx, job, time.Nanosecond)
	(&Orchestrator{}).scheduleRetry(context.Background(), job, time.Nanosecond)

	store := coverageVideoOrchExtraStoreNew()
	j := queueJobForTest("cancel-save-error")
	if err := store.MemoryStore.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	store.updateErrAt = 1
	if err := (&Orchestrator{Store: store, Providers: Registry{"fake": providerWithDefaults()}}).Cancel(context.Background(), j.JobID); err == nil {
		t.Fatal("cancel persistence failure suppressed")
	}
	if err := (&Orchestrator{Store: coverageVideoOrchExtraStoreNew()}).Cancel(context.Background(), "missing"); err == nil {
		t.Fatal("missing job cancellation succeeded")
	}
}

func TestCoverageVideoOrchExtraAdmissionAndPersistenceFailurePaths(t *testing.T) {
	ctx := context.Background()
	t.Run("create-queue-failure-state-not-durable", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		store.updateErrAt = 1
		q := queue.New(1)
		if err := q.Enqueue(ctx, queueJobForTest("already-queued")); err != nil {
			t.Fatal(err)
		}
		ledger := &cost.Ledger{}
		_, err := (&Orchestrator{Store: store, Queue: q, Providers: Registry{"fake": providerWithDefaults()}, Ledger: ledger}).Create(ctx, requestWithDefaults())
		if err == nil || !strings.Contains(err.Error(), "failure state not durable") {
			t.Fatalf("queue/store dual failure=%v", err)
		}
		if ledger.Reserved() != 0 {
			t.Fatalf("reservation leaked after queue failure: %v", ledger.Reserved())
		}
	})
	t.Run("queued-admission-persist-error", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		j := queueJobForTest("admission-persist-error")
		if err := store.MemoryStore.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		store.updateErrAt = 1
		if err := (&Orchestrator{Store: store, Providers: Registry{"fake": providerWithDefaults()}}).RunJob(ctx, j); err == nil || !strings.Contains(err.Error(), "persist provider-admission") {
			t.Fatalf("admission persistence error=%v", err)
		}
	})
	t.Run("submission-id-not-durably-saved", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		j := queueJobForTest("submission-save-error")
		if err := store.MemoryStore.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		store.updateErrAt = 2 // admission succeeds; saving the confirmed provider ID fails.
		p := providerWithDefaults()
		if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}}).RunJob(ctx, j); err == nil || !strings.Contains(err.Error(), "do not resubmit") {
			t.Fatalf("submission ID persistence error=%v", err)
		}
	})
	noIDStore := coverageVideoOrchExtraStoreNew()
	noID := queueJobForTest("admitted-without-id-run")
	noID.State = video.StateAdmitted
	if err := noIDStore.MemoryStore.Create(ctx, noID); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{Store: noIDStore, Providers: Registry{"fake": providerWithDefaults()}}).RunJob(ctx, noID); err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("admitted job without ID error=%v", err)
	}
}

func TestCoverageVideoOrchExtraPollAndCancelProviderFailures(t *testing.T) {
	ctx := context.Background()
	for name, configure := range map[string]func(*fakeProvider, video.JobState){
		"different-id": func(p *fakeProvider, _ video.JobState) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "wrong", State: video.StateProcessing, Progress: .2}}
		},
		"invalid-infinite-progress": func(p *fakeProvider, _ video.JobState) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateProcessing, Progress: math.Inf(1)}}
		},
		"provider-failed": func(p *fakeProvider, _ video.JobState) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateFailed, Progress: .2}}
		},
		"upload-state-regression": func(p *fakeProvider, start video.JobState) {
			p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateProcessing, Progress: .2}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := coverageVideoOrchExtraStoreNew()
			state := video.StateSubmitted
			if name == "upload-state-regression" {
				state = video.StateUploading
			}
			j := queueJobForTest(name)
			j.Provider, j.ProviderJobID, j.State = "fake", "remote", state
			if err := store.MemoryStore.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			p := providerWithDefaults()
			configure(p, state)
			err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, j)
			if err == nil {
				t.Fatal("invalid remote status was accepted")
			}
			got, _ := store.MemoryStore.Get(ctx, j.JobID)
			if name == "provider-failed" {
				if got.State != video.StateFailed {
					t.Fatalf("provider failure state=%s", got.State)
				}
			} else if got.State != video.StateNeedsManualAction {
				t.Fatalf("manual status fence state=%s", got.State)
			}
		})
	}
	t.Run("provider-error-state-not-durable", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		j := queueJobForTest("status-error-save")
		j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateSubmitted
		if err := store.MemoryStore.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		store.updateErrAt = 1
		p := providerWithDefaults()
		p.statusErrs = []error{errors.New("status outage")}
		if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 1}).RunJob(ctx, j); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("status state persistence error=%v", err)
		}
	})
	t.Run("cancel-provider-error-and-save-error", func(t *testing.T) {
		for _, persistFailure := range []bool{false, true} {
			store := coverageVideoOrchExtraStoreNew()
			j := queueJobForTest("cancel-provider-error")
			j.Provider, j.ProviderJobID, j.State = "fake", "remote", video.StateProcessing
			if err := store.MemoryStore.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			p := providerWithDefaults()
			if persistFailure {
				store.updateErrAt = 1
			} else {
				p.cancelErr = errors.New("provider refused cancellation")
			}
			err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}}).Cancel(ctx, j.JobID)
			if err == nil {
				t.Fatal("provider or persistence cancellation error suppressed")
			}
		}
	})
}
