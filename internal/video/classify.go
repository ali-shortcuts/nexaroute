package video

import "strings"

// JobType is the classified kind of a video job brief (Phase 19,
// #510 item 6). It is derived deterministically from the brief's
// structure — never from an LLM and never from prompt text — so the
// same brief always classifies the same way.
type JobType string

const (
	JobTextToVideo  JobType = "text_to_video"
	JobImageToVideo JobType = "image_to_video"
	// JobEditExtend covers editing or extending an existing clip:
	// the brief carries a source video or a continuity anchor to a
	// previous episode's output.
	JobEditExtend JobType = "edit_extend"
	// JobCompose is a storyboard-driven brief (two or more shots).
	// Its segments generate as text-to-video and composition stays
	// orchestrator-owned (see Capabilities.SupportsMultiShot), so
	// its routing Mode is text_to_video.
	JobCompose JobType = "compose"
)

// JobClassification is the result of ClassifyBrief: the job type,
// the routing mode it maps to, the reason codes that fired, and
// whether the decision rests on hard asset evidence in the brief
// (as opposed to the absence of evidence). Declared modes are only
// ever contradicted by asset evidence.
type JobClassification struct {
	Type          JobType  `json:"type"`
	Mode          Mode     `json:"mode"`
	Reasons       []string `json:"reasons"`
	AssetEvidence bool     `json:"asset_evidence"`
}

// ClassifyBrief classifies a video job brief by its structure.
// Precedence (first match wins): a source video or a continuity
// extension anchor makes it an edit/extend job; a first- or
// last-frame asset makes it image-to-video; a multi-shot storyboard
// makes it a compose job; anything else is text-to-video. Generic
// reference assets (style/character/location) are conditioning
// inputs, not job-type evidence.
func ClassifyBrief(r VideoRequest) JobClassification {
	for _, a := range r.ReferenceAssets {
		if strings.EqualFold(strings.TrimSpace(a.Kind), "video") {
			return JobClassification{Type: JobEditExtend, Mode: ModeVideoToVideo, Reasons: []string{"source_video_reference"}, AssetEvidence: true}
		}
	}
	if r.ContinuityContext != nil && (r.ContinuityContext.PreviousFinalFrame != nil || r.ContinuityContext.PreviousManifest != nil) {
		return JobClassification{Type: JobEditExtend, Mode: ModeVideoToVideo, Reasons: []string{"continuity_extension"}, AssetEvidence: true}
	}
	if r.FirstFrameAsset != nil || r.LastFrameAsset != nil {
		reason := "first_frame_asset"
		if r.FirstFrameAsset == nil {
			reason = "last_frame_asset"
		}
		return JobClassification{Type: JobImageToVideo, Mode: ModeImageToVideo, Reasons: []string{reason}, AssetEvidence: true}
	}
	if len(r.ShotPlan) >= 2 {
		return JobClassification{Type: JobCompose, Mode: ModeTextToVideo, Reasons: []string{"multi_shot_storyboard"}}
	}
	return JobClassification{Type: JobTextToVideo, Mode: ModeTextToVideo, Reasons: []string{"prompt_only"}}
}

// HasSourceVideo reports whether the brief carries a source-video
// reference asset (an edit input, as opposed to conditioning
// references).
func (r VideoRequest) HasSourceVideo() bool {
	for _, a := range r.ReferenceAssets {
		if strings.EqualFold(strings.TrimSpace(a.Kind), "video") {
			return true
		}
	}
	return false
}

// IsContinuityExtension reports whether the brief extends a
// previous clip or episode (continuity anchors present).
func (r VideoRequest) IsContinuityExtension() bool {
	return r.ContinuityContext != nil && (r.ContinuityContext.PreviousFinalFrame != nil || r.ContinuityContext.PreviousManifest != nil)
}
