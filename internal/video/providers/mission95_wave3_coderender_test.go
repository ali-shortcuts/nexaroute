package providers

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

var errWave3NarratorBoom = errors.New("boom")

// Wave-3 CodeRender.run coverage: the work-root branches, RenderPlan
// failure with live and cancelled contexts, the narration/subtitle
// error branches, compose failure, the QC failure, and the asset
// size limit. run is synchronous, so no polling is needed. The
// second-MkdirTemp race, the BuildSRT-empty branch (unreachable:
// a validated plan has positive duration and non-empty narration)
// and the subtitle-write race are deliberately not faked.

type wave3FakeNarrator struct {
	write func(path string) error
	calls int
}

func (n *wave3FakeNarrator) Synthesize(_ context.Context, _, output string) error {
	n.calls++
	if n.write != nil {
		return n.write(output)
	}
	return nil
}

func wave3WriteWAVFile(path string, byteRate uint32, dataBytes int) error {
	buf := make([]byte, 0, 44+dataBytes)
	buf = append(buf, []byte("RIFF")...)
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(36+dataBytes))
	buf = append(buf, size...)
	buf = append(buf, []byte("WAVE")...)
	buf = append(buf, []byte("fmt ")...)
	fmtSize := make([]byte, 4)
	binary.LittleEndian.PutUint32(fmtSize, 16)
	buf = append(buf, fmtSize...)
	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtBody[0:2], 1)
	binary.LittleEndian.PutUint16(fmtBody[2:4], 1)
	binary.LittleEndian.PutUint32(fmtBody[4:8], 8000)
	binary.LittleEndian.PutUint32(fmtBody[8:12], byteRate)
	binary.LittleEndian.PutUint16(fmtBody[12:14], 2)
	binary.LittleEndian.PutUint16(fmtBody[14:16], 16)
	buf = append(buf, fmtBody...)
	buf = append(buf, []byte("data")...)
	dataSize := make([]byte, 4)
	binary.LittleEndian.PutUint32(dataSize, uint32(dataBytes))
	buf = append(buf, dataSize...)
	buf = append(buf, make([]byte, dataBytes)...)
	return os.WriteFile(path, buf, 0600)
}

func wave3Plan(textColor, background string) video.Plan {
	return video.Plan{
		SchemaVersion: video.PlanSchemaVersion,
		Title:         "wave3",
		Width:         320, Height: 240, FPS: 12,
		Scenes: []video.PlanScene{{
			ID: "s1", DurationSeconds: 1.5, Background: background,
			Elements: []video.PlanElement{{
				Type: "text", Text: "hello", X: 0.1, Y: 0.1,
				Color: textColor, FontSize: 24,
			}},
		}},
	}
}

func wave3RunJob(plan video.Plan) (*codeRenderJob, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	job := &codeRenderJob{
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		plan: plan, usage: map[string]string{},
	}
	return job, cancel
}

func wave3JobResult(job *codeRenderJob) (video.JobState, string) {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.state, job.err
}

func TestWave3CodeRenderEmptyWorkRoot(t *testing.T) {
	p := &CodeRender{Renderer: &render.Renderer{}}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || err != "code-render work root is not configured" {
		t.Fatalf("empty work root: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderWorkRootUnderFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: blocker}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "create code-render work root") {
		t.Fatalf("work root under a file: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderRenderPlanErrorLiveContext(t *testing.T) {
	p := &CodeRender{Renderer: &render.Renderer{FFmpeg: "/nonexistent/ffmpeg-binary"}, WorkRoot: t.TempDir()}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "render plan:") {
		t.Fatalf("render failure: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderRenderPlanErrorCancelledContext(t *testing.T) {
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir()}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	ctx, ctxCancel := context.WithCancel(context.Background())
	ctxCancel()
	p.run(ctx, job, video.VideoRequest{})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateCancelled || err != "" {
		t.Fatalf("cancelled render: state=%v err=%q, want cancelled with empty error", state, err)
	}
	cancel()
}

func TestWave3CodeRenderNarrationRequiredButNoNarrator(t *testing.T) {
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir(), Narrator: nil}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{NarrationText: "hello narration"})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "no local Piper narrator is configured") {
		t.Fatalf("missing narrator: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderSubtitlesWithoutNarrationText(t *testing.T) {
	narrator := &wave3FakeNarrator{}
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir(), Narrator: narrator}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{SubtitleLanguage: "en"})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "subtitles were requested but the plan contains no narration") {
		t.Fatalf("subtitles without text: state=%v err=%q", state, err)
	}
	if narrator.calls != 0 {
		t.Fatalf("narrator must not be called when there is no narration text, got %d calls", narrator.calls)
	}
}

func TestWave3CodeRenderNarratorError(t *testing.T) {
	narrator := &wave3FakeNarrator{write: func(string) error { return errWave3NarratorBoom }}
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir(), Narrator: narrator}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{NarrationText: "hello narration"})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "synthesize narration:") {
		t.Fatalf("narrator error: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderNarrationAudioInvalid(t *testing.T) {
	narrator := &wave3FakeNarrator{write: func(path string) error {
		return os.WriteFile(path, []byte("this is not a wav file at all"), 0600)
	}}
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir(), Narrator: narrator}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{NarrationText: "hello narration"})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "validate narration audio:") {
		t.Fatalf("invalid narration audio: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderNarrationTooLong(t *testing.T) {
	narrator := &wave3FakeNarrator{write: func(path string) error {
		// byteRate 100, 360000 data bytes = 3600s, far beyond the
		// 1.5s plan plus the 2s grace.
		return wave3WriteWAVFile(path, 100, 360000)
	}}
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir(), Narrator: narrator}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{NarrationText: "hello narration"})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "exceeds the video duration") {
		t.Fatalf("overlong narration: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderComposeFailure(t *testing.T) {
	t.Setenv("NEXAROUTE_FFMPEG", "/bin/false")
	narrator := &wave3FakeNarrator{write: func(path string) error {
		return wave3WriteWAVFile(path, 16000, 6400)
	}}
	// The renderer pins the real ffmpeg explicitly so only the
	// compose step (which reads the env var) fails.
	p := &CodeRender{Renderer: &render.Renderer{FFmpeg: "ffmpeg"}, WorkRoot: t.TempDir(), Narrator: narrator}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{NarrationText: "hello narration", SubtitleLanguage: "en"})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "compose narration/subtitles:") {
		t.Fatalf("compose failure: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderQualityCheckFailsOnBlankFrame(t *testing.T) {
	// Text in exactly the background color renders an invisible
	// frame: the QC luminance-variance gate must reject it.
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir()}
	job, cancel := wave3RunJob(wave3Plan("#000000", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "output quality check failed:") {
		t.Fatalf("blank frame QC: state=%v err=%q", state, err)
	}
}

func TestWave3CodeRenderAssetSizeLimit(t *testing.T) {
	p := &CodeRender{Renderer: &render.Renderer{}, WorkRoot: t.TempDir(), MaxAssetBytes: 1}
	job, cancel := wave3RunJob(wave3Plan("#FFFFFF", "#000000"))
	defer cancel()
	p.run(context.Background(), job, video.VideoRequest{})
	<-job.done
	state, err := wave3JobResult(job)
	if state != video.StateFailed || !strings.Contains(err, "exceeds the configured asset size limit") {
		t.Fatalf("asset size limit: state=%v err=%q", state, err)
	}
}
