package tts

import (
	"context"
	"encoding/binary"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Agent C coverage: the WAV chunk-offset Seek error and the Piper input
// write failure.

// TestWAVDurationPipeSeekFails drives WAVDuration on a FIFO: the header
// and one chunk header read fine, but Seek on a pipe fails with ESPIPE,
// so the chunk-offset inspection must report it.
func TestWAVDurationPipeSeekFails(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "narration.wav")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("Mkfifo: %v", err)
	}
	// Open read-write so neither side blocks, then stage a valid header
	// plus one chunk header for the reader.
	w, err := os.OpenFile(fifo, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	header := make([]byte, 20)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	if _, err := w.Write(header); err != nil {
		t.Fatal(err)
	}
	_, err = WAVDuration(fifo)
	if err == nil || !strings.Contains(err.Error(), "chunk offset") {
		t.Fatalf("pipe WAV err = %v, want the chunk-offset error", err)
	}
}

// TestPiperSynthesizeInputWriteFailsOnFileSizeLimit caps the process file
// size at one byte, so writing the narration text to the private input
// file must fail with EFBIG instead of invoking Piper.
func TestPiperSynthesizeInputWriteFailsOnFileSizeLimit(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewPiper("/bin/true", model)
	if err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	err = p.Synthesize(context.Background(), "hello narration", filepath.Join(dir, "out.wav"))
	if err == nil || !strings.Contains(err.Error(), "could not write Piper input text") {
		t.Fatalf("Synthesize err = %v, want the input-write error", err)
	}
}
