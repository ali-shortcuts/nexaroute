package orchestrator

import (
	"context"
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

func coverageGapEpisodeJob(id string, state video.JobState) video.VideoJob {
	req := episodeRequest().Normalized()
	job := video.NewJob(id, req)
	job.Provider, job.Model = "fake", "model-a"
	job.State = state
	return job
}

func TestCoverageGapRecoverEpisodeBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("checkpoint list mismatch", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := coverageGapEpisodeJob("ep-mismatch", video.StateProcessing)
		job.ShotExecutions = job.ShotExecutions[:1]
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Queue: queue.New(4)}
		if err := o.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, job.JobID)
		if got.State != video.StateNeedsManualAction {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("parent provider mismatch", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := coverageGapEpisodeJob("ep-parent-mismatch", video.StateProcessing)
		job.ProviderJobID = "parent-1"
		job.CurrentShot = 1
		job.ShotExecutions[0].ProviderJobID = "other"
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Queue: queue.New(4)}
		if err := o.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, job.JobID)
		if got.State != video.StateNeedsManualAction {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("queued episode re-enqueued", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := coverageGapEpisodeJob("ep-queued", video.StateQueued)
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		q := queue.New(4)
		o := &Orchestrator{Store: store, Queue: q}
		if err := o.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if q.Len() != 1 {
			t.Fatalf("queue=%d, want 1", q.Len())
		}
	})
	t.Run("non-resumable episode state", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := coverageGapEpisodeJob("ep-planning", video.StatePlanning)
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Queue: queue.New(4)}
		if err := o.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, job.JobID)
		if got.State != video.StateNeedsManualAction {
			t.Fatalf("state=%s", got.State)
		}
	})
}

func TestCoverageGapRunEpisodeGuards(t *testing.T) {
	ctx := context.Background()
	sinkRoot := t.TempDir()
	newSink := func() *episodeTestSink { return &episodeTestSink{root: sinkRoot} }

	t.Run("terminal noop", func(t *testing.T) {
		o := &Orchestrator{}
		job := coverageGapEpisodeJob("term", video.StateCompleted)
		if err := o.runEpisodeJob(ctx, job); err != nil {
			t.Fatalf("terminal error=%v", err)
		}
	})
	t.Run("needs manual", func(t *testing.T) {
		o := &Orchestrator{}
		job := coverageGapEpisodeJob("manual", video.StateNeedsManualAction)
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("manual job ran")
		}
	})
	t.Run("checkpoint mismatch", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("ckpt", video.StateProcessing)
		job.ShotExecutions = nil
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := (&Orchestrator{Store: store}).runEpisodeJob(ctx, job); err == nil {
			t.Fatal("mismatched checkpoints accepted")
		}
	})
	t.Run("missing sink composer", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("nosink", video.StateProcessing)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := (&Orchestrator{Store: store}).runEpisodeJob(ctx, job); err == nil {
			t.Fatal("missing sink/composer accepted")
		}
	})
	t.Run("provider unavailable while shots remain", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("noprov", video.StateProcessing)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("missing provider registry accepted")
		}
	})
	t.Run("cancel without known job id", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("cancel-noid", video.StateCancelRequested)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": p}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("cancel without id accepted")
		}
	})
	t.Run("cancel provider error", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("cancel-err", video.StateCancelRequested)
		job.ProviderJobID = "remote"
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.cancelErr = errors.New("remote down")
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": p}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("cancel error suppressed")
		}
	})
	t.Run("cancel success releases reservation", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("cancel-ok", video.StateCancelRequested)
		job.ProviderJobID = "remote"
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": p}}
		if err := o.runEpisodeJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateCancelled || got.ProviderJobID != "" {
			t.Fatalf("cancel result=%+v", got)
		}
	})
	t.Run("queued admission persist error", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("admit-err", video.StateQueued)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		store.updateErrAt = 1
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": providerWithDefaults()}}
		if err := o.runEpisodeJob(ctx, job); err == nil || !strings.Contains(err.Error(), "persist episode admission") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("invalid persisted output checkpoint", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("bad-checkpoint", video.StateProcessing)
		bad := video.Asset{ID: "bad", URI: "", SizeBytes: 0, SHA256: "short"}
		job.Request.ShotPlan[0].OutputAsset = &bad
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": providerWithDefaults()}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	})
	t.Run("shot complete without asset", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("complete-noasset", video.StateProcessing)
		job.ShotExecutions[0].State = video.StateCompleted
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": providerWithDefaults()}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("complete-without-asset accepted")
		}
	})
	t.Run("planning without id is fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("planning-fence", video.StateProcessing)
		job.ShotExecutions[0].State = video.StatePlanning
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": providerWithDefaults()}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("ambiguous planning accepted")
		}
	})
	t.Run("capabilities outage retries", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("caps-down", video.StateProcessing)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.capsErr = errors.New("caps down")
		o := &Orchestrator{Store: store, Queue: queue.New(4), Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": p}, PollInterval: time.Nanosecond}
		if err := o.runEpisodeJob(ctx, job); err == nil || !strings.Contains(err.Error(), "capabilities") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("unsupported shot fails job", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("unsupported-shot", video.StateProcessing)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.caps.MaxDurationSeconds = 0.1
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": p}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("unsupported shot accepted")
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateFailed {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("submission uncertainty fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("submit-uncertain", video.StateProcessing)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.submitErr = errors.New("uncertain")
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": p}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("uncertain submission accepted")
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateNeedsManualAction {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("missing provider job id fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("submit-noid", video.StateProcessing)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.submit = video.ProviderJob{ProviderJobID: "  ", Accepted: true, Provider: "fake", Model: "model-a"}
		o := &Orchestrator{Store: store, Assets: newSink(), Composer: &episodeTestComposer{}, Providers: Registry{"fake": p}}
		if err := o.runEpisodeJob(ctx, job); err == nil {
			t.Fatal("missing provider id accepted")
		}
	})
}

func TestCoverageGapPollEpisodeShot(t *testing.T) {
	ctx := context.Background()
	newJob := func(id string) video.VideoJob {
		job := coverageGapEpisodeJob(id, video.StateProcessing)
		job.ShotExecutions[0].ProviderJobID = "remote-1"
		job.ShotExecutions[0].State = video.StateSubmitted
		return job
	}
	t.Run("different remote id", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-diff-id")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "other", State: video.StateProcessing, Progress: 0.1}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 1}
		if _, err := o.pollEpisodeShot(ctx, p, &job, 0); err == nil {
			t.Fatal("different id accepted")
		}
	})
	t.Run("invalid progress", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-bad-progress")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateProcessing, Progress: 5}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 1}
		if _, err := o.pollEpisodeShot(ctx, p, &job, 0); err == nil {
			t.Fatal("invalid progress accepted")
		}
	})
	t.Run("provider failed shot", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-failed")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateFailed, Progress: 0.2}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 1}
		if _, err := o.pollEpisodeShot(ctx, p, &job, 0); err == nil {
			t.Fatal("failed shot accepted")
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateFailed {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("cancelled shot releases job", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-cancelled")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCancelled}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 1}
		status, err := o.pollEpisodeShot(ctx, p, &job, 0)
		if err != nil || status.State != video.StateCancelled {
			t.Fatalf("status=%+v err=%v", status, err)
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateCancelled {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("completed shot persists progress", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-completed")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Usage: map[string]string{"k": "v"}, Outputs: []video.Asset{{ID: "o"}}}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 1}
		status, err := o.pollEpisodeShot(ctx, p, &job, 0)
		if err != nil || status.State != video.StateCompleted {
			t.Fatalf("status=%+v err=%v", status, err)
		}
		if job.ShotExecutions[0].Progress != 1 || job.UsageMetadata["shot_001.k"] != "v" {
			t.Fatalf("progress/usage not merged: %+v usage=%v", job.ShotExecutions[0], job.UsageMetadata)
		}
	})
	t.Run("queued status keeps submitted shot", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-queued")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateQueued, Progress: 0}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 2}
		if _, err := o.pollEpisodeShot(ctx, p, &job, 0); err == nil {
			t.Fatal("queued poll should hit deadline")
		}
	})
	t.Run("unsupported shot state", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-unsupported")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateExpired, Progress: 0.1}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 1}
		// Expired maps to the failed-shot path and marks the job failed.
		if _, err := o.pollEpisodeShot(ctx, p, &job, 0); err == nil {
			t.Fatal("expired shot accepted")
		}
	})
	t.Run("transient error then deadline", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-transient")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statusErrs = []error{errors.New("blip")}
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateProcessing, Progress: 0.1}}
		o := &Orchestrator{Store: store, PollInterval: time.Nanosecond, MaxPolls: 1}
		if _, err := o.pollEpisodeShot(ctx, p, &job, 0); err == nil {
			t.Fatal("transient error should still hit deadline with MaxPolls=1")
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.RetryCount != 2 {
			t.Fatalf("retry=%d, want transient plus deadline", got.RetryCount)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := newJob("poll-ctx")
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		o := &Orchestrator{Store: store, PollInterval: time.Hour}
		if _, err := o.pollEpisodeShot(cancelled, providerWithDefaults(), &job, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestCoverageGapComposeEpisodeGuards(t *testing.T) {
	ctx := context.Background()
	newComposingJob := func(id, root string) video.VideoJob {
		job := coverageGapEpisodeJob(id, video.StateComposing)
		for i := range job.Request.ShotPlan {
			path := filepath.Join(root, filepath.Base(id)+"-shot.mp4")
			if err := os.WriteFile(path, []byte("clip"), 0600); err != nil {
				panic(err)
			}
			sum := [32]byte{}
			copy(sum[:], strings.Repeat("a", 32))
			asset := video.Asset{ID: id + "-shot", URI: path, Filename: "shot.mp4", Kind: "video", ContentType: "video/mp4", SizeBytes: 4, SHA256: strings.Repeat("a", 64)}
			_ = sum
			job.Request.ShotPlan[i].OutputAsset = &asset
		}
		return job
	}
	t.Run("missing shot asset", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("compose-missing", video.StateComposing)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Assets: &episodeTestSink{root: t.TempDir()}, Composer: &episodeTestComposer{}}
		if err := o.composeEpisode(ctx, &job); err == nil {
			t.Fatal("missing assets accepted")
		}
	})
	t.Run("composer failure retries then fails", func(t *testing.T) {
		root := t.TempDir()
		store := coverageVideoOrchExtraStoreNew()
		job := newComposingJob("compose-retry", root)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		comp := &episodeTestComposer{fail: 5}
		o := &Orchestrator{Store: store, Queue: queue.New(4), Assets: &episodeTestSink{root: filepath.Join(root, "assets")}, Composer: comp, PollInterval: time.Nanosecond}
		if err := o.composeEpisode(ctx, &job); err == nil {
			t.Fatal("composer failure accepted")
		}
		job.CompositionRetryCount = 2
		if err := store.MemoryStore.Update(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := o.composeEpisode(ctx, &job); err == nil {
			t.Fatal("repeated composer failure should fail job")
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateFailed {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("composer success without file fails", func(t *testing.T) {
		root := t.TempDir()
		store := coverageVideoOrchExtraStoreNew()
		job := newComposingJob("compose-nofile", root)
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		comp := &failingComposeNoFile{}
		o := &Orchestrator{Store: store, Assets: &episodeTestSink{root: filepath.Join(root, "assets")}, Composer: comp}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "without an output file") {
			t.Fatalf("error=%v", err)
		}
	})
}

type failingComposeNoFile struct{}

func (failingComposeNoFile) ComposeEpisode(_ context.Context, _ composer.EpisodeSpec) (composer.Manifest, error) {
	return composer.Manifest{}, nil
}
