package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// mission95HandlingProvider wraps fakeProvider and claims to handle narration
// itself, exercising the providerHandlesNarration exemption in Create.
type mission95HandlingProvider struct{ *fakeProvider }

func (p *mission95HandlingProvider) HandlesNarration() bool { return true }

// mission95CancellingComposer cancels ctx, then fails: the compose path must
// surface the cancellation, not the composer error.
type mission95CancellingComposer struct {
	cancel context.CancelFunc
	*mission95Composer
}

func (c *mission95CancellingComposer) ComposeEpisode(ctx context.Context, spec composer.EpisodeSpec) (composer.Manifest, error) {
	c.cancel()
	return composer.Manifest{}, errors.New("ffmpeg down")
}

// mission95CancellingSink cancels ctx, then fails its Put.
type mission95CancellingSink struct{ cancel context.CancelFunc }

func (s *mission95CancellingSink) Put(context.Context, video.Asset, []byte) (video.Asset, error) {
	s.cancel()
	return video.Asset{}, errors.New("sink down")
}

// mission95CancellingProvider cancels ctx inside DownloadOutputs, then
// delegates: the checkpoint Put that follows must surface context.Canceled.
type mission95CancellingProvider struct {
	*fakeProvider
	cancel context.CancelFunc
}

func (p *mission95CancellingProvider) DownloadOutputs(ctx context.Context, st video.ProviderJobStatus, sink video.AssetSink) ([]video.Asset, error) {
	p.cancel()
	return p.fakeProvider.DownloadOutputs(ctx, st, sink)
}

func TestMission95CreateRemainingBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("validate error", func(t *testing.T) {
		req := requestWithDefaults()
		req.ProjectID = ""
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		if _, err := o.Create(ctx, req); err == nil || !strings.Contains(err.Error(), "project_id is required") {
			t.Fatalf("validate error=%v", err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		o := &Orchestrator{Providers: Registry{"fake": providerWithDefaults()}}
		if _, err := o.Create(ctx, requestWithDefaults()); err == nil || !strings.Contains(err.Error(), "not fully configured") {
			t.Fatalf("not-configured error=%v", err)
		}
	})
	t.Run("no models advertised", func(t *testing.T) {
		p := providerWithDefaults()
		p.caps.Models = nil
		req := requestWithDefaults()
		req.ModelPreference = "model-x"
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": p}}
		if _, err := o.Create(ctx, req); err == nil || !strings.Contains(err.Error(), "did not advertise its model capabilities") {
			t.Fatalf("no-models error=%v", err)
		}
	})
	t.Run("handling provider inherits narration segment", func(t *testing.T) {
		req := requestWithDefaults()
		req.ShotPlan = []video.Shot{{ShotID: "s1", Prompt: "a scene", DurationSeconds: 2, NarrationSegment: "hello"}}
		p := &mission95HandlingProvider{fakeProvider: providerWithDefaults()}
		o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4), Providers: Registry{"fake": p}}
		got, err := o.Create(ctx, req)
		if err != nil {
			t.Fatalf("handling provider create error=%v", err)
		}
		if got.Request.NarrationText != "hello" {
			t.Fatalf("narration text=%q, want hello", got.Request.NarrationText)
		}
	})
	t.Run("dry run create generic error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), createErr: errors.New("boom")}
		req := requestWithDefaults()
		req.DryRun = true
		o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		if _, err := o.Create(ctx, req); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("dry-run create error=%v", err)
		}
	})
	t.Run("dry run conflict resolves to winner", func(t *testing.T) {
		winner := queueJobForTest("dry-winner")
		winner.IdempotencyKey = "k1"
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(),
			createErr:         queue.ErrIdempotencyConflict,
			idempotencySwapAt: 2, idempotencySwap: winner}
		req := requestWithDefaults()
		req.DryRun, req.IdempotencyKey = true, "k1"
		o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		got, err := o.Create(ctx, req)
		if err != nil || got.JobID != winner.JobID {
			t.Fatalf("dry-run conflict got=%s err=%v", got.JobID, err)
		}
	})
	t.Run("dry run conflict resolve error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(),
			createErr:      queue.ErrIdempotencyConflict,
			idempotencyErr: errors.New("lookup down"), idempotencyErrAt: 2}
		req := requestWithDefaults()
		req.DryRun, req.IdempotencyKey = true, "k1"
		o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{"fake": providerWithDefaults()}}
		if _, err := o.Create(ctx, req); err == nil || !strings.Contains(err.Error(), "resolve concurrent idempotent create") {
			t.Fatalf("dry-run resolve error=%v", err)
		}
	})
}

func TestMission95RecoverMarkNeedsManualErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("missing checkpoint list", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95EpisodeJob("rec-a", video.StateQueued)
		job.ShotExecutions = job.ShotExecutions[:1]
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Queue: queue.New(10)}
		if err := o.Recover(ctx); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("recover checkpoint error=%v", err)
		}
	})
	t.Run("parent id mismatch", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95EpisodeJob("rec-b", video.StateQueued)
		job.ProviderJobID = "parent"
		job.CurrentShot = 1
		job.ShotExecutions[0].ProviderJobID = ""
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Queue: queue.New(10)}
		if err := o.Recover(ctx); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("recover parent error=%v", err)
		}
	})
}

func TestMission95RetryAfterExpiredLease(t *testing.T) {
	ctx := context.Background()
	q := queue.New(10)
	o := &Orchestrator{Queue: q}
	job := video.NewJob("rl-1", requestWithDefaults())
	job.LeaseUntil = time.Now().Add(-time.Minute)
	o.retryAfterLease(ctx, job)
	deadline := time.Now().Add(2 * time.Second)
	for q.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if q.Len() != 1 {
		t.Fatalf("expired-lease retry queued=%d, want 1", q.Len())
	}
}

func TestMission95RunJobRemainingBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("queued submit error save failure", func(t *testing.T) {
		p := providerWithDefaults()
		p.submitErr = errors.New("uncertain")
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		job := video.NewJob("rj-subsave", requestWithDefaults())
		job.Provider = "fake"
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, p)
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("submit save error=%v", err)
		}
	})
	t.Run("queued empty id save failure", func(t *testing.T) {
		p := providerWithDefaults()
		p.submit = video.ProviderJob{ProviderJobID: " ", Provider: "fake", Model: "model-a", Accepted: true}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		job := video.NewJob("rj-emptysave", requestWithDefaults())
		job.Provider = "fake"
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, p)
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("empty id save error=%v", err)
		}
	})
	t.Run("submitted no id save failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := video.NewJob("rj-activesave", requestWithDefaults())
		job.State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, providerWithDefaults())
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("active save error=%v", err)
		}
	})
	t.Run("unsupported state", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("rj-unsup", requestWithDefaults())
		job.State = video.StateComposing
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, providerWithDefaults())
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "unsupported video job state") {
			t.Fatalf("unsupported state error=%v", err)
		}
		if got := mission95Get(t, store, "rj-unsup"); got.State != video.StateNeedsManualAction {
			t.Fatalf("unsupported state stored=%s", got.State)
		}
	})
	t.Run("unsupported state save failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := video.NewJob("rj-unsupsave", requestWithDefaults())
		job.State = video.StateComposing
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, providerWithDefaults())
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("unsupported save error=%v", err)
		}
	})
	t.Run("cancel requested update failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := video.NewJob("rj-cxsave", requestWithDefaults())
		job.State = video.StateCancelRequested
		job.Provider = "fake"
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, providerWithDefaults())
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("cancel save error=%v", err)
		}
	})
	t.Run("cancel requested settle failure is logged not fatal", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("rj-cxsettle", requestWithDefaults())
		job.State = video.StateCancelRequested
		job.Provider = "fake"
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		o := mission95SingleOrch(store, providerWithDefaults())
		o.Ledger = &mission95Ledger{settleEstimateErr: errors.New("ledger down")}
		if err := o.RunJob(ctx, job); err != nil {
			t.Fatalf("cancel settle error=%v", err)
		}
		if got := mission95Get(t, store, "rj-cxsettle"); got.State != video.StateCancelled {
			t.Fatalf("cancel settle state=%s", got.State)
		}
	})
}

func TestMission95DownloadAndCompleteDirect(t *testing.T) {
	ctx := context.Background()
	clipDir := t.TempDir()
	clip := mission95ClipAsset(t, clipDir)
	status := video.ProviderJobStatus{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{clip}}
	t.Run("already completed is noop", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("dl-done", requestWithDefaults())
		job.State = video.StateCompleted
		mission95Stored(t, store, job)
		ledger := &mission95Ledger{}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Ledger: ledger}
		if err := o.downloadAndComplete(ctx, providerWithDefaults(), job, status); err != nil {
			t.Fatalf("completed noop error=%v", err)
		}
		if ledger.settleActualCalls != 0 || ledger.settleEstimateCalls != 0 {
			t.Fatalf("completed noop settled actual=%d estimate=%d", ledger.settleActualCalls, ledger.settleEstimateCalls)
		}
	})
	t.Run("upload stage transition failure", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("dl-stage", requestWithDefaults())
		job.State = video.StateAdmitted
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}}
		if err := o.downloadAndComplete(ctx, providerWithDefaults(), job, status); err == nil || !strings.Contains(err.Error(), "could not enter output-upload stage") {
			t.Fatalf("upload stage error=%v", err)
		}
		if got := mission95Get(t, store, "dl-stage"); got.State != video.StateNeedsManualAction {
			t.Fatalf("upload stage state=%s", got.State)
		}
	})
	t.Run("settle plus save failure combined", func(t *testing.T) {
		p := providerWithDefaults()
		p.downloaded = []video.Asset{clip}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		job := video.NewJob("dl-combo", requestWithDefaults())
		job.State = video.StateSubmitted
		mission95Stored(t, store, job)
		ledger := &mission95Ledger{settleEstimateErr: errors.New("ledger down")}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Ledger: ledger}
		if err := o.downloadAndComplete(ctx, p, job, status); err == nil || !strings.Contains(err.Error(), "cost settlement failed (") || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("settle combo error=%v", err)
		}
	})
}

func TestMission95ForceFailRemaining(t *testing.T) {
	ctx := context.Background()
	t.Run("not configured", func(t *testing.T) {
		o := &Orchestrator{}
		if _, _, err := o.ForceFail(ctx, "x"); err == nil || !strings.Contains(err.Error(), "not fully configured") {
			t.Fatalf("force-fail configured error=%v", err)
		}
	})
	t.Run("second get failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getErrAt: 2}
		job := video.NewJob("ff-get2", requestWithDefaults())
		job.State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store}
		if _, _, err := o.ForceFail(ctx, "ff-get2"); err == nil || !strings.Contains(err.Error(), "mission95 injected get failure") {
			t.Fatalf("force-fail second get error=%v", err)
		}
	})
	t.Run("second get terminal", func(t *testing.T) {
		job := video.NewJob("ff-term2", requestWithDefaults())
		job.State = video.StateSubmitted
		terminal := job
		terminal.State = video.StateCompleted
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getSwapAt: 2, getSwap: terminal}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store}
		if _, _, err := o.ForceFail(ctx, "ff-term2"); !errors.Is(err, ErrForceFailNotActive) {
			t.Fatalf("force-fail second terminal error=%v", err)
		}
	})
	t.Run("no attempt success", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("ff-noatt", requestWithDefaults())
		job.State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store}
		attempted, confirmed, err := o.ForceFail(ctx, "ff-noatt")
		if err != nil || attempted || confirmed {
			t.Fatalf("force-fail no-attempt=%v %v err=%v", attempted, confirmed, err)
		}
		got := mission95Get(t, store, "ff-noatt")
		if got.State != video.StateFailed || !got.OperatorForceFailed || !strings.Contains(got.LastError, "before provider cancellation could be attempted") {
			t.Fatalf("force-fail no-attempt job=%+v", got)
		}
	})
	t.Run("confirmed success", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("ff-conf", requestWithDefaults())
		job.State = video.StateSubmitted
		job.Provider = "fake"
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Providers: Registry{"fake": providerWithDefaults()}}
		attempted, confirmed, err := o.ForceFail(ctx, "ff-conf")
		if err != nil || !attempted || !confirmed {
			t.Fatalf("force-fail confirmed=%v %v err=%v", attempted, confirmed, err)
		}
		if got := mission95Get(t, store, "ff-conf"); !strings.Contains(got.LastError, "provider acknowledged cancellation") {
			t.Fatalf("force-fail confirmed last error=%q", got.LastError)
		}
	})
	t.Run("unconfirmed success", func(t *testing.T) {
		p := providerWithDefaults()
		p.cancelErr = errors.New("remote down")
		store := queue.NewMemoryStore()
		job := video.NewJob("ff-unconf", requestWithDefaults())
		job.State = video.StateSubmitted
		job.Provider = "fake"
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Providers: Registry{"fake": p}}
		attempted, confirmed, err := o.ForceFail(ctx, "ff-unconf")
		if err != nil || !attempted || confirmed {
			t.Fatalf("force-fail unconfirmed=%v %v err=%v", attempted, confirmed, err)
		}
		if got := mission95Get(t, store, "ff-unconf"); !strings.Contains(got.LastError, "was not confirmed") {
			t.Fatalf("force-fail unconfirmed last error=%q", got.LastError)
		}
	})
	t.Run("persist failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := video.NewJob("ff-perr", requestWithDefaults())
		job.State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store}
		if _, _, err := o.ForceFail(ctx, "ff-perr"); err == nil || !strings.Contains(err.Error(), "persist operator force-fail") {
			t.Fatalf("force-fail persist error=%v", err)
		}
	})
}

func TestMission95FailAndCancelDirect(t *testing.T) {
	ctx := context.Background()
	t.Run("fail transition error", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("fl-trans", requestWithDefaults())
		job.State = video.StateCompleted
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store}
		if err := o.fail(ctx, job, "boom"); err == nil || !strings.Contains(err.Error(), "cannot mark video job failed") {
			t.Fatalf("fail transition error=%v", err)
		}
	})
	t.Run("fail settle failure logged not fatal", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("fl-settle", requestWithDefaults())
		job.State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Ledger: &mission95Ledger{settleEstimateErr: errors.New("ledger down")}}
		if err := o.fail(ctx, job, "boom msg"); err == nil || err.Error() != "boom msg" {
			t.Fatalf("fail settle error=%v", err)
		}
		if got := mission95Get(t, store, "fl-settle"); got.State != video.StateFailed {
			t.Fatalf("fail settle state=%s", got.State)
		}
	})
	t.Run("cancel update failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := video.NewJob("cx-perr", requestWithDefaults())
		job.State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store}
		if err := o.Cancel(ctx, "cx-perr"); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("cancel update error=%v", err)
		}
	})
	t.Run("cancel settle failure logged not fatal", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := video.NewJob("cx-settle", requestWithDefaults())
		job.State = video.StateSubmitted
		job.Provider = "fake"
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Providers: Registry{"fake": providerWithDefaults()}, Ledger: &mission95Ledger{settleEstimateErr: errors.New("ledger down")}}
		if err := o.Cancel(ctx, "cx-settle"); err != nil {
			t.Fatalf("cancel settle error=%v", err)
		}
		if got := mission95Get(t, store, "cx-settle"); got.State != video.StateCancelled {
			t.Fatalf("cancel settle state=%s", got.State)
		}
	})
}

func TestMission95EpisodeRemainingBranches(t *testing.T) {
	ctx := context.Background()
	clipDir := t.TempDir()
	clip := mission95ClipAsset(t, clipDir)
	t.Run("episode cancel provider error", func(t *testing.T) {
		p := providerWithDefaults()
		p.cancelErr = errors.New("remote down")
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-cxerr2", video.StateCancelRequested)
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		if err := o.RunJob(ctx, job); !errors.Is(err, p.cancelErr) {
			t.Fatalf("episode cancel error=%v", err)
		}
	})
	t.Run("episode cancel update failure", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95EpisodeJob("ep-cxsave", video.StateCancelRequested)
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, providerWithDefaults(), &mission95Sink{}, &mission95Composer{})
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "mission95 injected update failure") {
			t.Fatalf("episode cancel save error=%v", err)
		}
	})
	t.Run("episode cancel releases ledger", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-cxrel", video.StateCancelRequested)
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		ledger := &mission95Ledger{}
		o := mission95EpisodeOrch(store, providerWithDefaults(), &mission95Sink{}, &mission95Composer{})
		o.Ledger = ledger
		if err := o.RunJob(ctx, job); err != nil {
			t.Fatalf("episode cancel release error=%v", err)
		}
		if len(ledger.released) != 1 || ledger.released[0] != "ep-cxrel" {
			t.Fatalf("episode cancel released=%v", ledger.released)
		}
	})
	t.Run("resume path with metadata copies completes", func(t *testing.T) {
		p := providerWithDefaults()
		metaClip := clip
		metaClip.Metadata = map[string]string{"d": "1"}
		p.downloaded = []video.Asset{metaClip}
		outClip := clip
		outClip.Metadata = map[string]string{"k": "v"}
		p.statuses = []video.ProviderJobStatus{
			{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{outClip}},
			{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{outClip}},
		}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-resume", video.StateAdmitted)
		job.ShotExecutions[0].ProviderJobID = "remote-1"
		job.ShotExecutions[0].State = video.StateSubmitted
		mission95Stored(t, store, job)
		comp := &mission95Composer{}
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, comp)
		if err := o.RunJob(ctx, job); err != nil {
			t.Fatalf("resume run error=%v", err)
		}
		if got := mission95Get(t, store, "ep-resume"); got.State != video.StateCompleted {
			t.Fatalf("resume final state=%s", got.State)
		}
		if comp.calls != 1 || len(comp.specs) != 1 || len(comp.specs[0].Shots) != 2 {
			t.Fatalf("resume composer calls=%d specs=%+v", comp.calls, comp.specs)
		}
	})
	t.Run("last shot admitted transition failure", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{clip}}}
		p.downloaded = []video.Asset{clip}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-lasttrans", video.StateAdmitted)
		ckpt := mission95CheckpointAsset()
		job.Request.ShotPlan[0].OutputAsset = &ckpt
		job.ShotExecutions[1].ProviderJobID = "remote-1"
		job.ShotExecutions[1].State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		if err := o.RunJob(ctx, job); err == nil {
			t.Fatal("last-shot admitted transition error accepted")
		}
	})
	t.Run("poll cancel admitted transition failure", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCancelled}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-pollcxtrans", video.StateAdmitted)
		job.ShotExecutions[0].ProviderJobID = "remote-1"
		job.ShotExecutions[0].State = video.StateSubmitted
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		if err := o.RunJob(ctx, job); err == nil {
			t.Fatal("poll cancel admitted transition error accepted")
		}
	})
	t.Run("poll cancel releases ledger", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCancelled}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-pollcxrel", video.StateSubmitted)
		job.ShotExecutions[0].ProviderJobID = "remote-1"
		job.ShotExecutions[0].State = video.StateSubmitted
		mission95Stored(t, store, job)
		ledger := &mission95Ledger{}
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		o.Ledger = ledger
		if err := o.RunJob(ctx, job); err != nil {
			t.Fatalf("poll cancel release error=%v", err)
		}
		if len(ledger.released) != 1 || ledger.released[0] != "ep-pollcxrel" {
			t.Fatalf("poll cancel released=%v", ledger.released)
		}
		if got := mission95Get(t, store, "ep-pollcxrel"); got.State != video.StateCancelled {
			t.Fatalf("poll cancel state=%s", got.State)
		}
	})
	t.Run("poll status lookup save failure", func(t *testing.T) {
		p := providerWithDefaults()
		p.statusErrs = []error{errors.New("lookup down")}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 3}
		job := mission95EpisodeJob("ep-pollsave", video.StateAdmitted)
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "shot status lookup failed and checkpoint could not be saved") {
			t.Fatalf("poll save error=%v", err)
		}
	})
	t.Run("downloaded non-video asset", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{clip}}}
		audio := clip
		audio.ContentType = "audio/mpeg"
		p.downloaded = []video.Asset{audio}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-audio", video.StateAdmitted)
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "not a video asset") {
			t.Fatalf("non-video error=%v", err)
		}
	})
	t.Run("checkpoint put cancellation surfaces", func(t *testing.T) {
		ctx2, cancel := context.WithCancel(ctx)
		base := providerWithDefaults()
		base.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{clip}}}
		base.downloaded = []video.Asset{clip}
		p := &mission95CancellingProvider{fakeProvider: base, cancel: cancel}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-ckptcx", video.StateAdmitted)
		mission95Stored(t, store, job)
		sink := &mission95Sink{putErr: errors.New("sink down")}
		o := &Orchestrator{Store: store, Queue: queue.New(10), Providers: Registry{"fake": p},
			Assets: sink, Composer: &mission95Composer{}, MaxAssetBytes: 1 << 20,
			PollInterval: time.Nanosecond, MaxPolls: 5}
		if err := o.RunJob(ctx2, job); !errors.Is(err, context.Canceled) {
			t.Fatalf("checkpoint cancel error=%v", err)
		}
	})
	t.Run("compose narration error return", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95CheckpointJob("ce-narrerr", video.StateComposing)
		job.Request.NarrationText = "hello"
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Composer: &mission95Composer{}, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "narrator is unavailable") {
			t.Fatalf("compose narration error=%v", err)
		}
	})
	t.Run("compose subtitle error return", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95CheckpointJob("ce-suberr", video.StateComposing)
		job.Request.SubtitleLanguage = "en"
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Composer: &mission95Composer{}, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "captions require narration text") {
			t.Fatalf("compose subtitle error=%v", err)
		}
	})
	t.Run("compose cancellation surfaces", func(t *testing.T) {
		ctx2, cancel := context.WithCancel(ctx)
		store := queue.NewMemoryStore()
		job := mission95CheckpointJob("ce-cx", video.StateComposing)
		mission95Stored(t, store, job)
		comp := &mission95CancellingComposer{cancel: cancel, mission95Composer: &mission95Composer{}}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx2, &job); !errors.Is(err, context.Canceled) {
			t.Fatalf("compose cancel error=%v", err)
		}
	})
	t.Run("compose final put cancellation surfaces", func(t *testing.T) {
		ctx2, cancel := context.WithCancel(ctx)
		store := queue.NewMemoryStore()
		job := mission95CheckpointJob("ce-putcx", video.StateComposing)
		mission95Stored(t, store, job)
		sink := &mission95CancellingSink{cancel: cancel}
		o := &Orchestrator{Store: store, Assets: sink, Composer: &mission95Composer{}, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx2, &job); !errors.Is(err, context.Canceled) {
			t.Fatalf("compose put cancel error=%v", err)
		}
	})
	t.Run("compose settle plus save failure combined", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95CheckpointJob("ce-setcombo", video.StateComposing)
		mission95Stored(t, store, job)
		ledger := &mission95Ledger{settleEstimateErr: errors.New("ledger down")}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Composer: &mission95Composer{}, MaxAssetBytes: 1 << 20, Ledger: ledger}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "episode cost settlement failed (") || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("compose settle combo error=%v", err)
		}
	})
	t.Run("narration put cancellation surfaces", func(t *testing.T) {
		ctx2, cancel := context.WithCancel(ctx)
		dir := t.TempDir()
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("n-putcx", video.StateComposing)
		job.Request.DurationSeconds = 4
		job.Request.NarrationText = "hello world"
		mission95Stored(t, store, job)
		wavBytes, err := os.ReadFile(mission95WAV(t, dir, 1).URI)
		if err != nil {
			t.Fatal(err)
		}
		narrator := &mission95Narrator{write: func(path string) error {
			return os.WriteFile(path, wavBytes, 0600)
		}}
		o := &Orchestrator{Store: store, Assets: &mission95CancellingSink{cancel: cancel}, Narrator: narrator, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx2, &job, filepath.Join(dir, "out.wav")); !errors.Is(err, context.Canceled) {
			t.Fatalf("narration put cancel error=%v", err)
		}
	})
	t.Run("subtitle narration duration failure", func(t *testing.T) {
		dir := t.TempDir()
		data := []byte("this is not a wav file")
		path := filepath.Join(dir, "fake.wav")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		sumBytes := sha256.Sum256(data)
		sum := hex.EncodeToString(sumBytes[:])
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("s-durerr", video.StateComposing)
		job.Request.DurationSeconds = 4
		job.Request.NarrationText = "hello world"
		job.Request.SubtitleLanguage = "en"
		fake := video.Asset{ID: "narr", URI: path, ContentType: "audio/wav", SizeBytes: int64(len(data)), SHA256: sum}
		job.NarrationAsset = &fake
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "cannot determine narration duration for captions") {
			t.Fatalf("subtitle duration error=%v", err)
		}
	})
	t.Run("subtitle put cancellation surfaces", func(t *testing.T) {
		ctx2, cancel := context.WithCancel(ctx)
		dir := t.TempDir()
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("s-putcx", video.StateComposing)
		job.Request.DurationSeconds = 4
		job.Request.NarrationText = "hello world"
		job.Request.SubtitleLanguage = "en"
		wav := mission95WAV(t, dir, 1)
		job.NarrationAsset = &wav
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95CancellingSink{cancel: cancel}, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureSubtitleAsset(ctx2, &job); !errors.Is(err, context.Canceled) {
			t.Fatalf("subtitle put cancel error=%v", err)
		}
	})
	t.Run("subtitle checksum mismatch", func(t *testing.T) {
		dir := t.TempDir()
		srt := mission95SRTAsset(t, dir)
		orig, err := os.ReadFile(srt.URI)
		if err != nil {
			t.Fatal(err)
		}
		tampered := make([]byte, len(orig))
		copy(tampered, orig)
		tampered[len(tampered)-1] ^= 0xff
		if err := os.WriteFile(srt.URI, tampered, 0600); err != nil {
			t.Fatal(err)
		}
		if err := verifySubtitleAsset(srt, 1<<20); err == nil || !strings.Contains(err.Error(), "subtitle file checksum does not match") {
			t.Fatalf("subtitle checksum error=%v", err)
		}
	})
}
