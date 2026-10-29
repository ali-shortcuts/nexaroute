package composer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mission95FFmpeg returns a fake ffmpeg whose failure mode is selected by
// MISSION95_FFMPEG_MODE: "concat" fails only the concat step, "final" fails
// only the final mux, "normalize" fails only the normalize step, and any
// other value writes the last argument like the shared fakeFFmpeg helper.
func mission95FFmpeg(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-ffmpeg")
	script := `#!/bin/sh
case "$MISSION95_FFMPEG_MODE" in
  concat) case "$*" in *concat*) exit 7;; esac;;
  final) case "$*" in *+faststart*) exit 7;; esac;;
  normalize) case "$*" in *-vf*) exit 7;; esac;;
esac
last=""
for arg in "$@"; do last="$arg"; done
printf 'composed output\n' > "$last"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISSION95_FFMPEG_MODE", mode)
	return binary
}

func mission95Shot(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("shot bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mission95BaseSpec(shots []string, output string) EpisodeSpec {
	return EpisodeSpec{Shots: shots, OutputPath: output, Width: 1280, Height: 720, FPS: 30}
}

func TestMission95PreflightEncoderProbeFailure(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ffmpeg-probe")
	script := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo \"ffmpeg version test\"; exit 0; fi\nexit 3\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	err := New(binary, "").Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "inspect ffmpeg encoders") {
		t.Fatalf("preflight encoder-probe error=%v", err)
	}
}

func TestMission95NormalizeHonorsWorkDir(t *testing.T) {
	binary, _, _ := fakeFFmpeg(t)
	work := t.TempDir()
	input := mission95Shot(t, t.TempDir(), "in.mp4")
	output := filepath.Join(t.TempDir(), "out.mp4")
	if err := New(binary, work).Normalize(context.Background(), input, output, 640, 360, 24); err != nil {
		t.Fatalf("Normalize with WorkDir: %v", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("normalized output missing: %v", err)
	}
}

func TestMission95NormalizeAndConcatRejectOutputUnderAFile(t *testing.T) {
	binary, _, _ := fakeFFmpeg(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(binary, "")
	if err := r.Normalize(context.Background(), "in.mp4", filepath.Join(blocker, "out.mp4"), 640, 360, 24); err == nil {
		t.Fatal("Normalize below a regular file succeeded")
	}
	if _, err := r.Concat(context.Background(), []string{"a.mp4"}, filepath.Join(blocker, "out.mp4")); err == nil {
		t.Fatal("Concat below a regular file succeeded")
	}
}

func TestMission95PathHelpers(t *testing.T) {
	if _, err := temporaryOutputPath("noext"); err == nil {
		t.Fatal("extensionless output path accepted")
	}
	if _, err := temporaryOutputPath("clip."); err == nil {
		t.Fatal("dot-only extension accepted")
	}
	tmp, err := temporaryOutputPath("clip.mp4")
	if err != nil || tmp != "clip.tmp.mp4" {
		t.Fatalf("temporaryOutputPath=%q err=%v", tmp, err)
	}
	if _, err := New("", "").resolvePath("   "); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("empty resolvePath error=%v", err)
	}
	joined, err := New("", "/work").resolvePath("shots/a.mp4")
	if err != nil || joined != "/work/shots/a.mp4" {
		t.Fatalf("relative resolvePath=%q err=%v", joined, err)
	}
	abs, err := New("", "/work").resolvePath("/elsewhere/b.mp4")
	if err != nil || abs != "/elsewhere/b.mp4" {
		t.Fatalf("absolute resolvePath=%q err=%v", abs, err)
	}
	if err := requireRegularFile(filepath.Join(t.TempDir(), "missing.mp4"), "shot 1"); err == nil {
		t.Fatal("missing input accepted")
	}
	if err := requireRegularFile(t.TempDir(), "shot 1"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory input error=%v", err)
	}
	if !sameLocalPath("/a/b.mp4", "/a/b.mp4") {
		t.Fatal("identical paths not recognized as the same file")
	}
	if sameLocalPath("/a/b.mp4", "/a/c.mp4") {
		t.Fatal("different paths reported as the same file")
	}
	dir := t.TempDir()
	target := mission95Shot(t, dir, "target.mp4")
	link := filepath.Join(dir, "link.mp4")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if !sameLocalPath(link, target) {
		t.Fatal("symlink alias not recognized as the same file")
	}
}

func TestMission95CopyFileBehaviors(t *testing.T) {
	dir := t.TempDir()
	src := mission95Shot(t, dir, "src.srt")
	dst := filepath.Join(dir, "dst.srt")
	if err := copyFile(dst, src); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "shot bytes" {
		t.Fatalf("copied bytes=%q err=%v", data, err)
	}
	if err := copyFile(dst, src); err == nil {
		t.Fatal("copyFile overwrote an existing destination")
	}
	if err := copyFile(filepath.Join(dir, "dst2.srt"), filepath.Join(dir, "missing.srt")); err == nil {
		t.Fatal("copyFile from a missing source succeeded")
	}
	if err := copyFile(filepath.Join(dir, "dst3.srt"), dir); err == nil {
		t.Fatal("copyFile from a directory succeeded")
	}
}

func TestMission95EscapeFilterPath(t *testing.T) {
	got := escapeFilterPath(`C:\media\sub's:file.srt`)
	if !strings.Contains(got, "\\:") || !strings.Contains(got, "\\'") || !strings.Contains(got, "\\\\") {
		t.Fatalf("escapeFilterPath=%q", got)
	}
}

func TestMission95ComposeEpisodeEarlyValidation(t *testing.T) {
	root := t.TempDir()
	shot := mission95Shot(t, root, "shot-1.mp4")
	r := New(mission95FFmpeg(t, "ok"), "")
	if _, err := r.ComposeEpisode(context.Background(), mission95BaseSpec([]string{shot}, "")); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("empty output error=%v", err)
	}
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ComposeEpisode(context.Background(), mission95BaseSpec([]string{shot}, filepath.Join(blocker, "episode.mp4"))); err == nil {
		t.Fatal("output below a regular file accepted")
	}
	if _, err := r.ComposeEpisode(context.Background(), mission95BaseSpec([]string{""}, filepath.Join(root, "episode.mp4"))); err == nil || !strings.Contains(err.Error(), "shot 1") {
		t.Fatalf("empty shot error=%v", err)
	}
}

func TestMission95ComposeEpisodeStepFailures(t *testing.T) {
	root := t.TempDir()
	shot1 := mission95Shot(t, root, "shot-1.mp4")
	shot2 := mission95Shot(t, root, "shot-2.mp4")
	shots := []string{shot1, shot2}
	if _, err := New(mission95FFmpeg(t, "normalize"), "").ComposeEpisode(context.Background(), mission95BaseSpec(shots, filepath.Join(root, "episode.mp4"))); err == nil || !strings.Contains(err.Error(), "normalize shot 1") {
		t.Fatalf("normalize failure error=%v", err)
	}
	if _, err := New(mission95FFmpeg(t, "concat"), "").ComposeEpisode(context.Background(), mission95BaseSpec(shots, filepath.Join(root, "episode.mp4"))); err == nil || !strings.Contains(err.Error(), "concatenate episode shots") {
		t.Fatalf("concat failure error=%v", err)
	}
	if _, err := New(mission95FFmpeg(t, "final"), "").ComposeEpisode(context.Background(), mission95BaseSpec(shots, filepath.Join(root, "episode.mp4"))); err == nil || !strings.Contains(err.Error(), "compose episode output") {
		t.Fatalf("final mux failure error=%v", err)
	}
}

func TestMission95ComposeEpisodeOverwriteGuards(t *testing.T) {
	root := t.TempDir()
	shot1 := mission95Shot(t, root, "shot-1.mp4")
	shot2 := mission95Shot(t, root, "shot-2.mp4")
	shots := []string{shot1, shot2}
	r := New(mission95FFmpeg(t, "ok"), "")

	output := filepath.Join(root, "episode.mp4")
	if err := os.WriteFile(output, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	spec := mission95BaseSpec(shots, output)
	spec.NarrationPath = output
	if _, err := r.ComposeEpisode(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "must not overwrite narration") {
		t.Fatalf("narration overwrite error=%v", err)
	}
	spec = mission95BaseSpec(shots, output)
	spec.MusicPath = filepath.Join(root, "missing-music.wav")
	if _, err := r.ComposeEpisode(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "music input") {
		t.Fatalf("missing music error=%v", err)
	}
	spec = mission95BaseSpec(shots, output)
	spec.MusicPath = output
	if _, err := r.ComposeEpisode(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "must not overwrite music") {
		t.Fatalf("music overwrite error=%v", err)
	}
	spec = mission95BaseSpec(shots, output)
	spec.SubtitlePath = filepath.Join(root, "missing.srt")
	if _, err := r.ComposeEpisode(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "subtitles input") {
		t.Fatalf("missing subtitles error=%v", err)
	}
	spec = mission95BaseSpec(shots, output)
	spec.SubtitlePath = output
	if _, err := r.ComposeEpisode(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "must not overwrite subtitles") {
		t.Fatalf("subtitles overwrite error=%v", err)
	}
}

func TestMission95ComposeEpisodeSubtitleSizeLimit(t *testing.T) {
	root := t.TempDir()
	shot1 := mission95Shot(t, root, "shot-1.mp4")
	shot2 := mission95Shot(t, root, "shot-2.mp4")
	huge := filepath.Join(root, "huge.srt")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse file: no 16 MiB actually written.
	if err := f.Truncate((16 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	spec := mission95BaseSpec([]string{shot1, shot2}, filepath.Join(root, "episode.mp4"))
	spec.SubtitlePath = huge
	if _, err := New(mission95FFmpeg(t, "ok"), "").ComposeEpisode(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize subtitles error=%v", err)
	}
}

func TestMission95ComposeEpisodeSubtitleStageFailure(t *testing.T) {
	// /proc/self/mem stats as a regular file but reads fail with EIO,
	// so subtitle staging fails deterministically at the copy step.
	root := t.TempDir()
	shot1 := mission95Shot(t, root, "shot-1.mp4")
	shot2 := mission95Shot(t, root, "shot-2.mp4")
	spec := mission95BaseSpec([]string{shot1, shot2}, filepath.Join(root, "episode.mp4"))
	spec.SubtitlePath = "/proc/self/mem"
	if _, err := New(mission95FFmpeg(t, "ok"), "").ComposeEpisode(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "stage subtitles") {
		t.Fatalf("subtitle staging error=%v", err)
	}
}

func TestMission95ComposeEpisodePublishFailure(t *testing.T) {
	root := t.TempDir()
	shot1 := mission95Shot(t, root, "shot-1.mp4")
	shot2 := mission95Shot(t, root, "shot-2.mp4")
	outputDir := filepath.Join(root, "episode.mp4")
	if err := os.Mkdir(outputDir, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := New(mission95FFmpeg(t, "ok"), "").ComposeEpisode(context.Background(), mission95BaseSpec([]string{shot1, shot2}, outputDir))
	if err == nil || !strings.Contains(err.Error(), "publish episode output") {
		t.Fatalf("publish failure error=%v", err)
	}
}

func TestMission95ComposeEpisodeSuccessManifest(t *testing.T) {
	binary, _, _ := fakeFFmpeg(t)
	root := t.TempDir()
	shot1 := mission95Shot(t, root, "shot-1.mp4")
	shot2 := mission95Shot(t, root, "shot-2.mp4")
	output := filepath.Join(root, "rendered", "episode.mp4")
	manifest, err := New(binary, "").ComposeEpisode(context.Background(), mission95BaseSpec([]string{shot1, shot2}, output))
	if err != nil {
		t.Fatalf("ComposeEpisode: %v", err)
	}
	// Two normalize commands, the concat args command, and the final mux.
	if len(manifest.Commands) != 4 {
		t.Fatalf("manifest commands=%d, want 4 (2 normalize + concat + final mux)", len(manifest.Commands))
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "composed output\n" {
		t.Fatalf("episode output=%q err=%v", data, err)
	}
}
