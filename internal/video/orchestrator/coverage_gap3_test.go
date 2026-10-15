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
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type failingNarrator struct{ err error }

func (n failingNarrator) Synthesize(context.Context, string, string) error { return n.err }

type failingSink struct{ err error }

func (s failingSink) Put(context.Context, video.Asset, []byte) (video.Asset, error) {
	return video.Asset{}, s.err
}

func coverageGapNarratedJob(id string) video.VideoJob {
	req := episodeRequest().Normalized()
	req.ShotPlan[0].NarrationSegment = "Part one."
	req.ShotPlan[1].NarrationSegment = "Part two."
	req.NarrationText = "Part one.\n\nPart two."
	job := video.NewJob(id, req)
	job.Provider, job.Model = "fake", "model-a"
	job.State = video.StateComposing
	return job
}

func TestCoverageGapNarrationGuards(t *testing.T) {
	ctx := context.Background()
	t.Run("no text returns empty", func(t *testing.T) {
		o := &Orchestrator{}
		job := coverageGapEpisodeJob("narr-empty", video.StateComposing)
		path, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(t.TempDir(), "n.wav"))
		if err != nil || path != "" {
			t.Fatalf("path=%q err=%v", path, err)
		}
	})
	t.Run("missing narrator fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("narr-nonarrator")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Assets: &episodeTestSink{root: t.TempDir()}}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(t.TempDir(), "n.wav")); err == nil {
			t.Fatal("missing narrator accepted")
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateNeedsManualAction {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("missing sink fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("narr-nosink")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Narrator: &episodeTestNarrator{}}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(t.TempDir(), "n.wav")); err == nil {
			t.Fatal("missing sink accepted")
		}
	})
	t.Run("synthesis failure retries", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("narr-synth-fail")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Queue: queue.New(4), Assets: &episodeTestSink{root: t.TempDir()}, Narrator: failingNarrator{errors.New("piper down")}, PollInterval: time.Nanosecond}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(t.TempDir(), "n.wav")); err == nil {
			t.Fatal("synthesis failure accepted")
		}
	})
	t.Run("over-long narration fails job", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("narr-toolong")
		job.Request.DurationSeconds = 0.5
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Assets: &episodeTestSink{root: t.TempDir()}, Narrator: &episodeTestNarrator{}}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(t.TempDir(), "n.wav")); err == nil {
			t.Fatal("over-long narration accepted")
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateFailed {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("persistence failure retries", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("narr-put-fail")
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Queue: queue.New(4), Assets: failingSink{errors.New("sink down")}, Narrator: &episodeTestNarrator{}, PollInterval: time.Nanosecond}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(t.TempDir(), "n.wav")); err == nil {
			t.Fatal("persistence failure accepted")
		}
	})
}

func TestCoverageGapSubtitleGuards(t *testing.T) {
	ctx := context.Background()
	t.Run("no language returns empty", func(t *testing.T) {
		o := &Orchestrator{}
		job := coverageGapEpisodeJob("sub-empty", video.StateComposing)
		path, err := o.ensureSubtitleAsset(ctx, &job)
		if err != nil || path != "" {
			t.Fatalf("path=%q err=%v", path, err)
		}
	})
	t.Run("captions without narration fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapEpisodeJob("sub-nonarr", video.StateComposing)
		job.Request.SubtitleLanguage = "en"
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store}
		if _, err := o.ensureSubtitleAsset(ctx, &job); err == nil {
			t.Fatal("captions without narration accepted")
		}
	})
	t.Run("captions without wav checkpoint fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("sub-nowav")
		job.Request.SubtitleLanguage = "en"
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store}
		if _, err := o.ensureSubtitleAsset(ctx, &job); err == nil {
			t.Fatal("captions without wav accepted")
		}
	})
	t.Run("captions without sink fenced", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("sub-nosink")
		job.Request.SubtitleLanguage = "en"
		wav := filepath.Join(t.TempDir(), "n.wav")
		if err := writeEpisodeTestWAV(wav); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(wav)
		sink := &episodeTestSink{root: t.TempDir()}
		saved, err := sink.Put(ctx, video.Asset{ID: job.JobID + "-narration", Filename: "n.wav", ContentType: "audio/wav"}, data)
		if err != nil {
			t.Fatal(err)
		}
		job.NarrationAsset = &saved
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store}
		if _, err := o.ensureSubtitleAsset(ctx, &job); err == nil {
			t.Fatal("captions without sink accepted")
		}
	})
	t.Run("subtitle persistence failure retries", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := coverageGapNarratedJob("sub-put-fail")
		job.Request.SubtitleLanguage = "en"
		wav := filepath.Join(t.TempDir(), "n.wav")
		if err := writeEpisodeTestWAV(wav); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(wav)
		disk := &episodeTestSink{root: t.TempDir()}
		saved, err := disk.Put(ctx, video.Asset{ID: job.JobID + "-narration", Filename: "n.wav", ContentType: "audio/wav"}, data)
		if err != nil {
			t.Fatal(err)
		}
		job.NarrationAsset = &saved
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Queue: queue.New(4), Assets: failingSink{errors.New("sink down")}, PollInterval: time.Nanosecond}
		if _, err := o.ensureSubtitleAsset(ctx, &job); err == nil {
			t.Fatal("subtitle persistence failure accepted")
		}
	})
}

func TestCoverageGapRunJobSingleShotGuards(t *testing.T) {
	ctx := context.Background()
	t.Run("nil store", func(t *testing.T) {
		if err := (&Orchestrator{}).RunJob(ctx, queueJobForTest("x")); err == nil {
			t.Fatal("nil store accepted")
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		o := &Orchestrator{Store: queue.NewMemoryStore()}
		if err := o.RunJob(cancelled, queueJobForTest("x")); !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("missing provider registry for single shot", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := queueJobForTest("single-noreg")
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := (&Orchestrator{Store: store}).RunJob(ctx, job); err == nil {
			t.Fatal("missing registry accepted")
		}
	})
	t.Run("provider unavailable after admission", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := queueJobForTest("gone-after-admit")
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		o := &Orchestrator{Store: store, Providers: Registry{}}
		if err := o.RunJob(ctx, job); err == nil {
			t.Fatal("unavailable provider accepted")
		}
		got, _ := store.Get(ctx, job.JobID)
		if got.State != video.StateFailed {
			t.Fatalf("state=%s", got.State)
		}
	})
}

func TestCoverageGapPollDownloadCancelRemaining(t *testing.T) {
	ctx := context.Background()
	t.Run("poll defaults and cancelled status save", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		job := queueJobForTest("poll-defaults")
		job.Provider, job.ProviderJobID, job.State = "fake", "remote", video.StateSubmitted
		if err := store.MemoryStore.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCancelled}}
		// Default interval and max polls still observe the terminal status.
		if err := (&Orchestrator{Store: store, Providers: Registry{"fake": p}}).RunJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, _ := store.MemoryStore.Get(ctx, job.JobID)
		if got.State != video.StateCancelled {
			t.Fatalf("state=%s", got.State)
		}
	})
	t.Run("download rejects malformed checksum", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := queueJobForTest("bad-checksum")
		job.Provider, job.ProviderJobID, job.State = "fake", "remote", video.StateSubmitted
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{Filename: "o.mp4"}}}}
		p.downloaded = []video.Asset{{URI: "/tmp/a.mp4", ContentType: "video/mp4", SizeBytes: 1, SHA256: strings.Repeat("z", 64)}}
		o := &Orchestrator{Store: store, Providers: Registry{"fake": p}, Assets: testSink{}, PollInterval: time.Nanosecond, MaxPolls: 1}
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("actual cost settlement fallback", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := queueJobForTest("actual-cost")
		job.Provider, job.ProviderJobID, job.State, job.EstimatedCostUSD = "fake", "remote", video.StateSubmitted, 2
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		ledger := &cost.Ledger{}
		if err := ledger.ReserveForJob(job.JobID, providerWithDefaults().estimate, 0); err != nil {
			t.Fatal(err)
		}
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote", State: video.StateCompleted, Progress: 1, Usage: map[string]string{"actual_cost_usd": "1.5"}, Outputs: []video.Asset{{Filename: "o.mp4"}}}}
		p.downloaded = []video.Asset{validStoredAsset()}
		o := &Orchestrator{Store: store, Providers: Registry{"fake": p}, Assets: testSink{}, Ledger: ledger, PollInterval: time.Nanosecond, MaxPolls: 1}
		if err := o.RunJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if ledger.Spent() != 1.5 {
			t.Fatalf("spent=%v, want actual 1.5", ledger.Spent())
		}
	})
	t.Run("fail transition error", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := queueJobForTest("fail-transition")
		job.State = video.StateCompleted
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := (&Orchestrator{Store: store}).fail(ctx, job, "boom"); err == nil {
			t.Fatal("failed-to-failed transition accepted")
		}
	})
	t.Run("cancel composing episode rejected", func(t *testing.T) {
		store := queue.NewMemoryStore()
		req := episodeRequest().Normalized()
		job := video.NewJob("ep-composing", req)
		job.State = video.StateComposing
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := (&Orchestrator{Store: store}).Cancel(ctx, job.JobID); err == nil || !strings.Contains(err.Error(), "composing locally") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("cancel without registry for active job", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := queueJobForTest("cancel-noreg")
		job.Provider, job.ProviderJobID = "fake", "remote"
		if err := store.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := (&Orchestrator{Store: store}).Cancel(ctx, job.JobID); err == nil {
			t.Fatal("cancel without registry accepted")
		}
	})
	t.Run("webhook store error", func(t *testing.T) {
		store := coverageVideoOrchExtraStoreNew()
		store.listErr = errors.New("list down")
		wp := NewWebhookProcessor(store)
		if err := wp.Apply(ctx, "evt-1", video.ProviderJobStatus{ProviderJobID: "x"}); err == nil {
			t.Fatal("webhook list error suppressed")
		}
	})
}
