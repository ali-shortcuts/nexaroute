package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// mission95Store is a MemoryStore wrapper with injectable failures at
// specific call counts, so error paths that depend on "the Nth store
// call fails" can be exercised deterministically.
type mission95Store struct {
	*queue.MemoryStore
	getCalls, tryClaimCalls, updateCalls int
	getErrAt, tryClaimErrAt, updateErrAt int
	getSwapAt                            int
	getSwap                              video.VideoJob
	createErr, idempotencyErr, listErr   error
	deleteErr                            error
	idempotencyCalls, idempotencyErrAt   int
	idempotencySwapAt                    int
	idempotencySwap                      video.VideoJob
}

func (s *mission95Store) Get(ctx context.Context, id string) (video.VideoJob, error) {
	s.getCalls++
	if s.getErrAt != 0 && s.getCalls == s.getErrAt {
		return video.VideoJob{}, errors.New("mission95 injected get failure")
	}
	if s.getSwapAt != 0 && s.getCalls == s.getSwapAt {
		return s.getSwap, nil
	}
	return s.MemoryStore.Get(ctx, id)
}

func (s *mission95Store) GetByIdempotency(ctx context.Context, key string) (video.VideoJob, error) {
	s.idempotencyCalls++
	if s.idempotencySwapAt != 0 && s.idempotencyCalls == s.idempotencySwapAt {
		return s.idempotencySwap, nil
	}
	if s.idempotencyErr != nil && s.idempotencyErrAt != 0 && s.idempotencyCalls == s.idempotencyErrAt {
		return video.VideoJob{}, s.idempotencyErr
	}
	return s.MemoryStore.GetByIdempotency(ctx, key)
}

func (s *mission95Store) Create(ctx context.Context, j video.VideoJob) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.MemoryStore.Create(ctx, j)
}

func (s *mission95Store) Update(ctx context.Context, j video.VideoJob) error {
	s.updateCalls++
	if s.updateErrAt != 0 && s.updateCalls == s.updateErrAt {
		return errors.New("mission95 injected update failure")
	}
	return s.MemoryStore.Update(ctx, j)
}

func (s *mission95Store) List(ctx context.Context) ([]video.VideoJob, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemoryStore.List(ctx)
}

func (s *mission95Store) Delete(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.MemoryStore.Delete(ctx, id)
}

func (s *mission95Store) TryClaim(ctx context.Context, id, owner string, ttl time.Duration, now time.Time) (video.VideoJob, bool, error) {
	s.tryClaimCalls++
	if s.tryClaimErrAt != 0 && s.tryClaimCalls == s.tryClaimErrAt {
		return video.VideoJob{}, false, errors.New("mission95 injected claim failure")
	}
	return s.MemoryStore.TryClaim(ctx, id, owner, ttl, now)
}

// mission95Ledger is a CostLedger with injectable settlement failures.
type mission95Ledger struct {
	reserveErr, settleActualErr, settleEstimateErr error
	settleActualCalls, settleEstimateCalls         int
	released                                       []string
}

func (l *mission95Ledger) ReserveForJob(string, video.CostEstimate, float64) error {
	return l.reserveErr
}
func (l *mission95Ledger) ReleaseForJob(id string) { l.released = append(l.released, id) }
func (l *mission95Ledger) SettleActual(string, float64, float64) error {
	l.settleActualCalls++
	return l.settleActualErr
}
func (l *mission95Ledger) SettleEstimate(string, float64) error {
	l.settleEstimateCalls++
	return l.settleEstimateErr
}
func (l *mission95Ledger) Entries() []video.CostEstimate { return nil }
func (l *mission95Ledger) Reserved() float64             { return 0 }
func (l *mission95Ledger) Spent() float64                { return 0 }

// mission95Sink is an AssetSink that records puts and, unless told
// otherwise, persists the bytes to a real temp file so downstream Stat
// and checksum checks pass.
type mission95Sink struct {
	putErr   error
	putAsset *video.Asset
	puts     []video.Asset
}

func (s *mission95Sink) Put(_ context.Context, a video.Asset, data []byte) (video.Asset, error) {
	s.puts = append(s.puts, a)
	if s.putErr != nil {
		return video.Asset{}, s.putErr
	}
	if s.putAsset != nil {
		return *s.putAsset, nil
	}
	if a.URI == "" {
		f, err := os.CreateTemp("", "mission95-sink-*.bin")
		if err != nil {
			return video.Asset{}, err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return video.Asset{}, err
		}
		if err := f.Close(); err != nil {
			return video.Asset{}, err
		}
		a.URI = f.Name()
	}
	sum := sha256.Sum256(data)
	a.SHA256 = hex.EncodeToString(sum[:])
	a.SizeBytes = int64(len(data))
	return a, nil
}

func mission95SingleOrch(store queue.JobStore, p video.VideoProvider) *Orchestrator {
	return &Orchestrator{
		Store: store, Queue: queue.New(10),
		Providers: Registry{"fake": p}, Assets: &mission95Sink{},
		PollInterval: time.Nanosecond, MaxPolls: 5,
	}
}

func mission95Stored(t *testing.T, store queue.JobStore, job video.VideoJob) video.VideoJob {
	t.Helper()
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func mission95Get(t *testing.T, store queue.JobStore, id string) video.VideoJob {
	t.Helper()
	job, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestMission95CreateValidationAndConflictBranches(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (&Orchestrator{}).Create(cancelled, requestWithDefaults()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create error=%v", err)
	}
	if _, err := (&Orchestrator{}).Create(ctx, requestWithDefaults()); err == nil || !strings.Contains(err.Error(), "not fully configured") {
		t.Fatalf("unconfigured create error=%v", err)
	}
	// Single-shot prompt inheritance: an empty shot prompt takes the
	// request prompt, and the shot plan collapses to a plain request.
	single := requestWithDefaults()
	single.ShotPlan = []video.Shot{{ShotID: "s1", DurationSeconds: 2}}
	o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
	job, err := o.Create(ctx, single)
	if err != nil {
		t.Fatalf("single-shot create: %v", err)
	}
	if job.Request.Prompt != "a scene" || len(job.Request.ShotPlan) != 0 {
		t.Fatalf("single-shot job prompt=%q shots=%d", job.Request.Prompt, len(job.Request.ShotPlan))
	}
	mismatch := requestWithDefaults()
	mismatch.DurationSeconds = 5
	mismatch.ShotPlan = []video.Shot{{ShotID: "s1", DurationSeconds: 2}}
	if _, err := o.Create(ctx, mismatch); err == nil || !strings.Contains(err.Error(), "does not match shot duration") {
		t.Fatalf("duration mismatch error=%v", err)
	}
	// Multi-shot shot with an invalid duration fails before validation.
	multi := episodeRequest()
	multi.ShotPlan[0].DurationSeconds = 0
	if _, err := o.Create(ctx, multi); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("multi-shot bad duration error=%v", err)
	}
	// Multi-shot model conflict inside shot-plan validation.
	conflict := episodeRequest()
	conflict.ShotPlan[1].Model = "other"
	episodeOrch := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4),
		Providers: Registry{"fake": providerWithDefaults()},
		Assets:    &mission95Sink{}, Composer: &episodeTestComposer{}}
	if _, err := episodeOrch.Create(ctx, conflict); err == nil || !strings.Contains(err.Error(), "selects model") {
		t.Fatalf("shot model conflict error=%v", err)
	}
	// Dry-run idempotency conflict resolves to the existing job.
	store := &mission95Store{MemoryStore: queue.NewMemoryStore()}
	existing := queueJobForTest("dry-existing")
	existing.IdempotencyKey = "k1"
	if err := store.MemoryStore.Create(ctx, existing); err != nil {
		t.Fatal(err)
	}
	store.createErr = queue.ErrIdempotencyConflict
	dry := requestWithDefaults()
	dry.DryRun, dry.IdempotencyKey = true, "k1"
	got, err := (&Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}).Create(ctx, dry)
	if err != nil || got.JobID != existing.JobID {
		t.Fatalf("dry-run conflict got=%s err=%v", got.JobID, err)
	}
	// The same conflict with a failing pre-check lookup surfaces the
	// lookup error (the dry-run path wraps it as "lookup idempotency key").
	store.idempotencyErr = errors.New("lookup down")
	store.idempotencyErrAt = store.idempotencyCalls + 1
	if _, err := (&Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}).Create(ctx, dry); err == nil || !strings.Contains(err.Error(), "lookup idempotency key") {
		t.Fatalf("dry-run resolve error=%v", err)
	}
	// Non-dry-run conflict whose post-conflict lookup fails: the
	// resolution path wraps it as "resolve concurrent idempotent create".
	// The pre-check lookup (call 1) must miss so Create runs and conflicts.
	resolveStore := &mission95Store{MemoryStore: queue.NewMemoryStore(),
		createErr:      queue.ErrIdempotencyConflict,
		idempotencyErr: errors.New("lookup down"), idempotencyErrAt: 2}
	plain := requestWithDefaults()
	plain.IdempotencyKey = "k1"
	if _, err := (&Orchestrator{Store: resolveStore, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}).Create(ctx, plain); err == nil || !strings.Contains(err.Error(), "resolve concurrent idempotent create") {
		t.Fatalf("plain resolve error=%v", err)
	}
	// Queue admission failure whose failure state cannot be persisted.
	full := queue.New(1)
	if err := full.Enqueue(ctx, queueJobForTest("occupied")); err != nil {
		t.Fatal(err)
	}
	failStore := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
	if _, err := (&Orchestrator{Store: failStore, Queue: full, Providers: Registry{"fake": providerWithDefaults()}}).Create(ctx, requestWithDefaults()); err == nil || !strings.Contains(err.Error(), "failure state not durable") {
		t.Fatalf("admission failure error=%v", err)
	}
}

func TestMission95ClaimLeaseDefaults(t *testing.T) {
	ctx := context.Background()
	store := queue.NewMemoryStore()
	job := queueJobForTest("leased")
	mission95Stored(t, store, job)
	o := &Orchestrator{Store: store, InstanceID: "test-owner"}
	claimed, err := o.claim(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.LeaseOwner != "test-owner" {
		t.Fatalf("lease owner=%q", claimed.LeaseOwner)
	}
	if until := time.Until(claimed.LeaseUntil); until < 4*time.Minute || until > 6*time.Minute {
		t.Fatalf("default lease TTL=%v, want ~5m", until)
	}
	o2 := &Orchestrator{Store: store, InstanceID: "test-owner", LeaseTTL: time.Minute}
	claimed2, err := o2.claim(ctx, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(claimed2.LeaseUntil); until < 50*time.Second || until > 70*time.Second {
		t.Fatalf("explicit lease TTL=%v, want ~1m", until)
	}
}

func TestMission95RecoverErrorBranches(t *testing.T) {
	ctx := context.Background()
	// Ambiguous episode checkpoint whose manual marking cannot persist.
	ambStore := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
	amb := coverageGapEpisodeJob("ep-amb", video.StateProcessing)
	amb.ShotExecutions[0].State = video.StateSubmitted
	mission95Stored(t, ambStore, amb)
	if err := (&Orchestrator{Store: ambStore, Queue: queue.New(4)}).Recover(ctx); err == nil {
		t.Fatal("ambiguous episode recovery with failing update succeeded")
	}
	// Recovered episode job that cannot be enqueued (closed queue).
	closed := queue.New(4)
	closed.Close()
	epStore := queue.NewMemoryStore()
	mission95Stored(t, epStore, coverageGapEpisodeJob("ep-closed", video.StateQueued))
	if err := (&Orchestrator{Store: epStore, Queue: closed}).Recover(ctx); err == nil || !strings.Contains(err.Error(), "enqueue recovered episode job") {
		t.Fatalf("closed-queue episode recovery error=%v", err)
	}
	// Non-resumable episode state whose manual marking cannot persist.
	nrStore := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
	mission95Stored(t, nrStore, coverageGapEpisodeJob("ep-planning", video.StatePlanning))
	if err := (&Orchestrator{Store: nrStore, Queue: queue.New(4)}).Recover(ctx); err == nil {
		t.Fatal("non-resumable episode recovery with failing update succeeded")
	}
	// Single-shot admitted job without a provider id, unpersistable.
	adStore := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
	admitted := queueJobForTest("admitted-unknown")
	admitted.State = video.StateAdmitted
	mission95Stored(t, adStore, admitted)
	if err := (&Orchestrator{Store: adStore, Queue: queue.New(4)}).Recover(ctx); err == nil {
		t.Fatal("admitted-unknown recovery with failing update succeeded")
	}
	// Single-shot admitted job with a provider id, closed queue.
	admitted2 := queueJobForTest("admitted-closed")
	admitted2.State, admitted2.ProviderJobID = video.StateAdmitted, "remote-1"
	closedStore := queue.NewMemoryStore()
	mission95Stored(t, closedStore, admitted2)
	if err := (&Orchestrator{Store: closedStore, Queue: closed}).Recover(ctx); err == nil || !strings.Contains(err.Error(), "enqueue recovered video job") {
		t.Fatalf("admitted closed-queue recovery error=%v", err)
	}
	// Active job without a provider id, unpersistable.
	actStore := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
	active := queueJobForTest("active-unknown")
	active.State = video.StateProcessing
	mission95Stored(t, actStore, active)
	if err := (&Orchestrator{Store: actStore, Queue: queue.New(4)}).Recover(ctx); err == nil {
		t.Fatal("active-unknown recovery with failing update succeeded")
	}
	// Active job with a provider id, closed queue.
	actClosed := queue.NewMemoryStore()
	active2 := queueJobForTest("active-closed")
	active2.State, active2.ProviderJobID = video.StateProcessing, "remote-1"
	mission95Stored(t, actClosed, active2)
	if err := (&Orchestrator{Store: actClosed, Queue: closed}).Recover(ctx); err == nil || !strings.Contains(err.Error(), "enqueue recovered video job") {
		t.Fatalf("active closed-queue recovery error=%v", err)
	}
	// Backpressure: a full queue plus a context that expires mid-wait.
	full := queue.New(1)
	if err := full.Enqueue(ctx, queueJobForTest("occupied")); err != nil {
		t.Fatal(err)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	if err := (&Orchestrator{Queue: full}).enqueueWithBackpressure(timeoutCtx, queueJobForTest("waiter")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backpressure timeout error=%v", err)
	}
}

func TestMission95RunJobSingleShotBranches(t *testing.T) {
	ctx := context.Background()
	newJob := func(id string, state video.JobState, providerJobID string) video.VideoJob {
		j := queueJobForTest(id)
		j.State, j.ProviderJobID = state, providerJobID
		return j
	}
	t.Run("store get error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getErrAt: 1}
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, queueJobForTest("x")); err == nil || !strings.Contains(err.Error(), "load video job before execution") {
			t.Fatalf("runjob get error=%v", err)
		}
	})
	t.Run("claim error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), tryClaimErrAt: 1}
		mission95Stored(t, store, newJob("claim-err", video.StateQueued, ""))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("claim-err", video.StateQueued, "")); err == nil || !strings.Contains(err.Error(), "claim video job") {
			t.Fatalf("runjob claim error=%v", err)
		}
	})
	t.Run("needs manual", func(t *testing.T) {
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("manual", video.StateNeedsManualAction, ""))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("manual", video.StateNeedsManualAction, "")); err == nil || !strings.Contains(err.Error(), "requires manual action") {
			t.Fatalf("runjob manual error=%v", err)
		}
	})
	t.Run("admission persist error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		mission95Stored(t, store, newJob("adm-err", video.StateQueued, ""))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("adm-err", video.StateQueued, "")); err == nil || !strings.Contains(err.Error(), "persist provider-admission state") {
			t.Fatalf("runjob admission error=%v", err)
		}
	})
	t.Run("provider gone after admission", func(t *testing.T) {
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("gone", video.StateQueued, ""))
		o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{}, PollInterval: time.Nanosecond}
		if err := o.RunJob(ctx, newJob("gone", video.StateQueued, "")); err == nil || err.Error() != "provider unavailable" {
			t.Fatalf("runjob provider-gone error=%v", err)
		}
		if got := mission95Get(t, store, "gone"); got.State != video.StateFailed {
			t.Fatalf("provider-gone state=%s", got.State)
		}
	})
	t.Run("submit error", func(t *testing.T) {
		p := providerWithDefaults()
		p.submitErr = errors.New("uncertain")
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("sub-err", video.StateQueued, ""))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newJob("sub-err", video.StateQueued, "")); err == nil || !strings.Contains(err.Error(), "submission outcome is unknown") {
			t.Fatalf("runjob submit error=%v", err)
		}
		if got := mission95Get(t, store, "sub-err"); got.State != video.StateNeedsManualAction {
			t.Fatalf("submit-error state=%s", got.State)
		}
	})
	t.Run("submit empty id", func(t *testing.T) {
		p := providerWithDefaults()
		p.submit = video.ProviderJob{ProviderJobID: " ", Provider: "fake", Model: "model-a", Accepted: true}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("sub-empty", video.StateQueued, ""))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newJob("sub-empty", video.StateQueued, "")); err == nil || !strings.Contains(err.Error(), "did not confirm a job ID") {
			t.Fatalf("runjob empty-id error=%v", err)
		}
	})
	t.Run("submit persist error cancels provider", func(t *testing.T) {
		p := providerWithDefaults()
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		mission95Stored(t, store, newJob("sub-persist", video.StateQueued, ""))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newJob("sub-persist", video.StateQueued, "")); err == nil || !strings.Contains(err.Error(), "not durably saved") {
			t.Fatalf("runjob submit-persist error=%v", err)
		}
		if p.cancelCalls != 1 {
			t.Fatalf("best-effort cancel calls=%d, want 1", p.cancelCalls)
		}
	})
	t.Run("submit inconsistent", func(t *testing.T) {
		p := providerWithDefaults()
		p.submit = video.ProviderJob{ProviderJobID: "remote-9", Provider: "fake", Model: "model-a", Accepted: false}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("sub-inc", video.StateQueued, ""))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newJob("sub-inc", video.StateQueued, "")); err == nil || !strings.Contains(err.Error(), "inconsistent") {
			t.Fatalf("runjob inconsistent error=%v", err)
		}
		if got := mission95Get(t, store, "sub-inc"); got.ProviderJobID != "remote-9" {
			t.Fatalf("inconsistent job id=%q, want retained remote-9", got.ProviderJobID)
		}
	})
	t.Run("admitted no id", func(t *testing.T) {
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("adm-noid", video.StateAdmitted, ""))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("adm-noid", video.StateAdmitted, "")); err == nil || !strings.Contains(err.Error(), "submission outcome is unknown") {
			t.Fatalf("runjob admitted-noid error=%v", err)
		}
	})
	t.Run("admitted persist error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		mission95Stored(t, store, newJob("adm-perr", video.StateAdmitted, "remote-1"))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("adm-perr", video.StateAdmitted, "remote-1")); err == nil {
			t.Fatal("admitted persist error accepted")
		}
	})
	t.Run("active no id", func(t *testing.T) {
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("act-noid", video.StateProcessing, ""))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("act-noid", video.StateProcessing, "")); err == nil || !strings.Contains(err.Error(), "no provider job ID") {
			t.Fatalf("runjob active-noid error=%v", err)
		}
	})
	t.Run("provider gone after switch", func(t *testing.T) {
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("gone2", video.StateSubmitted, "remote-1"))
		o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{}, PollInterval: time.Nanosecond}
		if err := o.RunJob(ctx, newJob("gone2", video.StateSubmitted, "remote-1")); err == nil || err.Error() != "provider unavailable" {
			t.Fatalf("runjob provider-gone2 error=%v", err)
		}
	})
	t.Run("cancel requested provider error", func(t *testing.T) {
		p := providerWithDefaults()
		p.cancelErr = errors.New("remote down")
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("cx-err", video.StateCancelRequested, "remote-1"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newJob("cx-err", video.StateCancelRequested, "remote-1")); !errors.Is(err, p.cancelErr) {
			t.Fatalf("runjob cancel error=%v", err)
		}
	})
	t.Run("terminal is noop", func(t *testing.T) {
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newJob("done", video.StateCompleted, "remote-1"))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("done", video.StateCompleted, "remote-1")); err != nil {
			t.Fatalf("terminal runjob error=%v", err)
		}
	})
	t.Run("lease lost while polling", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), tryClaimErrAt: 2}
		mission95Stored(t, store, newJob("lease-lost", video.StateSubmitted, "remote-1"))
		if err := mission95SingleOrch(store, providerWithDefaults()).RunJob(ctx, newJob("lease-lost", video.StateSubmitted, "remote-1")); err == nil || !strings.Contains(err.Error(), "lease lost while polling") {
			t.Fatalf("runjob lease-lost error=%v", err)
		}
	})
	t.Run("poll cancelled context", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := newJob("poll-cx", video.StateSubmitted, "remote-1")
		mission95Stored(t, store, job)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		o := mission95SingleOrch(store, providerWithDefaults())
		if err := o.pollAndFinalize(cancelled, providerWithDefaults(), job); !errors.Is(err, context.Canceled) {
			t.Fatalf("poll cancelled error=%v", err)
		}
	})
}

func TestMission95PollAndDownloadBranches(t *testing.T) {
	ctx := context.Background()
	newSubmitted := func(id string) video.VideoJob {
		j := queueJobForTest(id)
		j.State, j.ProviderJobID = video.StateSubmitted, "remote-1"
		return j
	}
	status := func(state video.JobState, progress float64, id string, outputs []video.Asset) video.ProviderJobStatus {
		return video.ProviderJobStatus{ProviderJobID: id, State: state, Progress: progress, Outputs: outputs}
	}
	t.Run("status error plus save error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statusErrs = []error{errors.New("outage")}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		mission95Stored(t, store, newSubmitted("st-err"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("st-err")); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("status save error=%v", err)
		}
	})
	t.Run("different job id", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateProcessing, 0.1, "other", nil)}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("diff-id"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("diff-id")); err == nil || !strings.Contains(err.Error(), "different job ID") {
			t.Fatalf("different id error=%v", err)
		}
	})
	t.Run("invalid progress", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateProcessing, 2, "remote-1", nil)}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("bad-prog"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("bad-prog")); err == nil || !strings.Contains(err.Error(), "invalid progress") {
			t.Fatalf("invalid progress error=%v", err)
		}
	})
	t.Run("provider failed", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateFailed, 0.5, "remote-1", nil)}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("prov-fail"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("prov-fail")); err == nil || !strings.Contains(err.Error(), "provider reported job failure") {
			t.Fatalf("provider failed error=%v", err)
		}
		if got := mission95Get(t, store, "prov-fail"); got.State != video.StateFailed {
			t.Fatalf("provider-failed state=%s", got.State)
		}
	})
	t.Run("cancelled persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCancelled, 0, "remote-1", nil)}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		mission95Stored(t, store, newSubmitted("cx-perr"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("cx-perr")); err == nil {
			t.Fatal("cancelled persist error accepted")
		}
	})
	t.Run("status regressed after upload", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateProcessing, 0.5, "remote-1", nil)}
		store := queue.NewMemoryStore()
		job := queueJobForTest("regressed")
		job.State, job.ProviderJobID = video.StateUploading, "remote-1"
		mission95Stored(t, store, job)
		if err := mission95SingleOrch(store, p).RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "regressed after output upload began") {
			t.Fatalf("regression error=%v", err)
		}
	})
	t.Run("stale submitted does not regress processing", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateSubmitted, 0.2, "remote-1", nil)}
		store := queue.NewMemoryStore()
		job := queueJobForTest("stale")
		job.State, job.ProviderJobID = video.StateProcessing, "remote-1"
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, p)
		o.MaxPolls = 1
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "polling deadline") {
			t.Fatalf("stale deadline error=%v", err)
		}
		if got := mission95Get(t, store, "stale"); got.State != video.StateProcessing {
			t.Fatalf("stale state=%s, want processing", got.State)
		}
	})
	t.Run("deadline persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateProcessing, 0.2, "remote-1", nil)}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		mission95Stored(t, store, newSubmitted("dl-perr"))
		o := mission95SingleOrch(store, p)
		o.MaxPolls = 1
		if err := o.RunJob(ctx, newSubmitted("dl-perr")); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("deadline persist error=%v", err)
		}
	})
	t.Run("download no outputs", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", nil)}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-noout"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-noout")); err == nil || !strings.Contains(err.Error(), "no output assets") {
			t.Fatalf("download no-outputs error=%v", err)
		}
	})
	t.Run("download no sink", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-nosink"))
		o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 5}
		if err := o.RunJob(ctx, newSubmitted("dl-nosink")); err == nil || !strings.Contains(err.Error(), "no output asset sink") {
			t.Fatalf("download no-sink error=%v", err)
		}
	})
	t.Run("download upload persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		mission95Stored(t, store, newSubmitted("dl-uperr"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-uperr")); err == nil || !strings.Contains(err.Error(), "persist output-upload state") {
			t.Fatalf("download upload error=%v", err)
		}
	})
	t.Run("download error plus save error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		p.downloadErr = errors.New("sink down")
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		mission95Stored(t, store, newSubmitted("dl-err"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-err")); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("download save error=%v", err)
		}
	})
	t.Run("download no persisted assets", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		p.downloaded = nil
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-nopersist"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-nopersist")); err == nil || !strings.Contains(err.Error(), "no persisted output assets") {
			t.Fatalf("download no-persisted error=%v", err)
		}
	})
	t.Run("download asset invalid", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		bad := validStoredAsset()
		bad.SizeBytes = 0
		p.downloaded = []video.Asset{bad}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-bad"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-bad")); err == nil || !strings.Contains(err.Error(), "not persisted with URI, content type, and non-empty size") {
			t.Fatalf("download invalid error=%v", err)
		}
	})
	t.Run("download sha short", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		bad := validStoredAsset()
		bad.SHA256 = "abc"
		p.downloaded = []video.Asset{bad}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-sha"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-sha")); err == nil || !strings.Contains(err.Error(), "SHA-256 checksum") {
			t.Fatalf("download sha error=%v", err)
		}
	})
	t.Run("download sha malformed", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		bad := validStoredAsset()
		bad.SHA256 = strings.Repeat("z", 64)
		p.downloaded = []video.Asset{bad}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-mal"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-mal")); err == nil || !strings.Contains(err.Error(), "checksum is malformed") {
			t.Fatalf("download malformed error=%v", err)
		}
	})
	t.Run("download reload get error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		p.downloaded = []video.Asset{validStoredAsset()}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getErrAt: 2}
		mission95Stored(t, store, newSubmitted("dl-geterr"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-geterr")); err == nil || !strings.Contains(err.Error(), "reload video job before completion commit") {
			t.Fatalf("download reload error=%v", err)
		}
	})
	t.Run("download reload terminal is noop", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		p.downloaded = []video.Asset{validStoredAsset()}
		job := newSubmitted("dl-term")
		terminal := job
		terminal.State = video.StateCompleted
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getSwapAt: 2, getSwap: terminal}
		mission95Stored(t, store, job)
		if err := mission95SingleOrch(store, p).RunJob(ctx, job); err != nil {
			t.Fatalf("download reload terminal error=%v", err)
		}
	})
	t.Run("download settle error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		p.downloaded = []video.Asset{validStoredAsset()}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-settle"))
		o := mission95SingleOrch(store, p)
		o.Ledger = &mission95Ledger{settleEstimateErr: errors.New("ledger down")}
		if err := o.RunJob(ctx, newSubmitted("dl-settle")); err == nil || !strings.Contains(err.Error(), "cost settlement failed") {
			t.Fatalf("download settle error=%v", err)
		}
	})
	t.Run("download completion transition error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		p.downloaded = []video.Asset{validStoredAsset()}
		job := newSubmitted("dl-trans")
		queued := job
		queued.State = video.StateQueued
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getSwapAt: 2, getSwap: queued}
		mission95Stored(t, store, job)
		if err := mission95SingleOrch(store, p).RunJob(ctx, job); err == nil {
			t.Fatal("download transition error accepted")
		}
	})
	t.Run("download final persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{status(video.StateCompleted, 1, "remote-1", []video.Asset{validStoredAsset()})}
		p.downloaded = []video.Asset{validStoredAsset()}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		mission95Stored(t, store, newSubmitted("dl-finerr"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-finerr")); err == nil || !strings.Contains(err.Error(), "completion state could not be saved") {
			t.Fatalf("download final error=%v", err)
		}
	})
	t.Run("download success reconciles actual", func(t *testing.T) {
		actual := 1.5
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{
			ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1,
			Outputs: []video.Asset{validStoredAsset()}, ActualCostUSD: &actual,
		}}
		p.downloaded = []video.Asset{validStoredAsset()}
		store := queue.NewMemoryStore()
		mission95Stored(t, store, newSubmitted("dl-ok"))
		if err := mission95SingleOrch(store, p).RunJob(ctx, newSubmitted("dl-ok")); err != nil {
			t.Fatalf("download success error=%v", err)
		}
		got := mission95Get(t, store, "dl-ok")
		if got.State != video.StateCompleted || !got.CostReconciled || got.ActualCostUSD != 1.5 || got.CostBasis != "actual" {
			t.Fatalf("reconciled job=%+v", got)
		}
	})
}

func TestMission95ForceFailAndRetryAfterLease(t *testing.T) {
	ctx := context.Background()
	store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getErrAt: 1}
	if _, _, err := (&Orchestrator{Store: store}).ForceFail(ctx, "nope"); err == nil {
		t.Fatal("force-fail get error accepted")
	}
	store2 := queue.NewMemoryStore()
	terminal := queueJobForTest("term")
	terminal.State = video.StateCompleted
	mission95Stored(t, store2, terminal)
	attempted, confirmed, err := (&Orchestrator{Store: store2}).ForceFail(ctx, "term")
	if attempted || confirmed || !errors.Is(err, ErrForceFailNotActive) {
		t.Fatalf("force-fail terminal attempted=%v confirmed=%v err=%v", attempted, confirmed, err)
	}
	// A live foreign lease requeues the job once the lease expires.
	shared := queue.NewMemoryStore()
	job := queueJobForTest("leased-retry")
	mission95Stored(t, shared, job)
	other := &Orchestrator{Store: shared, InstanceID: "other", LeaseTTL: 50 * time.Millisecond}
	if _, err := other.claim(ctx, job.JobID); err != nil {
		t.Fatal(err)
	}
	q := queue.New(4)
	mine := &Orchestrator{Store: shared, Queue: q, InstanceID: "mine", PollInterval: time.Nanosecond}
	if err := mine.RunJob(ctx, job); err != nil {
		t.Fatalf("leased retry runjob error=%v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for q.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if q.Len() != 1 {
		t.Fatalf("requeued jobs=%d, want 1 after lease expiry", q.Len())
	}
}

func TestMission95PruneTerminalJobsBranches(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	if n, err := (&Orchestrator{}).PruneTerminalJobs(ctx, now, 0); n != 0 || err != nil {
		t.Fatalf("disabled prune=%d err=%v", n, err)
	}
	if n, err := (&Orchestrator{Store: nil}).PruneTerminalJobs(ctx, now, time.Hour); n != 0 || err != nil {
		t.Fatalf("nil-store prune=%d err=%v", n, err)
	}
	failing := &failingJobStore{MemoryStore: queue.NewMemoryStore(), listErr: errors.New("list down")}
	if n, err := (&Orchestrator{Store: failing}).PruneTerminalJobs(ctx, now, time.Hour); n != 0 || err == nil {
		t.Fatalf("list-error prune=%d err=%v", n, err)
	}
	delStore := &mission95Store{MemoryStore: queue.NewMemoryStore(), deleteErr: errors.New("delete down")}
	old := queueJobForTest("old")
	old.State, old.CompletedAt = video.StateCompleted, func() *time.Time { t := now.Add(-48 * time.Hour); return &t }()
	mission95Stored(t, delStore, old)
	if n, err := (&Orchestrator{Store: delStore}).PruneTerminalJobs(ctx, now, time.Hour); n != 0 || err == nil {
		t.Fatalf("delete-error prune=%d err=%v", n, err)
	}
	// Happy path: only the old terminal job is pruned.
	happy := queue.NewMemoryStore()
	recent := queueJobForTest("recent")
	recent.State, recent.CompletedAt = video.StateCompleted, func() *time.Time { t := now.Add(-time.Minute); return &t }()
	active := queueJobForTest("active")
	active.State = video.StateProcessing
	mission95Stored(t, happy, old)
	mission95Stored(t, happy, recent)
	mission95Stored(t, happy, active)
	if n, err := (&Orchestrator{Store: happy}).PruneTerminalJobs(ctx, now, time.Hour); n != 1 || err != nil {
		t.Fatalf("happy prune=%d err=%v", n, err)
	}
	if _, err := happy.Get(ctx, "old"); err == nil {
		t.Fatal("old job survived pruning")
	}
	if _, err := happy.Get(ctx, "recent"); err != nil {
		t.Fatalf("recent job pruned: %v", err)
	}
}

func TestMission95BestEffortCancelProvider(t *testing.T) {
	// Nil provider and empty id are silent no-ops.
	bestEffortCancelProvider(context.Background(), nil, "remote-1")
	bestEffortCancelProvider(context.Background(), providerWithDefaults(), "")
	// A cancelled context still attempts the cancel (WithoutCancel).
	p := providerWithDefaults()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	bestEffortCancelProvider(cancelled, p, "remote-1")
	if p.cancelCalls != 1 {
		t.Fatalf("cancel calls=%d, want 1", p.cancelCalls)
	}
}
