package audio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSynthesizePiperWritesPrivateWAVAtomically(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "story with spaces.txt")
	modelPath := filepath.Join(root, "voice model.onnx")
	outputPath := filepath.Join(root, "narration.wav")
	if err := os.WriteFile(textPath, []byte("Hello from Noora."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelPath, []byte("test model placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	piper := filepath.Join(root, "fake piper")
	script := "#!/bin/sh\nset -eu\nout=\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--output_file\" ]; then shift; out=$1; fi\n  shift\ndone\ncat >/dev/null\nprintf 'RIFF....WAVE' > \"$out\"\n"
	if err := os.WriteFile(piper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	if err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: piper, TextPath: textPath, ModelPath: modelPath, Output: outputPath,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "RIFF....WAVE" {
		t.Fatalf("output=%q", data)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("output permissions=%o, want 600", info.Mode().Perm())
	}
}

func TestSynthesizePiperPreservesExistingOutputOnFailure(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "story.txt")
	modelPath := filepath.Join(root, "voice.onnx")
	outputPath := filepath.Join(root, "narration.wav")
	for path, data := range map[string][]byte{
		textPath: []byte("Hello."), modelPath: []byte("model"), outputPath: []byte("previous output"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	piper := filepath.Join(root, "failing-piper")
	if err := os.WriteFile(piper, []byte("#!/bin/sh\necho 'model error' >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	err := SynthesizePiper(context.Background(), PiperOptions{Binary: piper, TextPath: textPath, ModelPath: modelPath, Output: outputPath})
	if err == nil || !strings.Contains(err.Error(), "model error") {
		t.Fatalf("error=%v, want bounded Piper diagnostic", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "previous output" {
		t.Fatalf("failed synthesis changed existing output: %q", data)
	}
}

func TestSynthesizePiperRejectsInvalidInputs(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "story.wav")
	modelPath := filepath.Join(root, "voice.onnx")
	if err := os.WriteFile(textPath, []byte("Hello."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelPath, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	base := PiperOptions{TextPath: textPath, ModelPath: modelPath, Output: filepath.Join(root, "out.wav"), Binary: "/does/not/exist"}
	if err := SynthesizePiper(context.Background(), base); err == nil {
		t.Fatal("missing Piper binary unexpectedly succeeded")
	}
	badOutput := base
	badOutput.Output = filepath.Join(root, "out.mp3")
	if err := SynthesizePiper(context.Background(), badOutput); err == nil || !strings.Contains(err.Error(), ".wav") {
		t.Fatalf("invalid output extension error=%v", err)
	}
	overwrite := base
	overwrite.Output = textPath
	if err := SynthesizePiper(context.Background(), overwrite); err == nil || !strings.Contains(err.Error(), "must not overwrite") {
		t.Fatalf("overwrite error=%v", err)
	}
	if err := os.WriteFile(textPath, []byte{0xff, 0xfe}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := SynthesizePiper(context.Background(), base); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 error=%v", err)
	}
}

func TestSynthesizePiperHonorsTimeout(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "story.txt")
	modelPath := filepath.Join(root, "voice.onnx")
	piper := filepath.Join(root, "slow-piper")
	for path, data := range map[string][]byte{
		textPath: []byte("Hello."), modelPath: []byte("model"),
		piper: []byte("#!/bin/sh\nsleep 2\n"),
	} {
		mode := os.FileMode(0600)
		if path == piper {
			mode = 0700
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	err := SynthesizePiper(context.Background(), PiperOptions{
		Binary: piper, TextPath: textPath, ModelPath: modelPath,
		Output: filepath.Join(root, "out.wav"), Timeout: 20 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout error=%v", err)
	}
}

