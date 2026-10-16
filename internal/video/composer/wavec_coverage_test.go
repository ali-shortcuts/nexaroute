package composer

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFFmpeg installs an executable stand-in for FFmpeg with three modes:
//
//	ok:      -version/-encoders answer like a real build with libx264;
//	         any other invocation copies the first -i input onto the last
//	         argument (the output), exiting 0.
//	no264:    -encoders output omits libx264.
//	badver:   -version exits 1.
func fakeFFmpegMode(t *testing.T, name, mode string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	script := `#!/bin/sh
if [ "$1" = "-version" ]; then
  [ "$FF_MODE" = "badver" ] && { echo "broken"; exit 1; }
  echo "ffmpeg version test"; exit 0
fi
if [ "$1" = "-hide_banner" ]; then
  [ "$FF_MODE" = "no264" ] && { echo "Encoders:"; exit 0; }
  echo "Encoders:"; echo " V..... libx264"; exit 0
fi
# Transcode stand-in: copy first -i input to the final argument.
in=""; prev=""
for a in "$@"; do
  if [ "$prev" = "-i" ] && [ -z "$in" ]; then in="$a"; fi
  prev="$a"; last="$a"
done
if [ -n "$in" ] && [ -n "$last" ]; then cp "$in" "$last"; exit 0; fi
echo "unexpected invocation: $*" 1>&2; exit 1
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FF_MODE", mode)
	return p
}

func shotFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPreflightBranches(t *testing.T) {
	ctx := context.Background()
	bin := fakeFFmpegMode(t, "ffmpeg-ok", "ok")
	if err := New(bin, "").Preflight(ctx); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// Missing binary.
	if err := New(filepath.Join(t.TempDir(), "nope"), "").Preflight(ctx); err == nil {
		t.Fatal("missing binary accepted")
	}
	// A Runner whose binary trims to empty never shells out.
	if err := (&Runner{Binary: "   "}).Preflight(ctx); err == nil || !strings.Contains(err.Error(), "binary is required") {
		t.Fatalf("empty binary: %v", err)
	}
	// -version failure.
	bad := fakeFFmpegMode(t, "ffmpeg-badver", "badver")
	if err := New(bad, "").Preflight(ctx); err == nil {
		t.Fatal("bad -version accepted")
	}
	// No libx264 encoder.
	no264 := fakeFFmpegMode(t, "ffmpeg-no264", "no264")
	if err := New(no264, "").Preflight(ctx); err == nil || !strings.Contains(err.Error(), "libx264") {
		t.Fatalf("no264: %v", err)
	}
	// Cancelled context.
	c, cancel := context.WithCancel(ctx)
	cancel()
	if err := New(bin, "").Preflight(c); err != context.Canceled {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestNewDefaults(t *testing.T) {
	if r := New("", "w"); r.Binary != "ffmpeg" || r.WorkDir != "w" {
		t.Fatalf("%+v", r)
	}
}

func TestNormalizeAndConcat(t *testing.T) {
	dir := t.TempDir()
	bin := fakeFFmpegMode(t, "ffmpeg", "ok")
	r := New(bin, "")
	ctx := context.Background()
	in := shotFile(t, dir, "in.mp4", "video-bytes")
	out := filepath.Join(dir, "norm.mp4")
	if err := r.Normalize(ctx, in, out, 1280, 720, 30); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if raw, err := os.ReadFile(out); err != nil || string(raw) != "video-bytes" {
		t.Fatalf("normalize output: %v %q", err, raw)
	}
	if err := r.Normalize(ctx, in, filepath.Join(dir, "bad.mp4"), 0, 720, 30); err == nil {
		t.Fatal("zero width accepted")
	}
	if err := r.Normalize(ctx, in, filepath.Join(dir, "noext"), 640, 480, 30); err == nil {
		t.Fatal("extensionless output accepted")
	}
	// Concat: empty inputs rejected; success copies the first shot.
	if _, err := r.Concat(ctx, nil, filepath.Join(dir, "c.mp4")); err == nil {
		t.Fatal("empty concat accepted")
	}
	in2 := shotFile(t, dir, "in2.mp4", "second-bytes")
	m, err := r.Concat(ctx, []string{in, in2}, filepath.Join(dir, "c.mp4"))
	if err != nil {
		t.Fatalf("concat: %v", err)
	}
	// The fake copies the concat list (the -i input) onto the output;
	// the point under test is the list content and manifest.
	if raw, _ := os.ReadFile(filepath.Join(dir, "c.mp4")); !strings.Contains(string(raw), "in2.mp4") {
		t.Fatalf("concat output: %q", raw)
	}
	if len(m.Commands) != 2 || !strings.Contains(strings.Join(m.Commands[1], " "), "concat") {
		t.Fatalf("manifest: %+v", m.Commands)
	}
	if got := string(m.JSON()); !strings.Contains(got, "commands") {
		t.Fatalf("manifest JSON: %q", got)
	}
	// Quote-escaping: a shot path with a quote must be escaped in the
	// concat list (exercised through the fake — the point is no error).
	quoted := shotFile(t, dir, "it's.mp4", "q")
	if _, err := r.Concat(ctx, []string{quoted}, filepath.Join(dir, "q.mp4")); err != nil {
		t.Fatalf("quoted path: %v", err)
	}
}

func TestTemporaryOutputPath(t *testing.T) {
	got, err := temporaryOutputPath(filepath.Join("d", "x.mp4"))
	if err != nil || got != filepath.Join("d", "x.tmp.mp4") {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"noext", "d."} {
		if _, err := temporaryOutputPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestComposeEpisodeValidation(t *testing.T) {
	dir := t.TempDir()
	bin := fakeFFmpegMode(t, "ffmpeg", "ok")
	r := New(bin, "")
	ctx := context.Background()
	shot := shotFile(t, dir, "shot1.mp4", "shot-bytes")
	base := EpisodeSpec{Shots: []string{shot}, OutputPath: filepath.Join(dir, "ep.mp4"), Width: 1280, Height: 720, FPS: 30}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.ComposeEpisode(c, base); err != context.Canceled {
		t.Fatalf("cancelled: %v", err)
	}
	badSpecs := map[string]EpisodeSpec{
		"no shots":     {OutputPath: base.OutputPath, Width: 1280, Height: 720, FPS: 30},
		"bad dims":     {Shots: base.Shots, OutputPath: base.OutputPath, Width: 32, Height: 720, FPS: 30},
		"bad fps":      {Shots: base.Shots, OutputPath: base.OutputPath, Width: 1280, Height: 720, FPS: 240},
		"nan volume":   {Shots: base.Shots, OutputPath: base.OutputPath, Width: 1280, Height: 720, FPS: 30, NarrationVolume: math.NaN()},
		"hot volume":   {Shots: base.Shots, OutputPath: base.OutputPath, Width: 1280, Height: 720, FPS: 30, MusicVolume: 5},
		"non-mp4 out":  {Shots: base.Shots, OutputPath: filepath.Join(dir, "ep.mkv"), Width: 1280, Height: 720, FPS: 30},
		"missing shot": {Shots: []string{filepath.Join(dir, "no.mp4")}, OutputPath: base.OutputPath, Width: 1280, Height: 720, FPS: 30},
		"shot is dir":  {Shots: []string{dir}, OutputPath: base.OutputPath, Width: 1280, Height: 720, FPS: 30},
		"shot==output": {Shots: []string{base.OutputPath}, OutputPath: base.OutputPath, Width: 1280, Height: 720, FPS: 30},
	}
	for name, spec := range badSpecs {
		if _, err := r.ComposeEpisode(ctx, spec); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Asset guards: missing narration, overwrite of the output by an asset.
	missing := base
	missing.NarrationPath = filepath.Join(dir, "no.wav")
	if _, err := r.ComposeEpisode(ctx, missing); err == nil {
		t.Error("missing narration accepted")
	}
	narr := shotFile(t, dir, "narr.wav", "audio")
	overwrite := base
	overwrite.NarrationPath = base.OutputPath
	_ = narr
	if _, err := r.ComposeEpisode(ctx, overwrite); err == nil {
		t.Error("narration==output accepted")
	}
}

func TestComposeEpisodeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := fakeFFmpegMode(t, "ffmpeg", "ok")
	r := New(bin, "")
	ctx := context.Background()
	s1 := shotFile(t, dir, "s1.mp4", "shot-one")
	s2 := shotFile(t, dir, "s2.mp4", "shot-two")
	narr := shotFile(t, dir, "n.wav", "narr-audio")
	music := shotFile(t, dir, "m.mp3", "music-audio")
	sub := shotFile(t, dir, "s.srt", "1\n00:00:00,000 --> 00:00:01,000\nhi\n")
	spec := EpisodeSpec{
		Shots: []string{s1, s2}, NarrationPath: narr, MusicPath: music, SubtitlePath: sub,
		OutputPath: filepath.Join(dir, "episode.mp4"), Width: 1280, Height: 720, FPS: 30,
	}
	m, err := r.ComposeEpisode(ctx, spec)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	// The copy-fake propagates inputs: the joined segment is the concat
	// list itself, so the final output carries the list's shot entries.
	raw, err := os.ReadFile(spec.OutputPath)
	if err != nil || !strings.Contains(string(raw), "shot-001.mp4") {
		t.Fatalf("episode output: %v %q", err, raw)
	}
	// Manifest records normalize + concat + final compose commands.
	if len(m.Commands) != 4 {
		t.Fatalf("commands=%d: %+v", len(m.Commands), m.Commands)
	}
	joined := strings.Join([]string{
		strings.Join(m.Commands[0], " "),
		strings.Join(m.Commands[2], " "),
		strings.Join(m.Commands[3], " "),
	}, "\n")
	for _, want := range []string{"libx264", "filter_complex", "amix", "subtitles=", "volume=1.000", "volume=0.120"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("manifest missing %q:\n%s", want, joined)
		}
	}
	// Narration-only and music-only variants exercise the other
	// audio-filter branches.
	narrOnly := spec
	narrOnly.MusicPath = ""
	narrOnly.SubtitlePath = ""
	narrOnly.OutputPath = filepath.Join(dir, "narr-only.mp4")
	if _, err := r.ComposeEpisode(ctx, narrOnly); err != nil {
		t.Fatalf("narration-only: %v", err)
	}
	musicOnly := spec
	musicOnly.NarrationPath = ""
	musicOnly.SubtitlePath = ""
	musicOnly.OutputPath = filepath.Join(dir, "music-only.mp4")
	m2, err := r.ComposeEpisode(ctx, musicOnly)
	if err != nil {
		t.Fatalf("music-only: %v", err)
	}
	last := strings.Join(m2.Commands[len(m2.Commands)-1], " ")
	if !strings.Contains(last, "-an") && !strings.Contains(last, "apad") {
		t.Fatalf("music-only audio: %q", last)
	}
	// Silent variant: no audio inputs at all.
	silent := spec
	silent.NarrationPath = ""
	silent.MusicPath = ""
	silent.SubtitlePath = ""
	silent.OutputPath = filepath.Join(dir, "silent.mp4")
	m3, err := r.ComposeEpisode(ctx, silent)
	if err != nil {
		t.Fatalf("silent: %v", err)
	}
	if last := strings.Join(m3.Commands[len(m3.Commands)-1], " "); !strings.Contains(last, "-an") {
		t.Fatalf("silent missing -an: %q", last)
	}
	// Oversize subtitles rejected before staging.
	big := shotFile(t, dir, "big.srt", "x")
	if err := os.Truncate(big, maxSubtitleBytes+1); err != nil {
		t.Fatal(err)
	}
	tooBig := spec
	tooBig.SubtitlePath = big
	tooBig.OutputPath = filepath.Join(dir, "big.mp4")
	if _, err := r.ComposeEpisode(ctx, tooBig); err == nil {
		t.Error("oversize subtitles accepted")
	}
}

func TestPathHelpers(t *testing.T) {
	dir := t.TempDir()
	f := shotFile(t, dir, "f.txt", "data")
	if err := requireRegularFile(f, "f"); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := requireRegularFile(dir, "d"); err == nil {
		t.Fatal("dir accepted")
	}
	if err := requireRegularFile(filepath.Join(dir, "nope"), "m"); err == nil {
		t.Fatal("missing accepted")
	}
	got, err := New("", "").resolvePath("rel.txt")
	if err != nil || !filepath.IsAbs(got) {
		t.Fatalf("resolvePath: %q %v", got, err)
	}
	if _, err := New("", "").resolvePath(""); err == nil {
		t.Fatal("empty accepted")
	}
	if !sameLocalPath(filepath.Join(dir, "a"), dir+"/a") || sameLocalPath(filepath.Join(dir, "a"), filepath.Join(dir, "b")) {
		t.Fatal("sameLocalPath")
	}
	dst := filepath.Join(dir, "copy.txt")
	if err := copyFile(dst, f); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if raw, _ := os.ReadFile(dst); string(raw) != "data" {
		t.Fatal("copy content")
	}
	if err := copyFile(filepath.Join(dir, "no-dir-here", "x.txt"), f); err == nil {
		t.Fatal("copy to missing dir accepted")
	}
	if got := escapeFilterPath(`C:\we'ird\[x].srt`); !strings.Contains(got, `\'`) {
		t.Fatalf("escape: %q", got)
	}
}
