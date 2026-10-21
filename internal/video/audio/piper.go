package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxNarrationTextBytes = 1 << 20
	maxPiperDiagnostic    = 32 << 10
	defaultPiperTimeout   = 30 * time.Minute
)

// PiperOptions describes local, offline speech synthesis. The Piper executable,
// voice model and model config are user-installed assets; this package downloads
// nothing and never invokes a shell.
type PiperOptions struct {
	Binary    string
	TextPath  string
	ModelPath string
	Output    string
	Timeout   time.Duration
}

// SynthesizePiper creates a WAV narration file from UTF-8 text using a local
// Piper CLI installation. Output is written to a same-directory temporary file
// and atomically renamed only after Piper produces a valid non-empty WAV.
func SynthesizePiper(ctx context.Context, opts PiperOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	binary := strings.TrimSpace(opts.Binary)
	if binary == "" {
		binary = "piper"
	}
	textPath, err := absoluteRegularFile(opts.TextPath, "text")
	if err != nil {
		return err
	}
	modelPath, err := absoluteRegularFile(opts.ModelPath, "voice model")
	if err != nil {
		return err
	}
	output := strings.TrimSpace(opts.Output)
	if output == "" {
		return errors.New("output WAV path is required")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	if !strings.EqualFold(filepath.Ext(output), ".wav") {
		return errors.New("narration output must use the .wav extension")
	}
	if samePath(output, textPath) || samePath(output, modelPath) {
		return errors.New("narration output must not overwrite its text input or voice model")
	}
	info, err := os.Stat(textPath)
	if err != nil {
		return fmt.Errorf("stat narration text: %w", err)
	}
	if info.Size() < 1 || info.Size() > maxNarrationTextBytes {
		return fmt.Errorf("narration text must be between 1 byte and %d bytes", maxNarrationTextBytes)
	}
	text, err := os.ReadFile(textPath)
	if err != nil {
		return fmt.Errorf("read narration text: %w", err)
	}
	if len(text) > maxNarrationTextBytes {
		return fmt.Errorf("narration text exceeds %d bytes", maxNarrationTextBytes)
	}
	if len(bytes.TrimSpace(text)) == 0 {
		return errors.New("narration text is empty")
	}
	// Reject binary/non-UTF-8 input instead of passing arbitrary bytes to TTS.
	if !utf8.Valid(text) || bytes.ContainsRune(text, '\u0000') {
		return errors.New("narration text must be UTF-8 text without NUL bytes")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultPiperTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := filepath.Dir(output)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create narration output directory: %w", err)
	}
	tmpFile, err := os.CreateTemp(dir, ".nexaroute-narration-*.wav")
	if err != nil {
		return fmt.Errorf("create temporary narration output: %w", err)
	}
	tmp := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temporary narration output: %w", err)
	}
	defer os.Remove(tmp)

	cmd := exec.CommandContext(runCtx, binary, "--model", modelPath, "--output_file", tmp)
	cmd.Stdin = bytes.NewReader(text)
	var diagnostics limitedBuffer
	diagnostics.limit = maxPiperDiagnostic
	cmd.Stderr = &diagnostics
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("piper synthesis exceeded timeout %s", timeout)
		}
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		detail := strings.TrimSpace(diagnostics.String())
		if detail != "" {
			return fmt.Errorf("piper synthesis failed: %w: %s", err, detail)
		}
		return fmt.Errorf("piper synthesis failed: %w", err)
	}
	if err := runCtx.Err(); err != nil {
		return err
	}
	if err := validateWAV(tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return fmt.Errorf("set narration output permissions: %w", err)
	}
	if err := os.Rename(tmp, output); err != nil {
		return fmt.Errorf("publish narration output: %w", err)
	}
	return nil
}

func absoluteRegularFile(path, label string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", label, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s path must be a regular file", label)
	}
	if info.Size() < 1 {
		return "", fmt.Errorf("%s file is empty", label)
	}
	return abs, nil
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func validateWAV(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("piper did not create narration output: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat Piper output: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 12 {
		return errors.New("piper output is not a non-empty WAV file")
	}
	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return fmt.Errorf("read Piper WAV header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return errors.New("piper output does not have a RIFF/WAVE header")
	}
	return nil
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return n, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
