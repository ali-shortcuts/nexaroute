package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
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

// mission95Composer is an EpisodeComposer with scripted outcomes.
type mission95Composer struct {
	err    error
	noFile bool
	empty  bool
	dirOut bool
	specs  []composer.EpisodeSpec
	calls  int
}

func (c *mission95Composer) ComposeEpisode(_ context.Context, spec composer.EpisodeSpec) (composer.Manifest, error) {
	c.calls++
	c.specs = append(c.specs, spec)
	if c.err != nil {
		return composer.Manifest{}, c.err
	}
	if c.noFile {
		return composer.Manifest{Commands: [][]string{{"fake-ffmpeg"}}}, nil
	}
	if c.dirOut {
		if err := os.Mkdir(spec.OutputPath, 0700); err != nil {
			return composer.Manifest{}, err
		}
		return composer.Manifest{}, nil
	}
	content := []byte("final composed episode")
	if c.empty {
		content = nil
	}
	if err := os.MkdirAll(filepath.Dir(spec.OutputPath), 0700); err != nil {
		return composer.Manifest{}, err
	}
	if err := os.WriteFile(spec.OutputPath, content, 0600); err != nil {
		return composer.Manifest{}, err
	}
	return composer.Manifest{Commands: [][]string{{"fake-ffmpeg"}}}, nil
}

// mission95Narrator is a Narrator with a scripted Synthesize.
type mission95Narrator struct {
	err   error
	write func(path string) error
	calls int
}

func (n *mission95Narrator) Synthesize(_ context.Context, _ string, path string) error {
	n.calls++
	if n.err != nil {
		return n.err
	}
	if n.write != nil {
		return n.write(path)
	}
	return nil
}

func mission95EpisodeJob(id string, state video.JobState) video.VideoJob {
	job := video.NewJob(id, episodeRequest())
	job.Provider, job.Model = "fake", "model-a"
	job.State = state
	return job
}

func mission95EpisodeOrch(store queue.JobStore, p *fakeProvider, sink video.AssetSink, comp EpisodeComposer) *Orchestrator {
	return &Orchestrator{
		Store: store, Queue: queue.New(10), Providers: Registry{"fake": p},
		Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20,
		PollInterval: time.Nanosecond, MaxPolls: 5,
	}
}

func mission95ClipAsset(t *testing.T, dir string) video.Asset {
	t.Helper()
	path := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(path, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	return video.Asset{ID: "out-1", Filename: "one.mp4", ContentType: "video/mp4",
		URI: path, SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
}

func mission95CheckpointAsset() video.Asset {
	return video.Asset{ID: "ckpt", URI: "clip.mp4", ContentType: "video/mp4",
		SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
}

func mission95CheckpointJob(id string, state video.JobState) video.VideoJob {
	job := mission95EpisodeJob(id, state)
	asset := mission95CheckpointAsset()
	job.Request.ShotPlan[0].OutputAsset = &asset
	asset2 := mission95CheckpointAsset()
	job.Request.ShotPlan[1].OutputAsset = &asset2
	return job
}

// mission95WAV writes a minimal PCM WAV (8000 Hz mono 16-bit) of the given
// duration and returns the asset record describing it.
func mission95WAV(t *testing.T, dir string, seconds float64) video.Asset {
	t.Helper()
	byteRate := uint32(16000)
	dataLen := int(byteRate * uint32(seconds))
	buf := make([]byte, 0, 44+dataLen)
	buf = append(buf, []byte("RIFF")...)
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(36+dataLen))
	buf = append(buf, size...)
	buf = append(buf, []byte("WAVE")...)
	buf = append(buf, []byte("fmt ")...)
	buf = append(buf, []byte{16, 0, 0, 0}...)
	buf = append(buf, []byte{1, 0, 1, 0}...)
	sr := make([]byte, 4)
	binary.LittleEndian.PutUint32(sr, 8000)
	buf = append(buf, sr...)
	br := make([]byte, 4)
	binary.LittleEndian.PutUint32(br, byteRate)
	buf = append(buf, br...)
	buf = append(buf, []byte{2, 0, 16, 0}...)
	buf = append(buf, []byte("data")...)
	dl := make([]byte, 4)
	binary.LittleEndian.PutUint32(dl, uint32(dataLen))
	buf = append(buf, dl...)
	buf = append(buf, make([]byte, dataLen)...)
	path := filepath.Join(dir, "narration.wav")
	if err := os.WriteFile(path, buf, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf)
	return video.Asset{ID: "narr", URI: path, ContentType: "audio/wav",
		SizeBytes: int64(len(buf)), SHA256: hex.EncodeToString(sum[:])}
}

func mission95SRTAsset(t *testing.T, dir string) video.Asset {
	t.Helper()
	data := []byte("1\n00:00:00,000 --> 00:00:01,000\nHello.\n")
	path := filepath.Join(dir, "subs.srt")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return video.Asset{ID: "subs", URI: path, ContentType: "application/x-subrip",
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func TestMission95RunEpisodeJobBranches(t *testing.T) {
	ctx := context.Background()
	clipDir := t.TempDir()
	completedStatus := func(id string, outputs []video.Asset) video.ProviderJobStatus {
		return video.ProviderJobStatus{ProviderJobID: id, State: video.StateCompleted, Progress: 1, Outputs: outputs}
	}
	run := func(t *testing.T, store queue.JobStore, p *fakeProvider, job video.VideoJob, sink *mission95Sink, comp *mission95Composer) error {
		t.Helper()
		if sink == nil {
			sink = &mission95Sink{}
		}
		if comp == nil {
			comp = &mission95Composer{}
		}
		return mission95EpisodeOrch(store, p, sink, comp).RunJob(ctx, job)
	}
	t.Run("providers nil", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-noprov", video.StateAdmitted)
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Queue: queue.New(4), Assets: &mission95Sink{}, Composer: &mission95Composer{}}
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "provider registry is unavailable") {
			t.Fatalf("providers-nil error=%v", err)
		}
		if got := mission95Get(t, store, "ep-noprov"); got.State != video.StateNeedsManualAction {
			t.Fatalf("providers-nil state=%s", got.State)
		}
	})
	t.Run("provider missing", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-gone", video.StateAdmitted)
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Queue: queue.New(4), Providers: Registry{},
			Assets: &mission95Sink{}, Composer: &mission95Composer{}}
		if err := o.RunJob(ctx, job); err == nil || err.Error() != "provider unavailable" {
			t.Fatalf("provider-missing error=%v", err)
		}
		if got := mission95Get(t, store, "ep-gone"); got.State != video.StateFailed {
			t.Fatalf("provider-missing state=%s", got.State)
		}
	})
	t.Run("cancel requested no id", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-cx-noid", video.StateCancelRequested)
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "without a known active provider job ID") {
			t.Fatalf("cancel-noid error=%v", err)
		}
	})
	t.Run("cancel requested provider error", func(t *testing.T) {
		p := providerWithDefaults()
		p.cancelErr = errors.New("remote down")
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-cx-err", video.StateCancelRequested)
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); !errors.Is(err, p.cancelErr) {
			t.Fatalf("cancel error=%v", err)
		}
	})
	t.Run("cancel requested success", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-cx-ok", video.StateCancelRequested)
		job.ProviderJobID = "remote-1"
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err != nil {
			t.Fatalf("cancel success error=%v", err)
		}
		if got := mission95Get(t, store, "ep-cx-ok"); got.State != video.StateCancelled || got.ProviderJobID != "" {
			t.Fatalf("cancel success state=%s id=%q", got.State, got.ProviderJobID)
		}
	})
	t.Run("queued admission persist error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95EpisodeJob("ep-adm-err", video.StateQueued)
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "persist episode admission") {
			t.Fatalf("admission error=%v", err)
		}
	})
	t.Run("reload get error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getErrAt: 2}
		job := mission95EpisodeJob("ep-geterr", video.StateQueued)
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "reload episode before shot 1") {
			t.Fatalf("reload error=%v", err)
		}
	})
	t.Run("reload terminal is noop", func(t *testing.T) {
		job := mission95EpisodeJob("ep-term", video.StateQueued)
		terminal := job
		terminal.State = video.StateCompleted
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getSwapAt: 2, getSwap: terminal}
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err != nil {
			t.Fatalf("reload terminal error=%v", err)
		}
	})
	t.Run("invalid checkpoint asset", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-badckpt", video.StateAdmitted)
		bad := video.Asset{URI: "", SizeBytes: 0, SHA256: ""}
		job.Request.ShotPlan[0].OutputAsset = &bad
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "invalid persisted output asset checkpoint") {
			t.Fatalf("checkpoint error=%v", err)
		}
	})
	t.Run("parent id mismatch", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-parent", video.StateAdmitted)
		job.ProviderJobID = "parent"
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "without a matching shot checkpoint") {
			t.Fatalf("parent mismatch error=%v", err)
		}
	})
	t.Run("run completed without asset", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-runcomp", video.StateAdmitted)
		job.ShotExecutions[0].State = video.StateCompleted
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "marked complete without a persisted output asset") {
			t.Fatalf("run-completed error=%v", err)
		}
	})
	t.Run("run planning without id", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-runplan", video.StateAdmitted)
		job.ShotExecutions[0].State = video.StatePlanning
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "may have been submitted before a crash") {
			t.Fatalf("run-planning error=%v", err)
		}
	})
	t.Run("caps error", func(t *testing.T) {
		p := providerWithDefaults()
		p.capsErr = errors.New("caps down")
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-capserr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "read provider capabilities") {
			t.Fatalf("caps error=%v", err)
		}
	})
	t.Run("caps error plus save error", func(t *testing.T) {
		p := providerWithDefaults()
		p.capsErr = errors.New("caps down")
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95EpisodeJob("ep-capsave", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("caps save error=%v", err)
		}
	})
	t.Run("shot no longer supported", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-unsup", video.StateAdmitted)
		job.Model = "other-model"
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "no longer supported") {
			t.Fatalf("unsupported shot error=%v", err)
		}
		if got := mission95Get(t, store, "ep-unsup"); got.State != video.StateFailed {
			t.Fatalf("unsupported shot state=%s", got.State)
		}
	})
	t.Run("provider rejects shot", func(t *testing.T) {
		p := providerWithDefaults()
		p.validateErr = errors.New("nope")
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-reject", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "provider rejected shot") {
			t.Fatalf("reject shot error=%v", err)
		}
	})
	t.Run("persist intent error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95EpisodeJob("ep-intent", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "persist shot") {
			t.Fatalf("intent error=%v", err)
		}
	})
	t.Run("submit error", func(t *testing.T) {
		p := providerWithDefaults()
		p.submitErr = errors.New("uncertain")
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-suberr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "submission outcome is unknown") {
			t.Fatalf("submit error=%v", err)
		}
	})
	t.Run("submit empty id", func(t *testing.T) {
		p := providerWithDefaults()
		p.submit = video.ProviderJob{ProviderJobID: " ", Provider: "fake", Model: "model-a", Accepted: true}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-subempty", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "no job ID for shot") {
			t.Fatalf("submit empty error=%v", err)
		}
	})
	t.Run("submit persist error", func(t *testing.T) {
		p := providerWithDefaults()
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 2}
		job := mission95EpisodeJob("ep-subperr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "not durably saved") {
			t.Fatalf("submit persist error=%v", err)
		}
		if p.cancelCalls != 1 {
			t.Fatalf("submit persist cancel calls=%d, want 1", p.cancelCalls)
		}
	})
	t.Run("submit inconsistent", func(t *testing.T) {
		p := providerWithDefaults()
		p.submit = video.ProviderJob{ProviderJobID: "remote-9", Provider: "fake", Model: "model-a", Accepted: false}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-subinc", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "inconsistent") {
			t.Fatalf("submit inconsistent error=%v", err)
		}
	})
	t.Run("poll different shot id", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "other", State: video.StateProcessing, Progress: 0.1}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-diffid", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "different shot job ID") {
			t.Fatalf("poll diff id error=%v", err)
		}
	})
	t.Run("poll invalid progress", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateProcessing, Progress: 2}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-badprog", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "invalid shot progress") {
			t.Fatalf("poll progress error=%v", err)
		}
	})
	t.Run("poll shot failed", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateFailed}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-shotfail", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "ended without a video output") {
			t.Fatalf("poll failed error=%v", err)
		}
		if got := mission95Get(t, store, "ep-shotfail"); got.State != video.StateFailed {
			t.Fatalf("poll failed state=%s", got.State)
		}
	})
	t.Run("poll shot cancelled", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCancelled}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-shotcx", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err != nil {
			t.Fatalf("poll cancelled error=%v", err)
		}
		if got := mission95Get(t, store, "ep-shotcx"); got.State != video.StateCancelled {
			t.Fatalf("poll cancelled state=%s", got.State)
		}
	})
	t.Run("poll cancelled persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateCancelled}}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 3}
		job := mission95EpisodeJob("ep-cxperr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil {
			t.Fatal("poll cancelled persist error accepted")
		}
	})
	t.Run("poll completed persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 3}
		job := mission95EpisodeJob("ep-compperr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "persist completed shot status") {
			t.Fatalf("poll completed persist error=%v", err)
		}
	})
	t.Run("poll progress then deadline", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateProcessing, Progress: 0.5}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-prog", video.StateAdmitted)
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		o.MaxPolls = 1
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "polling deadline") {
			t.Fatalf("poll deadline error=%v", err)
		}
		if got := mission95Get(t, store, "ep-prog"); got.State != video.StateProcessing {
			t.Fatalf("poll deadline state=%s, want processing", got.State)
		}
	})
	t.Run("poll progress persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateProcessing, Progress: 0.5}}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 3}
		job := mission95EpisodeJob("ep-progperr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "persist shot progress") {
			t.Fatalf("poll progress persist error=%v", err)
		}
	})
	t.Run("poll unsupported state", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateAdmitted}}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-unsupst", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "unsupported shot state") {
			t.Fatalf("poll unsupported error=%v", err)
		}
	})
	t.Run("poll deadline persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{{ProviderJobID: "remote-1", State: video.StateProcessing, Progress: 0.5}}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 4}
		job := mission95EpisodeJob("ep-dlperr", video.StateAdmitted)
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		o.MaxPolls = 1
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("poll deadline persist error=%v", err)
		}
	})
	t.Run("shot completed without outputs", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", nil)}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-noout", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "without output assets") {
			t.Fatalf("no-outputs error=%v", err)
		}
	})
	t.Run("shot upload persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 4}
		job := mission95EpisodeJob("ep-uperr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "persist shot") {
			t.Fatalf("upload persist error=%v", err)
		}
	})
	t.Run("shot download error retries", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloadErr = errors.New("sink down")
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-dlerr", video.StateAdmitted)
		mission95Stored(t, store, job)
		q := queue.New(10)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		o.Queue = q
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "download shot") {
			t.Fatalf("download error=%v", err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for q.Len() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if q.Len() != 1 {
			t.Fatalf("download retry queued=%d, want 1", q.Len())
		}
	})
	t.Run("shot download save error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloadErr = errors.New("sink down")
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 5}
		job := mission95EpisodeJob("ep-dlsave", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("download save error=%v", err)
		}
	})
	t.Run("shot download count", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		clip := mission95ClipAsset(t, clipDir)
		p.downloaded = []video.Asset{clip, clip}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-count", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "exactly one video asset") {
			t.Fatalf("download count error=%v", err)
		}
	})
	t.Run("shot download invalid", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		bad := mission95ClipAsset(t, clipDir)
		bad.SizeBytes = 0
		p.downloaded = []video.Asset{bad}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-bad", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "not durably persisted") {
			t.Fatalf("download invalid error=%v", err)
		}
	})
	t.Run("shot download sha short", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		bad := mission95ClipAsset(t, clipDir)
		bad.SHA256 = "abc"
		p.downloaded = []video.Asset{bad}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-sha", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "not durably persisted") {
			t.Fatalf("download sha error=%v", err)
		}
	})
	t.Run("shot download sha malformed", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		bad := mission95ClipAsset(t, clipDir)
		bad.SHA256 = strings.Repeat("z", 64)
		p.downloaded = []video.Asset{bad}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-mal", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "checksum is malformed") {
			t.Fatalf("download malformed error=%v", err)
		}
	})
	t.Run("shot download oversize", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloaded = []video.Asset{mission95ClipAsset(t, clipDir)}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-big", video.StateAdmitted)
		mission95Stored(t, store, job)
		o := mission95EpisodeOrch(store, p, &mission95Sink{}, &mission95Composer{})
		o.MaxAssetBytes = 5
		if err := o.RunJob(ctx, job); err == nil || !strings.Contains(err.Error(), "exceeds the configured asset limit") {
			t.Fatalf("oversize error=%v", err)
		}
	})
	t.Run("shot download missing file", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		missing := mission95ClipAsset(t, clipDir)
		missing.URI = filepath.Join(t.TempDir(), "missing.mp4")
		p.downloaded = []video.Asset{missing}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-missing", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "not a valid local file") {
			t.Fatalf("missing file error=%v", err)
		}
	})
	t.Run("shot checkpoint put error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloaded = []video.Asset{mission95ClipAsset(t, clipDir)}
		sink := &mission95Sink{putErr: errors.New("sink down")}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-puterr", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, sink, nil); err == nil || !strings.Contains(err.Error(), "persist unique output for shot") {
			t.Fatalf("checkpoint put error=%v", err)
		}
	})
	t.Run("shot checkpoint put save error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloaded = []video.Asset{mission95ClipAsset(t, clipDir)}
		sink := &mission95Sink{putErr: errors.New("sink down")}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 5}
		job := mission95EpisodeJob("ep-putsave", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, sink, nil); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("checkpoint save error=%v", err)
		}
	})
	t.Run("shot checkpoint saved invalid", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloaded = []video.Asset{mission95ClipAsset(t, clipDir)}
		bad := mission95ClipAsset(t, clipDir)
		bad.SizeBytes = 999
		sink := &mission95Sink{putAsset: &bad}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-savebad", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, sink, nil); err == nil || !strings.Contains(err.Error(), "not persisted with valid metadata") {
			t.Fatalf("checkpoint invalid error=%v", err)
		}
	})
	t.Run("shot checkpoint saved sha malformed", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloaded = []video.Asset{mission95ClipAsset(t, clipDir)}
		bad := mission95ClipAsset(t, clipDir)
		bad.SHA256 = strings.Repeat("z", 64)
		sink := &mission95Sink{putAsset: &bad}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-savemal", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, sink, nil); err == nil || !strings.Contains(err.Error(), "unique checkpoint checksum is malformed") {
			t.Fatalf("checkpoint malformed error=%v", err)
		}
	})
	t.Run("shot checkpoint saved file invalid", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloaded = []video.Asset{mission95ClipAsset(t, clipDir)}
		bad := mission95ClipAsset(t, clipDir)
		bad.URI = filepath.Join(t.TempDir(), "missing.mp4")
		sink := &mission95Sink{putAsset: &bad}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-savefile", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, sink, nil); err == nil || !strings.Contains(err.Error(), "unique checkpoint file is invalid") {
			t.Fatalf("checkpoint file error=%v", err)
		}
	})
	t.Run("shot checkpoint fallbacks", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{
			completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)}),
			completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)}),
		}
		blank := mission95ClipAsset(t, clipDir)
		blank.ID, blank.Filename = "", ""
		p.downloaded = []video.Asset{blank}
		sink := &mission95Sink{}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-fallback", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, sink, &mission95Composer{}); err != nil {
			t.Fatalf("fallback run error=%v", err)
		}
		if len(sink.puts) == 0 || sink.puts[0].Filename != "shot-001-clip.mp4" || sink.puts[0].ID != "ep-fallback-shot-001" {
			t.Fatalf("fallback puts=%+v", sink.puts)
		}
		if got := mission95Get(t, store, "ep-fallback"); got.State != video.StateCompleted {
			t.Fatalf("fallback final state=%s", got.State)
		}
	})
	t.Run("completed shot persist error", func(t *testing.T) {
		p := providerWithDefaults()
		p.statuses = []video.ProviderJobStatus{completedStatus("remote-1", []video.Asset{mission95ClipAsset(t, clipDir)})}
		p.downloaded = []video.Asset{mission95ClipAsset(t, clipDir)}
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 5}
		job := mission95EpisodeJob("ep-compdone", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, p, job, nil, nil); err == nil || !strings.Contains(err.Error(), "persist completed shot") {
			t.Fatalf("completed persist error=%v", err)
		}
	})
	t.Run("post loop transition error", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95CheckpointJob("ep-posttrans", video.StateAdmitted)
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil {
			t.Fatal("post-loop transition error accepted")
		}
	})
	t.Run("post loop persist error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := mission95CheckpointJob("ep-postperr", video.StateComposing)
		mission95Stored(t, store, job)
		if err := run(t, store, providerWithDefaults(), job, nil, nil); err == nil || !strings.Contains(err.Error(), "persist episode composition stage") {
			t.Fatalf("post-loop persist error=%v", err)
		}
	})
	t.Run("full success with default fps", func(t *testing.T) {
		base := providerWithDefaults()
		base.statuses = []video.ProviderJobStatus{
			{ProviderJobID: "remote-1", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-1", Filename: "one.mp4", ContentType: "video/mp4"}}},
			{ProviderJobID: "remote-2", State: video.StateCompleted, Progress: 1, Outputs: []video.Asset{{ID: "out-2", Filename: "two.mp4", ContentType: "video/mp4"}}},
		}
		provider := &episodeTestProvider{fakeProvider: base}
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ep-full", video.StateAdmitted)
		mission95Stored(t, store, job)
		comp := &episodeTestComposer{}
		sink := &episodeTestSink{root: filepath.Join(t.TempDir(), "assets")}
		o := newEpisodeOrchestrator(t, store, queue.New(10), provider, sink, comp)
		if err := o.RunJob(ctx, job); err != nil {
			t.Fatalf("full episode error=%v", err)
		}
		got := mission95Get(t, store, "ep-full")
		if got.State != video.StateCompleted || len(got.OutputAssets) != 1 {
			t.Fatalf("full episode state=%s assets=%d", got.State, len(got.OutputAssets))
		}
		if len(comp.specs) != 1 || comp.specs[0].FPS != 24 {
			t.Fatalf("composer specs=%+v, want FPS 24 default", comp.specs)
		}
	})
}

func TestMission95ComposeEpisodeDirectBranches(t *testing.T) {
	ctx := context.Background()
	composeJob := func(id string, state video.JobState) (video.VideoJob, *mission95Sink, *mission95Composer) {
		job := mission95CheckpointJob(id, state)
		return job, &mission95Sink{}, &mission95Composer{}
	}
	t.Run("missing shot asset", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ce-missing", video.StateComposing)
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Composer: &mission95Composer{}}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "missing a persisted shot asset") {
			t.Fatalf("compose missing error=%v", err)
		}
	})
	t.Run("fewer than two clips", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := mission95EpisodeJob("ce-one", video.StateComposing)
		job.Request.ShotPlan = job.Request.ShotPlan[:1]
		asset := mission95CheckpointAsset()
		job.Request.ShotPlan[0].OutputAsset = &asset
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Composer: &mission95Composer{}}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "at least two persisted shot assets") {
			t.Fatalf("compose one-clip error=%v", err)
		}
	})
	t.Run("workspace creation failure", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", blocker)
		store := queue.NewMemoryStore()
		job, sink, comp := composeJob("ce-tmp", video.StateComposing)
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "could not create episode composition workspace") {
			t.Fatalf("compose workspace error=%v", err)
		}
		if got := mission95Get(t, store, "ce-tmp"); got.State != video.StateFailed {
			t.Fatalf("workspace failure state=%s", got.State)
		}
	})
	t.Run("composer failure retries then fails", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, sink, comp := composeJob("ce-retry", video.StateComposing)
		comp.err = errors.New("ffmpeg down")
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "compose episode") {
			t.Fatalf("compose retry error=%v", err)
		}
		if got := mission95Get(t, store, "ce-retry"); got.CompositionRetryCount != 1 || got.State != video.StateComposing {
			t.Fatalf("compose retry job=%+v", got)
		}
		job2, sink2, comp2 := composeJob("ce-retry3", video.StateComposing)
		job2.CompositionRetryCount = 2
		comp2.err = errors.New("ffmpeg down")
		mission95Stored(t, store, job2)
		o2 := &Orchestrator{Store: store, Assets: sink2, Composer: comp2, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if err := o2.composeEpisode(ctx, &job2); err == nil || !strings.Contains(err.Error(), "failed after repeated attempts") {
			t.Fatalf("compose retry3 error=%v", err)
		}
		if got := mission95Get(t, store, "ce-retry3"); got.State != video.StateFailed {
			t.Fatalf("compose retry3 state=%s", got.State)
		}
	})
	t.Run("composer failure save error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job, sink, comp := composeJob("ce-saveerr", video.StateComposing)
		comp.err = errors.New("ffmpeg down")
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("compose save error=%v", err)
		}
	})
	t.Run("composer success without file", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, sink, comp := composeJob("ce-nofile", video.StateComposing)
		comp.noFile = true
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "without an output file") {
			t.Fatalf("compose nofile error=%v", err)
		}
	})
	t.Run("composer empty output", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, sink, comp := composeJob("ce-empty", video.StateComposing)
		comp.empty = true
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "empty or exceeds") {
			t.Fatalf("compose empty error=%v", err)
		}
	})
	t.Run("composer output is a directory", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, sink, comp := composeJob("ce-dir", video.StateComposing)
		comp.dirOut = true
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		// A directory output is rejected fail-closed. Which check fires
		// first is filesystem-dependent: filesystems that report a fresh
		// directory size of 0 trip the empty-output check, while others
		// reach the ReadFile check. Either rejection plus a Failed job
		// is the asserted behavior.
		err := o.composeEpisode(ctx, &job)
		if err == nil || (!strings.Contains(err.Error(), "could not read composed episode output") && !strings.Contains(err.Error(), "empty or exceeds")) {
			t.Fatalf("compose dir error=%v", err)
		}
		if got := mission95Get(t, store, "ce-dir"); got.State != video.StateFailed {
			t.Fatalf("compose dir state=%s, want failed", got.State)
		}
	})
	t.Run("final put error", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, _, comp := composeJob("ce-puterr", video.StateComposing)
		sink := &mission95Sink{putErr: errors.New("sink down")}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "persist final episode") {
			t.Fatalf("final put error=%v", err)
		}
	})
	t.Run("final put save error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job, _, comp := composeJob("ce-putsave", video.StateComposing)
		sink := &mission95Sink{putErr: errors.New("sink down")}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "could not be saved") {
			t.Fatalf("final put save error=%v", err)
		}
	})
	t.Run("final saved invalid", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, _, comp := composeJob("ce-savebad", video.StateComposing)
		bad := video.Asset{URI: "x", ContentType: "video/mp4", SizeBytes: 0, SHA256: ""}
		sink := &mission95Sink{putAsset: &bad}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "not persisted with valid") {
			t.Fatalf("final saved invalid error=%v", err)
		}
	})
	t.Run("final saved sha malformed", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, _, comp := composeJob("ce-savemal", video.StateComposing)
		bad := video.Asset{URI: "x", ContentType: "video/mp4", SizeBytes: 22, SHA256: strings.Repeat("z", 64)}
		sink := &mission95Sink{putAsset: &bad}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "checksum is malformed") {
			t.Fatalf("final saved malformed error=%v", err)
		}
	})
	t.Run("reload get error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getErrAt: 1}
		job, sink, comp := composeJob("ce-geterr", video.StateComposing)
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "reload episode before completion commit") {
			t.Fatalf("reload get error=%v", err)
		}
	})
	t.Run("reload terminal is noop", func(t *testing.T) {
		job, sink, comp := composeJob("ce-term", video.StateComposing)
		terminal := job
		terminal.State = video.StateCompleted
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getSwapAt: 1, getSwap: terminal}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err != nil {
			t.Fatalf("reload terminal error=%v", err)
		}
		if len(job.OutputAssets) != 0 {
			t.Fatalf("reload terminal assets=%d, want 0", len(job.OutputAssets))
		}
	})
	t.Run("settle error", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, sink, comp := composeJob("ce-settle", video.StateComposing)
		mission95Stored(t, store, job)
		ledger := &mission95Ledger{settleEstimateErr: errors.New("ledger down")}
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond, Ledger: ledger}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "cost settlement failed") {
			t.Fatalf("settle error=%v", err)
		}
		if got := mission95Get(t, store, "ce-settle"); !strings.Contains(got.LastError, "settlement failed") {
			t.Fatalf("settle last error=%q", got.LastError)
		}
	})
	t.Run("settle actual success", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job, sink, comp := composeJob("ce-settleok", video.StateComposing)
		actual := 1.0
		for i := range job.ShotExecutions {
			job.ShotExecutions[i].ActualCostUSD = &actual
		}
		mission95Stored(t, store, job)
		ledger := &mission95Ledger{}
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20, Ledger: ledger}
		if err := o.composeEpisode(ctx, &job); err != nil {
			t.Fatalf("settle actual error=%v", err)
		}
		if ledger.settleActualCalls != 1 {
			t.Fatalf("settle actual calls=%d, want 1", ledger.settleActualCalls)
		}
		if got := mission95Get(t, store, "ce-settleok"); got.State != video.StateCompleted || got.ActualCostUSD != 2.0 {
			t.Fatalf("settle actual job state=%s actual=%v", got.State, got.ActualCostUSD)
		}
	})
	t.Run("completion transition error", func(t *testing.T) {
		job, sink, comp := composeJob("ce-trans", video.StateComposing)
		queued := job
		queued.State = video.StateQueued
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), getSwapAt: 1, getSwap: queued}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil {
			t.Fatal("completion transition error accepted")
		}
	})
	t.Run("final update error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job, sink, comp := composeJob("ce-finerr", video.StateComposing)
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: sink, Composer: comp, MaxAssetBytes: 1 << 20}
		if err := o.composeEpisode(ctx, &job); err == nil || !strings.Contains(err.Error(), "completion state could not be saved") {
			t.Fatalf("final update error=%v", err)
		}
	})
}

func TestMission95NarrationAndSubtitleBranches(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	narrJob := func(id string) video.VideoJob {
		job := mission95EpisodeJob(id, video.StateComposing)
		job.Request.DurationSeconds = 4
		job.Request.NarrationText = "hello world"
		return job
	}
	t.Run("no narration text", func(t *testing.T) {
		job := mission95EpisodeJob("n-none", video.StateComposing)
		o := &Orchestrator{Store: queue.NewMemoryStore()}
		path, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav"))
		if err != nil || path != "" {
			t.Fatalf("no-text narration path=%q err=%v", path, err)
		}
	})
	t.Run("narrator unavailable", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-nonarr")
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "narrator is unavailable") {
			t.Fatalf("narrator unavailable error=%v", err)
		}
	})
	t.Run("invalid checkpoint removed then narrator unavailable", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-invckpt")
		bad := video.Asset{ID: "bad", URI: filepath.Join(dir, "missing.wav"), ContentType: "audio/wav", SizeBytes: 100, SHA256: strings.Repeat("a", 64)}
		job.NarrationAsset = &bad
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "narrator is unavailable") {
			t.Fatalf("invalid checkpoint error=%v", err)
		}
		if job.NarrationAsset != nil {
			t.Fatal("invalid narration checkpoint was not cleared")
		}
	})
	t.Run("invalid checkpoint removal persist error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := narrJob("n-remperr")
		bad := video.Asset{ID: "bad", URI: filepath.Join(dir, "missing.wav"), ContentType: "audio/wav", SizeBytes: 100, SHA256: strings.Repeat("a", 64)}
		job.NarrationAsset = &bad
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "persist invalid narration checkpoint removal") {
			t.Fatalf("removal persist error=%v", err)
		}
	})
	t.Run("valid checkpoint short", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-valid")
		wav := mission95WAV(t, dir, 1)
		job.NarrationAsset = &wav
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, MaxAssetBytes: 1 << 20}
		path, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav"))
		if err != nil || path != wav.URI {
			t.Fatalf("valid checkpoint path=%q err=%v", path, err)
		}
	})
	t.Run("valid checkpoint too long fails", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-long")
		wav := mission95WAV(t, dir, 6)
		job.NarrationAsset = &wav
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "narration exceeds episode duration") {
			t.Fatalf("long narration error=%v", err)
		}
		if got := mission95Get(t, store, "n-long"); got.State != video.StateFailed {
			t.Fatalf("long narration state=%s", got.State)
		}
	})
	t.Run("recovered checkpoint persists", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-rec")
		wav := mission95WAV(t, dir, 1)
		wav.ID = job.JobID + "-narration"
		job.SourceAssets = []video.Asset{wav}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, MaxAssetBytes: 1 << 20}
		path, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav"))
		if err != nil || path != wav.URI {
			t.Fatalf("recovered path=%q err=%v", path, err)
		}
		if got := mission95Get(t, store, "n-rec"); got.NarrationAsset == nil {
			t.Fatal("recovered narration checkpoint was not persisted")
		}
	})
	t.Run("recovered checkpoint persist error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := narrJob("n-recerr")
		wav := mission95WAV(t, dir, 1)
		wav.ID = job.JobID + "-narration"
		job.SourceAssets = []video.Asset{wav}
		mission95Stored(t, store, job)
		o := &Orchestrator{Store: store, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "persist recovered narration checkpoint") {
			t.Fatalf("recovered persist error=%v", err)
		}
	})
	t.Run("narrator synthesizes valid wav", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-synth")
		mission95Stored(t, store, job)
		wavBytes := func() []byte {
			asset := mission95WAV(t, dir, 1)
			data, err := os.ReadFile(asset.URI)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}()
		narrator := &mission95Narrator{write: func(path string) error {
			return os.WriteFile(path, wavBytes, 0600)
		}}
		sink := &mission95Sink{}
		o := &Orchestrator{Store: store, Assets: sink, Narrator: narrator, MaxAssetBytes: 1 << 20}
		path, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "synth.wav"))
		if err != nil || path == "" {
			t.Fatalf("synth path=%q err=%v", path, err)
		}
		if got := mission95Get(t, store, "n-synth"); got.NarrationAsset == nil || got.NarrationAsset.ID != "n-synth-narration" {
			t.Fatalf("synth asset=%+v", got.NarrationAsset)
		}
	})
	t.Run("narrator error retries", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-syntherr")
		mission95Stored(t, store, job)
		narrator := &mission95Narrator{err: errors.New("piper down")}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Narrator: narrator, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "synthesize narration") {
			t.Fatalf("synth error=%v", err)
		}
		if got := mission95Get(t, store, "n-syntherr"); got.CompositionRetryCount != 1 {
			t.Fatalf("synth retry count=%d", got.CompositionRetryCount)
		}
	})
	t.Run("narrator invalid outputs", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"tiny":    []byte("short"),
			"garbage": []byte("not a wav file!!"),
		} {
			store := queue.NewMemoryStore()
			job := narrJob("n-inv-" + name)
			mission95Stored(t, store, job)
			narrator := &mission95Narrator{write: func(path string) error {
				return os.WriteFile(path, data, 0600)
			}}
			o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Narrator: narrator, MaxAssetBytes: 1 << 20}
			if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil {
				t.Fatalf("%s narrator output accepted", name)
			}
		}
	})
	t.Run("narration put error retries", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-puterr")
		mission95Stored(t, store, job)
		wavBytes, err := os.ReadFile(mission95WAV(t, dir, 1).URI)
		if err != nil {
			t.Fatal(err)
		}
		narrator := &mission95Narrator{write: func(path string) error {
			return os.WriteFile(path, wavBytes, 0600)
		}}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{putErr: errors.New("sink down")}, Narrator: narrator, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "narration persistence failed") {
			t.Fatalf("narration put error=%v", err)
		}
	})
	t.Run("narration saved invalid", func(t *testing.T) {
		store := queue.NewMemoryStore()
		job := narrJob("n-savebad")
		mission95Stored(t, store, job)
		wavBytes, err := os.ReadFile(mission95WAV(t, dir, 1).URI)
		if err != nil {
			t.Fatal(err)
		}
		narrator := &mission95Narrator{write: func(path string) error {
			return os.WriteFile(path, wavBytes, 0600)
		}}
		bad := video.Asset{URI: "x", ContentType: "audio/wav", SizeBytes: 0, SHA256: ""}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{putAsset: &bad}, Narrator: narrator, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "not persisted with valid") {
			t.Fatalf("narration saved invalid error=%v", err)
		}
	})
	t.Run("narration saved persist error", func(t *testing.T) {
		store := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job := narrJob("n-saveperr")
		mission95Stored(t, store, job)
		wavBytes, err := os.ReadFile(mission95WAV(t, dir, 1).URI)
		if err != nil {
			t.Fatal(err)
		}
		narrator := &mission95Narrator{write: func(path string) error {
			return os.WriteFile(path, wavBytes, 0600)
		}}
		o := &Orchestrator{Store: store, Assets: &mission95Sink{}, Narrator: narrator, MaxAssetBytes: 1 << 20}
		if _, err := o.ensureNarrationAsset(ctx, &job, filepath.Join(dir, "out.wav")); err == nil || !strings.Contains(err.Error(), "checkpoint state could not be saved") {
			t.Fatalf("narration save persist error=%v", err)
		}
	})
	t.Run("subtitle branches", func(t *testing.T) {
		// No language: no subtitles, no error.
		job := mission95EpisodeJob("s-none", video.StateComposing)
		o := &Orchestrator{Store: queue.NewMemoryStore()}
		if path, err := o.ensureSubtitleAsset(ctx, &job); err != nil || path != "" {
			t.Fatalf("no-language subtitle path=%q err=%v", path, err)
		}
		// Language without narration text (job must exist: the fail path
		// persists the failure state).
		subStore := queue.NewMemoryStore()
		job = mission95EpisodeJob("s-nonarr", video.StateComposing)
		job.Request.SubtitleLanguage = "en"
		mission95Stored(t, subStore, job)
		oSub := &Orchestrator{Store: subStore, MaxAssetBytes: 1 << 20}
		if _, err := oSub.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "captions require narration text") {
			t.Fatalf("subtitle no-text error=%v", err)
		}
		// Language with narration text but no narration checkpoint.
		subStore2 := queue.NewMemoryStore()
		job = narrJob("s-noasset")
		job.Request.SubtitleLanguage = "en"
		mission95Stored(t, subStore2, job)
		oSub2 := &Orchestrator{Store: subStore2, MaxAssetBytes: 1 << 20}
		if _, err := oSub2.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "persisted narration WAV checkpoint") {
			t.Fatalf("subtitle no-asset error=%v", err)
		}
		// Valid subtitle checkpoint returns its URI without persisting.
		store := queue.NewMemoryStore()
		job = narrJob("s-valid")
		job.Request.SubtitleLanguage = "en"
		wav := mission95WAV(t, dir, 1)
		job.NarrationAsset = &wav
		srt := mission95SRTAsset(t, dir)
		job.SubtitleAsset = &srt
		mission95Stored(t, store, job)
		o2 := &Orchestrator{Store: store, MaxAssetBytes: 1 << 20}
		if path, err := o2.ensureSubtitleAsset(ctx, &job); err != nil || path != srt.URI {
			t.Fatalf("valid subtitle path=%q err=%v", path, err)
		}
		// Recovered subtitle checkpoint persists.
		store2 := queue.NewMemoryStore()
		job = narrJob("s-rec")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		recovered := mission95SRTAsset(t, dir)
		recovered.ID = job.JobID + "-subtitles"
		job.SourceAssets = []video.Asset{recovered}
		mission95Stored(t, store2, job)
		o3 := &Orchestrator{Store: store2, MaxAssetBytes: 1 << 20}
		if path, err := o3.ensureSubtitleAsset(ctx, &job); err != nil || path != recovered.URI {
			t.Fatalf("recovered subtitle path=%q err=%v", path, err)
		}
		if got := mission95Get(t, store2, "s-rec"); got.SubtitleAsset == nil {
			t.Fatal("recovered subtitle checkpoint was not persisted")
		}
		// Recovered subtitle persist error (the recovered asset id must
		// match this job's id, or the recovery lookup misses).
		store3 := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job = narrJob("s-recerr")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		recoveredErr := recovered
		recoveredErr.ID = job.JobID + "-subtitles"
		job.SourceAssets = []video.Asset{recoveredErr}
		mission95Stored(t, store3, job)
		o4 := &Orchestrator{Store: store3, MaxAssetBytes: 1 << 20}
		if _, err := o4.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "persist recovered subtitle checkpoint") {
			t.Fatalf("recovered subtitle persist error=%v", err)
		}
		// Invalid subtitle checkpoint is removed and regenerated.
		store4 := queue.NewMemoryStore()
		job = narrJob("s-regen")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		bad := video.Asset{ID: "bad", URI: filepath.Join(dir, "missing.srt"), ContentType: "application/x-subrip", SizeBytes: 50, SHA256: strings.Repeat("a", 64)}
		job.SubtitleAsset = &bad
		mission95Stored(t, store4, job)
		o5 := &Orchestrator{Store: store4, Assets: &mission95Sink{}, MaxAssetBytes: 1 << 20}
		if path, err := o5.ensureSubtitleAsset(ctx, &job); err != nil || path == "" {
			t.Fatalf("regenerated subtitle path=%q err=%v", path, err)
		}
		if got := mission95Get(t, store4, "s-regen"); got.SubtitleAsset == nil || got.SubtitleAsset.ID != "s-regen-subtitles" {
			t.Fatalf("regenerated subtitle asset=%+v", got.SubtitleAsset)
		}
		// Invalid subtitle removal persist error.
		store5 := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job = narrJob("s-remerr")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		job.SubtitleAsset = &bad
		mission95Stored(t, store5, job)
		o6 := &Orchestrator{Store: store5, Assets: &mission95Sink{}, MaxAssetBytes: 1 << 20}
		if _, err := o6.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "persist invalid subtitle checkpoint removal") {
			t.Fatalf("subtitle removal persist error=%v", err)
		}
		// Oversized SRT generation fails before any sink use.
		storeBig := queue.NewMemoryStore()
		job = narrJob("s-big")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		mission95Stored(t, storeBig, job)
		o7 := &Orchestrator{Store: storeBig, MaxAssetBytes: 10}
		if _, err := o7.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "empty or oversized SRT") {
			t.Fatalf("oversized SRT error=%v", err)
		}
		// No asset sink after SRT generation.
		storeNoSink := queue.NewMemoryStore()
		job = narrJob("s-nosink")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		mission95Stored(t, storeNoSink, job)
		o8 := &Orchestrator{Store: storeNoSink, MaxAssetBytes: 1 << 20}
		if _, err := o8.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "no asset sink is configured") {
			t.Fatalf("subtitle no-sink error=%v", err)
		}
		// Subtitle put error retries.
		store6 := queue.NewMemoryStore()
		job = narrJob("s-puterr")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		mission95Stored(t, store6, job)
		o9 := &Orchestrator{Store: store6, Assets: &mission95Sink{putErr: errors.New("sink down")}, MaxAssetBytes: 1 << 20, PollInterval: time.Nanosecond}
		if _, err := o9.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "subtitle persistence failed") {
			t.Fatalf("subtitle put error=%v", err)
		}
		// Subtitle saved invalid.
		store7 := queue.NewMemoryStore()
		job = narrJob("s-savebad")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		mission95Stored(t, store7, job)
		badSaved := video.Asset{URI: "x", ContentType: "application/x-subrip", SizeBytes: 0, SHA256: ""}
		o10 := &Orchestrator{Store: store7, Assets: &mission95Sink{putAsset: &badSaved}, MaxAssetBytes: 1 << 20}
		if _, err := o10.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "not persisted with valid") {
			t.Fatalf("subtitle saved invalid error=%v", err)
		}
		// Subtitle saved persist error.
		store8 := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
		job = narrJob("s-saveperr")
		job.Request.SubtitleLanguage = "en"
		job.NarrationAsset = &wav
		mission95Stored(t, store8, job)
		o11 := &Orchestrator{Store: store8, Assets: &mission95Sink{}, MaxAssetBytes: 1 << 20}
		if _, err := o11.ensureSubtitleAsset(ctx, &job); err == nil || !strings.Contains(err.Error(), "checkpoint state could not be saved") {
			t.Fatalf("subtitle save persist error=%v", err)
		}
	})
}

func TestMission95EpisodeHelpers(t *testing.T) {
	// Cost reconciliation: summed actuals, missing actuals, empty shots.
	job := mission95EpisodeJob("h-cost", video.StateComposing)
	one, two := 1.0, 2.5
	job.ShotExecutions[0].ActualCostUSD = &one
	job.ShotExecutions[1].ActualCostUSD = &two
	applyEpisodeCostReconciliation(&job)
	if !job.CostReconciled || job.ActualCostUSD != 3.5 || job.CostBasis != "actual" {
		t.Fatalf("summed reconciliation=%+v", job)
	}
	job.ShotExecutions[1].ActualCostUSD = nil
	job.ShotExecutions[1].UsageMetadata = map[string]string{"actual_cost_usd": "bad"}
	applyEpisodeCostReconciliation(&job)
	if job.CostReconciled || job.CostBasis != "estimate" {
		t.Fatalf("unreconciled job=%+v", job)
	}
	job.ShotExecutions[1].UsageMetadata = map[string]string{"actual_cost_usd": "2.5"}
	applyEpisodeCostReconciliation(&job)
	if !job.CostReconciled || job.ActualCostUSD != 3.5 {
		t.Fatalf("legacy usage reconciliation=%+v", job)
	}
	empty := mission95EpisodeJob("h-empty", video.StateComposing)
	empty.ShotExecutions = nil
	applyEpisodeCostReconciliation(&empty)
	if empty.CostReconciled {
		t.Fatal("empty shots reconciled")
	}
	// Dimensions, limits, intervals.
	req := episodeRequest()
	if w, h := episodeDimensions(req); w != 1280 || h != 720 {
		t.Fatalf("default dims=%dx%d", w, h)
	}
	req.Width, req.Height = 640, 360
	if w, h := episodeDimensions(req); w != 640 || h != 360 {
		t.Fatalf("explicit dims=%dx%d", w, h)
	}
	req.Width, req.Height = 0, 0
	req.AspectRatio = video.Aspect9x16
	if w, h := episodeDimensions(req); w != 720 || h != 1280 {
		t.Fatalf("9:16 dims=%dx%d", w, h)
	}
	req.AspectRatio = video.Aspect1x1
	if w, h := episodeDimensions(req); w != 1024 || h != 1024 {
		t.Fatalf("1:1 dims=%dx%d", w, h)
	}
	if got := (&Orchestrator{}).maxAssetBytes(); got != 512<<20 {
		t.Fatalf("default max bytes=%d", got)
	}
	if got := (&Orchestrator{MaxAssetBytes: 42}).maxAssetBytes(); got != 42 {
		t.Fatalf("explicit max bytes=%d", got)
	}
	if got := (&Orchestrator{}).pollInterval(); got != 100*time.Millisecond {
		t.Fatalf("default poll interval=%v", got)
	}
	if got := (&Orchestrator{PollInterval: time.Second}).pollInterval(); got != time.Second {
		t.Fatalf("explicit poll interval=%v", got)
	}
	// Source-asset helpers: find, dedupe, remove.
	j := mission95EpisodeJob("h-src", video.StateComposing)
	asset := video.Asset{ID: "a1", URI: "/x/a.mp4"}
	if found := findEpisodeSourceAsset(j.SourceAssets, "a1"); found != nil {
		t.Fatal("found asset in empty list")
	}
	addSourceAsset(&j, asset)
	addSourceAsset(&j, asset)
	if len(j.SourceAssets) != 1 {
		t.Fatalf("dedupe sources=%d", len(j.SourceAssets))
	}
	addSourceAsset(&j, video.Asset{ID: "a1", URI: "/y/a.mp4"})
	if len(j.SourceAssets) != 2 {
		t.Fatalf("distinct URI sources=%d", len(j.SourceAssets))
	}
	if found := findEpisodeSourceAsset(j.SourceAssets, "a1"); found == nil || found.URI != "/x/a.mp4" {
		t.Fatalf("find source=%+v", found)
	}
	removeEpisodeSourceAsset(&j, "a1")
	if len(j.SourceAssets) != 0 {
		t.Fatalf("remove sources=%d", len(j.SourceAssets))
	}
	// Narration duration guard.
	if err := checkNarrationDuration(video.VideoJob{Request: video.VideoRequest{DurationSeconds: 4}}, 4*time.Second); err != nil {
		t.Fatalf("within-limit duration error=%v", err)
	}
	if err := checkNarrationDuration(video.VideoJob{Request: video.VideoRequest{DurationSeconds: 4}}, 5*time.Second); err == nil {
		t.Fatal("over-limit duration accepted")
	}
	if err := checkNarrationDuration(video.VideoJob{}, 10*time.Second); err != nil {
		t.Fatalf("unbounded duration error=%v", err)
	}
	// Retry helper: below threshold wraps, at threshold fails, save
	// failure combines, nil cause yields the bare message.
	store := queue.NewMemoryStore()
	rj := mission95EpisodeJob("h-retry", video.StateComposing)
	mission95Stored(t, store, rj)
	o := &Orchestrator{Store: store, PollInterval: time.Nanosecond}
	if err := o.retryEpisodeComposition(context.Background(), &rj, "try again", errors.New("boom")); err == nil || !strings.Contains(err.Error(), "try again: boom") {
		t.Fatalf("retry error=%v", err)
	}
	if rj.CompositionRetryCount != 1 {
		t.Fatalf("retry count=%d", rj.CompositionRetryCount)
	}
	rj.CompositionRetryCount = 2
	if err := o.retryEpisodeComposition(context.Background(), &rj, "try again", errors.New("boom")); err == nil || !strings.Contains(err.Error(), "failed after repeated attempts") {
		t.Fatalf("retry threshold error=%v", err)
	}
	if got := mission95Get(t, store, "h-retry"); got.State != video.StateFailed {
		t.Fatalf("retry threshold state=%s", got.State)
	}
	failStore := &mission95Store{MemoryStore: queue.NewMemoryStore(), updateErrAt: 1}
	rj2 := mission95EpisodeJob("h-retryerr", video.StateComposing)
	mission95Stored(t, failStore, rj2)
	o2 := &Orchestrator{Store: failStore, PollInterval: time.Nanosecond}
	if err := o2.retryEpisodeComposition(context.Background(), &rj2, "try again", errors.New("boom")); err == nil || !strings.Contains(err.Error(), "could not be saved") {
		t.Fatalf("retry save error=%v", err)
	}
	rj3 := mission95EpisodeJob("h-retrynil", video.StateComposing)
	mission95Stored(t, store, rj3)
	if err := o.retryEpisodeComposition(context.Background(), &rj3, "bare message", nil); err == nil || err.Error() != "bare message" {
		t.Fatalf("retry nil-cause error=%v", err)
	}
	// Verify helpers: valid and invalid narration/subtitle assets.
	dir := t.TempDir()
	wav := mission95WAV(t, dir, 1)
	if err := verifyNarrationAsset(wav, 1<<20); err != nil {
		t.Fatalf("valid narration verify=%v", err)
	}
	for name, mutate := range map[string]func(*video.Asset){
		"empty uri": func(a *video.Asset) { a.URI = "" },
		"bad type":  func(a *video.Asset) { a.ContentType = "video/mp4" },
		"small":     func(a *video.Asset) { a.SizeBytes = 5 },
		"bad sha":   func(a *video.Asset) { a.SHA256 = "xyz" },
		"missing":   func(a *video.Asset) { a.URI = filepath.Join(dir, "missing.wav") },
	} {
		bad := wav
		mutate(&bad)
		if err := verifyNarrationAsset(bad, 1<<20); err == nil {
			t.Fatalf("narration %s accepted", name)
		}
	}
	// Same size, different bytes: size passes, checksum fails.
	orig, err := os.ReadFile(wav.URI)
	if err != nil {
		t.Fatal(err)
	}
	tampered := make([]byte, len(orig))
	copy(tampered, orig)
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(wav.URI, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyNarrationAsset(wav, 1<<20); err == nil {
		t.Fatal("tampered narration accepted")
	}
	srt := mission95SRTAsset(t, dir)
	if err := verifySubtitleAsset(srt, 1<<20); err != nil {
		t.Fatalf("valid subtitle verify=%v", err)
	}
	badSrt := srt
	badSrt.ContentType = "text/plain"
	if err := verifySubtitleAsset(badSrt, 1<<20); err == nil {
		t.Fatal("wrong-type subtitle accepted")
	}
	if err := verifySubtitleAsset(srt, 5); err == nil {
		t.Fatal("oversize subtitle accepted")
	}
}
