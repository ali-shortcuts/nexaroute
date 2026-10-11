package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type episodeTestProvider struct {
	*fakeProvider
	nextID   int
	requests []video.VideoRequest
}

func (p *episodeTestProvider) CreateJob(_ context.Context, request video.VideoRequest) (video.ProviderJob, error) {
	p.nextID++
	p.requests = append(p.requests, request)
	id := fmt.Sprintf("remote-%d", p.nextID)
	return video.ProviderJob{ProviderJobID: id, Provider: p.id, Model: request.ModelPreference, Accepted: true}, nil
}

func (p *episodeTestProvider) DownloadOutputs(ctx context.Context, status video.ProviderJobStatus, sink video.AssetSink) ([]video.Asset, error) {
	asset := video.Asset{
		ID: "clip-" + status.ProviderJobID, Filename: status.ProviderJobID + ".mp4",
		Kind: "video", ContentType: "video/mp4",
	}
	return []video.Asset{mustPutEpisodeAsset(ctx, sink, asset, []byte("clip for "+status.ProviderJobID))}, nil
}

func mustPutEpisodeAsset(ctx context.Context, sink video.AssetSink, asset video.Asset, data []byte) video.Asset {
	saved, err := sink.Put(ctx, asset, data)
	if err != nil {
		panic(err)
	}
	return saved
}

type episodeTestSink struct{ root string }

func (s *episodeTestSink) Put(_ context.Context, asset video.Asset, data []byte) (video.Asset, error) {
	if len(data) == 0 {
		return video.Asset{}, errors.New("empty asset")
	}
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return video.Asset{}, err
	}
	name := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(asset.ID)
	if name == "" {
		name = "asset"
	}
	path := filepath.Join(s.root, name+".mp4")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return video.Asset{}, err
	}
	sum := sha256.Sum256(data)
	asset.URI = path
	asset.SizeBytes = int64(len(data))
	asset.SHA256 = hex.EncodeToString(sum[:])
	if asset.ContentType == "" {
		asset.ContentType = "video/mp4"
	}
	if asset.Kind == "" {
		asset.Kind = "video"
	}
	return asset, nil
}

type episodeTestComposer struct {
	calls int
	fail  int
	specs []composer.EpisodeSpec
}

func (c *episodeTestComposer) ComposeEpisode(_ context.Context, spec composer.EpisodeSpec) (composer.Manifest, error) {
	c.calls++
	c.specs = append(c.specs, spec)
	if c.fail > 0 {
		c.fail--
		return composer.Manifest{}, errors.New("simulated FFmpeg failure")
	}
	if len(spec.Shots) < 2 {
		return composer.Manifest{}, errors.New("expected at least two clips")
	}
	for _, path := range spec.Shots {
		if _, err := os.Stat(path); err != nil {
			return composer.Manifest{}, fmt.Errorf("shot asset %q missing: %w", path, err)
		}
	}
	if err := os.WriteFile(spec.OutputPath, []byte("final composed episode"), 0600); err != nil {
		return composer.Manifest{}, err
	}
	return composer.Manifest{Commands: [][]string{{"fake-ffmpeg"}}}, nil
}

func episodeRequest() video.VideoRequest {
	return video.VideoRequest{
		ProjectID: "project-episode", EpisodeID: "episode-1",
		ProviderPreference: "fake", ModelPreference: "model-a",
		Mode: video.ModeTextToVideo, DurationSeconds: 4, AspectRatio: video.Aspect16x9,
		ShotPlan: []video.Shot{
			{ShotID: "shot-1", Prompt: "Noora enters Persepolis", CameraMovement: "slow pan", DurationSeconds: 2},
			{ShotID: "shot-2", Prompt: "Noora walks through the gate", SubjectAction: "walks forward", DurationSeconds: 2},
		},
	}
}

func newEpisodeOrchestrator(t *testing.T, store queue.JobStore, q *queue.Queue, provider *episodeTestProvider, sink *episodeTestSink, comp *episodeTestComposer) *Orchestrator {
	t.Helper()
	return &Orchestrator{
		Store: store, Queue: q, Providers: Registry{"fake": provider},
		Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20,
		PollInterval: time.Nanosecond, MaxPolls: 5,
	}
}

func TestMultiShotEpisodePersistsPerShotAndComposesFinalMP4(t *testing.T) {
	root := t.TempDir()
	base := providerWithDefaults()
	base.statuses = []video.ProviderJobStatus{
		{ProviderJobID: "remote-1", State: video.StateQueued, Progress: 0},
		{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-1", Filename: "one.mp4", ContentType: "video/mp4"}}},
		{ProviderJobID: "remote-2", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-2", Filename: "two.mp4", ContentType: "video/mp4"}}},
	}
	provider := &episodeTestProvider{fakeProvider: base}
	store := queue.NewMemoryStore()
	q := queue.New(10)
	sink := &episodeTestSink{root: filepath.Join(root, "assets")}
	comp := &episodeTestComposer{}
	orch := newEpisodeOrchestrator(t, store, q, provider, sink, comp)

	job, err := orch.Create(context.Background(), episodeRequest())
	if err != nil {
		t.Fatalf("Create multi-shot episode: %v", err)
	}
	if len(job.ShotExecutions) != 2 || job.TotalShots != 2 || job.EstimatedCostUSD != 4 {
		t.Fatalf("created episode checkpoint/cost metadata: %+v", job)
	}
	if err := orch.RunJob(context.Background(), job); err != nil {
		t.Fatalf("RunJob multi-shot episode: %v", err)
	}
	got, err := store.Get(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted || got.Progress != 1 || got.CurrentShot != 2 {
		t.Fatalf("final episode state/progress: state=%s progress=%v current=%d", got.State, got.Progress, got.CurrentShot)
	}
	if len(got.ShotExecutions) != 2 || got.ShotExecutions[0].State != video.StateCompleted || got.ShotExecutions[1].State != video.StateCompleted {
		t.Fatalf("shot checkpoints not completed: %+v", got.ShotExecutions)
	}
	if got.Request.ShotPlan[0].OutputAsset == nil || got.Request.ShotPlan[1].OutputAsset == nil || len(got.SourceAssets) != 2 {
		t.Fatalf("source shot assets were not persisted: request=%+v sources=%+v", got.Request.ShotPlan, got.SourceAssets)
	}
	if len(got.OutputAssets) != 1 || got.OutputAssets[0].ContentType != "video/mp4" || got.OutputAssets[0].SHA256 == "" {
		t.Fatalf("final episode asset invalid: %+v", got.OutputAssets)
	}
	if provider.nextID != 2 || len(provider.requests) != 2 {
		t.Fatalf("provider jobs=%d requests=%d, want one per shot", provider.nextID, len(provider.requests))
	}
	if !strings.Contains(provider.requests[0].Prompt, "Camera movement: slow pan") || !strings.Contains(provider.requests[1].Prompt, "Subject action: walks forward") {
		t.Fatalf("shot direction metadata was not included in prompts: %+v", provider.requests)
	}
	if comp.calls != 1 || len(comp.specs[0].Shots) != 2 {
		t.Fatalf("composer calls/specs=%d/%+v", comp.calls, comp.specs)
	}
}

func TestRecoveryResumesEpisodeCompositionFromPersistedShotAssets(t *testing.T) {
	root := t.TempDir()
	store := queue.NewMemoryStore()
	q := queue.New(10)
	request := episodeRequest().Normalized()
	job := video.NewJob("episode-recover", request)
	job.Provider, job.Model = "fake", "model-a"
	job.State = video.StateComposing
	job.Progress = 0.85
	for i := range job.Request.ShotPlan {
		path := filepath.Join(root, fmt.Sprintf("shot-%d.mp4", i+1))
		if err := os.WriteFile(path, []byte("persisted clip"), 0600); err != nil {
			t.Fatal(err)
		}
		asset := video.Asset{ID: fmt.Sprintf("shot-asset-%d", i+1), URI: path, Filename: filepath.Base(path), Kind: "video", ContentType: "video/mp4", SizeBytes: 14, SHA256: strings.Repeat("a", 64)}
		job.Request.ShotPlan[i].OutputAsset = &asset
		job.ShotExecutions[i].ProviderJobID = fmt.Sprintf("remote-%d", i+1)
		job.ShotExecutions[i].State = video.StateCompleted
		job.ShotExecutions[i].Progress = 1
	}
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	provider := &episodeTestProvider{fakeProvider: providerWithDefaults()}
	sink := &episodeTestSink{root: filepath.Join(root, "assets")}
	comp := &episodeTestComposer{}
	orch := newEpisodeOrchestrator(t, store, q, provider, sink, comp)
	if err := orch.Recover(context.Background()); err != nil {
		t.Fatalf("Recover composition checkpoint: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("recovery queue length=%d, want 1", q.Len())
	}
	queued, err := q.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := orch.RunJob(context.Background(), queued); err != nil {
		t.Fatalf("resume composition: %v", err)
	}
	got, err := store.Get(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted || len(got.OutputAssets) != 1 || comp.calls != 1 {
		t.Fatalf("resumed job state=%s outputs=%d composer_calls=%d", got.State, len(got.OutputAssets), comp.calls)
	}
	if provider.nextID != 0 {
		t.Fatalf("recovery resubmitted provider work %d times", provider.nextID)
	}
}

func TestRecoveryQuarantinesAmbiguousShotSubmission(t *testing.T) {
	request := episodeRequest().Normalized()
	job := video.NewJob("episode-ambiguous", request)
	job.Provider, job.Model = "fake", "model-a"
	job.State = video.StateProcessing
	job.ShotExecutions[0].State = video.StatePlanning
	job.ShotExecutions[0].ProviderJobID = ""
	store := queue.NewMemoryStore()
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	q := queue.New(10)
	provider := &episodeTestProvider{fakeProvider: providerWithDefaults()}
	orch := newEpisodeOrchestrator(t, store, q, provider, &episodeTestSink{root: t.TempDir()}, &episodeTestComposer{})
	if err := orch.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateNeedsManualAction || q.Len() != 0 || provider.nextID != 0 {
		t.Fatalf("ambiguous recovery state=%s queue=%d submissions=%d", got.State, q.Len(), provider.nextID)
	}
}


func TestSingleShotPollingAcceptsProviderQueuedState(t *testing.T) {
	provider := providerWithDefaults()
	provider.statuses = []video.ProviderJobStatus{
		{ProviderJobID: "remote-1", State: video.StateQueued, Progress: 0},
		{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "output", Filename: "output.mp4", ContentType: "video/mp4"}}},
	}
	provider.downloaded = []video.Asset{validStoredAsset()}
	store := queue.NewMemoryStore()
	orch := &Orchestrator{
		Store: store, Queue: queue.New(4), Providers: Registry{"fake": provider},
		Assets: &episodeTestSink{root: t.TempDir()}, PollInterval: time.Nanosecond, MaxPolls: 3,
	}
	job, err := orch.Create(context.Background(), requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := orch.RunJob(context.Background(), job); err != nil {
		t.Fatalf("queued provider status should remain resumable: %v", err)
	}
	got, err := store.Get(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted || provider.statusCalls != 2 {
		t.Fatalf("state=%s status calls=%d, want completed after queued then completed", got.State, provider.statusCalls)
	}
}


type failingPreflightComposer struct{ *episodeTestComposer }

func (c *failingPreflightComposer) Preflight(context.Context) error {
	return errors.New("ffmpeg executable missing")
}

func TestMultiShotAdmissionFailsBeforeProviderEstimateWhenComposerUnavailable(t *testing.T) {
	provider := &episodeTestProvider{fakeProvider: providerWithDefaults()}
	store := queue.NewMemoryStore()
	q := queue.New(10)
	comp := &failingPreflightComposer{episodeTestComposer: &episodeTestComposer{}}
	orch := newEpisodeOrchestrator(t, store, q, provider, &episodeTestSink{root: t.TempDir()}, comp)
	if _, err := orch.Create(context.Background(), episodeRequest()); err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("Create error=%v, want composer preflight failure", err)
	}
	jobs, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 || q.Len() != 0 || provider.nextID != 0 {
		t.Fatalf("failed preflight admitted work: stored=%d queued=%d provider submissions=%d", len(jobs), q.Len(), provider.nextID)
	}
}
