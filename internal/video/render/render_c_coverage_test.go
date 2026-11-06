package render_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

// Agent C coverage: the ffmpeg-env override, the Preflight filter
// failure, the path failures, the scene-directory/file failures, the
// slide_left animation, the concat failure, and the FrameStats failure.

func withDeletedCWD(t *testing.T) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sub); err != nil {
		_ = os.Chdir(orig)
		t.Fatalf("remove current directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func writeRenderScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRendererFFmpegEnvOverride(t *testing.T) {
	t.Setenv("NEXAROUTE_FFMPEG", "/bin/false")
	r := &render.Renderer{}
	if err := r.Preflight(context.Background()); err == nil {
		t.Fatal("Preflight with NEXAROUTE_FFMPEG=/bin/false must fail")
	}
}

func TestRendererPreflightFiltersFailure(t *testing.T) {
	script := writeRenderScript(t, "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then exit 0; fi\nif [ \"$1\" = \"-hide_banner\" ] && [ \"$2\" = \"-encoders\" ]; then echo \" libx264 \"; exit 0; fi\nif [ \"$1\" = \"-hide_banner\" ] && [ \"$2\" = \"-filters\" ]; then exit 1; fi\nexit 0\n")
	r := &render.Renderer{FFmpeg: script}
	err := r.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "inspect ffmpeg filters") {
		t.Fatalf("Preflight filters err = %v, want the filters error", err)
	}
}

func TestRenderPlanAbsFailsWithDeletedCWD(t *testing.T) {
	r := requireFFmpeg(t)
	withDeletedCWD(t)
	_, err := r.RenderPlan(context.Background(), testPlan(), "relative/dir")
	if err == nil || !strings.Contains(err.Error(), "resolve render directory") {
		t.Fatalf("deleted-CWD dir err = %v, want the resolve-directory error", err)
	}
}

func TestRenderPlanMkdirAllFailsUnderAFile(t *testing.T) {
	r := requireFFmpeg(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.RenderPlan(context.Background(), testPlan(), filepath.Join(blocker, "sub"))
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("MkdirAll err = %v, want the not-a-directory error", err)
	}
}

func TestRenderPlanFontFileMustBeRegular(t *testing.T) {
	r := requireFFmpeg(t)
	r.FontFile = t.TempDir()
	_, err := r.RenderPlan(context.Background(), testPlan(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("font file err = %v, want the not-a-regular-file error", err)
	}
}

func TestRenderPlanConcatFailure(t *testing.T) {
	r := requireFFmpeg(t)
	script := writeRenderScript(t, "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = \"concat\" ]; then exit 1; fi; done\nexec ffmpeg \"$@\"\n")
	r.FFmpeg = script
	_, err := r.RenderPlan(context.Background(), testPlan(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "concatenate scenes") {
		t.Fatalf("concat err = %v, want the concatenate-scenes error", err)
	}
}

func TestRenderPlanClampsTinyFrames(t *testing.T) {
	r := requireFFmpeg(t)
	plan := video.Plan{
		SchemaVersion: video.PlanSchemaVersion, Title: "tiny", Width: 320, Height: 240, FPS: 12,
		Scenes: []video.PlanScene{{ID: "a", DurationSeconds: 0.01, Background: "#101828", Camera: "zoom_in",
			Elements: []video.PlanElement{{Type: "text", Text: "hi", X: 0.5, Y: 0.5, FontSize: 40, Color: "#ffffff"}}}},
	}
	if _, err := r.RenderPlan(context.Background(), plan, t.TempDir()); err != nil {
		t.Fatalf("tiny-frames RenderPlan: %v", err)
	}
}

func TestRenderPlanSceneDirMustBeDirectory(t *testing.T) {
	r := requireFFmpeg(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scene-001-text"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.RenderPlan(context.Background(), testPlan(), dir)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("scene dir err = %v, want the not-a-directory error", err)
	}
}

func TestRenderPlanTextWriteFailsOnDirectory(t *testing.T) {
	r := requireFFmpeg(t)
	dir := t.TempDir()
	sceneDir := filepath.Join(dir, "scene-001-text")
	if err := os.MkdirAll(filepath.Join(sceneDir, "text-01.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := r.RenderPlan(context.Background(), testPlan(), dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("text write err = %v, want the is-a-directory error", err)
	}
}

func TestRenderPlanCaptionWriteFailsOnDirectory(t *testing.T) {
	r := requireFFmpeg(t)
	dir := t.TempDir()
	// The caption belongs to the second scene ("bars").
	sceneDir := filepath.Join(dir, "scene-002-text")
	if err := os.MkdirAll(filepath.Join(sceneDir, "caption.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := r.RenderPlan(context.Background(), testPlan(), dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("caption write err = %v, want the is-a-directory error", err)
	}
}

func TestRenderPlanSlideLeftText(t *testing.T) {
	r := requireFFmpeg(t)
	plan := testPlan()
	plan.Scenes[0].Elements[0].Animation = "slide_left"
	if _, err := r.RenderPlan(context.Background(), plan, t.TempDir()); err != nil {
		t.Fatalf("slide_left RenderPlan: %v", err)
	}
}

func TestValidateOutputFrameStatsFailure(t *testing.T) {
	r := requireFFmpeg(t)
	res, err := r.RenderPlan(context.Background(), testPlan(), t.TempDir())
	if err != nil {
		t.Fatalf("RenderPlan: %v", err)
	}
	broken := &render.Renderer{FFmpeg: "/bin/false"}
	err = broken.ValidateOutput(context.Background(), testPlan(), res.OutputPath)
	if err == nil || !strings.Contains(err.Error(), "quality check") {
		t.Fatalf("FrameStats err = %v, want the quality-check error", err)
	}
}
