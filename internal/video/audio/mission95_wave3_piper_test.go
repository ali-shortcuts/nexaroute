package audio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Wave-3 SynthesizePiper coverage: whitespace-only text, output
// directory creation failure, temp-file creation failure, publish
// failure, and parent-context cancellation mid-run. The Abs/Stat/
// Read/grow TOCTOU branches, the temp-close race, the post-success
// context race and the chmod branch need an OS fault or a mid-call
// race and are deliberately not faked.

func wave3WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func wave3PiperInputs(t *testing.T, dir, text string) (string, string) {
	t.Helper()
	textPath := filepath.Join(dir, "text.txt")
	modelPath := filepath.Join(dir, "model.onnx")
	wave3WriteFile(t, textPath, text)
	wave3WriteFile(t, modelPath, "fake-model")
	return textPath, modelPath
}

// wave3FakePiperScript writes a shell script that renders a minimal
// valid WAV (12-byte RIFF/WAVE header, enough for validateWAV) to the
// --output_file argument. With a fifo path it first writes one byte
// to the FIFO and then blocks in sleep, so the test can cancel the
// parent context deterministically once the script is running.
func wave3FakePiperScript(t *testing.T, dir, fifo string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-piper.sh")
	body := "#!/bin/sh\n" +
		"out=\"\"\nprev=\"\"\n" +
		"for a in \"$@\"; do if [ \"$prev\" = \"--output_file\" ]; then out=\"$a\"; fi; prev=\"$a\"; done\n"
	if fifo != "" {
		body += "printf 'x' > " + fifo + "\n"
	}
	body += "printf 'RIFF' > \"$out\"\n" +
		"printf '\\024\\000\\000\\000' >> \"$out\"\n" +
		"printf 'WAVE' >> \"$out\"\n"
	if fifo != "" {
		body += "exec sleep 60\n"
	}
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestWave3SynthesizePiperWhitespaceOnlyText(t *testing.T) {
	dir := t.TempDir()
	textPath, modelPath := wave3PiperInputs(t, dir, "  \n\t  ")
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: "piper", TextPath: textPath, ModelPath: modelPath,
		Output: filepath.Join(dir, "out.wav"),
	})
	if err == nil || err.Error() != "narration text is empty" {
		t.Fatalf("whitespace-only text error = %v", err)
	}
}

func TestWave3SynthesizePiperOutputDirectoryCreationFails(t *testing.T) {
	if st, err := os.Stat("/proc/self"); err != nil || !st.IsDir() {
		t.Skip("read-only /proc layout required for this test is unavailable")
	}
	dir := t.TempDir()
	textPath, modelPath := wave3PiperInputs(t, dir, "hello")
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: "piper", TextPath: textPath, ModelPath: modelPath,
		Output: "/proc/self/wave3-missing-dir/out.wav",
	})
	if err == nil || !strings.Contains(err.Error(), "create narration output directory") {
		t.Fatalf("output directory error = %v", err)
	}
}

func TestWave3SynthesizePiperTempCreationFails(t *testing.T) {
	if st, err := os.Stat("/proc/self"); err != nil || !st.IsDir() {
		t.Skip("read-only /proc layout required for this test is unavailable")
	}
	dir := t.TempDir()
	textPath, modelPath := wave3PiperInputs(t, dir, "hello")
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: "piper", TextPath: textPath, ModelPath: modelPath,
		Output: "/proc/self/out.wav",
	})
	if err == nil || !strings.Contains(err.Error(), "create temporary narration output") {
		t.Fatalf("temp creation error = %v", err)
	}
}

func TestWave3SynthesizePiperPublishFailsOntoDirectory(t *testing.T) {
	dir := t.TempDir()
	textPath, modelPath := wave3PiperInputs(t, dir, "hello")
	script := wave3FakePiperScript(t, dir, "")
	outDir := filepath.Join(dir, "outdir")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outDir, "out.wav")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: script, TextPath: textPath, ModelPath: modelPath, Output: output,
	})
	if err == nil || !strings.Contains(err.Error(), "publish narration output") {
		t.Fatalf("publish error = %v", err)
	}
}

func TestWave3SynthesizePiperParentContextCancelledMidRun(t *testing.T) {
	dir := t.TempDir()
	textPath, modelPath := wave3PiperInputs(t, dir, "hello")
	fifo := filepath.Join(dir, "ready.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	script := wave3FakePiperScript(t, dir, fifo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		buf := make([]byte, 1)
		if _, err := f.Read(buf); err == nil {
			cancel()
		}
	}()
	err := SynthesizePiper(ctx, PiperOptions{
		Binary: script, TextPath: textPath, ModelPath: modelPath,
		Output: filepath.Join(dir, "out.wav"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-run cancellation error = %v, want context.Canceled", err)
	}
}
