package render_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

const validFakeProbeJSON = "{\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\",\"width\":320,\"height\":240}],\"format\":{\"duration\":\"3.0\"}}"

func fakeRenderer(t *testing.T) (*render.Renderer, string, string) {
	t.Helper()
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "fake-ffmpeg")
	ffprobePath := filepath.Join(dir, "fake-ffprobe")
	fontPath := filepath.Join(dir, "test-font.ttf")
	ffmpeg := "#!/bin/sh\n" +
		"set -e\n" +
		"if [ \"$1\" = \"-version\" ]; then echo 'ffmpeg fake version'; exit 0; fi\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$arg\" in\n" +
		"    -encoders)\n" +
		"      if [ \"$NEXAROUTE_FAKE_NO_X264\" = \"1\" ]; then echo 'no h264 encoder'; else echo ' V..... libx264 H.264 / AVC / MPEG-4 AVC'; fi\n" +
		"      exit 0 ;;\n" +
		"    -filters)\n" +
		"      for f in drawtext drawbox gradients zoompan; do\n" +
		"        if [ \"$NEXAROUTE_FAKE_OMIT_FILTER\" != \"$f\" ]; then printf ' T.C %s filter description\\n' \"$f\"; fi\n" +
		"      done\n" +
		"      exit 0 ;;\n" +
		"  esac\n" +
		"done\n" +
		"last=''\n" +
		"for arg in \"$@\"; do last=\"$arg\"; done\n" +
		"case \"$NEXAROUTE_FAKE_FFMPEG_MODE:$last\" in\n" +
		"  fail:*) echo 'intentional test ffmpeg failure' >&2; exit 17 ;;\n" +
		"  missing:*) exit 0 ;;\n" +
		"  frames:-) python3 -c 'import sys; sys.stdout.buffer.write(bytes([0, 255]) * 1152)'; exit 0 ;;\n" +
		"  blank:-) dd if=/dev/zero bs=2304 count=1 2>/dev/null; exit 0 ;;\n" +
		"  empty-frame:-) exit 0 ;;\n" +
		"  *) printf 'fake-media-output' > \"$last\" ;;\n" +
		"esac\n"
	ffprobe := "#!/bin/sh\n" +
		"if [ \"$NEXAROUTE_FAKE_FFPROBE_MODE\" = \"fail\" ]; then echo 'intentional test ffprobe failure' >&2; exit 19; fi\n" +
		"printf '%s\\n' \"$NEXAROUTE_FAKE_FFPROBE_JSON\"\n"
	if err := os.WriteFile(ffmpegPath, []byte(ffmpeg), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffprobePath, []byte(ffprobe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fontPath, []byte("fake-font"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_FAKE_FFMPEG_MODE", "file")
	t.Setenv("NEXAROUTE_FAKE_NO_X264", "")
	t.Setenv("NEXAROUTE_FAKE_OMIT_FILTER", "")
	t.Setenv("NEXAROUTE_FAKE_FFPROBE_MODE", "")
	t.Setenv("NEXAROUTE_FAKE_FFPROBE_JSON", validFakeProbeJSON)
	return &render.Renderer{FFmpeg: ffmpegPath, FFprobe: ffprobePath, FontFile: fontPath}, ffmpegPath, ffprobePath
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
			doc: map[string]any{"streams": []any{map[string]any{"codec_type":"video","codec_name":"h264","width":320,"height":240}}, "format": map[string]any{"duration":"3.0"}},
			mode: "frames",
		},
		{
			name: "wrong codec",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type":"video","codec_name":"vp9","width":320,"height":240}}, "format": map[string]any{"duration":"3.0"}},
			mode: "frames", want: "want h264",
		},
		{
			name: "wrong dimensions",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type":"video","codec_name":"h264","width":640,"height":480}}, "format": map[string]any{"duration":"3.0"}},
			mode: "frames", want: "do not match plan",
		},
		{
			name: "wrong duration",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type":"video","codec_name":"h264","width":320,"height":240}}, "format": map[string]any{"duration":"30.0"}},
			mode: "frames", want: "does not match plan total",
		},
		{
			name: "blank scene frame",
			doc: map[string]any{"streams": []any{map[string]any{"codec_type":"video","codec_name":"h264","width":320,"height":240}}, "format": map[string]any{"duration":"3.0"}},
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
