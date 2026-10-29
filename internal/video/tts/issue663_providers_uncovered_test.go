package tts

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeIssue663WAV builds a minimal RIFF WAV with the given chunks in order.
// Each chunk is (id, payload); odd payloads get the RIFF word-alignment pad.
func writeIssue663WAV(t *testing.T, chunks [][2]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "n.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var body []byte
	for _, c := range chunks {
		id := c[0].(string)
		payload := c[1].([]byte)
		body = append(body, id...)
		var sz [4]byte
		binary.LittleEndian.PutUint32(sz[:], uint32(len(payload)))
		body = append(body, sz[:]...)
		body = append(body, payload...)
		if len(payload)%2 == 1 {
			body = append(body, 0)
		}
	}
	header := []byte("RIFF")
	var total [4]byte
	binary.LittleEndian.PutUint32(total[:], uint32(4+len(body)))
	header = append(header, total[:]...)
	header = append(header, "WAVE"...)
	if _, err := f.Write(append(header, body...)); err != nil {
		t.Fatal(err)
	}
	return path
}

func issue663FmtChunk(byteRate uint32) []byte {
	// fmt chunk: audioFormat(2) channels(2) sampleRate(4) byteRate(4) blockAlign(2) bits(2).
	b := make([]byte, 16)
	binary.LittleEndian.PutUint32(b[8:12], byteRate)
	return b
}

func TestIssue663ProvidersWAVDurationChunkShapes(t *testing.T) {
	// Data chunk before the fmt chunk: duration is only returned once the
	// fmt chunk supplies a byte rate, via the post-chunk check.
	path := writeIssue663WAV(t, [][2]any{
		{"data", make([]byte, 16000)},
		{"fmt ", issue663FmtChunk(16000)},
	})
	d, err := WAVDuration(path)
	if err != nil || d != time.Second {
		t.Fatalf("data-before-fmt duration=%v err=%v, want 1s", d, err)
	}
	// Unknown chunk with odd size: padding must be skipped correctly.
	path = writeIssue663WAV(t, [][2]any{
		{"fmt ", issue663FmtChunk(8000)},
		{"JUNK", []byte("abc")},
		{"data", make([]byte, 8000)},
	})
	d, err = WAVDuration(path)
	if err != nil || d != time.Second {
		t.Fatalf("unknown-chunk duration=%v err=%v, want 1s", d, err)
	}
	// fmt chunk larger than 16 bytes: extension bytes are skipped.
	ext := append(issue663FmtChunk(8000), []byte("ext!")...)
	path = writeIssue663WAV(t, [][2]any{
		{"fmt ", ext},
		{"data", make([]byte, 16000)},
	})
	d, err = WAVDuration(path)
	if err != nil || d != 2*time.Second {
		t.Fatalf("extended-fmt duration=%v err=%v, want 2s", d, err)
	}
	// Empty data chunk after a valid fmt chunk is an explicit error.
	path = writeIssue663WAV(t, [][2]any{
		{"fmt ", issue663FmtChunk(8000)},
		{"data", []byte{}},
	})
	if _, err := WAVDuration(path); err == nil || !strings.Contains(err.Error(), "no audio data") {
		t.Fatalf("empty data err=%v, want no audio data", err)
	}
}

func TestIssue663ProvidersWAVDurationRejectsMalformed(t *testing.T) {
	// Not a RIFF file at all.
	path := filepath.Join(t.TempDir(), "bad.wav")
	if err := os.WriteFile(path, []byte("NOPE12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := WAVDuration(path); err == nil || !strings.Contains(err.Error(), "RIFF/WAVE") {
		t.Fatalf("non-RIFF err=%v, want RIFF/WAVE rejection", err)
	}
	// Truncated header.
	path = filepath.Join(t.TempDir(), "short.wav")
	if err := os.WriteFile(path, []byte("RI"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := WAVDuration(path); err == nil || !strings.Contains(err.Error(), "complete WAV") {
		t.Fatalf("short header err=%v, want complete WAV rejection", err)
	}
	// fmt chunk smaller than 16 bytes.
	path = writeIssue663WAV(t, [][2]any{{"fmt ", []byte{1, 2, 3}}})
	if _, err := WAVDuration(path); err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("small fmt err=%v, want too small", err)
	}
	// Declared chunk payload larger than the file: incomplete chunk.
	path = filepath.Join(t.TempDir(), "cut.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("RIFF"))
	var total [4]byte
	binary.LittleEndian.PutUint32(total[:], 100)
	_, _ = f.Write(total[:])
	_, _ = f.Write([]byte("WAVE"))
	_, _ = f.Write([]byte("data"))
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], 1000000)
	_, _ = f.Write(sz[:])
	_ = f.Close()
	if _, err := WAVDuration(path); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("oversized chunk err=%v, want incomplete", err)
	}
	// No fmt and no data at all: missing format/data error.
	path = writeIssue663WAV(t, [][2]any{{"JUNK", []byte("ab")}})
	if _, err := WAVDuration(path); err == nil || !strings.Contains(err.Error(), "missing its format") {
		t.Fatalf("no fmt/data err=%v, want missing format", err)
	}
	// Missing file.
	if _, err := WAVDuration(filepath.Join(t.TempDir(), "nope.wav")); err == nil || !strings.Contains(err.Error(), "could not open") {
		t.Fatalf("missing file err=%v, want could not open", err)
	}
}

func TestIssue663ProvidersBuildSRTAndPiperValidation(t *testing.T) {
	// Empty text and non-positive duration produce no captions.
	if got := BuildSRT("", time.Second); got != nil {
		t.Fatalf("empty text SRT=%q, want nil", got)
	}
	if got := BuildSRT("hello world", 0); got != nil {
		t.Fatalf("zero duration SRT=%q, want nil", got)
	}
	// Sentence boundaries split chunks and timestamps are monotonic.
	srt := string(BuildSRT("Hello world. This is a second sentence that keeps going on and on and on.", 10*time.Second))
	if !strings.Contains(srt, "00:00:00,000 -->") || !strings.Contains(srt, "Hello world.") {
		t.Fatalf("SRT missing first cue: %q", srt)
	}
	if !strings.Contains(srt, "00:00:10,000") {
		t.Fatalf("last SRT cue must end at the full duration: %q", srt)
	}
	// A single very long word is one chunk even when it exceeds the cap.
	srt = string(BuildSRT(strings.Repeat("a", 100), 2*time.Second))
	if !strings.Contains(srt, strings.Repeat("a", 100)) {
		t.Fatalf("long word was split: %q", srt)
	}
	// NewPiper validation branches: missing binary, missing model, missing sidecar.
	if _, err := NewPiper("definitely-not-a-piper-binary-xyz", "x.onnx"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing binary err=%v, want unavailable", err)
	}
	dir := t.TempDir()
	model := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPiper("true", model); err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("missing sidecar err=%v, want sidecar", err)
	}
	if err := os.WriteFile(model+".json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPiper("true", ""); err == nil || !strings.Contains(err.Error(), "model path is required") {
		t.Fatalf("empty model err=%v, want model path required", err)
	}
	// A directory as the model is not a regular file.
	if _, err := NewPiper("true", dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory model err=%v, want regular file", err)
	}
	// Synthesize validation: unconfigured, empty text, existing output.
	var nilPiper *Piper
	if err := nilPiper.Synthesize(context.Background(), "hi", "out.wav"); err == nil {
		t.Fatal("nil Piper synthesize was accepted")
	}
	p, err := NewPiper("true", model)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Synthesize(context.Background(), "   ", filepath.Join(dir, "o.wav")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty text err=%v, want empty", err)
	}
	existing := filepath.Join(dir, "exists.wav")
	if err := os.WriteFile(existing, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Synthesize(context.Background(), "hi", existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing output err=%v, want already exists", err)
	}
	if err := p.Synthesize(context.Background(), "hi", filepath.Join(dir, "no-such-dir", "o.wav")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing output dir err=%v, want does not exist", err)
	}
	// A fake "true" binary produces no output file: the empty-output branch.
	if err := p.Synthesize(context.Background(), "hi", filepath.Join(dir, "made.wav")); err == nil || !strings.Contains(err.Error(), "without producing") {
		t.Fatalf("no-output binary err=%v, want without producing", err)
	}
	// Canceled context fails before any work.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Synthesize(ctx, "hi", filepath.Join(dir, "c.wav")); err == nil {
		t.Fatal("canceled synthesize was accepted")
	}
}

func TestIssue663ProvidersSRTZeroShareAndChunkClamp(t *testing.T) {
	// With a 1ms total duration and many cues, early cues share start=end=0,
	// so the end must be bumped by 1ms instead of emitting an empty cue.
	words := make([]string, 60)
	for i := range words {
		words[i] = "word"
	}
	srt := string(BuildSRT(strings.Join(words, " "), time.Millisecond))
	if !strings.Contains(srt, "00:00:00,001") {
		t.Fatalf("zero-share SRT missing bumped end timestamp: %q", srt)
	}
	// A maxRunes below the floor is clamped to 8, so short words still group.
	chunks := subtitleChunks("aa bb cc dd ee ff gg hh ii jj", 4)
	if len(chunks) == 0 {
		t.Fatal("clamped subtitleChunks returned nothing")
	}
	for _, c := range chunks {
		if len(strings.Fields(c)) > 4 {
			t.Fatalf("clamped chunk holds too many words: %q", c)
		}
	}
	// Sentence-ending punctuation flushes a chunk even mid-capacity.
	chunks = subtitleChunks("one two. three four", 42)
	if len(chunks) != 2 || chunks[0] != "one two." || chunks[1] != "three four" {
		t.Fatalf("sentence flush chunks=%q, want [one two. three four]", chunks)
	}
}

func TestIssue663ProvidersPiperRunErrorsAndDefaultBinary(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	// An empty binary name defaults to "piper" before the lookup.
	p, err := NewPiper("", model)
	if err != nil {
		if !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("default binary err=%v, want unavailable", err)
		}
	} else if p.Binary == "" {
		t.Fatal("default binary produced an empty resolved path")
	}
	// A binary that exits non-zero with no stderr yields the generic error.
	p, err = NewPiper("false", model)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "f.wav")
	if err := p.Synthesize(context.Background(), "hi", out); err == nil || err.Error() != "Piper narration synthesis failed" {
		t.Fatalf("false binary err=%v, want generic synthesis failure", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("failed synthesis left output behind: %v", statErr)
	}
	// A failing binary whose stderr names the model path must not leak it.
	script := filepath.Join(dir, "noisy.sh")
	scriptBody := "#!/bin/sh\necho \"cannot load $2\" >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0755); err != nil {
		t.Fatal(err)
	}
	p, err = NewPiper(script, model)
	if err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(dir, "n.wav")
	err = p.Synthesize(context.Background(), "hi", out)
	if err == nil || !strings.Contains(err.Error(), "[voice-model]") || strings.Contains(err.Error(), model) {
		t.Fatalf("stderr redaction err=%v, want [voice-model] without the real path", err)
	}
	// A binary that never exits is cut off by the caller's context deadline,
	// and the context error (not a synthesis error) is returned.
	sleepScript := filepath.Join(dir, "sleeper.sh")
	if err := os.WriteFile(sleepScript, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	p, err = NewPiper(sleepScript, model)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out = filepath.Join(dir, "s.wav")
	if err := p.Synthesize(ctx, "hi", out); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("sleeping binary err=%v, want deadline exceeded", err)
	}
}
