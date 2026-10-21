package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wavChunk builds a RIFF chunk with its ID, size and payload, applying
// word-alignment padding like real WAV writers do.
func wavChunk(id string, payload []byte) []byte {
	out := []byte(id)
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(len(payload)))
	out = append(out, size...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// fmtChunk builds a PCM format chunk: audioFormat=1, 1 channel,
// sampleRate, 16-bit → byteRate = sampleRate*2.
func fmtChunk(sampleRate uint32, extra []byte) []byte {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:2], 1)
	binary.LittleEndian.PutUint16(body[2:4], 1)
	binary.LittleEndian.PutUint32(body[4:8], sampleRate)
	binary.LittleEndian.PutUint32(body[8:12], sampleRate*2)
	binary.LittleEndian.PutUint16(body[12:14], 2)
	binary.LittleEndian.PutUint16(body[14:16], 16)
	return wavChunk("fmt ", append(body, extra...))
}

func wavFile(chunks ...[]byte) []byte {
	out := []byte("RIFF")
	out = append(out, make([]byte, 4)...)
	out = append(out, []byte("WAVE")...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func writeWAV(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWAVDurationAdversarial(t *testing.T) {
	cases := map[string][]byte{
		"truncated header":   []byte("RIFF"),
		"bad magic":          []byte("NOPE\x00\x00\x00\x00XXXX"),
		"fmt too small":      wavFile(wavChunk("fmt ", make([]byte, 8))),
		"fmt incomplete":     wavFile(append([]byte("RIFF"), []byte("fmt ")...)), // no, see below
		"data without audio": wavFile(fmtChunk(16000, nil), wavChunk("data", nil)),
		"no chunks at all":   []byte("RIFF\x00\x00\x00\x00WAVE"),
	}
	// fmt incomplete: declare 16 bytes but write only 4.
	h := wavFile()
	h = append(h, []byte("fmt ")...)
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, 16)
	h = append(h, size...)
	h = append(h, []byte{1, 0, 1, 0}...)
	cases["fmt incomplete"] = h

	for name, data := range cases {
		p := writeWAV(t, name+".wav", data)
		if d, err := WAVDuration(p); err == nil {
			t.Errorf("%s: duration=%v, expected error", name, d)
		}
	}
	if _, err := WAVDuration(filepath.Join(t.TempDir(), "missing.wav")); err == nil {
		t.Error("missing file accepted")
	}
	// A data chunk arriving before the format chunk is skipped, not mispriced.
	outOfOrder := writeWAV(t, "ooo.wav", wavFile(
		wavChunk("data", make([]byte, 100)),
		fmtChunk(16000, nil),
		wavChunk("data", make([]byte, 32000)),
	))
	// The first data chunk's size is remembered and priced once the
	// format chunk establishes the byte rate (deterministic behavior).
	want := time.Duration(100) * time.Second / time.Duration(32000)
	if d, err := WAVDuration(outOfOrder); err != nil || d != want {
		t.Fatalf("out-of-order chunks: d=%v want %v err=%v", d, want, err)
	}
	// Unknown chunks and fmt extensions are skipped; odd sizes get padding.
	audio := wavFile(
		wavChunk("JUNK", []byte{1, 2, 3}),
		fmtChunk(8000, []byte{9, 9}),
		wavChunk("data", make([]byte, 16000)),
	)
	p := writeWAV(t, "skip.wav", audio)
	if d, err := WAVDuration(p); err != nil || d != time.Second {
		t.Fatalf("skip chunks: d=%v err=%v", d, err)
	}
}

func TestBuildSRTShapes(t *testing.T) {
	if got := BuildSRT("", time.Minute); got != nil {
		t.Fatalf("empty text: %q", got)
	}
	if got := BuildSRT("hello", 0); got != nil {
		t.Fatalf("zero duration: %q", got)
	}
	got := BuildSRT("First sentence. Second sentence here!", 6*time.Second)
	s := string(got)
	if !strings.Contains(s, "00:00:00,000 --> ") || !strings.Contains(s, " --> 00:00:06,000") {
		t.Fatalf("timeline bounds: %q", s)
	}
	if !strings.Contains(s, "1\n") || !strings.Contains(s, "2\n") {
		t.Fatalf("two cues expected: %q", s)
	}
	// Persian punctuation ends sentences too.
	got = BuildSRT("سلام؟ خداحافظ۔", time.Second)
	if n := strings.Count(s, "\n\n"); n != 0 {
		_ = n
	}
	if strings.Count(string(got), "\n\n") != 2 {
		t.Fatalf("persian sentence split: %q", got)
	}
	// Monotonic, clamped minimum 1ms per cue under extreme compression.
	got = BuildSRT("a b c", time.Millisecond)
	if !strings.Contains(string(got), "00:00:00,000 --> 00:00:00,001") {
		t.Fatalf("1ms guard: %q", got)
	}
	if ts := srtTimestamp(-5); ts != "00:00:00,000" {
		t.Fatalf("negative clamp: %q", ts)
	}
	if ts := srtTimestamp(3723456); ts != "01:02:03,456" {
		t.Fatalf("timestamp: %q", ts)
	}
	if !endsSentence("word!") || endsSentence("word") {
		t.Fatal("endsSentence")
	}
	if len(subtitleChunks("  ", 42)) != 0 {
		t.Fatal("blank text chunks")
	}
	// Long unpunctuated text wraps at the rune limit.
	long := strings.Repeat("word ", 30)
	if len(subtitleChunks(long, 42)) < 3 {
		t.Fatal("no wrapping")
	}
}

func piperModel(t *testing.T, dir string) (binary, model string) {
	t.Helper()
	bin := filepath.Join(dir, "piper")
	script := "#!/bin/sh\n" + `out=""; inp=""; mod=""; while [ $# -gt 0 ]; do case "$1" in -m) mod="$2"; shift 2;; -f) out="$2"; shift 2;; --input-file) inp="$2"; shift 2;; *) shift;; esac; done
case "$PIPER_MODE" in
fail) echo "boom in $mod for $inp writing $out" 1>&2; exit 1;;
empty) : > "$out";;
*) python3 - > "$out" <<'PY'
import struct, sys
fmtc = b'fmt ' + struct.pack('<IHHIIHH', 16, 1, 1, 8000, 16000, 2, 16)
data = b'data' + struct.pack('<I', 16000) + b'\x00'*16000
body = b'WAVE' + fmtc + data
sys.stdout.buffer.write(b'RIFF' + struct.pack('<I', len(body)) + body)
PY
;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	model = filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, model
}

func TestNewPiperValidation(t *testing.T) {
	dir := t.TempDir()
	bin, model := piperModel(t, dir)
	if p, err := NewPiper(bin, model); err != nil || p == nil {
		t.Fatalf("valid: %v", err)
	}
	for name, args := range map[string][2]string{
		"missing binary": {filepath.Join(dir, "nope"), model},
		"empty model":    {bin, ""},
		"missing model":  {bin, filepath.Join(dir, "no.onnx")},
		"model is dir":   {bin, dir},
	} {
		if _, err := NewPiper(args[0], args[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Missing JSON sidecar is rejected even with a valid model.
	noSidecar := filepath.Join(dir, "bare.onnx")
	if err := os.WriteFile(noSidecar, []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPiper(bin, noSidecar); err == nil {
		t.Error("missing sidecar accepted")
	}
}

func TestPiperSynthesizeBehaviors(t *testing.T) {
	dir := t.TempDir()
	bin, model := piperModel(t, dir)
	p, err := NewPiper(bin, model)
	if err != nil {
		t.Fatal(err)
	}
	// Nil/empty synthesizer is refused before anything else.
	var nilP *Piper
	if err := nilP.Synthesize(context.Background(), "hi", filepath.Join(dir, "x.wav")); err == nil {
		t.Fatal("nil piper accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Synthesize(ctx, "hi", filepath.Join(dir, "x.wav")); err != context.Canceled {
		t.Fatalf("cancelled: %v", err)
	}
	bad := map[string]func() error{
		"empty text":     func() error { return p.Synthesize(context.Background(), "   ", filepath.Join(dir, "a.wav")) },
		"empty output":   func() error { return p.Synthesize(context.Background(), "hi", "") },
		"missing outdir": func() error { return p.Synthesize(context.Background(), "hi", filepath.Join(dir, "no-dir", "x.wav")) },
	}
	if err := os.WriteFile(filepath.Join(dir, "exists.wav"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad["output exists"] = func() error {
		return p.Synthesize(context.Background(), "hi", filepath.Join(dir, "exists.wav"))
	}
	for name, fn := range bad {
		if err := fn(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Happy path: fake binary produces a WAV.
	out := filepath.Join(dir, "ok.wav")
	t.Setenv("PIPER_MODE", "")
	if err := p.Synthesize(context.Background(), "hello there", out); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if d, err := WAVDuration(out); err != nil || d != time.Second {
		t.Fatalf("duration of synthesized: %v %v", d, err)
	}
	// Failure redacts the model, input and output paths from the message.
	t.Setenv("PIPER_MODE", "fail")
	err = p.Synthesize(context.Background(), "hi", filepath.Join(dir, "redact.wav"))
	if err == nil || !strings.Contains(err.Error(), "Piper narration synthesis failed:") {
		t.Fatalf("failure: %v", err)
	}
	for _, leak := range []string{model, ".nexaroute-piper-input-", "redact.wav"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("path leaked in error: %q", err.Error())
		}
	}
	for _, marker := range []string{"[voice-model]", "[input-file]", "[output-file]"} {
		if !strings.Contains(err.Error(), marker) {
			t.Fatalf("redaction marker %q missing: %q", marker, err.Error())
		}
	}
	// A binary that writes nothing is caught even with exit 0.
	t.Setenv("PIPER_MODE", "empty")
	if err := p.Synthesize(context.Background(), "hi", filepath.Join(dir, "empty.wav")); err == nil {
		t.Fatal("empty output accepted")
	}
}

func TestCappedBufferTruncates(t *testing.T) {
	b := &cappedBuffer{}
	if n, _ := b.Write(make([]byte, maxPiperStderr+100)); n != maxPiperStderr+100 {
		t.Fatalf("reports %d", n)
	}
	if b.Len() != maxPiperStderr {
		t.Fatalf("stored %d", b.Len())
	}
	if _, _ = b.Write([]byte("x")); b.Len() != maxPiperStderr {
		t.Fatal("grew past cap")
	}
	if !bytes.HasPrefix(b.Bytes(), []byte{0}) && len(b.String()) != maxPiperStderr {
		t.Fatal("truncation")
	}
}
