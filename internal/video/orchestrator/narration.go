package orchestrator

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/tts"
)

var errNarrationExceedsEpisodeDuration = errors.New("narration exceeds episode duration")

func episodeNarrationText(r video.VideoRequest) string {
	if parent := strings.TrimSpace(r.NarrationText); parent != "" {
		return parent
	}
	segments := make([]string, 0, len(r.ShotPlan))
	for _, shot := range r.ShotPlan {
		if segment := strings.TrimSpace(shot.NarrationSegment); segment != "" {
			segments = append(segments, segment)
		}
	}
	return strings.TrimSpace(strings.Join(segments, "\n"))
}

func (o *Orchestrator) prepareNarration(ctx context.Context, j *video.VideoJob, tempDir string) (string, string, error) {
	text := episodeNarrationText(j.Request)
	if text == "" {
		return "", "", nil
	}
	if o.Narrator == nil {
		return "", "", errors.New("local narration synthesizer is not configured")
	}
	wavPath := filepath.Join(tempDir, "narration.wav")
	if err := o.Narrator.Synthesize(ctx, text, wavPath); err != nil {
		return "", "", fmt.Errorf("synthesize narration: %w", err)
	}
	info, err := os.Stat(wavPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > o.maxAssetBytes() {
		return "", "", errors.New("narration synthesizer did not produce a valid file within the asset size limit")
	}
	duration, err := tts.WAVDuration(wavPath)
	if err != nil {
		return "", "", fmt.Errorf("validate synthesized narration WAV: %w", err)
	}
	requestedDuration := time.Duration(j.Request.DurationSeconds * float64(time.Second))
	if duration > requestedDuration+250*time.Millisecond {
		return "", "", fmt.Errorf("%w: audio is %s but the episode is %s; shorten narration or increase shot durations", errNarrationExceedsEpisodeDuration, duration.Round(time.Millisecond), requestedDuration.Round(time.Millisecond))
	}
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return "", "", errors.New("could not read synthesized narration")
	}
	narrationAsset := video.Asset{
		ID: j.JobID + "-narration", Kind: "audio", Filename: "narration.wav", ContentType: "audio/wav",
		Metadata: map[string]string{"project_id": j.ProjectID, "episode_id": j.EpisodeID, "job_id": j.JobID, "asset_role": "narration"},
	}
	savedNarration, err := o.Assets.Put(ctx, narrationAsset, data)
	if err != nil {
		return "", "", fmt.Errorf("persist narration asset: %w", err)
	}
	if err := validateNarrationAsset(savedNarration, int64(len(data)), o.maxAssetBytes(), "audio/"); err != nil {
		return "", "", fmt.Errorf("validate persisted narration asset: %w", err)
	}
	addSourceAsset(j, savedNarration)

	subtitlePath := ""
	if language := strings.TrimSpace(j.Request.SubtitleLanguage); language != "" {
		subtitleData := tts.BuildSRT(text, duration)
		if len(subtitleData) == 0 {
			return "", "", errors.New("could not generate subtitles from narration text")
		}
		subtitleAsset := video.Asset{
			ID: j.JobID + "-subtitles", Kind: "subtitle", Filename: "narration.srt", ContentType: "application/x-subrip",
			Metadata: map[string]string{"project_id": j.ProjectID, "episode_id": j.EpisodeID, "job_id": j.JobID, "asset_role": "subtitles", "language": language},
		}
		savedSubtitles, err := o.Assets.Put(ctx, subtitleAsset, subtitleData)
		if err != nil {
			return "", "", fmt.Errorf("persist subtitle asset: %w", err)
		}
		if err := validateNarrationAsset(savedSubtitles, int64(len(subtitleData)), o.maxAssetBytes(), "application/"); err != nil {
			return "", "", fmt.Errorf("validate persisted subtitle asset: %w", err)
		}
		addSourceAsset(j, savedSubtitles)
		subtitlePath = savedSubtitles.URI
	}
	if err := o.Store.Update(ctx, *j); err != nil {
		return "", "", fmt.Errorf("persist narration checkpoints: %w", err)
	}
	return savedNarration.URI, subtitlePath, nil
}

func validateNarrationAsset(asset video.Asset, expectedSize, maxBytes int64, contentTypePrefix string) error {
	if strings.TrimSpace(asset.URI) == "" || asset.SizeBytes != expectedSize || asset.SizeBytes <= 0 || asset.SizeBytes > maxBytes || len(asset.SHA256) != 64 || !strings.HasPrefix(strings.ToLower(asset.ContentType), contentTypePrefix) {
		return errors.New("persisted narration asset has invalid URI, content type, size, or SHA-256")
	}
	if _, err := hex.DecodeString(asset.SHA256); err != nil {
		return errors.New("persisted narration asset checksum is malformed")
	}
	return nil
}

func (o *Orchestrator) maxAssetBytes() int64 {
	if o.MaxAssetBytes > 0 {
		return o.MaxAssetBytes
	}
	return 512 << 20
}
