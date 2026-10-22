package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// The filter-graph builders are pure functions: their output IS the
// render contract with ffmpeg, so exact-shape assertions are behavior
// tests, not implementation trivia.

func TestSlideOffsetShapes(t *testing.T) {
	if got := slideOffset(0); got != "0" {
		t.Fatalf("slideOffset(0) = %q, want 0", got)
	}
	got := slideOffset(60)
	if !strings.Contains(got, "*60") || !strings.Contains(got, "min(t/0.7") {
		t.Fatalf("slideOffset(60) = %q, want a timed min() expression scaled by 60", got)
	}
}

func TestDrawBoxGeometryAndAnimation(t *testing.T) {
	el := video.PlanElement{X: 0.5, Y: 0.25, Width: 0.5, Height: 0.5, Color: "#ff0000"}
	got := drawBox(el, 1280, 720)
	want := "drawbox=x=640:y=180:w=640:h=360:color=0xff0000:t=fill"
	if got != want {
		t.Fatalf("drawBox = %q, want %q", got, want)
	}
	el.Animation = "slide_up"
	got = drawBox(el, 1280, 720)
	if !strings.Contains(got, "y='180+") {
		t.Fatalf("slide_up drawBox = %q, want animated y expression", got)
	}
	el.Animation = "slide_left"
	got = drawBox(el, 1280, 720)
	if !strings.Contains(got, "x='640+") {
		t.Fatalf("slide_left drawBox = %q, want animated x expression", got)
	}
}

func TestChartBoxesScalingAndMinimumHeight(t *testing.T) {
	el := video.PlanElement{X: 0, Y: 0, Width: 1, Height: 0.5, Color: "00ff00", Data: []float64{10, 5, 0}}
	boxes := chartBoxes(el, 900, 720)
	if len(boxes) != 3 {
		t.Fatalf("chartBoxes returned %d boxes, want 3", len(boxes))
	}
	// Tallest bar (value 10 of max 10) fills the full 360px plot height.
	if !strings.Contains(boxes[0], ":h=360:") {
		t.Fatalf("tallest bar = %q, want h=360", boxes[0])
	}
	// A zero value still renders at the 2px minimum so the series is visible.
	if !strings.Contains(boxes[2], ":h=2:") {
		t.Fatalf("zero bar = %q, want minimum h=2", boxes[2])
	}
	// All-negative data must not divide by zero or invert the chart.
	neg := chartBoxes(video.PlanElement{Width: 1, Height: 0.5, Data: []float64{-3, -1}}, 900, 720)
	if len(neg) != 2 {
		t.Fatalf("negative-data chartBoxes returned %d boxes", len(neg))
	}
}

func TestDrawTextSizingPositionAndFade(t *testing.T) {
	el := video.PlanElement{X: 0.5, Y: 0.5, FontSize: 36, Color: "#ffffff"}
	got := drawText("fontfile=/f.ttf", "/tmp/t.txt", el, 1280, 720, "")
	if !strings.Contains(got, "fontsize=36") {
		t.Fatalf("drawText = %q, want fontsize scaled to 36 at 720p", got)
	}
	if !strings.Contains(got, "textfile=/tmp/t.txt") {
		t.Fatalf("drawText = %q, want the textfile wired", got)
	}
	// Tiny fonts are floored at 8px so captions stay legible.
	small := drawText("fontfile=/f.ttf", "/tmp/t.txt", video.PlanElement{FontSize: 1}, 1280, 720, "")
	if !strings.Contains(small, "fontsize=8") {
		t.Fatalf("drawText small = %q, want fontsize floor 8", small)
	}
	el.Animation = "fade"
	faded := drawText("fontfile=/f.ttf", "/tmp/t.txt", el, 1280, 720, "")
	if !strings.Contains(faded, ":alpha='if(lt(t") {
		t.Fatalf("fade drawText = %q, want an alpha ramp", faded)
	}
}

func TestEscapeFilterValue(t *testing.T) {
	cases := map[string]string{
		`plain`:        `plain`,
		`a:b`:          `a\:b`,
		`it's`:         `it\'s`,
		`back\slash`:   `back\\slash`,
		`C:\path:it's`: `C\:\\path\:it\'s`,
	}
	for in, want := range cases {
		if got := escapeFilterValue(in); got != want {
			t.Fatalf("escapeFilterValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFFmpegColorAndFnum(t *testing.T) {
	if got := ffmpegColor("#a1b2c3"); got != "0xa1b2c3" {
		t.Fatalf("ffmpegColor = %q", got)
	}
	if got := ffmpegColor("a1b2c3"); got != "0xa1b2c3" {
		t.Fatalf("ffmpegColor without hash = %q", got)
	}
	if got := fnum(0.5); got != "0.5" {
		t.Fatalf("fnum(0.5) = %q", got)
	}
	if got := fnum(2); got != "2" {
		t.Fatalf("fnum(2) = %q", got)
	}
}

// stubProbe installs a fake ffprobe binary emitting canned JSON, so the
// Probe parsing contract is tested without a media toolchain.
func stubProbe(t *testing.T, jsonOut string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\ncat <<'JSON'\n" + jsonOut + "\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestProbeParsesStubbedFFprobeOutput(t *testing.T) {
	media := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(media, []byte("not-really-media"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Renderer{FFprobe: stubProbe(t, `{"streams":[{"codec_type":"audio","codec_name":"aac"},{"codec_type":"video","codec_name":"h264","width":1280,"height":720}],"format":{"duration":"12.5"}}`)}
	info, err := r.Probe(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if info.Codec != "h264" || info.Width != 1280 || info.Height != 720 || info.Seconds != 12.5 {
		t.Fatalf("probe info = %+v", info)
	}
	if info.Size != int64(len("not-really-media")) {
		t.Fatalf("probe size = %d", info.Size)
	}
}

func TestProbeRejectsMissingEmptyAndStreamlessFiles(t *testing.T) {
	r := &Renderer{FFprobe: stubProbe(t, `{"streams":[],"format":{"duration":"0"}}`)}
	ctx := context.Background()
	if _, err := r.Probe(ctx, filepath.Join(t.TempDir(), "nope.mp4")); err == nil {
		t.Fatal("missing file probed successfully")
	}
	empty := filepath.Join(t.TempDir(), "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Probe(ctx, empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty file error = %v, want an empty-file error", err)
	}
	media := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(media, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Probe(ctx, media); err == nil || !strings.Contains(err.Error(), "no video stream") {
		t.Fatalf("streamless file error = %v, want a no-video-stream error", err)
	}
}
