package tts

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
)

const maxPiperStderr = 32 << 10

// Piper synthesizes narration with a locally installed Piper CLI and a user-supplied
// ONNX voice model. No shell is used and narration text is passed through a private
// temporary input file rather than the command line.
type Piper struct {
	Binary    string
	ModelPath string
}

// NewPiper validates the executable and the model plus its JSON sidecar at startup.
func NewPiper(binary, modelPath string) (*Piper, error) {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		binary = "piper"
	}
	resolvedBinary, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("Piper executable %q is unavailable", binary)
	}
	modelPath = strings.TrimSpace(modelPath)
	if modelPath == "" {
		return nil, errors.New("Piper model path is required")
	}
	modelPath, err = filepath.Abs(modelPath)
	if err != nil {
		return nil, errors.New("Piper model path could not be resolved")
	}
	info, err := os.Stat(modelPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Piper model must be an existing regular file")
	}
	configPath := modelPath + ".json"
	configInfo, err := os.Stat(configPath)
	if err != nil || !configInfo.Mode().IsRegular() {
		return nil, errors.New("Piper model JSON sidecar is missing or is not a regular file")
	}
	return &Piper{Binary: resolvedBinary, ModelPath: modelPath}, nil
}

// Synthesize writes a WAV file to outputPath. The output must not already exist.
func (p *Piper) Synthesize(ctx context.Context, text, outputPath string) error {
	if p == nil || strings.TrimSpace(p.Binary) == "" || strings.TrimSpace(p.ModelPath) == "" {
		return errors.New("Piper synthesizer is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("narration text is empty")
	}
	outputPath = strings.TrimSpace(outputPath)
	if outputPath == "" {
		return errors.New("narration output path is required")
	}
	outputPath, err := filepath.Abs(outputPath)
	if err != nil {
		return errors.New("narration output path is invalid")
	}
	if info, err := os.Stat(filepath.Dir(outputPath)); err != nil || !info.IsDir() {
		return errors.New("narration output directory does not exist")
	}
	if _, err := os.Lstat(outputPath); err == nil {
		return errors.New("narration output already exists; refusing to overwrite")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("narration output path could not be checked")
	}

	input, err := os.CreateTemp(filepath.Dir(outputPath), ".nexaroute-piper-input-*.txt")
	if err != nil {
		return errors.New("could not create private Piper input file")
	}
	inputPath := input.Name()
	defer os.Remove(inputPath)
	if err := input.Chmod(0600); err != nil {
		_ = input.Close()
		return errors.New("could not secure Piper input file")
	}
	if _, err := io.WriteString(input, text); err != nil {
		_ = input.Close()
		return errors.New("could not write Piper input text")
	}
	if err := input.Sync(); err != nil {
		_ = input.Close()
		return errors.New("could not flush Piper input text")
	}
	if err := input.Close(); err != nil {
		return errors.New("could not close Piper input text")
	}

	cmd := exec.CommandContext(ctx, p.Binary, "-m", p.ModelPath, "-f", outputPath, "--input-file", inputPath)
	stderr := &cappedBuffer{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		_ = os.Remove(outputPath)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		message = strings.ReplaceAll(message, p.ModelPath, "[voice-model]")
		message = strings.ReplaceAll(message, inputPath, "[input-file]")
		message = strings.ReplaceAll(message, outputPath, "[output-file]")
		if message == "" {
			return errors.New("Piper narration synthesis failed")
		}
		return fmt.Errorf("Piper narration synthesis failed: %s", message)
	}
	info, err := os.Stat(outputPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		_ = os.Remove(outputPath)
		return errors.New("Piper completed without producing a non-empty regular output file")
	}
	return nil
}

type cappedBuffer struct {
	bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	remaining := maxPiperStderr - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return originalLength, nil
}
