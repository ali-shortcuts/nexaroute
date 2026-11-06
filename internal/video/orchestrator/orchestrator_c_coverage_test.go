package orchestrator

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// Agent C coverage: the branches the earlier suites never reached — the
// crypto-failure fallbacks, the poll/cancel error paths, the nil-provider
// episode guard, and the unreadable-file checkpoint paths.

func TestWrapStoreErrNilReturnsNil(t *testing.T) {
	if err := wrapStoreErr(nil); err != nil {
		t.Fatalf("wrapStoreErr(nil) = %v, want nil", err)
	}
}

func TestOwnerAndNewVideoJobIDRandFailureFallbacks(t *testing.T) {
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("synthetic rand failure") }
	t.Cleanup(func() { randRead = old })

	o := &Orchestrator{}
	id := o.owner()
	suffix := strings.TrimPrefix(id, "orch-")
	if id == suffix {
		t.Fatalf("owner id %q lacks the orch- prefix", id)
	}
	if _, err := strconv.ParseInt(suffix, 10, 64); err != nil {
		t.Fatalf("owner fallback id %q is not orch-<unixnano>: %v", id, err)
	}

	jobID := newVideoJobID()
	parts := strings.Split(jobID, "-")
	if len(parts) != 4 || parts[0] != "video" {
		t.Fatalf("job id fallback %q, want video-<nano>-<pid>-<counter>", jobID)
	}
	if _, err := strconv.ParseInt(parts[1], 10, 64); err != nil {
		t.Fatalf("job id fallback %q has a non-numeric timestamp: %v", jobID, err)
	}
	if _, err := strconv.Atoi(parts[2]); err != nil {
		t.Fatalf("job id fallback %q has a non-numeric pid: %v", jobID, err)
	}
}

func TestCreateRequiresFullConfiguration(t *testing.T) {
	o := &Orchestrator{Store: queue.NewMemoryStore(), Queue: queue.New(4)}
	if _, err := o.Create(context.Background(), requestWithDefaults()); err == nil || !strings.Contains(err.Error(), "not fully configured") {
		t.Fatalf("Create with nil providers err = %v, want not-fully-configured", err)
	}
}

type createFailingStore struct {
	*queue.MemoryStore
	createErr error
}

func (s *createFailingStore) Create(context.Context, video.VideoJob) error { return s.createErr }

func TestCreateGenericStoreErrorIsNotIdempotencyConflict(t *testing.T) {
	store := &createFailingStore{MemoryStore: queue.NewMemoryStore(), createErr: errors.New("boom")}
	o := &Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers: Registry{"fake": providerWithDefaults()},
	}
	r := requestWithDefaults()
	r.IdempotencyKey = "key-1"
	_, err := o.Create(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("Create with a generic store error err = %v, want the wrapped boom error", err)
	}
}

func pollJob(id string, state video.JobState) (video.VideoJob, *queue.MemoryStore) {
	store := queue.NewMemoryStore()
	job := video.VideoJob{JobID: id, State: state, Provider: "fake", Model: "model-a", ProviderJobID: "remote-1"}
	if err := store.Create(context.Background(), job); err != nil {
		panic(err)
	}
	return job, store
}

func TestPollAndFinalizeCancelledMidPoll(t *testing.T) {
	job, store := pollJob("poll-mid-cancel", video.StateSubmitted)
	p := providerWithDefaults()
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateProcessing, Progress: 0.1}}
	o := &Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers: Registry{"fake": p}, PollInterval: 10 * time.Second, MaxPolls: 5,
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := o.pollAndFinalize(ctx, p, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-poll cancel err = %v, want context.Canceled", err)
	}
}

func TestPollAndFinalizeCancelledTransitionError(t *testing.T) {
	job, store := pollJob("poll-cancel-trans", video.StateComposing)
	p := providerWithDefaults()
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCancelled}}
	o := &Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 5,
	}
	err := o.pollAndFinalize(context.Background(), p, job)
	if err == nil || !strings.Contains(err.Error(), "invalid video job transition") {
		t.Fatalf("Composing + Cancelled err = %v, want the invalid-transition error", err)
	}
}

func TestPollAndFinalizeUnsupportedStateTransition(t *testing.T) {
	job, store := pollJob("poll-unsupported-trans", video.StateCancelRequested)
	p := providerWithDefaults()
	p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateSubmitted, Progress: 0}}
	o := &Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers: Registry{"fake": p}, PollInterval: time.Nanosecond, MaxPolls: 5,
	}
	err := o.pollAndFinalize(context.Background(), p, job)
	if err == nil || !strings.Contains(err.Error(), "unsupported state transition") {
		t.Fatalf("CancelRequested + Submitted err = %v, want the unsupported-transition error", err)
	}
}

func TestRunEpisodeJobNilProviderIsUnavailable(t *testing.T) {
	store := queue.NewMemoryStore()
	job := video.NewJob(newVideoJobID(), episodeRequest())
	job.Provider, job.Model = "fake", "model-a"
	job.State = video.StateQueued
	if err := store.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{
		Store: store, Queue: queue.New(4),
		Providers: Registry{"fake": nil},
		Assets:    &episodeTestSink{root: t.TempDir()}, Composer: &episodeTestComposer{},
		PollInterval: time.Nanosecond, MaxPolls: 5,
	}
	err := o.RunJob(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "provider is unavailable while a shot still requires generation") {
		t.Fatalf("nil provider err = %v, want the provider-unavailable error", err)
	}
}

// unreadableDownloadProvider returns a shot asset that stats as a huge
// regular file but cannot be opened for reading, so the checkpoint copy
// must fail at ReadFile.
type unreadableDownloadProvider struct {
	*episodeTestProvider
	size int64
}

func (p *unreadableDownloadProvider) DownloadOutputs(_ context.Context, _ video.ProviderJobStatus, _ video.AssetSink) ([]video.Asset, error) {
	return []video.Asset{{
		ID: "shot-output", Filename: "output.mp4", Kind: "video",
		ContentType: "video/mp4", URI: "/proc/kcore",
		SizeBytes: p.size, SHA256: strings.Repeat("a", 64),
	}}, nil
}

func TestRunEpisodeJobUnreadableShotCheckpoint(t *testing.T) {
	// /proc/kcore stats as a regular file but cannot be opened without
	// CAP_SYS_RAWIO. On a machine where it CAN be opened, reading it
	// would try to consume the whole kernel address space, so skip
	// there: the branch is untestable safely on such a host.
	if f, err := os.Open("/proc/kcore"); err == nil {
		_ = f.Close()
		t.Skip("/proc/kcore is readable here; skipping the unreadable-checkpoint test")
	}
	info, err := os.Stat("/proc/kcore")
	if err != nil {
		t.Skipf("/proc/kcore is not available: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		t.Skipf("/proc/kcore does not stat as a non-empty regular file: %v", info)
	}

	root := t.TempDir()
	base := providerWithDefaults()
	base.statuses = []video.ProviderJobStatus{
		{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-1", Filename: "one.mp4", ContentType: "video/mp4"}}},
	}
	provider := &unreadableDownloadProvider{episodeTestProvider: &episodeTestProvider{fakeProvider: base}, size: info.Size()}
	store := queue.NewMemoryStore()
	q := queue.New(10)
	sink := &episodeTestSink{root: root}
	comp := &episodeTestComposer{}
	orch := &Orchestrator{
		Store: store, Queue: q, Providers: Registry{"fake": provider},
		Assets: sink, Composer: comp, MaxAssetBytes: math.MaxInt64,
		PollInterval: time.Nanosecond, MaxPolls: 5,
	}

	job, err := orch.Create(context.Background(), episodeRequest())
	if err != nil {
		t.Fatalf("Create multi-shot episode: %v", err)
	}
	err = orch.RunJob(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "could not be read for checkpointing") {
		t.Fatalf("unreadable checkpoint err = %v, want the checkpoint-read error", err)
	}
	got, err := store.Get(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != video.StateNeedsManualAction {
		t.Fatalf("job state = %s, want needs-manual-action", got.State)
	}
}

// kcoreOutputComposer reports success but leaves a symlink to /proc/kcore
// at the output path: it stats as a huge regular file, so the size check
// passes with a large MaxAssetBytes, but reading it fails because opening
// /proc/kcore requires CAP_SYS_RAWIO.
type kcoreOutputComposer struct{}

func (kcoreOutputComposer) ComposeEpisode(_ context.Context, spec composer.EpisodeSpec) (composer.Manifest, error) {
	if err := os.Symlink("/proc/kcore", spec.OutputPath); err != nil {
		return composer.Manifest{}, err
	}
	return composer.Manifest{}, nil
}

func TestComposeEpisodeUnreadableOutput(t *testing.T) {
	// Same guard as the unreadable-checkpoint test: on a machine where
	// /proc/kcore can be opened, reading it would try to consume the
	// whole kernel address space, so skip there.
	if f, err := os.Open("/proc/kcore"); err == nil {
		_ = f.Close()
		t.Skip("/proc/kcore is readable here; skipping the unreadable-output test")
	}
	if _, err := os.Stat("/proc/kcore"); err != nil {
		t.Skipf("/proc/kcore is not available: %v", err)
	}
	ctx := context.Background()
	store := queue.NewMemoryStore()
	job := mission95CheckpointJob("ce-kcore", video.StateComposing)
	mission95Stored(t, store, job)
	o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Composer: kcoreOutputComposer{}, MaxAssetBytes: math.MaxInt64}
	if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "could not read composed episode output") {
		t.Fatalf("kcore output err = %v, want the unreadable-output error", err)
	}
}

func TestEnsureNarrationAssetReadFailure(t *testing.T) {
	old := readFile
	readFile = func(string) ([]byte, error) { return nil, errors.New("synthetic read failure") }
	t.Cleanup(func() { readFile = old })

	root := t.TempDir()
	base := providerWithDefaults()
	base.statuses = []video.ProviderJobStatus{
		{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-1", Filename: "one.mp4", ContentType: "video/mp4"}}},
		{ProviderJobID: "remote-2", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-2", Filename: "two.mp4", ContentType: "video/mp4"}}},
	}
	provider := &episodeTestProvider{fakeProvider: base}
	store := queue.NewMemoryStore()
	sink := &episodeTestSink{root: filepath.Join(root, "assets")}
	orch := newEpisodeOrchestrator(t, store, queue.New(10), provider, sink, &episodeTestComposer{})
	orch.Narrator = &episodeTestNarrator{}

	request := episodeRequest()
	request.ShotPlan[0].NarrationSegment = "Part one tells the origin."
	request.ShotPlan[1].NarrationSegment = "Part two describes the royal city."
	request.SubtitleLanguage = "same-as-narration"
	job, err := orch.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("Create narrated episode: %v", err)
	}
	err = orch.RunJob(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "narration output could not be read") {
		t.Fatalf("narration read failure err = %v, want the narration-read error", err)
	}
}
