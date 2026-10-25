package render_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

const validFakeProbeJSON = "{\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\",\"width\":320,\"height\":240}],\"format\":{\"duration\":\"3.0\"}}"

var (
	fakeToolDir     string
	fakeFFmpegPath  string
	fakeFFprobePath string
)

// init turns a copy of the Go test executable into a deterministic FFmpeg or
// ffprobe test double. Using a native helper keeps these tests portable and
// avoids requiring system multimedia packages in ordinary unit-test runners.
func init() {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	switch name {
	case "nexaroute-fake-ffmpeg":
		os.Exit(runFakeFFmpeg(os.Args[1:]))
	case "nexaroute-fake-ffprobe":
		os.Exit(runFakeFFprobe())
	}
}

func TestMain(m *testing.M) {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate test executable:", err)
		os.Exit(2)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read test executable:", err)
		os.Exit(2)
	}
	fakeToolDir, err = os.MkdirTemp("", "nexaroute-render-tools-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create fake-tool directory:", err)
		os.Exit(2)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	fakeFFmpegPath = filepath.Join(fakeToolDir, "nexaroute-fake-ffmpeg"+ext)
	fakeFFprobePath = filepath.Join(fakeToolDir, "nexaroute-fake-ffprobe"+ext)
	if err := os.WriteFile(fakeFFmpegPath, binary, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "write fake ffmpeg:", err)
		_ = os.RemoveAll(fakeToolDir)
		os.Exit(2)
	}
	if err := os.WriteFile(fakeFFprobePath, binary, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "write fake ffprobe:", err)
		_ = os.RemoveAll(fakeToolDir)
		os.Exit(2)
	}
	code := m.Run()
	_ = os.RemoveAll(fakeToolDir)
	os.Exit(code)
}

func runFakeFFmpeg(args []string) int {
	if len(args) > 0 && args[0] == "-version" {
		fmt.Println("ffmpeg fake version")
		return 0
	}
	for _, arg := range args {
		switch arg {
		case "-encoders":
			if os.Getenv("NEXAROUTE_FAKE_NO_X264") == "1" {
				fmt.Println("no H.264 encoder")
			} else {
				fmt.Println(" V..... libx264 H.264 / AVC / MPEG-4 AVC")
			}
			return 0
		case "-filters":
			for _, filter := range []string{"drawtext", "drawbox", "gradients", "zoompan"} {
				if os.Getenv("NEXAROUTE_FAKE_OMIT_FILTER") != filter {
					fmt.Printf(" T.C %s filter description\n", filter)
				}
			}
			return 0
		}
	}
	mode := os.Getenv("NEXAROUTE_FAKE_FFMPEG_MODE")
	if mode == "" {
		mode = "file"
	}
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "intentional test ffmpeg failure")
		return 17
	}
	if mode == "missing" {
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake ffmpeg received no arguments")
		return 2
	}
	last := args[len(args)-1]
	if last == "-" {
		switch mode {
		case "frames":
			_, _ = os.Stdout.Write(bytes.Repeat([]byte{0, 255}, 1152))
			return 0
		case "blank":
			_, _ = os.Stdout.Write(make([]byte, 2304))
			return 0
		case "empty-frame":
			return 0
		}
	}
	returnCode := 0
	if err := os.WriteFile(last, []byte("fake-media-output"), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "write fake media output:", err)
		returnCode = 3
	}
	return returnCode
}

func runFakeFFprobe() int {
	if os.Getenv("NEXAROUTE_FAKE_FFPROBE_MODE") == "fail" {
		fmt.Fprintln(os.Stderr, "intentional test ffprobe failure")
		return 19
	}
	fmt.Fprintln(os.Stdout, os.Getenv("NEXAROUTE_FAKE_FFPROBE_JSON"))
	return 0
}

func fakeRenderer(t *testing.T) (*render.Renderer, string, string) {
	t.Helper()
	fontPath := filepath.Join(t.TempDir(), "test-font.ttf")
	if err := os.WriteFile(fontPath, []byte("fake-font"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_FAKE_FFMPEG_MODE", "file")
	t.Setenv("NEXAROUTE_FAKE_NO_X264", "")
	t.Setenv("NEXAROUTE_FAKE_OMIT_FILTER", "")
	t.Setenv("NEXAROUTE_FAKE_FFPROBE_MODE", "")
	t.Setenv("NEXAROUTE_FAKE_FFPROBE_JSON", validFakeProbeJSON)
	return &render.Renderer{FFmpeg: fakeFFmpegPath, FFprobe: fakeFFprobePath, FontFile: fontPath}, fakeFFmpegPath, fakeFFprobePath
}

func smallValidPlan() video.Plan {
	return video.Plan{
		SchemaVersion: video.PlanSchemaVersion,
		Title:         "Fake renderer behavior test",
		Width:         320,
		Height:        240,
		FPS:           12,
		Scenes: []video.PlanScene{{
			ID:              "one",
			DurationSeconds: 1,
			Background:      "#101828",
			Camera:          "none",
			Transition:      "none",
			Elements: []video.PlanElement{{
				Type: "text", Text: "Safe text file", X: 0.3, Y: 0.4,
				FontSize: 24, Color: "#ffffff",
			}},
		}},
	}
}

func TestRenderPlanWithFakeFFmpegExercisesSceneVariants(t *testing.T) {
	r, _, _ := fakeRenderer(t)
	plan := testPlan()
	plan.Scenes = append(plan.Scenes,
		video.PlanScene{
			ID: "zoom-out", DurationSeconds: 1, Background: "#010203",
			Camera: "zoom_out", Transition: "none",
			Elements: []video.PlanElement{{
				Type: "text", Text: "Zoom out", X: 0.3, Y: 0.3, FontSize: 24,
				Color: "#ffffff", Animation: "fade",
			}},
		},
		video.PlanScene{
			ID: "pan-left", DurationSeconds: 1, Background: "#000000",
			BackgroundGradientTo: "#123456", Camera: "pan_left", Transition: "fade",
			Elements: []video.PlanElement{{
				Type: "shape", Shape: "rect", X: 0.1, Y: 0.1, Width: 0.5,
				Height: 0.2, Color: "#ff0000", Animation: "slide_left",
			}},
		},
		video.PlanScene{
			ID: "static-chart", DurationSeconds: 1, Background: "#111111",
			Camera: "none", Transition: "none", Caption: "Static chart",
			Elements: []video.PlanElement{{
				Type: "chart", Data: []float64{0, 0}, X: 0.1, Y: 0.2,
				Width: 0.8, Height: 0.5, Color: "#2e90fa",
			}},
		},
	)
	plan.DurationSeconds = 0

	dir := t.TempDir()
	result, err := r.RenderPlan(context.Background(), plan, dir)
	if err != nil {
		t.Fatalf("RenderPlan with controlled FFmpeg: %v", err)
	}
	if len(result.ScenePaths) != len(plan.Scenes) {
		t.Fatalf("scene paths=%d, want %d", len(result.ScenePaths), len(plan.Scenes))
	}
	if len(result.Manifest.Commands) != 2 {
		t.Fatalf("concat manifest has %d commands, want renderer command and concat command", len(result.Manifest.Commands))
	}
	for _, path := range append(append([]string(nil), result.ScenePaths...), result.OutputPath) {
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			t.Fatalf("render output %q is absent or empty: info=%v err=%v", path, info, err)
		}
	}
	textPath := filepath.Join(dir, "scene-001-text", "text-01.txt")
	body, err := os.ReadFile(textPath)
	if err != nil || string(body) != "Hello Video" {
		t.Fatalf("scene text file body=%q err=%v", body, err)
	}
	info, err := os.Stat(textPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("scene text file mode=%04o, want 0600", info.Mode().Perm())
	}
}

func TestRenderPlanHandlesFFmpegFailureAndMissingOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want string
	}{
		{name: "process failure", mode: "fail", want: "ffmpeg scene render failed"},
		{name: "zero exit without output", mode: "missing", want: "produced no scene output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := fakeRenderer(t)
			t.Setenv("NEXAROUTE_FAKE_FFMPEG_MODE", tc.mode)
			_, err := r.RenderPlan(context.Background(), smallValidPlan(), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RenderPlan error=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRendererPreflightWithFakeToolsAndFailures(t *testing.T) {
	t.Run("all required features", func(t *testing.T) {
		r, _, _ := fakeRenderer(t)
		if err := r.Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
	})
	t.Run("missing required filter", func(t *testing.T) {
		r, _, _ := fakeRenderer(t)
		t.Setenv("NEXAROUTE_FAKE_OMIT_FILTER", "zoompan")
		if err := r.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "zoompan filter") {
			t.Fatalf("Preflight error=%v, want missing zoompan filter", err)
		}
	})
	t.Run("missing h264 encoder", func(t *testing.T) {
		r, _, _ := fakeRenderer(t)
		t.Setenv("NEXAROUTE_FAKE_NO_X264", "1")
		if err := r.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "libx264") {
			t.Fatalf("Preflight error=%v, want missing libx264 encoder", err)
		}
	})
	t.Run("missing ffprobe", func(t *testing.T) {
		r, _, _ := fakeRenderer(t)
		r.FFprobe = filepath.Join(t.TempDir(), "not-installed")
		if err := r.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "ffprobe executable") {
			t.Fatalf("Preflight error=%v, want missing ffprobe", err)
		}
	})
	t.Run("configured font must be a regular file", func(t *testing.T) {
		r, _, _ := fakeRenderer(t)
		r.FontFile = t.TempDir()
		if err := r.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("Preflight error=%v, want invalid font error", err)
		}
	})
}

func TestProbeParsesFactsAndRejectsBadMediaEvidence(t *testing.T) {
	r, _, _ := fakeRenderer(t)
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte("fake media bytes"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NEXAROUTE_FAKE_FFPROBE_JSON", validFakeProbeJSON)
	info, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe valid JSON: %v", err)
	}
	if info.Codec != "h264" || info.Width != 320 || info.Height != 240 || info.Seconds != 3 || info.Size != int64(len("fake media bytes")) {
		t.Fatalf("unexpected Probe facts: %+v", info)
	}

	for _, tc := range []struct {
		name string
		json string
		mode string
		want string
	}{
		{name: "invalid JSON", json: "{", want: "parse ffprobe output"},
		{name: "no video stream", json: "{\"streams\":[],\"format\":{\"duration\":\"3\"}}", want: "no video stream"},
		{name: "command failure", json: validFakeProbeJSON, mode: "fail", want: "ffprobe failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NEXAROUTE_FAKE_FFPROBE_JSON", tc.json)
			t.Setenv("NEXAROUTE_FAKE_FFPROBE_MODE", tc.mode)
			_, err := r.Probe(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Probe error=%v, want %q", err, tc.want)
			}
		})
	}

	empty := filepath.Join(t.TempDir(), "empty.mp4")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Probe(context.Background(), empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty media Probe error=%v", err)
	}
	if _, err := r.Probe(context.Background(), filepath.Join(t.TempDir(), "missing.mp4")); err == nil {
		t.Fatal("missing media file was accepted")
	}
}

func TestFrameStatsReadsPixelsAndRejectsInvalidFrameResults(t *testing.T) {
	r, _, _ := fakeRenderer(t)
	path := filepath.Join(t.TempDir(), "frame-source.mp4")
	if err := os.WriteFile(path, []byte("not used by fake ffmpeg"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NEXAROUTE_FAKE_FFMPEG_MODE", "frames")
	mean, variance, err := r.FrameStats(context.Background(), path, 0.5)
	if err != nil {
		t.Fatalf("FrameStats: %v", err)
	}
	if mean != 127.5 || variance < 16000 {
		t.Fatalf("FrameStats mean=%v variance=%v; expected alternating black/white pixels", mean, variance)
	}

	t.Setenv("NEXAROUTE_FAKE_FFMPEG_MODE", "empty-frame")
	if _, _, err := r.FrameStats(context.Background(), path, 0.5); err == nil || !strings.Contains(err.Error(), "no frame decoded") {
		t.Fatalf("empty frame error=%v", err)
	}
	t.Setenv("NEXAROUTE_FAKE_FFMPEG_MODE", "fail")
	if _, _, err := r.FrameStats(context.Background(), path, 0.5); err == nil || !strings.Contains(err.Error(), "extract frame") {
		t.Fatalf("failed ffmpeg error=%v", err)
	}
}

func TestValidateOutputEnforcesCodecDimensionsDurationAndVisibleFrame(t *testing.T) {
	cases := []struct {
		name string
		doc  map[string]any
		mode string
		want string
	}{
		{
			name: "valid H264 media",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type": "video", "codec_name": "h264", "width": 320, "height": 240}}, "format": map[string]any{"duration": "3.0"}},
			mode: "frames",
		},
		{
			name: "wrong codec",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type": "video", "codec_name": "vp9", "width": 320, "height": 240}}, "format": map[string]any{"duration": "3.0"}},
			mode: "frames", want: "want h264",
		},
		{
			name: "wrong dimensions",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type": "video", "codec_name": "h264", "width": 640, "height": 480}}, "format": map[string]any{"duration": "3.0"}},
			mode: "frames", want: "do not match plan",
		},
		{
			name: "wrong duration",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type": "video", "codec_name": "h264", "width": 320, "height": 240}}, "format": map[string]any{"duration": "30.0"}},
			mode: "frames", want: "does not match plan total",
		},
		{
			name: "blank scene frame",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type": "video", "codec_name": "h264", "width": 320, "height": 240}}, "format": map[string]any{"duration": "3.0"}},
			mode: "blank", want: "is blank",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := fakeRenderer(t)
			doc, err := json.Marshal(tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("NEXAROUTE_FAKE_FFPROBE_JSON", string(doc))
			t.Setenv("NEXAROUTE_FAKE_FFMPEG_MODE", tc.mode)
			path := filepath.Join(t.TempDir(), "result.mp4")
			if err := os.WriteFile(path, []byte("fake media"), 0600); err != nil {
				t.Fatal(err)
			}
			err = r.ValidateOutput(context.Background(), testPlan(), path)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateOutput valid media: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateOutput error=%v, want %q", err, tc.want)
			}
		})
	}
}
