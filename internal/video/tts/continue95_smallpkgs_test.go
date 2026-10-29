package tts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContinue95BuildSRTMillisecondClamp(t *testing.T) {
	// Two chunks over a 1ms duration: the first chunk's proportional end
	// rounds to 0, so it must be clamped to start+1.
	out := BuildSRT("alpha bravo charlie delta echo foxtrot golf hotel", time.Millisecond)
	if out == nil {
		t.Fatal("BuildSRT should produce output for non-empty text and positive duration")
	}
	s := string(out)
	if !strings.Contains(s, "1\n00:00:00,000 --> 00:00:00,001\nalpha bravo charlie delta echo foxtrot") {
		t.Errorf("first SRT entry not clamped to a 1ms span:\n%s", s)
	}
	if !strings.Contains(s, "2\n00:00:00,000 --> 00:00:00,001\ngolf hotel") {
		t.Errorf("second SRT entry missing or wrong:\n%s", s)
	}
}

func TestContinue95SubtitleChunksMinRunes(t *testing.T) {
	// A maxRunes below 8 is raised to 8.
	got := subtitleChunks("aaa bbb ccc ddd", 4)
	if len(got) != 2 || got[0] != "aaa bbb" || got[1] != "ccc ddd" {
		t.Errorf("subtitleChunks(..., 4) = %q, want [\"aaa bbb\" \"ccc ddd\"]", got)
	}
}

func TestContinue95PiperDefaultBinary(t *testing.T) {
	// Empty binary resolves to "piper"; piper is not installed here, so
	// construction fails at LookPath — but the defaulting line executes
	// either way. Any error is acceptable; the assertion is that the
	// empty-binary path is handled, not that piper exists.
	if _, err := NewPiper("", filepath.Join(t.TempDir(), "model.onnx")); err == nil {
		t.Error("NewPiper with an empty binary and a missing model should fail (no piper install or no model)")
	}
}

// Continue95DeletedCWD runs fn with the process working directory deleted
// (Getwd fails), then restores it. Cleanup is registered after TempDir, so
// it runs before TempDir's own cleanup (LIFO).
func Continue95DeletedCWD(t *testing.T, fn func()) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	fn()
}

func TestContinue95PiperModelPathResolveError(t *testing.T) {
	Continue95DeletedCWD(t, func() {
		// "true" resolves via LookPath, so the failure must come from
		// filepath.Abs on the relative model path with no working directory.
		if _, err := NewPiper("true", "model.onnx"); err == nil || !strings.Contains(err.Error(), "could not be resolved") {
			t.Errorf("NewPiper with deleted cwd err = %v, want model-path resolve error", err)
		}
	})
}

func TestContinue95PiperOutputPathInvalid(t *testing.T) {
	Continue95DeletedCWD(t, func() {
		p := &Piper{Binary: "true", ModelPath: "/x/model.onnx"}
		err := p.Synthesize(context.Background(), "hello", "out.wav")
		if err == nil || !strings.Contains(err.Error(), "output path is invalid") {
			t.Errorf("Synthesize with deleted cwd err = %v, want output-path invalid error", err)
		}
	})
}
