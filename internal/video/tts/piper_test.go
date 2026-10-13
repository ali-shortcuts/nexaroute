package tts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiperSynthesizeWritesOutputAndDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("test model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "fixture.wav")
	writeTestWAV(t, fixture, 1)
	script := filepath.Join(dir, "fake-piper.sh")
	scriptBody := fmt.Sprintf("#!/bin/sh\nout=''\ninput=''\nwhile [ \"$#\" -gt 0 ]; do\n  case \"$1\" in\n    -f) out=\"$2\"; shift 2 ;;\n    --input-file) input=\"$2\"; shift 2 ;;\n    *) shift ;;\n  esac\ndone\n[ -n \"$out\" ] && [ -s \"$input\" ] || exit 7\ncp %q \"$out\"\n", fixture)
	if err := os.WriteFile(script, []byte(scriptBody), 0700); err != nil {
		t.Fatal(err)
	}
	piper, err := NewPiper(script, model)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "narration.wav")
	if err := piper.Synthesize(context.Background(), "سلام به تخت جمشید", output); err != nil {
		t.Fatal(err)
	}
	duration, err := WAVDuration(output)
	if err != nil || duration != 1_000_000_000 {
		t.Fatalf("output duration=%s err=%v", duration, err)
	}
	if err := piper.Synthesize(context.Background(), "should not overwrite", output); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second synthesis error=%v, want overwrite refusal", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nexaroute-piper-input-") {
			t.Fatalf("temporary input file was not removed: %s", entry.Name())
		}
	}
}

func TestNewPiperRequiresModelAndSidecar(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-piper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPiper(script, model); err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("missing sidecar error=%v", err)
	}
	if _, err := NewPiper(filepath.Join(dir, "missing-binary"), model); err == nil {
		t.Fatal("missing Piper binary accepted")
	}
}

func TestPiperRejectsEmptyTextAndSanitizesCommandErrors(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-piper-fail.sh")
	scriptBody := "#!/bin/sh\necho '" + model + " failed' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0700); err != nil {
		t.Fatal(err)
	}
	piper, err := NewPiper(script, model)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output.wav")
	if err := piper.Synthesize(context.Background(), "  ", output); err == nil {
		t.Fatal("empty narration text accepted")
	}
	err = piper.Synthesize(context.Background(), "narration", output)
	if err == nil || strings.Contains(err.Error(), model) {
		t.Fatalf("command error=%v; expected failure without leaking local model path", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed synthesis left output file, stat error=%v", err)
	}
}

func TestCappedBufferWriteContract(t *testing.T) {
	b := &cappedBuffer{}

	// Small write is stored in full and reports its length.
	n, err := b.Write([]byte("hello"))
	if err != nil || n != 5 || b.String() != "hello" {
		t.Fatalf("small write: n=%d err=%v buf=%q", n, err, b.String())
	}

	// A write larger than the remaining capacity is truncated in storage
	// but must still report the ORIGINAL length so io.Copy callers and the
	// subprocess stderr pump never see a short write or an error.
	big := make([]byte, maxPiperStderr+1024)
	for i := range big {
		big[i] = 'x'
	}
	n, err = b.Write(big)
	if err != nil {
		t.Fatalf("big write error: %v", err)
	}
	if n != len(big) {
		t.Fatalf("big write must report original length %d, got %d", len(big), n)
	}
	if b.Len() != maxPiperStderr {
		t.Fatalf("buffer must cap at %d bytes, got %d", maxPiperStderr, b.Len())
	}

	// Writes after the cap is reached store nothing but still succeed fully.
	n, err = b.Write([]byte("more"))
	if err != nil || n != 4 || b.Len() != maxPiperStderr {
		t.Fatalf("post-cap write: n=%d err=%v len=%d", n, err, b.Len())
	}
}
