package composer

import (
	"context"
	"os/exec"
	"strings"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Preflight verifies that FFmpeg is executable and includes the H.264 encoder
// used by the normalizer. Call this before admitting billable multi-shot jobs.
func (r *Runner) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		return fmt.Errorf("ffmpeg binary is required")
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("ffmpeg executable %q not found: %w", binary, err)
	}
	if output, err := exec.CommandContext(ctx, path, "-version").CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg preflight failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	output, err := exec.CommandContext(ctx, path, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect ffmpeg encoders: %w", err)
	}
	if !strings.Contains(string(output), "libx264") {
		return fmt.Errorf("ffmpeg executable %q does not provide the required libx264 encoder", path)
	}
	return nil
}

type Runner struct {
	Binary  string
	WorkDir string
}
type Manifest struct {
	Commands [][]string `json:"commands"`
}

func New(binary, work string) *Runner {
	if binary == "" {
		binary = "ffmpeg"
	}
	return &Runner{Binary: binary, WorkDir: work}
}
func (r *Runner) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	if r.WorkDir != "" {
		cmd.Dir = r.WorkDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
func (r *Runner) Normalize(ctx context.Context, input, output string, width, height, fps int) error {
	if width < 1 || height < 1 || fps < 1 {
		return fmt.Errorf("invalid output dimensions or fps")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	tmp, err := temporaryOutputPath(output)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := r.run(ctx, "-y", "-i", input, "-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,fps=%d", width, height, width, height, fps), "-pix_fmt", "yuv420p", "-c:v", "libx264", "-an", tmp); err != nil {
		return err
	}
	return os.Rename(tmp, output)
}
func (r *Runner) Concat(ctx context.Context, inputs []string, output string) (Manifest, error) {
	if len(inputs) == 0 {
		return Manifest{}, fmt.Errorf("no input videos")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return Manifest{}, err
	}
	list, err := os.CreateTemp(filepath.Dir(output), "concat-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.Remove(list.Name())
	for _, in := range inputs {
		abs, _ := filepath.Abs(in)
		if _, err = fmt.Fprintf(list, "file '%s'\n", strings.ReplaceAll(abs, "'", "'\\''")); err != nil {
			list.Close()
			return Manifest{}, err
		}
	}
	if err = list.Close(); err != nil {
		return Manifest{}, err
	}
	tmp, err := temporaryOutputPath(output)
	if err != nil {
		return Manifest{}, err
	}
	defer os.Remove(tmp)
	args := []string{"-y", "-f", "concat", "-safe", "0", "-i", list.Name(), "-c", "copy", tmp}
	m := Manifest{Commands: [][]string{{r.Binary}, args}}
	if err = r.run(ctx, args...); err != nil {
		return m, err
	}
	return m, os.Rename(tmp, output)
}
func (m Manifest) JSON() []byte { b, _ := json.MarshalIndent(m, "", "  "); return b }

// temporaryOutputPath preserves the container extension so FFmpeg can infer
// the muxer from the temporary filename before the atomic rename.
func temporaryOutputPath(output string) (string, error) {
	ext := filepath.Ext(output)
	if ext == "" || ext == "." {
		return "", fmt.Errorf("output path must include a media file extension")
	}
	return strings.TrimSuffix(output, ext) + ".tmp" + ext, nil
}
