package orchestrator

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Phase 19 (#510 item 6): edit/extend briefs reach only adapters
// that declare the granular capabilities — the video_to_video
// mode alone is not enough.
func editCaps() video.Capabilities {
	return video.Capabilities{
		Modes:  []video.Mode{video.ModeTextToVideo, video.ModeImageToVideo, video.ModeVideoToVideo},
		Models: []string{"model-a"}, MaxDurationSeconds: 60,
		AspectRatios: []video.AspectRatio{video.Aspect16x9},
		ProviderKind: video.ProviderKindTest,
		// Reference inputs are admitted so the edit/extend checks
		// are the ones under test.
		SupportsReferenceFrames: true,
	}
}

func TestAdmissionEditExtendCapabilityGates(t *testing.T) {
	sourceVideo := video.VideoRequest{
		ProjectID: "p", Prompt: "restyle this", DurationSeconds: 2, Mode: video.ModeVideoToVideo,
		ReferenceAssets: []video.Asset{{ID: "src", Kind: "video", URI: "file:///src.mp4"}},
	}.Normalized()
	if _, err := validateProviderRequest(sourceVideo, editCaps()); err == nil || !strings.Contains(err.Error(), "video editing") {
		t.Fatalf("source-video brief without editing support: err=%v", err)
	}
	caps := editCaps()
	caps.SupportsVideoEditing = true
	if _, err := validateProviderRequest(sourceVideo, caps); err != nil {
		t.Fatalf("source-video brief with editing support must admit: %v", err)
	}

	frame := &video.Asset{ID: "f", Kind: "image"}
	extension := video.VideoRequest{
		ProjectID: "p", Prompt: "continue", DurationSeconds: 2, Mode: video.ModeVideoToVideo,
		ContinuityContext: &video.ContinuityContext{PreviousFinalFrame: frame},
	}.Normalized()
	if _, err := validateProviderRequest(extension, editCaps()); err == nil || !strings.Contains(err.Error(), "clip extension") {
		t.Fatalf("extension brief without extension support: err=%v", err)
	}
	caps2 := editCaps()
	caps2.SupportsClipExtension = true
	if _, err := validateProviderRequest(extension, caps2); err != nil {
		t.Fatalf("extension brief with extension support must admit: %v", err)
	}
}
