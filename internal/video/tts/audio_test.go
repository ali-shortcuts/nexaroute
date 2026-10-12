package tts

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestWAV(t *testing.T, path string, seconds int) {
	t.Helper()
	const sampleRate = uint32(22050)
	const channels = uint16(1)
	const bitsPerSample = uint16(16)
	dataSize := sampleRate * uint32(channels) * uint32(bitsPerSample/8) * uint32(seconds)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36+dataSize)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], channels)
	binary.LittleEndian.PutUint32(header[24:28], sampleRate)
	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample/8)
	binary.LittleEndian.PutUint32(header[28:32], byteRate)
	binary.LittleEndian.PutUint16(header[32:34], channels*bitsPerSample/8)
	binary.LittleEndian.PutUint16(header[34:36], bitsPerSample)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataSize)
	if _, err := file.Write(header); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(make([]byte, dataSize)); err != nil {
		t.Fatal(err)
	}
}

func TestWAVDurationAndSRT(t *testing.T) {
	wav := filepath.Join(t.TempDir(), "narration.wav")
	writeTestWAV(t, wav, 2)
	duration, err := WAVDuration(wav)
	if err != nil {
		t.Fatal(err)
	}
	if duration != 2*time.Second {
		t.Fatalf("WAV duration=%s, want 2s", duration)
	}
	srt := string(BuildSRT("سلام دنیا. به تخت جمشید خوش آمدید!", duration))
	if !strings.Contains(srt, "1\n00:00:00,000 -->") || !strings.Contains(srt, "سلام دنیا.") || !strings.Contains(srt, "به تخت جمشید خوش آمدید!") {
		t.Fatalf("unexpected SRT output:\n%s", srt)
	}
	if !strings.Contains(srt, "2\n") {
		t.Fatalf("expected at least two caption cues:\n%s", srt)
	}
}

func TestWAVDurationRejectsInvalidAndTruncatedFiles(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.wav")
	if err := os.WriteFile(invalid, []byte("not a wave"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := WAVDuration(invalid); err == nil {
		t.Fatal("invalid WAV accepted")
	}
	truncated := filepath.Join(dir, "truncated.wav")
	if err := os.WriteFile(truncated, []byte("RIFF0000WAVEfmt "), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := WAVDuration(truncated); err == nil {
		t.Fatal("truncated WAV accepted")
	}
}

func TestBuildSRTHandlesEmptyTextAndDuration(t *testing.T) {
	if got := BuildSRT("  ", time.Second); len(got) != 0 {
		t.Fatalf("empty text produced SRT: %q", got)
	}
	if got := BuildSRT("hello", 0); len(got) != 0 {
		t.Fatalf("zero duration produced SRT: %q", got)
	}
}
