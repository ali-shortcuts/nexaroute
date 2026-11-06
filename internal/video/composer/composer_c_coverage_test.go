package composer

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Agent C coverage: the long-path directory failures, the deleted-CWD
// path resolutions, and the concat list write failure.

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

// longDir builds a directory path of exactly target bytes under
// t.TempDir, using nested components that each fit NAME_MAX.
func longDir(t *testing.T, target int) string {
	t.Helper()
	path := t.TempDir()
	for len(path) < target {
		need := target - len(path)
		comp := need - 1
		if comp > 200 {
			comp = 200
		}
		if comp < 1 {
			break
		}
		path += "/" + strings.Repeat("d", comp)
	}
	if len(path) != target {
		t.Fatalf("directory length = %d, want %d", len(path), target)
	}
	return path
}

func TestComposeEpisodeWorkDirTooLong(t *testing.T) {
	// The output directory is 4090 bytes, so MkdirAll succeeds but the
	// work directory (dir + "/.nexaroute-episode-" + 10 random chars)
	// exceeds PATH_MAX.
	dir := longDir(t, 4090)
	runner := New("/bin/false", "")
	_, err := runner.ComposeEpisode(context.Background(), EpisodeSpec{
		Shots: []string{"shot.mp4"}, OutputPath: filepath.Join(dir, "episode.mp4"),
		Width: 640, Height: 360, FPS: 24,
	})
	if err == nil || !strings.Contains(err.Error(), "no such file or directory") && !strings.Contains(err.Error(), "file name too long") {
		t.Fatalf("long work dir err = %v, want the MkdirTemp failure", err)
	}
}

func TestComposeEpisodeResolvePathFailsWithDeletedCWD(t *testing.T) {
	root := t.TempDir()
	shot := writeEpisodeFixture(t, filepath.Join(root, "shot-1.mp4"), "clip one")
	narration := writeEpisodeFixture(t, filepath.Join(root, "narration.wav"), "narration")
	music := writeEpisodeFixture(t, filepath.Join(root, "music.wav"), "music")
	binary, _, _ := fakeFFmpeg(t)
	runner := New(binary, "")

	t.Run("narration", func(t *testing.T) {
		withDeletedCWD(t)
		_, err := runner.ComposeEpisode(context.Background(), EpisodeSpec{
			Shots: []string{shot}, NarrationPath: "relative/narration.wav",
			OutputPath: filepath.Join(root, "out.mp4"), Width: 640, Height: 360, FPS: 24,
		})
		if err == nil || !strings.Contains(err.Error(), "no such file or directory") {
			t.Fatalf("narration resolve err = %v, want the Getwd failure", err)
		}
	})
	t.Run("music", func(t *testing.T) {
		withDeletedCWD(t)
		_, err := runner.ComposeEpisode(context.Background(), EpisodeSpec{
			Shots: []string{shot}, NarrationPath: narration, MusicPath: "relative/music.wav",
			OutputPath: filepath.Join(root, "out.mp4"), Width: 640, Height: 360, FPS: 24,
		})
		if err == nil || !strings.Contains(err.Error(), "no such file or directory") {
			t.Fatalf("music resolve err = %v, want the Getwd failure", err)
		}
	})
	t.Run("subtitles", func(t *testing.T) {
		withDeletedCWD(t)
		_, err := runner.ComposeEpisode(context.Background(), EpisodeSpec{
			Shots: []string{shot}, NarrationPath: narration, MusicPath: music,
			SubtitlePath: "relative/subtitles.srt",
			OutputPath:   filepath.Join(root, "out.mp4"), Width: 640, Height: 360, FPS: 24,
		})
		if err == nil || !strings.Contains(err.Error(), "no such file or directory") {
			t.Fatalf("subtitles resolve err = %v, want the Getwd failure", err)
		}
	})
}

func TestConcatCreateTempFailsOnLongPath(t *testing.T) {
	// The output directory is 4090 bytes, so MkdirAll succeeds but the
	// concat list (dir + "/concat-" + 10 random chars) exceeds PATH_MAX.
	dir := longDir(t, 4090)
	runner := New("/bin/false", "")
	_, err := runner.Concat(context.Background(), []string{"first.mp4"}, filepath.Join(dir, "final.mp4"))
	if err == nil || !strings.Contains(err.Error(), "file name too long") {
		t.Fatalf("long concat path err = %v, want the CreateTemp failure", err)
	}
}

func TestConcatListWriteFailsOnFileSizeLimit(t *testing.T) {
	runner := New("/bin/false", "")
	root := t.TempDir()
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
	_, err := runner.Concat(context.Background(), []string{filepath.Join(root, "first.mp4")}, filepath.Join(root, "final.mp4"))
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("concat write err = %v, want the file-too-large error", err)
	}
}
