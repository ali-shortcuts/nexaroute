package tts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Wave-3 Piper.Synthesize coverage: an Lstat failure that is not
// "not exist", private-input creation failure, the silent-failure
// generic message, and parent-context cancellation mid-run. The
// Abs/Chmod/Write/Sync/Close branches need an OS fault or a mid-call
// race and are deliberately not faked.

// wave3FakeTTSScript writes a shell script that behaves like a Piper
// binary: with a fifo path it signals readiness on the FIFO and then
// blocks in sleep; without one it exits non-zero silently.
func wave3FakeTTSScript(t *testing.T, dir, fifo string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-piper.sh")
	body := "#!/bin/sh\n"
	if fifo != "" {
		body += "printf 'x' > " + fifo + "\n"
		body += "exec sleep 60\n"
	} else {
		body += "exit 3\n"
	}
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return script
}

func wave3TTSModel(t *testing.T, dir string) string {
	t.Helper()
	modelPath := filepath.Join(dir, "model.onnx")
	if err := os.WriteFile(modelPath, []byte("fake-model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelPath+".json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return modelPath
}

func TestWave3TTSSynthesizeOutputPathCheckFails(t *testing.T) {
	dir := t.TempDir()
	p, err := NewPiper("/bin/true", wave3TTSModel(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	// A final component longer than PATH_MAX makes Lstat fail with
	// ENAMETOOLONG (not "not exist") while the output directory
	// itself stays a normal, existing directory.
	output := filepath.Join(dir, strings.Repeat("a", 5000)+".wav")
	err = p.Synthesize(context.Background(), "hello", output)
	if err == nil || err.Error() != "narration output path could not be checked" {
		t.Fatalf("overlong output path error = %v", err)
	}
}

func TestWave3TTSSynthesizeInputCreationFails(t *testing.T) {
	if st, err := os.Stat("/proc/self"); err != nil || !st.IsDir() {
		t.Skip("read-only /proc layout required for this test is unavailable")
	}
	dir := t.TempDir()
	p, err := NewPiper("/bin/true", wave3TTSModel(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Synthesize(context.Background(), "hello", "/proc/self/out.wav")
	if err == nil || err.Error() != "could not create private Piper input file" {
		t.Fatalf("input creation error = %v", err)
	}
}

func TestWave3TTSSynthesizeSilentFailureGenericMessage(t *testing.T) {
	dir := t.TempDir()
	modelPath := wave3TTSModel(t, dir)
	script := wave3FakeTTSScript(t, dir, "")
	p, err := NewPiper(script, modelPath)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out.wav")
	err = p.Synthesize(context.Background(), "hello", output)
	if err == nil || err.Error() != "Piper narration synthesis failed" {
		t.Fatalf("silent failure error = %v, want the exact generic message", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("failed output must be removed, stat = %v", statErr)
	}
}

func TestWave3TTSSynthesizeParentContextCancelledMidRun(t *testing.T) {
	dir := t.TempDir()
	modelPath := wave3TTSModel(t, dir)
	fifo := filepath.Join(dir, "ready.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	script := wave3FakeTTSScript(t, dir, fifo)
	p, err := NewPiper(script, modelPath)
	if err != nil {
		t.Fatal(err)
	}
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
	err = p.Synthesize(ctx, "hello", filepath.Join(dir, "out.wav"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-run cancellation error = %v, want context.Canceled", err)
	}
}
