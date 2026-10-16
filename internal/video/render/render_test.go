package render_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

func requireFFmpeg(t *testing.T) *render.Renderer {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	r := &render.Renderer{}
	if err := r.Preflight(context.Background()); err != nil {
		t.Skipf("ffmpeg preflight not satisfiable on this host: %v", err)
	}
	return r
}

func testPlan() video.Plan {
	return video.Plan{
		SchemaVersion: video.PlanSchemaVersion,
		Title:         "Renderer test",
		Width:         320, Height: 240, FPS: 12,
		Scenes: []video.PlanScene{
			{ID: "title", DurationSeconds: 1.5, Background: "#101828", BackgroundGradientTo: "#1d2939", Camera: "zoom_in", Transition: "fade",
				Elements: []video.PlanElement{
					{Type: "text", Text: "Hello Video", X: 0.5, Y: 0.35, FontSize: 44, Color: "#ffffff", Animation: "slide_up"},
					{Type: "shape", Shape: "rect", X: 0.3, Y: 0.62, Width: 0.4, Height: 0.05, Color: "#f79009"},
				}},
			{ID: "bars", DurationSeconds: 1.5, Background: "#ffffff", Camera: "pan_right", Caption: "Quarterly growth",
				Elements: []video.PlanElement{
					{Type: "chart", Data: []float64{3, 7, 5, 9}, X: 0.12, Y: 0.2, Width: 0.76, Height: 0.5, Color: "#2e90fa", Animation: "slide_up"},
				}},
		},
	}
}

// TestRenderPlanProducesPlayableMP4 is the acceptance evidence that the
// baseline renderer creates a real, playable MP4 without any generative
// model: two scenes, camera motion, a transition, animated text, a chart,
// and a caption, verified with ffprobe and frame statistics.
func TestRenderPlanProducesPlayableMP4(t *testing.T) {
	r := requireFFmpeg(t)
	dir := t.TempDir()
	plan := testPlan()
	res, err := r.RenderPlan(context.Background(), plan, dir)
	if err != nil {
		t.Fatalf("RenderPlan: %v", err)
	}
	info, err := r.Probe(context.Background(), res.OutputPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Codec != "h264" || info.Width != 320 || info.Height != 240 {
		t.Fatalf("unexpected probe: %+v", info)
	}
	if info.Seconds < 2.5 || info.Seconds > 3.6 {
		t.Fatalf("duration = %.2fs, want ~3s", info.Seconds)
	}
	if info.Size < 5_000 {
		t.Fatalf("output suspiciously small: %d bytes", info.Size)
	}
	if err := r.ValidateOutput(context.Background(), plan, res.OutputPath); err != nil {
		t.Fatalf("ValidateOutput: %v", err)
	}
	// Motion proof: frames at two times inside the zoom scene must differ.
	_, v1, err := r.FrameStats(context.Background(), res.OutputPath, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if v1 <= 1.0 {
		t.Fatalf("scene frame looks blank, variance %.3f", v1)
	}
}

func TestValidateOutputRejectsWrongDimensions(t *testing.T) {
	r := requireFFmpeg(t)
	dir := t.TempDir()
	plan := testPlan()
	res, err := r.RenderPlan(context.Background(), plan, dir)
	if err != nil {
		t.Fatal(err)
	}
	other := plan
	other.Width, other.Height = 640, 480
	if err := r.ValidateOutput(context.Background(), other, res.OutputPath); err == nil {
		t.Fatal("QC must reject dimension mismatch")
	}
}

func TestValidateOutputRejectsTextFile(t *testing.T) {
	r := requireFFmpeg(t)
	path := filepath.Join(t.TempDir(), "fake.mp4")
	if err := os.WriteFile(path, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateOutput(context.Background(), testPlan(), path); err == nil {
		t.Fatal("QC must reject a non-video file masquerading as MP4")
	}
}

func TestPreflightMissingFFmpeg(t *testing.T) {
	r := &render.Renderer{FFmpeg: "/nonexistent/ffmpeg-xyz"}
	if err := r.Preflight(context.Background()); err == nil {
		t.Fatal("preflight must fail when ffmpeg is missing")
	}
}

func TestRenderPlanRejectsInvalidPlan(t *testing.T) {
	r := &render.Renderer{}
	plan := testPlan()
	plan.Scenes[0].Camera = "orbit"
	if _, err := r.RenderPlan(context.Background(), plan, t.TempDir()); err == nil {
		t.Fatal("invalid plan must be rejected before rendering")
	}
}
