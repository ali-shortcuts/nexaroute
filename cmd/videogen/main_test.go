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

func TestRenderCommandSynthesizesNarrationAndComposesWithoutGatewayToken(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "story.txt")
	modelPath := filepath.Join(root, "voice.onnx")
	piper := filepath.Join(root, "fake-piper")
	ffmpeg := filepath.Join(root, "fake-ffmpeg")
	shot := filepath.Join(root, "shot.mp4")
	narration := filepath.Join(root, "narration.wav")
	output := filepath.Join(root, "episode.mp4")
	for path, data := range map[string][]byte{
		textPath:  []byte("Noora tells the story of Persepolis."),
		modelPath: []byte("test model"),
		shot:      []byte("input clip"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	piperScript := "#!/bin/sh\nset -eu\nout=\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--output_file\" ]; then shift; out=$1; fi\n  shift\ndone\ncat >/dev/null\nprintf 'RIFF....WAVE' > \"$out\"\n"
	if err := os.WriteFile(piper, []byte(piperScript), 0700); err != nil {
		t.Fatal(err)
	}
	ffmpegScript := "#!/bin/sh\nset -eu\nlast=\nfor arg in \"$@\"; do last=\"$arg\"; done\nprintf 'fake ffmpeg output\\n' > \"$last\"\n"
	if err := os.WriteFile(ffmpeg, []byte(ffmpegScript), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_FFMPEG", ffmpeg)
	t.Setenv("NEXAROUTE_VIDEO_TOKEN", "")

	manifestPath := filepath.Join(root, "render.json")
	spec := renderManifest{
		EpisodeSpec:          composer.EpisodeSpec{
			Shots: []string{shot}, OutputPath: output, Width: 640, Height: 360, FPS: 24,
		},
		NarrationTextPath:   textPath,
		PiperModelPath:      modelPath,
		NarrationOutputPath: narration,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"render", "--file", manifestPath, "--piper-bin", piper}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("render exit=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(narration); err != nil {
		t.Fatalf("narration output missing: %v", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("episode output missing: %v", err)
	}
	if !strings.Contains(stdout.String(), "\"narration_path\"") || !strings.Contains(stdout.String(), "\"composition\"") {
		t.Fatalf("render result missing narration/composition metadata: %s", stdout.String())
	}
}

func TestRenderRejectsNarrationOutputCollidingWithEpisodeInput(t *testing.T) {
	root := t.TempDir()
	manifestPath := filepath.Join(root, "render.json")
	collision := filepath.Join(root, "shared.wav")
	spec := renderManifest{
		EpisodeSpec: composer.EpisodeSpec{
			Shots: []string{collision}, OutputPath: filepath.Join(root, "episode.mp4"),
			Width: 640, Height: 360, FPS: 24,
		},
		NarrationTextPath: filepath.Join(root, "story.txt"),
		PiperModelPath: filepath.Join(root, "voice.onnx"),
		NarrationOutputPath: collision,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"render", "--file", manifestPath}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "must not overwrite episode input or output") {
		t.Fatalf("render exit=%d stderr=%q", code, stderr.String())
	}
}
