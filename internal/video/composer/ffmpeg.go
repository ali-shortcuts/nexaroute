package composer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
	tmp := output + ".tmp"
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
	tmp := output + ".tmp"
	defer os.Remove(tmp)
	args := []string{"-y", "-f", "concat", "-safe", "0", "-i", list.Name(), "-c", "copy", tmp}
	m := Manifest{Commands: [][]string{{r.Binary}, args}}
	if err = r.run(ctx, args...); err != nil {
		return m, err
	}
	return m, os.Rename(tmp, output)
}
func (m Manifest) JSON() []byte { b, _ := json.MarshalIndent(m, "", "  "); return b }
