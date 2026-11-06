package audio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Agent C coverage: the deleted-CWD path-resolution failures, the
// oversized-text rejection, and the short-read WAV header.

// withDeletedCWD chdirs into a temporary directory and removes it, so
// filepath.Abs and os.Getwd fail until the test ends. It restores the
// original working directory via t.Cleanup.
func withDeletedCWD(t *testing.T) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sub); err != nil {
		_ = os.Chdir(orig)
		t.Fatalf("remove current directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func writeAudioFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSynthesizePiperOutputAbsFailsWithDeletedCWD(t *testing.T) {
	dir := t.TempDir()
	text := writeAudioFixture(t, dir, "text.txt", "hello")
	model := writeAudioFixture(t, dir, "voice.onnx", "model")
	withDeletedCWD(t)
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: "/bin/true", TextPath: text, ModelPath: model, Output: "relative/out.wav",
	})
	if err == nil || !strings.Contains(err.Error(), "resolve output path") {
		t.Fatalf("deleted-CWD output err = %v, want the resolve-output-path error", err)
	}
}

func TestSynthesizePiperInputAbsFailsWithDeletedCWD(t *testing.T) {
	dir := t.TempDir()
	model := writeAudioFixture(t, dir, "voice.onnx", "model")
	withDeletedCWD(t)
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: "/bin/true", TextPath: "relative/text.txt", ModelPath: model,
		Output: filepath.Join(dir, "out.wav"),
	})
	if err == nil || !strings.Contains(err.Error(), "resolve text path") {
		t.Fatalf("deleted-CWD text err = %v, want the resolve-text-path error", err)
	}
}

// TestSynthesizePiperUnreadableText proves the text read fails when the
// file cannot be opened. chmod does not affect root, so this only runs
// as a non-root user (CI); it is verified locally by running the package
// tests as the nobody user.
func TestSynthesizePiperUnreadableText(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based unreadability does not affect root")
	}
	dir := t.TempDir()
	text := writeAudioFixture(t, dir, "text.txt", "hello")
	model := writeAudioFixture(t, dir, "voice.onnx", "model")
	if err := os.Chmod(text, 0); err != nil {
		t.Fatal(err)
	}
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: "/bin/true", TextPath: text, ModelPath: model,
		Output: filepath.Join(dir, "out.wav"),
	})
	if err == nil || !strings.Contains(err.Error(), "read narration text") {
		t.Fatalf("unreadable text err = %v, want the read-narration-text error", err)
	}
}

func TestSynthesizePiperRejectsOversizedText(t *testing.T) {
	dir := t.TempDir()
	text := writeAudioFixture(t, dir, "big.txt", strings.Repeat("a", (1<<20)+1))
	model := writeAudioFixture(t, dir, "voice.onnx", "model")
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: "/bin/true", TextPath: text, ModelPath: model,
		Output: filepath.Join(dir, "out.wav"),
	})
	if err == nil || !strings.Contains(err.Error(), "between 1 byte and") {
		t.Fatalf("oversized text err = %v, want the size-range error", err)
	}
}

// TestSynthesizePiperShortSysfsWAVHeader proves the WAV header read
// fails when the Piper binary replaces its output with a symlink to a
// sysfs file: the file stats as a 4096-byte regular file but only holds
// a few bytes, so the 12-byte header read ends early.
func TestSynthesizePiperShortSysfsWAVHeader(t *testing.T) {
	sysfs := "/sys/devices/system/cpu/online"
	info, err := os.Stat(sysfs)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 12 {
		t.Skipf("sysfs fixture %s unavailable: %v", sysfs, err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-piper")
	body := "#!/bin/sh\nprev=\"\"\nfor arg in \"$@\"; do\n  if [ \"$prev\" = \"--output_file\" ]; then rm -f \"$arg\"; ln -s " + sysfs + " \"$arg\"; fi\n  prev=\"$arg\"\ndone\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	text := writeAudioFixture(t, dir, "text.txt", "hello")
	model := writeAudioFixture(t, dir, "voice.onnx", "model")
	err = SynthesizePiper(context.Background(), PiperOptions{
		Binary: script, TextPath: text, ModelPath: model,
		Output: filepath.Join(dir, "out.wav"),
	})
	if err == nil || !strings.Contains(err.Error(), "read Piper WAV header") {
		t.Fatalf("sysfs WAV err = %v, want the WAV-header error", err)
	}
}
