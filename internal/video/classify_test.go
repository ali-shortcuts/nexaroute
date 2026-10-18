package video

import (
	"strings"
	"testing"
)

// Phase 19 video classification (#510 item 6): the brief's own
// structure decides the job type — deterministically, with reasons.

func TestClassifyBrief(t *testing.T) {
	asset := func(kind string) Asset { return Asset{ID: "a1", Kind: kind, URI: "file:///a"} }
	frame := &Asset{ID: "f1", Kind: "image", URI: "file:///f.png"}
	cases := []struct {
		name    string
		req     VideoRequest
		wantTyp JobType
		wantMod Mode
		reason  string
		asset   bool
	}{
		{"plain prompt", VideoRequest{Prompt: "a scene"}, JobTextToVideo, ModeTextToVideo, "prompt_only", false},
		{"first frame", VideoRequest{Prompt: "animate", FirstFrameAsset: frame}, JobImageToVideo, ModeImageToVideo, "first_frame_asset", true},
		{"last frame only", VideoRequest{Prompt: "animate", LastFrameAsset: frame}, JobImageToVideo, ModeImageToVideo, "last_frame_asset", true},
		{"source video reference", VideoRequest{Prompt: "restyle", ReferenceAssets: []Asset{asset("video")}}, JobEditExtend, ModeVideoToVideo, "source_video_reference", true},
		{"continuity extension", VideoRequest{Prompt: "continue", ContinuityContext: &ContinuityContext{PreviousFinalFrame: frame}}, JobEditExtend, ModeVideoToVideo, "continuity_extension", true},
		{"previous manifest", VideoRequest{Prompt: "next episode", ContinuityContext: &ContinuityContext{PreviousManifest: &Asset{ID: "m"}}}, JobEditExtend, ModeVideoToVideo, "continuity_extension", true},
		{"storyboard", VideoRequest{ShotPlan: []Shot{{ShotID: "1"}, {ShotID: "2"}}}, JobCompose, ModeTextToVideo, "multi_shot_storyboard", false},
		{"single shot is not compose", VideoRequest{Prompt: "x", ShotPlan: []Shot{{ShotID: "1"}}}, JobTextToVideo, ModeTextToVideo, "prompt_only", false},
		{"image reference is conditioning, not image-to-video", VideoRequest{Prompt: "in this style", ReferenceAssets: []Asset{asset("image")}}, JobTextToVideo, ModeTextToVideo, "prompt_only", false},
		{"wardrobe-only continuity is not extension", VideoRequest{Prompt: "x", ContinuityContext: &ContinuityContext{WardrobeDescription: "red coat"}}, JobTextToVideo, ModeTextToVideo, "prompt_only", false},
		{"source video beats frames", VideoRequest{ReferenceAssets: []Asset{asset("video")}, FirstFrameAsset: frame}, JobEditExtend, ModeVideoToVideo, "source_video_reference", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := ClassifyBrief(tc.req)
			if cls.Type != tc.wantTyp || cls.Mode != tc.wantMod {
				t.Fatalf("classify = %s/%s, want %s/%s", cls.Type, cls.Mode, tc.wantTyp, tc.wantMod)
			}
			if len(cls.Reasons) != 1 || cls.Reasons[0] != tc.reason {
				t.Fatalf("reasons = %v, want [%s]", cls.Reasons, tc.reason)
			}
			if cls.AssetEvidence != tc.asset {
				t.Fatalf("assetEvidence = %v, want %v", cls.AssetEvidence, tc.asset)
			}
		})
	}
}

func TestNormalizedDerivesModeFromBrief(t *testing.T) {
	frame := &Asset{ID: "f1", Kind: "image"}
	if got := (VideoRequest{Prompt: "animate", FirstFrameAsset: frame}).Normalized().Mode; got != ModeImageToVideo {
		t.Fatalf("image brief normalized mode = %q, want image_to_video", got)
	}
	if got := (VideoRequest{Prompt: "plain"}).Normalized().Mode; got != ModeTextToVideo {
		t.Fatalf("plain brief normalized mode = %q, want text_to_video", got)
	}
	// An explicitly declared mode is never rewritten by derivation.
	if got := (VideoRequest{Prompt: "x", Mode: ModeTextToVideo}).Normalized().Mode; got != ModeTextToVideo {
		t.Fatalf("declared mode rewritten to %q", got)
	}
}

func TestValidateRejectsDeclaredModeContradictingBrief(t *testing.T) {
	frame := &Asset{ID: "f1", Kind: "image"}
	base := VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 2}
	bad := base
	bad.Mode = ModeTextToVideo
	bad.FirstFrameAsset = frame
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "contradicts the brief") {
		t.Fatalf("text mode + first-frame asset: err = %v, want contradiction", err)
	}
	good := base
	good.Mode = ModeImageToVideo
	good.FirstFrameAsset = frame
	if err := good.Validate(); err != nil {
		t.Fatalf("image mode + first-frame asset must validate: %v", err)
	}
	// Soft classifications never contradict: a declared text mode
	// with a multi-shot storyboard is a valid text job with a plan.
	sb := base
	sb.Mode = ModeTextToVideo
	sb.ShotPlan = []Shot{{ShotID: "1", DurationSeconds: 1, Prompt: "a"}, {ShotID: "2", DurationSeconds: 1, Prompt: "b"}}
	if err := sb.Validate(); err != nil {
		t.Fatalf("declared text mode + storyboard must validate: %v", err)
	}
}

func TestNewJobStampsClassification(t *testing.T) {
	frame := &Asset{ID: "f1", Kind: "image"}
	j := NewJob("job-1", VideoRequest{ProjectID: "p", Prompt: "animate", FirstFrameAsset: frame}.Normalized())
	if j.JobType != JobImageToVideo || len(j.JobTypeReasons) != 1 || j.JobTypeReasons[0] != "first_frame_asset" {
		t.Fatalf("job stamped %q/%v, want image_to_video/[first_frame_asset]", j.JobType, j.JobTypeReasons)
	}
	j2 := NewJob("job-2", VideoRequest{ProjectID: "p", ShotPlan: []Shot{{ShotID: "1"}, {ShotID: "2"}}}.Normalized())
	if j2.JobType != JobCompose {
		t.Fatalf("storyboard job type = %q, want compose", j2.JobType)
	}
}
