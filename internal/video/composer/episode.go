package composer

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxSubtitleBytes = 16 << 20

// EpisodeSpec describes the local assembly stage after individual shot videos
// have been generated. All input paths are local files; output is an MP4.
type EpisodeSpec struct {
	Shots           []string `json:"shots"`
	NarrationPath   string   `json:"narration_path,omitempty"`
	MusicPath       string   `json:"music_path,omitempty"`
	SubtitlePath    string   `json:"subtitle_path,omitempty"`
	OutputPath      string   `json:"output_path"`
	Width           int      `json:"width"`
	Height          int      `json:"height"`
	FPS             int      `json:"fps"`
	NarrationVolume float64  `json:"narration_volume,omitempty"`
	MusicVolume     float64  `json:"music_volume,omitempty"`
}

// ComposeEpisode normalizes every shot, concatenates them in the requested
// order, then muxes optional narration/music and burns optional subtitles.
// Temporary artifacts live beside the output and are removed on return.
func (r *Runner) ComposeEpisode(ctx context.Context, spec EpisodeSpec) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if len(spec.Shots) == 0 {
		return Manifest{}, fmt.Errorf("episode requires at least one shot")
	}
	if spec.Width < 64 || spec.Height < 64 || spec.Width > 4096 || spec.Height > 4096 || spec.FPS < 1 || spec.FPS > 120 {
		return Manifest{}, fmt.Errorf("episode dimensions must be 64..4096 pixels and fps must be 1..120")
	}
	narrationVolume := spec.NarrationVolume
	if narrationVolume == 0 {
		narrationVolume = 1
	}
	musicVolume := spec.MusicVolume
	if musicVolume == 0 {
		musicVolume = 0.12
	}
	if narrationVolume < 0 || narrationVolume > 4 || musicVolume < 0 || musicVolume > 4 ||
		math.IsNaN(narrationVolume) || math.IsInf(narrationVolume, 0) ||
		math.IsNaN(musicVolume) || math.IsInf(musicVolume, 0) {
		return Manifest{}, fmt.Errorf("narration and music volume must be between 0 and 4")
	}

	output, err := r.resolvePath(spec.OutputPath)
	if err != nil {
		return Manifest{}, err
	}
	if strings.ToLower(filepath.Ext(output)) != ".mp4" {
		return Manifest{}, fmt.Errorf("episode output must use the .mp4 extension")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return Manifest{}, err
	}
	work, err := os.MkdirTemp(filepath.Dir(output), ".nexaroute-episode-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(work)

	shotPaths := make([]string, 0, len(spec.Shots))
	manifest := Manifest{Commands: make([][]string, 0, len(spec.Shots)+2)}
	for i, rawPath := range spec.Shots {
		input, err := r.resolvePath(rawPath)
		if err != nil {
			return Manifest{}, fmt.Errorf("shot %d: %w", i+1, err)
		}
		if err := requireRegularFile(input, fmt.Sprintf("shot %d", i+1)); err != nil {
			return Manifest{}, err
		}
		if sameLocalPath(input, output) {
			return Manifest{}, fmt.Errorf("episode output must not overwrite a source shot")
		}
		normalized := filepath.Join(work, fmt.Sprintf("shot-%03d.mp4", i+1))
		tmp, err := temporaryOutputPath(normalized)
		if err != nil {
			return Manifest{}, err
		}
		args := []string{
			"-y", "-i", input,
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,fps=%d", spec.Width, spec.Height, spec.Width, spec.Height, spec.FPS),
			"-pix_fmt", "yuv420p", "-c:v", "libx264", "-an", tmp,
		}
		if err := r.Normalize(ctx, input, normalized, spec.Width, spec.Height, spec.FPS); err != nil {
			return Manifest{}, fmt.Errorf("normalize shot %d: %w", i+1, err)
		}
		manifest.Commands = append(manifest.Commands, append([]string{r.Binary}, args...))
		shotPaths = append(shotPaths, normalized)
	}

	joined := filepath.Join(work, "joined.mp4")
	concatManifest, err := r.Concat(ctx, shotPaths, joined)
	if err != nil {
		return Manifest{}, fmt.Errorf("concatenate episode shots: %w", err)
	}
	if len(concatManifest.Commands) >= 2 {
		command := append([]string{r.Binary}, concatManifest.Commands[1]...)
		manifest.Commands = append(manifest.Commands, command)
	}

	narration := strings.TrimSpace(spec.NarrationPath)
	music := strings.TrimSpace(spec.MusicPath)
	subtitles := strings.TrimSpace(spec.SubtitlePath)
	if narration != "" {
		narration, err = r.resolvePath(narration)
		if err != nil {
			return Manifest{}, err
		}
		if err := requireRegularFile(narration, "narration"); err != nil {
			return Manifest{}, err
		}
		if sameLocalPath(narration, output) {
			return Manifest{}, fmt.Errorf("episode output must not overwrite narration")
		}
	}
	if music != "" {
		music, err = r.resolvePath(music)
		if err != nil {
			return Manifest{}, err
		}
		if err := requireRegularFile(music, "music"); err != nil {
			return Manifest{}, err
		}
		if sameLocalPath(music, output) {
			return Manifest{}, fmt.Errorf("episode output must not overwrite music")
		}
	}
	if subtitles != "" {
		subtitles, err = r.resolvePath(subtitles)
		if err != nil {
			return Manifest{}, err
		}
		if err := requireRegularFile(subtitles, "subtitles"); err != nil {
			return Manifest{}, err
		}
		if sameLocalPath(subtitles, output) {
			return Manifest{}, fmt.Errorf("episode output must not overwrite subtitles")
		}
		info, err := os.Stat(subtitles)
		if err != nil {
			return Manifest{}, err
		}
		if info.Size() > maxSubtitleBytes {
			return Manifest{}, fmt.Errorf("subtitle file exceeds %d bytes", maxSubtitleBytes)
		}
		copied := filepath.Join(work, "subtitles.srt")
		if err := copyFile(copied, subtitles); err != nil {
			return Manifest{}, fmt.Errorf("stage subtitles: %w", err)
		}
		subtitles = copied
	}

	tmpOutput, err := temporaryOutputPath(output)
	if err != nil {
		return Manifest{}, err
	}
	defer os.Remove(tmpOutput)
	args := []string{"-y", "-i", joined}
	audioInputs := 0
	narrationIndex, musicIndex := -1, -1
	if narration != "" {
		audioInputs++
		narrationIndex = audioInputs
		args = append(args, "-i", narration)
	}
	if music != "" {
		audioInputs++
		musicIndex = audioInputs
		args = append(args, "-stream_loop", "-1", "-i", music)
	}
	filters := []string{
		fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", spec.Width, spec.Height),
		fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2", spec.Width, spec.Height),
		fmt.Sprintf("fps=%d", spec.FPS),
		"format=yuv420p",
	}
	if subtitles != "" {
		filters = append(filters, "subtitles='"+escapeFilterPath(subtitles)+"'")
	}
	args = append(args, "-map", "0:v:0", "-vf", strings.Join(filters, ","), "-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p")
	if audioInputs == 0 {
		args = append(args, "-an")
	} else {
		var audioFilter string
		switch {
		case narrationIndex >= 0 && musicIndex >= 0:
			audioFilter = fmt.Sprintf("[%d:a]volume=%s,apad[narr];[%d:a]volume=%s,apad[music];[narr][music]amix=inputs=2:duration=longest:dropout_transition=2[aout]",
				narrationIndex, strconv.FormatFloat(narrationVolume, 'f', 3, 64),
				musicIndex, strconv.FormatFloat(musicVolume, 'f', 3, 64))
		case narrationIndex >= 0:
			audioFilter = fmt.Sprintf("[%d:a]volume=%s,apad[aout]", narrationIndex, strconv.FormatFloat(narrationVolume, 'f', 3, 64))
		default:
			audioFilter = fmt.Sprintf("[%d:a]volume=%s,apad[aout]", musicIndex, strconv.FormatFloat(musicVolume, 'f', 3, 64))
		}
		args = append(args, "-filter_complex", audioFilter, "-map", "[aout]", "-c:a", "aac", "-b:a", "192k")
	}
	args = append(args, "-shortest", "-movflags", "+faststart", tmpOutput)
	if err := r.run(ctx, args...); err != nil {
		return Manifest{}, fmt.Errorf("compose episode output: %w", err)
	}
	manifest.Commands = append(manifest.Commands, append([]string{r.Binary}, args...))
	if err := os.Rename(tmpOutput, output); err != nil {
		return Manifest{}, fmt.Errorf("publish episode output: %w", err)
	}
	return manifest, nil
}

func (r *Runner) resolvePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(path) {
		base := strings.TrimSpace(r.WorkDir)
		if base == "" {
			var err error
			base, err = os.Getwd()
			if err != nil {
				return "", err
			}
		}
		path = filepath.Join(base, path)
	}
	return filepath.Abs(filepath.Clean(path))
}

func requireRegularFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s input %q: %w", label, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s input %q is not a regular file", label, path)
	}
	return nil
}

func sameLocalPath(a, b string) bool {
	a, _ = filepath.Abs(filepath.Clean(a))
	b, _ = filepath.Abs(filepath.Clean(b))
	if a == b {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func escapeFilterPath(path string) string {
	path = filepath.ToSlash(path)
	path = strings.ReplaceAll(path, "\\", "\\\\")
	path = strings.ReplaceAll(path, ":", "\\:")
	path = strings.ReplaceAll(path, "'", "\\'")
	return path
}
