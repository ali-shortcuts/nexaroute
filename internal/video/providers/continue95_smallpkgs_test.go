package providers

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

const Continue95PlanJSON = `{"schema_version":"video-plan.v1","title":"Provider test","duration_seconds":3,"width":320,"height":240,"fps":12,"scenes":[{"id":"a","duration_seconds":1.5,"background":"#101828","elements":[{"type":"text","text":"Planned by a text model","x":0.5,"y":0.4,"font_size":36,"color":"#ffffff","animation":"fade"}]},{"id":"b","duration_seconds":1.5,"background":"#ffffff","elements":[{"type":"chart","data":[1,4,2],"x":0.15,"y":0.25,"width":0.7,"height":0.45,"color":"#2e90fa"}]}]}`

type Continue95Planner struct{ out string }

func (f Continue95Planner) Complete(context.Context, string, string, string) (string, error) {
	return f.out, nil
}

func Continue95Renderer(t *testing.T) *render.Renderer {
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

func Continue95ParsePlan(t *testing.T) video.Plan {
	t.Helper()
	plan, err := agent.ParsePlan(Continue95PlanJSON)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func Continue95WaitTerminal(t *testing.T, p *CodeRender, id string) video.ProviderJobStatus {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
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

func TestContinue95CreateJobWithAgentPlan(t *testing.T) {
	r := Continue95Renderer(t)
	a := &agent.Agent{Planner: Continue95Planner{out: Continue95PlanJSON}, MaxAttempts: 2}
	p := NewCodeRender(a, r, nil, t.TempDir(), []string{"deepseek-chat"}, 120)
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "Explain routing in two scenes", Mode: video.ModeTextToVideo,
		ModelPreference: "deepseek-chat", DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12,
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateJob with agent plan: %v", err)
	}
	st := Continue95WaitTerminal(t, p, job.ProviderJobID)
	if st.State != video.StateCompleted {
		t.Fatalf("state = %s err = %s", st.State, st.Error)
	}
	if st.Usage["plan_attempts"] == "" || st.Usage["renderer"] != "nexaroute-tier-a-ffmpeg" {
		t.Errorf("usage = %v, want plan metadata", st.Usage)
	}
}

func TestContinue95CreateJobResolvePlanError(t *testing.T) {
	r := Continue95Renderer(t)
	p := NewCodeRender(nil, r, nil, t.TempDir(), []string{"deepseek-chat"}, 120)
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "no plan anywhere", Mode: video.ModeTextToVideo,
		ModelPreference: "deepseek-chat", DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12,
	}
	if _, err := p.CreateJob(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no planning model") {
		t.Errorf("CreateJob without agent or plan err = %v, want no-planning-model error", err)
	}
}

func Continue95Job(plan video.Plan) *codeRenderJob {
	ctx, cancel := context.WithCancel(context.Background())
	return &codeRenderJob{ctx: ctx, cancel: cancel, done: make(chan struct{}), state: video.StateProcessing, plan: plan, usage: map[string]string{"tools_used": "llm-planner,ffmpeg-renderer,ffprobe-qc"}}
}

func TestContinue95RunWorkRootBranches(t *testing.T) {
	r := Continue95Renderer(t)
	plan := Continue95ParsePlan(t)
	req := video.VideoRequest{ProjectID: "test", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12}
	// Empty work root.
	p := NewCodeRender(nil, r, nil, "", nil, 120)
	job := Continue95Job(plan)
	p.run(context.Background(), job, req)
	if job.state != video.StateFailed || !strings.Contains(job.err, "work root is not configured") {
		t.Errorf("empty work root: state=%s err=%q", job.state, job.err)
	}
	// Work root under a regular file: MkdirAll fails.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p = NewCodeRender(nil, r, nil, filepath.Join(blocker, "jobs"), nil, 120)
	job = Continue95Job(plan)
	p.run(context.Background(), job, req)
	if job.state != video.StateFailed || !strings.Contains(job.err, "work root") {
		t.Errorf("blocked work root: state=%s err=%q", job.state, job.err)
	}
	// Read-only work root: first MkdirTemp fails, MkdirAll succeeds
	// (the directory already exists), second MkdirTemp fails.
	roDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	p = NewCodeRender(nil, r, nil, roDir, nil, 120)
	job = Continue95Job(plan)
	p.run(context.Background(), job, req)
	if err := os.Chmod(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if job.state != video.StateFailed || !strings.Contains(job.err, "job directory") {
		t.Errorf("read-only work root: state=%s err=%q", job.state, job.err)
	}
}

// Continue95WAVNarrator writes a minimal valid PCM WAV (1s, 16kHz mono)
// so WAVDuration parses it; with lockDir it revokes write permission on
// the job directory after writing, to fail the later subtitle write.
type Continue95WAVNarrator struct{ lockDir bool }

func (n Continue95WAVNarrator) Synthesize(_ context.Context, _ string, wavPath string) error {
	if err := Continue95WriteWAV(wavPath, 1); err != nil {
		return err
	}
	if n.lockDir {
		return os.Chmod(filepath.Dir(wavPath), 0o555)
	}
	return nil
}

func Continue95WriteWAV(path string, seconds int) error {
	const sampleRate = 16000
	dataBytes := seconds * sampleRate * 2
	buf := make([]byte, 0, 44+dataBytes)
	buf = append(buf, []byte("RIFF")...)
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(36+dataBytes))
	buf = append(buf, size...)
	buf = append(buf, []byte("WAVEfmt ")...)
	binary.LittleEndian.PutUint32(size, 16)
	buf = append(buf, size...)
	tmp := make([]byte, 2)
	binary.LittleEndian.PutUint16(tmp, 1)
	buf = append(buf, tmp...)
	binary.LittleEndian.PutUint16(tmp, 1)
	buf = append(buf, tmp...)
	tmp4 := make([]byte, 4)
	binary.LittleEndian.PutUint32(tmp4, sampleRate)
	buf = append(buf, tmp4...)
	binary.LittleEndian.PutUint32(tmp4, sampleRate*2)
	buf = append(buf, tmp4...)
	binary.LittleEndian.PutUint16(tmp, 2)
	buf = append(buf, tmp...)
	binary.LittleEndian.PutUint16(tmp, 16)
	buf = append(buf, tmp...)
	buf = append(buf, []byte("data")...)
	binary.LittleEndian.PutUint32(tmp4, uint32(dataBytes))
	buf = append(buf, tmp4...)
	buf = append(buf, make([]byte, dataBytes)...)
	return os.WriteFile(path, buf, 0o600)
}

func TestContinue95RunSubtitleWriteFailure(t *testing.T) {
	r := Continue95Renderer(t)
	plan := Continue95ParsePlan(t)
	p := NewCodeRender(nil, r, Continue95WAVNarrator{lockDir: true}, t.TempDir(), nil, 120)
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3,
		AspectRatio: video.Aspect16x9, FPS: 12, NarrationText: "hello world", SubtitleLanguage: "en",
	}
	job := Continue95Job(plan)
	p.run(context.Background(), job, req)
	if job.workDir != "" {
		_ = os.Chmod(job.workDir, 0o755)
	}
	if job.state != video.StateFailed || !strings.Contains(job.err, "write subtitles") {
		t.Errorf("locked-dir subtitles: state=%s err=%q", job.state, job.err)
	}
}

func TestContinue95RunNarrationAndSubtitlesSuccess(t *testing.T) {
	r := Continue95Renderer(t)
	plan := Continue95ParsePlan(t)
	p := NewCodeRender(nil, r, Continue95WAVNarrator{}, t.TempDir(), nil, 120)
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "x", Mode: video.ModeTextToVideo, DurationSeconds: 3,
		AspectRatio: video.Aspect16x9, FPS: 12, NarrationText: "hello world", SubtitleLanguage: "en",
	}
	job := Continue95Job(plan)
	p.run(context.Background(), job, req)
	if job.state != video.StateCompleted {
		t.Fatalf("narration run: state=%s err=%q", job.state, job.err)
	}
	if !strings.Contains(job.usage["tools_used"], "narration-tts") || !strings.Contains(job.usage["tools_used"], "srt-subtitles") {
		t.Errorf("tools_used = %q, want narration-tts and srt-subtitles", job.usage["tools_used"])
	}
	if job.output == "" {
		t.Error("completed narration job should record an output path")
	}
}
