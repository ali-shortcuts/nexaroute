package providers_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
)

const planJSON = `{"schema_version":"video-plan.v1","title":"Provider test","duration_seconds":3,"width":320,"height":240,"fps":12,"scenes":[{"id":"a","duration_seconds":1.5,"background":"#101828","elements":[{"type":"text","text":"Planned by a text model","x":0.5,"y":0.4,"font_size":36,"color":"#ffffff","animation":"fade"}]},{"id":"b","duration_seconds":1.5,"background":"#ffffff","elements":[{"type":"chart","data":[1,4,2],"x":0.15,"y":0.25,"width":0.7,"height":0.45,"color":"#2e90fa"}]}]}`

type fakePlanner struct{ out string }

func (f fakePlanner) Complete(context.Context, string, string, string) (string, error) {
	return f.out, nil
}

func requireRenderStack(t *testing.T) *render.Renderer {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	r := &render.Renderer{}
	if err := r.Preflight(context.Background()); err != nil {
		t.Skipf("render stack unavailable: %v", err)
	}
	return r
}

func newProvider(t *testing.T, withAgent bool) *providers.CodeRender {
	t.Helper()
	var a *agent.Agent
	if withAgent {
		a = &agent.Agent{Planner: fakePlanner{out: planJSON}, MaxAttempts: 2}
	}
	p := providers.NewCodeRender(a, requireRenderStack(t), nil, t.TempDir(), []string{"deepseek-chat", "claude-sonnet"}, 120)
	p.MaxAssetBytes = 512 << 20
	return p
}

func waitCompleted(t *testing.T, p *providers.CodeRender, id string) video.ProviderJobStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == video.StateCompleted || st.State == video.StateFailed || st.State == video.StateCancelled {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("job did not reach a terminal state")
	return video.ProviderJobStatus{}
}

// TestCodeRenderEndToEndWithTextModel is the core acceptance proof: a
// text-only model (scripted planner) authors a plan, the deterministic
// renderer produces a real MP4, QC passes, and the asset sink persists it
// with checksum metadata.
func TestCodeRenderEndToEndWithTextModel(t *testing.T) {
	p := newProvider(t, true)
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "Explain routing in two scenes", Mode: video.ModeTextToVideo,
		ProviderPreference: providers.CodeRenderID, ModelPreference: "deepseek-chat",
		DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12, CreationMode: "programmatic",
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	st := waitCompleted(t, p, job.ProviderJobID)
	if st.State != video.StateCompleted {
		t.Fatalf("state = %s, err = %s", st.State, st.Error)
	}
	if st.Usage["native_video"] != "false" || st.Usage["planning_model"] != "deepseek-chat" {
		t.Fatalf("usage metadata must be honest about the mode: %v", st.Usage)
	}
	sink, err := storage.NewLocal(t.TempDir(), 512<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the orchestrator, which stamps ownership metadata onto the
	// status outputs before calling DownloadOutputs.
	st.Outputs[0].Metadata = map[string]string{"project_id": "test", "job_id": "job-1", "episode_id": ""}
	assets, err := p.DownloadOutputs(context.Background(), st, sink)
	if err != nil {
		t.Fatalf("DownloadOutputs: %v", err)
	}
	if len(assets) != 1 || assets[0].ContentType != "video/mp4" || assets[0].SizeBytes < 1_000 || len(assets[0].SHA256) != 64 {
		t.Fatalf("unexpected assets: %+v", assets)
	}
	// The persisted asset must be a real playable MP4.
	f, err := sink.Open(assets[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	head := make([]byte, 12)
	if _, err := f.ReadAt(head, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(head), "ftyp") {
		t.Fatalf("asset is not an MP4 (no ftyp box): %q", head)
	}
}

func TestCodeRenderExplicitPlanWithoutAgent(t *testing.T) {
	p := newProvider(t, false)
	plan, err := agent.ParsePlan(planJSON)
	if err != nil {
		t.Fatal(err)
	}
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "reviewed plan", Mode: video.ModeTextToVideo,
		ProviderPreference: providers.CodeRenderID, ModelPreference: "claude-sonnet",
		DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12, VideoPlan: &plan,
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateJob with explicit plan: %v", err)
	}
	if st := waitCompleted(t, p, job.ProviderJobID); st.State != video.StateCompleted {
		t.Fatalf("state = %s err = %s", st.State, st.Error)
	}
}

func TestCodeRenderRejectsNativeModeAndUnsupportedInputs(t *testing.T) {
	p := newProvider(t, true)
	base := video.VideoRequest{
		ProjectID: "t", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3,
		AspectRatio: video.Aspect16x9, ModelPreference: "deepseek-chat",
	}
	native := base
	native.CreationMode = "native"
	if err := p.ValidateRequest(native); err == nil || !strings.Contains(err.Error(), "native") {
		t.Fatalf("native mode must be rejected explicitly, got %v", err)
	}
	withRef := base
	withRef.ReferenceAssets = []video.Asset{{ID: "a"}}
	if err := p.ValidateRequest(withRef); err == nil {
		t.Fatal("reference assets must be rejected in the baseline")
	}
	narration := base
	narration.NarrationText = "hello"
	if err := p.ValidateRequest(narration); err == nil || !strings.Contains(err.Error(), "Piper") {
		t.Fatalf("narration without a narrator must be rejected with the Piper reason, got %v", err)
	}
	// Capabilities must never claim native video generation.
	caps, err := p.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(caps.Modes) != 1 || caps.Modes[0] != video.ModeTextToVideo || caps.SupportsWebhooks {
		t.Fatalf("capabilities overclaim: %+v", caps)
	}
	_ = os.Getenv // keep os import if unused in future edits
}

// fakeNarrator writes a valid silent WAV of the requested text length.
type fakeNarrator struct{}

func (fakeNarrator) Synthesize(_ context.Context, text, outPath string) error {
	// 0.4s of 8kHz 16-bit mono silence per call: a structurally real WAV.
	frames := 3200
	data := make([]byte, 44+frames*2)
	copy(data, "RIFF")
	data[8] = 'W'
	data[9] = 'A'
	data[10] = 'V'
	data[11] = 'E'
	copy(data[12:], "fmt ")
	data[16] = 16
	data[20] = 1 // PCM
	data[22] = 1 // mono
	data[24] = 0x40
	data[25] = 0x1f // 8000 Hz
	data[28] = 0x80
	data[29] = 0x3e // byte rate 16000
	data[32] = 2
	data[34] = 16
	copy(data[36:], "data")
	size := uint32(36 + frames*2)
	data[4] = byte(size)
	data[5] = byte(size >> 8)
	data[40] = byte(frames * 2)
	data[41] = byte((frames * 2) >> 8)
	_ = text
	return os.WriteFile(outPath, data, 0o600)
}

func TestCodeRenderNarrationAndSubtitles(t *testing.T) {
	requireRenderStack(t)
	a := &agent.Agent{Planner: fakePlanner{out: planJSON}, MaxAttempts: 1}
	p := providers.NewCodeRender(a, &render.Renderer{}, fakeNarrator{}, t.TempDir(), []string{"deepseek-chat"}, 120)
	p.MaxAssetBytes = 512 << 20
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "narrated", Mode: video.ModeTextToVideo,
		ProviderPreference: providers.CodeRenderID, ModelPreference: "deepseek-chat",
		DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12,
		NarrationText: "Routing explained simply.", SubtitleLanguage: "en",
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	st := waitCompleted(t, p, job.ProviderJobID)
	if st.State != video.StateCompleted {
		t.Fatalf("state = %s err = %s", st.State, st.Error)
	}
	if !strings.Contains(st.Usage["tools_used"], "narration-tts") || !strings.Contains(st.Usage["tools_used"], "srt-subtitles") {
		t.Fatalf("tools_used must record narration/subtitle stages: %v", st.Usage)
	}
	// The final asset must carry an audio stream now.
	sink, err := storage.NewLocal(t.TempDir(), 512<<20)
	if err != nil {
		t.Fatal(err)
	}
	st.Outputs[0].Metadata = map[string]string{"project_id": "test", "job_id": "job-9"}
	assets, err := p.DownloadOutputs(context.Background(), st, sink)
	if err != nil {
		t.Fatal(err)
	}
	f, err := sink.Open(assets[0])
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "audio") {
		t.Fatalf("narrated output must contain an audio stream, streams: %s", out)
	}
}

func TestCodeRenderPlannerFailureCreatesNoJob(t *testing.T) {
	requireRenderStack(t)
	a := &agent.Agent{Planner: fakePlanner{out: "definitely not a plan"}, MaxAttempts: 2}
	p := providers.NewCodeRender(a, &render.Renderer{}, nil, t.TempDir(), []string{"m"}, 120)
	req := video.VideoRequest{
		ProjectID: "t", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3,
		AspectRatio: video.Aspect16x9, ModelPreference: "m",
	}
	if _, err := p.CreateJob(context.Background(), req); err == nil || !strings.Contains(err.Error(), "after 2 attempt") {
		t.Fatalf("expected bounded planning failure, got %v", err)
	}
}

func TestCodeRenderCancelUnknownJob(t *testing.T) {
	p := newProvider(t, true)
	if err := p.CancelJob(context.Background(), "nope"); err == nil {
		t.Fatal("cancel of unknown job must fail")
	}
	if _, err := p.GetJob(context.Background(), "nope"); err == nil {
		t.Fatal("get of unknown job must fail")
	}
}
