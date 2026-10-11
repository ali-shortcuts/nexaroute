package composer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEpisodeFixture(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want || strings.Contains(arg, want) {
			return true
		}
	}
	return false
}

func TestComposeEpisodeCombinesShotsAudioAndSubtitles(t *testing.T) {
	binary, argsCapture, _ := fakeFFmpeg(t)
	root := t.TempDir()
	shot1 := writeEpisodeFixture(t, filepath.Join(root, "shot-1.mp4"), "clip one")
	shot2 := writeEpisodeFixture(t, filepath.Join(root, "shot-2.mp4"), "clip two")
	narration := writeEpisodeFixture(t, filepath.Join(root, "narration.wav"), "narration")
	music := writeEpisodeFixture(t, filepath.Join(root, "music.wav"), "music")
	subtitles := writeEpisodeFixture(t, filepath.Join(root, "subtitles.srt"), "1\n00:00:00,000 --> 00:00:01,000\nNoora enters Persepolis.\n")
	output := filepath.Join(root, "rendered", "episode.mp4")

	runner := New(binary, "")
	manifest, err := runner.ComposeEpisode(context.Background(), EpisodeSpec{
		Shots: []string{shot1, shot2}, NarrationPath: narration, MusicPath: music,
		SubtitlePath: subtitles, OutputPath: output, Width: 1280, Height: 720, FPS: 30,
		NarrationVolume: 1, MusicVolume: 0.15,
	})
	if err != nil {
		t.Fatalf("ComposeEpisode: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "composed output\n" {
		t.Fatalf("output=%q err=%v", data, err)
	}
	if len(manifest.Commands) != 4 {
		t.Fatalf("manifest command count=%d, want two normalizations, concat, and final mux", len(manifest.Commands))
	}
	args := readLines(t, argsCapture)
	for _, want := range []string{"-filter_complex", "amix=inputs=2", "-map", "[aout]", "-shortest", "+faststart", "subtitles="} {
		if !containsArg(args, want) {
			t.Errorf("final FFmpeg args %q do not include %q", args, want)
		}
	}
	if !containsArg(args, narration) || !containsArg(args, music) {
		t.Fatalf("narration/music inputs missing from final args: %q", args)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".nexaroute-episode-*")); err != nil || len(matches) != 0 {
		t.Fatalf("episode temporary work directories remain: %v err=%v", matches, err)
	}
}

func TestComposeEpisodeSupportsVideoOnlyOutput(t *testing.T) {
	binary, argsCapture, _ := fakeFFmpeg(t)
	root := t.TempDir()
	shot := writeEpisodeFixture(t, filepath.Join(root, "shot.mp4"), "clip")
	output := filepath.Join(root, "episode.mp4")
	runner := New(binary, "")
	if _, err := runner.ComposeEpisode(context.Background(), EpisodeSpec{
		Shots: []string{shot}, OutputPath: output, Width: 640, Height: 360, FPS: 24,
	}); err != nil {
		t.Fatalf("ComposeEpisode video-only: %v", err)
	}
	args := readLines(t, argsCapture)
	if !containsArg(args, "-an") || containsArg(args, "-filter_complex") {
		t.Fatalf("video-only final args=%q", args)
	}
}

func TestComposeEpisodeRejectsInvalidInputsBeforeRendering(t *testing.T) {
	root := t.TempDir()
	shot := writeEpisodeFixture(t, filepath.Join(root, "shot.mp4"), "clip")
	runner := New(filepath.Join(root, "missing-ffmpeg"), "")
	cases := []struct {
		name string
		spec EpisodeSpec
		want string
	}{
		{name: "no shots", spec: EpisodeSpec{OutputPath: filepath.Join(root, "out.mp4"), Width: 640, Height: 360, FPS: 24}, want: "at least one shot"},
		{name: "bad dimensions", spec: EpisodeSpec{Shots: []string{shot}, OutputPath: filepath.Join(root, "out.mp4"), Width: 0, Height: 360, FPS: 24}, want: "dimensions"},
		{name: "missing shot", spec: EpisodeSpec{Shots: []string{filepath.Join(root, "missing.mp4")}, OutputPath: filepath.Join(root, "out.mp4"), Width: 640, Height: 360, FPS: 24}, want: "shot 1 input"},
		{name: "bad extension", spec: EpisodeSpec{Shots: []string{shot}, OutputPath: filepath.Join(root, "out.mkv"), Width: 640, Height: 360, FPS: 24}, want: ".mp4 extension"},
		{name: "same input and output", spec: EpisodeSpec{Shots: []string{shot}, OutputPath: shot, Width: 640, Height: 360, FPS: 24}, want: "must not overwrite a source shot"},
		{name: "invalid volume", spec: EpisodeSpec{Shots: []string{shot}, OutputPath: filepath.Join(root, "out.mp4"), Width: 640, Height: 360, FPS: 24, MusicVolume: 5}, want: "volume must be between"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runner.ComposeEpisode(context.Background(), tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ComposeEpisode error=%v, want containing %q", err, tc.want)
			}
		})
	}
}
