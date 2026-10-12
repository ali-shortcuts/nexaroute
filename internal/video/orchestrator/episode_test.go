package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
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
		ID: "shared-output", Filename: "output.mp4",
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
	ext := filepath.Ext(asset.Filename)
	if ext == "" {
		ext = ".mp4"
	}
	path := filepath.Join(s.root, name+ext)
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
	if spec.NarrationPath != "" {
		if _, err := os.Stat(spec.NarrationPath); err != nil {
			return composer.Manifest{}, fmt.Errorf("narration asset %q missing: %w", spec.NarrationPath, err)
		}
	}
	if spec.SubtitlePath != "" {
		if _, err := os.Stat(spec.SubtitlePath); err != nil {
			return composer.Manifest{}, fmt.Errorf("subtitle asset %q missing: %w", spec.SubtitlePath, err)
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

func newEpisodeOrchestrator(t *testing.T, store queue.JobStore, q *queue.Queue, provider *episodeTestProvider, sink *episodeTestSink, comp EpisodeComposer) *Orchestrator {
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

	request := episodeRequest()
	request.ShotPlan[0].OutputAsset = &video.Asset{
		ID: "caller-forged-checkpoint", URI: "/etc/passwd", Filename: "forged.mp4",
		Kind: "video", ContentType: "video/mp4", SizeBytes: 1, SHA256: strings.Repeat("a", 64),
	}
	job, err := orch.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("Create multi-shot episode: %v", err)
	}
	if job.Request.ShotPlan[0].OutputAsset != nil {
		t.Fatal("caller-supplied shot output checkpoint was trusted")
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
	if got.SourceAssets[0].URI == got.SourceAssets[1].URI {
		t.Fatalf("shot checkpoints collided on one URI: %+v", got.SourceAssets)
	}
	firstClip, err := os.ReadFile(got.SourceAssets[0].URI)
	if err != nil || string(firstClip) != "clip for remote-1" {
		t.Fatalf("first shot checkpoint overwritten: content=%q err=%v", firstClip, err)
	}
	secondClip, err := os.ReadFile(got.SourceAssets[1].URI)
	if err != nil || string(secondClip) != "clip for remote-2" {
		t.Fatalf("second shot checkpoint content=%q err=%v", secondClip, err)
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

func TestSingleShotPlanNormalizesToSingleProviderRequest(t *testing.T) {
	provider := providerWithDefaults()
	store := queue.NewMemoryStore()
	orch := &Orchestrator{
		Store: store, Queue: queue.New(4), Providers: Registry{"fake": provider},
		Assets: &episodeTestSink{root: t.TempDir()},
	}
	request := requestWithDefaults()
	request.Prompt = ""
	request.ShotPlan = []video.Shot{{
		ShotID: "single", Prompt: "A single shot", CameraMovement: "slow pan",
		DurationSeconds: request.DurationSeconds,
	}}
	job, err := orch.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("Create single-shot plan: %v", err)
	}
	if len(job.Request.ShotPlan) != 0 || !strings.Contains(job.Request.Prompt, "A single shot") || !strings.Contains(job.Request.Prompt, "Camera movement: slow pan") {
		t.Fatalf("single shot was not normalized into provider request: %+v", job.Request)
	}
	if len(job.ShotExecutions) != 0 {
		t.Fatalf("single shot should use the ordinary provider path, got checkpoints %+v", job.ShotExecutions)
	}
}

type episodeTestNarrator struct {
	calls int
	text  string
}

func (n *episodeTestNarrator) Synthesize(ctx context.Context, text, outputPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.calls++
	n.text = text
	return writeEpisodeTestWAV(outputPath)
}

func writeEpisodeTestWAV(path string) error {
	const sampleRate = uint32(22050)
	const dataSize = uint32(44100)
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36+dataSize)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], sampleRate)
	binary.LittleEndian.PutUint32(header[28:32], sampleRate*2)
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataSize)
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := file.Write(header); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(make([]byte, dataSize)); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func TestMultiShotNarrationCheckpointIsReusedAfterCompositionFailure(t *testing.T) {
	root := t.TempDir()
	base := providerWithDefaults()
	base.statuses = []video.ProviderJobStatus{
		{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-1", Filename: "one.mp4", ContentType: "video/mp4"}}},
		{ProviderJobID: "remote-2", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-2", Filename: "two.mp4", ContentType: "video/mp4"}}},
	}
	provider := &episodeTestProvider{fakeProvider: base}
	store := queue.NewMemoryStore()
	sink := &episodeTestSink{root: filepath.Join(root, "assets")}
	composer := &episodeTestComposer{fail: 1}
	narrator := &episodeTestNarrator{}
	orch := newEpisodeOrchestrator(t, store, queue.New(10), provider, sink, composer)
	orch.Narrator = narrator

	request := episodeRequest()
	request.ShotPlan[0].NarrationSegment = "Part one tells the origin."
	request.ShotPlan[1].NarrationSegment = "Part two describes the royal city."
	request.SubtitleLanguage = "same-as-narration"
	job, err := orch.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("Create narrated episode: %v", err)
	}
	wantText := "Part one tells the origin.\n\nPart two describes the royal city."
	if job.Request.NarrationText != wantText {
		t.Fatalf("narration script=%q, want %q", job.Request.NarrationText, wantText)
	}
	if err := orch.RunJob(context.Background(), job); err == nil || !strings.Contains(err.Error(), "simulated FFmpeg failure") {
		t.Fatalf("first composition error=%v, want simulated failure", err)
	}
	checkpoint, err := store.Get(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.NarrationAsset == nil || checkpoint.NarrationAsset.ContentType != "audio/wav" || len(checkpoint.NarrationAsset.SHA256) != 64 {
		t.Fatalf("narration checkpoint missing or invalid: %+v", checkpoint.NarrationAsset)
	}
	if narrator.calls != 1 || composer.calls != 1 {
		t.Fatalf("narrator/composer calls after first attempt=%d/%d", narrator.calls, composer.calls)
	}
	if checkpoint.SubtitleAsset == nil || checkpoint.SubtitleAsset.ContentType != "application/x-subrip" || len(checkpoint.SubtitleAsset.SHA256) != 64 {
		t.Fatalf("subtitle checkpoint missing or invalid: %+v", checkpoint.SubtitleAsset)
	}
	if err := orch.RunJob(context.Background(), checkpoint); err != nil {
		t.Fatalf("retry episode composition: %v", err)
	}
	got, err := store.Get(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateCompleted || got.NarrationAsset == nil || len(got.OutputAssets) != 1 {
		t.Fatalf("final narrated episode state/assets: state=%s narration=%+v outputs=%+v", got.State, got.NarrationAsset, got.OutputAssets)
	}
	if len(got.SourceAssets) != 4 {
		t.Fatalf("source assets=%d, want two clips, narration, and captions", len(got.SourceAssets))
	}
	if narrator.calls != 1 {
		t.Fatalf("narrator ran %d times after retry; expected checkpoint reuse", narrator.calls)
	}
	if len(composer.specs) != 2 || composer.specs[0].NarrationPath != got.NarrationAsset.URI || composer.specs[1].NarrationPath != got.NarrationAsset.URI {
		t.Fatalf("composition did not reuse narration checkpoint: %+v", composer.specs)
	}
	if composer.specs[0].SubtitlePath != got.SubtitleAsset.URI || composer.specs[1].SubtitlePath != got.SubtitleAsset.URI {
		t.Fatalf("composition did not reuse subtitle checkpoint: %+v", composer.specs)
	}
	srt, err := os.ReadFile(got.SubtitleAsset.URI)
	if err != nil || !strings.Contains(string(srt), "Part one tells the origin.") || !strings.Contains(string(srt), "Part two describes the royal city.") {
		t.Fatalf("generated SRT content=%q err=%v", srt, err)
	}
	for i, req := range provider.requests {
		if req.NarrationText != "" || req.AudioRequested || req.DialogueRequested || req.SubtitleLanguage != "" {
			t.Errorf("provider shot request %d received episode narration/audio fields: %+v", i, req)
		}
	}
}

func TestNarrationRequiresConfiguredPiperAndMultiShotEpisode(t *testing.T) {
	provider := &episodeTestProvider{fakeProvider: providerWithDefaults()}
	store := queue.NewMemoryStore()
	orch := newEpisodeOrchestrator(t, store, queue.New(10), provider, &episodeTestSink{root: t.TempDir()}, &episodeTestComposer{})
	request := episodeRequest()
	request.NarrationText = "Narrate this episode."
	if _, err := orch.Create(context.Background(), request); err == nil || !strings.Contains(err.Error(), "video.piper_enabled") {
		t.Fatalf("Create without narrator error=%v", err)
	}
	if provider.nextID != 0 {
		t.Fatalf("provider received %d submissions before narration configuration was validated", provider.nextID)
	}

	orch.Narrator = &episodeTestNarrator{}
	request = requestWithDefaults()
	request.NarrationText = "Narrate a single shot."
	if _, err := orch.Create(context.Background(), request); err == nil || !strings.Contains(err.Error(), "multi-shot episode") {
		t.Fatalf("single-shot narration error=%v", err)
	}
}
