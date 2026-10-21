package audio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePiper installs an executable shell stand-in for the Piper CLI.
// The script behavior is keyed off its own filename:
//
//	piper-ok.sh   writes a minimal valid WAV to --output_file and exits 0
//	piper-bad.sh  writes non-WAV bytes to --output_file and exits 0
//	piper-fail.sh writes a diagnostic to stderr and exits 1
//	piper-slow.sh sleeps past any sane test timeout
func fakePiper(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func piperInputs(t *testing.T, dir string) (text, model string) {
	t.Helper()
	text = filepath.Join(dir, "text.txt")
	if err := os.WriteFile(text, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}
	model = filepath.Join(dir, "model.onnx")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	return text, model
}

func TestSynthesizePiperValidation(t *testing.T) {
	dir := t.TempDir()
	text, model := piperInputs(t, dir)
	out := filepath.Join(dir, "out.wav")
	badCases := map[string]PiperOptions{
		"missing text":   {TextPath: filepath.Join(dir, "nope.txt"), ModelPath: model, Output: out},
		"empty text":     {TextPath: "", ModelPath: model, Output: out},
		"text is dir":    {TextPath: dir, ModelPath: model, Output: out},
		"empty model":    {TextPath: text, ModelPath: "", Output: out},
		"model missing":  {TextPath: text, ModelPath: filepath.Join(dir, "no.onnx"), Output: out},
		"empty output":   {TextPath: text, ModelPath: model, Output: ""},
		"non-wav output": {TextPath: text, ModelPath: model, Output: filepath.Join(dir, "out.mp3")},
		"output==text":   {TextPath: text, ModelPath: model, Output: text},
		"output==model":  {TextPath: text, ModelPath: model, Output: model + ".wav"},
	}
	// output==model exact: model itself has .wav name.
	modelWav := filepath.Join(dir, "m.wav")
	if err := os.WriteFile(modelWav, []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	badCases["output==model-exact"] = PiperOptions{TextPath: text, ModelPath: modelWav, Output: modelWav}
	for name, opts := range badCases {
		if err := SynthesizePiper(context.Background(), opts); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// Cancelled context is refused before touching the disk.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SynthesizePiper(ctx, PiperOptions{TextPath: text, ModelPath: model, Output: out}); err != context.Canceled {
		t.Fatalf("cancelled: %v", err)
	}
	// Empty text file rejected.
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SynthesizePiper(context.Background(), PiperOptions{TextPath: empty, ModelPath: model, Output: out}); err == nil {
		t.Fatal("empty text file accepted")
	}
	// Binary/non-UTF-8 text rejected instead of being passed to TTS.
	binary := filepath.Join(dir, "bin.txt")
	if err := os.WriteFile(binary, []byte{0xff, 0xfe, 0x00, 'a'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SynthesizePiper(context.Background(), PiperOptions{TextPath: binary, ModelPath: model, Output: out}); err == nil {
		t.Fatal("non-UTF-8 text accepted")
	}
	// Oversize text rejected before synthesis.
	huge := filepath.Join(dir, "huge.txt")
	if err := os.WriteFile(huge, make([]byte, maxNarrationTextBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SynthesizePiper(context.Background(), PiperOptions{TextPath: huge, ModelPath: model, Output: out}); err == nil {
		t.Fatal("oversize text accepted")
	}
}

func TestSynthesizePiperSuccess(t *testing.T) {
	dir := t.TempDir()
	text, model := piperInputs(t, dir)
	bin := fakePiper(t, "piper-ok.sh", `out=""; while [ $# -gt 0 ]; do case "$1" in --output_file) out="$2"; shift 2;; *) shift;; esac; done
python3 - "$out" <<'PY' > "$out"
import sys
sys.stdout.buffer.write(b'RIFF\x24\x00\x00\x00WAVE' + b'\x00'*32)
PY`)
	out := filepath.Join(dir, "narration.wav")
	if err := SynthesizePiper(context.Background(), PiperOptions{Binary: bin, TextPath: text, ModelPath: model, Output: out}); err != nil {
		t.Fatalf("success: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatal("output is not a WAV")
	}
	if err := validateWAV(out); err != nil {
		t.Fatalf("validateWAV(happy): %v", err)
	}
}

func TestSynthesizePiperAdversarial(t *testing.T) {
	dir := t.TempDir()
	text, model := piperInputs(t, dir)
	// A "synthesizer" that produces non-WAV bytes: caught by validateWAV.
	bad := fakePiper(t, "piper-bad.sh", `out=""; while [ $# -gt 0 ]; do case "$1" in --output_file) out="$2"; shift 2;; *) shift;; esac; done
printf 'NOT A WAV AT ALL' > "$out"`)
	if err := SynthesizePiper(context.Background(), PiperOptions{Binary: bad, TextPath: text, ModelPath: model, Output: filepath.Join(dir, "bad.wav")}); err == nil {
		t.Fatal("non-WAV output accepted")
	}
	// A failing synthesizer with stderr: the diagnostic is in the error.
	fail := fakePiper(t, "piper-fail.sh", `echo "voice model checksum mismatch" 1>&2; exit 1`)
	err := SynthesizePiper(context.Background(), PiperOptions{Binary: fail, TextPath: text, ModelPath: model, Output: filepath.Join(dir, "f.wav")})
	if err == nil || !strings.Contains(err.Error(), "voice model checksum mismatch") {
		t.Fatalf("diagnostic lost: %v", err)
	}
	// A hanging synthesizer: the timeout bites.
	slow := fakePiper(t, "piper-slow.sh", `sleep 30`)
	err = SynthesizePiper(context.Background(), PiperOptions{Binary: slow, TextPath: text, ModelPath: model, Output: filepath.Join(dir, "s.wav"), Timeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout not enforced: %v", err)
	}
}

func TestValidateWAVAdversarial(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"too short": make([]byte, 5),
		"bad magic": []byte("NOPE\x00\x00\x00\x00XXXX\x00\x00\x00\x00"),
	}
	for name, data := range cases {
		p := filepath.Join(dir, name+".wav")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := validateWAV(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := validateWAV(filepath.Join(dir, "missing.wav")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestLimitedBufferCapsDiagnostics(t *testing.T) {
	var b limitedBuffer
	b.limit = 8
	if n, _ := b.Write([]byte("0123456789abcdef")); n != 16 {
		t.Fatalf("Write reports %d, want 16 (counts accepted bytes)", n)
	}
	if got := b.String(); got != "01234567" {
		t.Fatalf("buffer=%q", got)
	}
	// Writes after the cap keep reporting acceptance without growing.
	if _, _ = b.Write([]byte("more")); b.String() != "01234567" {
		t.Fatalf("buffer grew: %q", b.String())
	}
}

func TestAbsoluteRegularFileBranches(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := absoluteRegularFile(f, "f"); err != nil || got == "" {
		t.Fatalf("valid: %v %q", err, got)
	}
	for name, path := range map[string]string{
		"empty":   "",
		"missing": filepath.Join(dir, "nope"),
		"dir":     dir,
	} {
		if _, err := absoluteRegularFile(path, "f"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := absoluteRegularFile(empty, "f"); err == nil {
		t.Error("empty accepted")
	}
}
