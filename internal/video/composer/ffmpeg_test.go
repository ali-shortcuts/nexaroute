package composer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeFFmpeg(t *testing.T) (binary, argsCapture, listCapture string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "fake-ffmpeg")
	argsCapture = filepath.Join(dir, "args.txt")
	listCapture = filepath.Join(dir, "concat-list.txt")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$NEXA_TEST_ARGS"
last=""
previous=""
for arg in "$@"; do
    if [ "$previous" = "-i" ] && [ -n "${NEXA_TEST_LIST:-}" ]; then
        cp "$arg" "$NEXA_TEST_LIST"
    fi
    previous="$arg"
    last="$arg"
done
printf 'composed output\n' > "$last"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXA_TEST_ARGS", argsCapture)
	t.Setenv("NEXA_TEST_LIST", "")
	return binary, argsCapture, listCapture
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestNewAndManifestJSON(t *testing.T) {
	defaultRunner := New("", "/work")
	if defaultRunner.Binary != "ffmpeg" || defaultRunner.WorkDir != "/work" {
		t.Fatalf("default runner=%+v", defaultRunner)
	}
	customRunner := New("custom-ffmpeg", "")
	if customRunner.Binary != "custom-ffmpeg" || customRunner.WorkDir != "" {
		t.Fatalf("custom runner=%+v", customRunner)
	}

	manifest := Manifest{Commands: [][]string{{"ffmpeg", "-y"}, {"-i", "input.mp4", "output.mp4"}}}
	var decoded Manifest
	if err := json.Unmarshal(manifest.JSON(), &decoded); err != nil {
		t.Fatalf("Manifest.JSON is not JSON: %v", err)
	}
	if len(decoded.Commands) != 2 || decoded.Commands[0][0] != "ffmpeg" || decoded.Commands[1][2] != "output.mp4" {
		t.Fatalf("decoded manifest=%+v", decoded)
	}
	if got := string(manifest.JSON()); !strings.Contains(got, "\n  \"commands\":") {
		t.Fatalf("manifest is not indented JSON: %q", got)
	}
}

func TestNormalizeValidatesArgumentsBeforeRunning(t *testing.T) {
	runner := New(filepath.Join(t.TempDir(), "does-not-exist"), "")
	for _, tc := range []struct {
		name          string
		width, height int
		fps           int
	}{
		{name: "zero width", width: 0, height: 1080, fps: 24},
		{name: "zero height", width: 1920, height: 0, fps: 24},
		{name: "zero fps", width: 1920, height: 1080, fps: 0},
		{name: "negative width", width: -1, height: 1080, fps: 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "nested", "normalized.mp4")
			if err := runner.Normalize(context.Background(), "input.mp4", output, tc.width, tc.height, tc.fps); err == nil || !strings.Contains(err.Error(), "invalid output dimensions or fps") {
				t.Fatalf("Normalize error=%v", err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid Normalize created output: %v", err)
			}
		})
	}
}

func TestNormalizeUsesExpectedFFmpegArgumentsAndRenamesOutput(t *testing.T) {
	binary, argsCapture, _ := fakeFFmpeg(t)
	runner := New(binary, "")
	output := filepath.Join(t.TempDir(), "nested", "normalized.mp4")
	if err := runner.Normalize(context.Background(), "input clip.mp4", output, 1280, 720, 30); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read normalized output: %v", err)
	}
	if string(data) != "composed output\n" {
		t.Fatalf("output=%q", data)
	}
	args := readLines(t, argsCapture)
	want := []string{"-y", "-i", "input clip.mp4", "-vf", "scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2,fps=30", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-an"}
	if len(args) != len(want)+1 || strings.Join(args[:len(want)], "\x00") != strings.Join(want, "\x00") || !strings.HasSuffix(args[len(args)-1], ".mp4.tmp") {
		t.Fatalf("Normalize args=%q, want prefix=%q and temp output", args, want)
	}
	if _, err := os.Stat(output + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary output was not removed: %v", err)
	}
}

func TestConcatWritesEscapedListAndReturnsManifest(t *testing.T) {
	binary, argsCapture, listCapture := fakeFFmpeg(t)
	runner := New(binary, "")
	root := t.TempDir()
	first := filepath.Join(root, "first.mp4")
	second := filepath.Join(root, "director's cut.mp4")
	output := filepath.Join(root, "joined", "final.mp4")
	t.Setenv("NEXA_TEST_LIST", listCapture)
	manifest, err := runner.Concat(context.Background(), []string{first, second}, output)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	if got, err := os.ReadFile(output); err != nil || string(got) != "composed output\n" {
		t.Fatalf("concat output=%q err=%v", got, err)
	}
	list, err := os.ReadFile(listCapture)
	if err != nil {
		t.Fatalf("read concat list: %v", err)
	}
	firstAbs, _ := filepath.Abs(first)
	secondAbs, _ := filepath.Abs(second)
	wantList := "file '" + firstAbs + "'\nfile '" + strings.ReplaceAll(secondAbs, "'", "'\\''") + "'\n"
	if string(list) != wantList {
		t.Fatalf("concat list=%q, want %q", list, wantList)
	}
	if len(manifest.Commands) != 2 || len(manifest.Commands[0]) != 1 || manifest.Commands[0][0] != binary {
		t.Fatalf("manifest command prefix=%+v", manifest.Commands)
	}
	args := readLines(t, argsCapture)
	wantPrefix := []string{"-y", "-f", "concat", "-safe", "0", "-i"}
	// The helper records the temporary list path, whose value is intentionally nondeterministic.
	if len(args) != len(wantPrefix)+4 || strings.Join(args[:len(wantPrefix)], "\x00") != strings.Join(wantPrefix, "\x00") || args[len(wantPrefix)+1] != "-c" || args[len(wantPrefix)+2] != "copy" || !strings.HasSuffix(args[len(args)-1], ".mp4.tmp") {
		t.Fatalf("Concat args=%q", args)
	}
	if _, err := os.Stat(output + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary concat output was not removed: %v", err)
	}
}

func TestConcatRejectsEmptyInputs(t *testing.T) {
	runner := New(filepath.Join(t.TempDir(), "does-not-exist"), "")
	if _, err := runner.Concat(context.Background(), nil, filepath.Join(t.TempDir(), "out.mp4")); err == nil || !strings.Contains(err.Error(), "no input videos") {
		t.Fatalf("Concat empty inputs error=%v", err)
	}
}

func TestInjectedMissingBinaryIsReported(t *testing.T) {
	runner := New(filepath.Join(t.TempDir(), "missing-ffmpeg"), "")
	output := filepath.Join(t.TempDir(), "out.mp4")
	if err := runner.Normalize(context.Background(), "input.mp4", output, 320, 240, 24); err == nil || !strings.Contains(err.Error(), "ffmpeg failed") {
		t.Fatalf("Normalize missing binary error=%v", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing binary created output: %v", err)
	}
	if _, err := runner.Concat(context.Background(), []string{"input.mp4"}, output); err == nil || !strings.Contains(err.Error(), "ffmpeg failed") {
		t.Fatalf("Concat missing binary error=%v", err)
	}
}
