package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
)

func TestComposeCommandRunsLocallyWithoutGatewayToken(t *testing.T) {
	root := t.TempDir()
	ffmpeg := filepath.Join(root, "fake-ffmpeg")
	script := []byte("#!/bin/sh\nset -eu\nlast=\nfor arg in \"$@\"; do last=\"$arg\"; done\nprintf 'fake ffmpeg output\\n' > \"$last\"\n")
	if err := os.WriteFile(ffmpeg, script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_FFMPEG", ffmpeg)
	t.Setenv("NEXAROUTE_VIDEO_TOKEN", "")

	shot := filepath.Join(root, "shot.mp4")
	if err := os.WriteFile(shot, []byte("input clip"), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "episode.json")
	spec := composer.EpisodeSpec{
		Shots: []string{shot}, OutputPath: filepath.Join(root, "episode.mp4"),
		Width: 640, Height: 360, FPS: 24,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"compose", "--file", manifestPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("compose exit=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(spec.OutputPath); err != nil {
		t.Fatalf("episode output missing: %v", err)
	}
	if !strings.Contains(stdout.String(), "\"commands\"") {
		t.Fatalf("compose manifest was not printed as JSON: %s", stdout.String())
	}
}

func TestComposeCommandRequiresManifest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"compose"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "compose requires --file") {
		t.Fatalf("compose missing manifest exit=%d stderr=%q", code, stderr.String())
	}
}

func TestNarrateCommandRunsLocallyWithoutGatewayToken(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "story.txt")
	modelPath := filepath.Join(root, "voice.onnx")
	piper := filepath.Join(root, "fake-piper")
	outputPath := filepath.Join(root, "narration.wav")
	if err := os.WriteFile(textPath, []byte("Noora tells the story of Persepolis."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelPath, []byte("test model"), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nset -eu\nout=\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--output_file\" ]; then shift; out=$1; fi\n  shift\ndone\ncat >/dev/null\nprintf 'RIFF....WAVE' > \"$out\"\n"
	if err := os.WriteFile(piper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_VIDEO_TOKEN", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"narrate", "--text-file", textPath, "--model", modelPath, "--out", outputPath, "--piper-bin", piper}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("narrate exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Narration written to") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("narration output missing: %v", err)
	}
}

func TestNarrateCommandRequiresInputs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"narrate"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "requires --text-file, --model, and --out") {
		t.Fatalf("narrate exit=%d stderr=%q", code, stderr.String())
	}
}
