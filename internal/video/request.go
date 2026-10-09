package video

import "strings"

type Shot struct {
	ShotID                string   `json:"shot_id"`
	DurationSeconds       float64  `json:"duration_seconds"`
	Prompt                string   `json:"prompt"`
	NegativePrompt        string   `json:"negative_prompt,omitempty"`
	CameraMovement        string   `json:"camera_movement,omitempty"`
	SubjectAction         string   `json:"subject_action,omitempty"`
	StartState            string   `json:"start_state,omitempty"`
	EndState              string   `json:"end_state,omitempty"`
	SoundRequirements     []string `json:"sound_requirements,omitempty"`
	NarrationSegment      string   `json:"narration_segment,omitempty"`
	RequiredReferences    []Asset  `json:"required_references,omitempty"`
	ContinuityConstraints []string `json:"continuity_constraints,omitempty"`
	Provider              string   `json:"provider,omitempty"`
	Model                 string   `json:"model,omitempty"`
	OutputAsset           *Asset   `json:"output_asset,omitempty"`
}

type VideoRequest struct {
	SchemaVersion       string             `json:"schema_version"`
	ProjectID           string             `json:"project_id"`
	EpisodeID           string             `json:"episode_id,omitempty"`
	Prompt              string             `json:"prompt"`
	NegativePrompt      string             `json:"negative_prompt,omitempty"`
	Mode                Mode               `json:"mode"`
	ProviderPreference  string             `json:"provider_preference,omitempty"`
	ModelPreference     string             `json:"model_preference,omitempty"`
	DurationSeconds     float64            `json:"duration_seconds"`
	AspectRatio         AspectRatio        `json:"aspect_ratio"`
	Width               int                `json:"width,omitempty"`
	Height              int                `json:"height,omitempty"`
	Resolution          string             `json:"resolution,omitempty"`
	FPS                 int                `json:"fps,omitempty"`
	AudioRequested      bool               `json:"audio_requested,omitempty"`
	DialogueRequested   bool               `json:"dialogue_requested,omitempty"`
	Seed                *int64             `json:"seed,omitempty"`
	ReferenceAssets     []Asset            `json:"reference_assets,omitempty"`
	FirstFrameAsset     *Asset             `json:"first_frame_asset,omitempty"`
	LastFrameAsset      *Asset             `json:"last_frame_asset,omitempty"`
	CharacterReferences []Asset            `json:"character_reference_assets,omitempty"`
	LocationReferences  []Asset            `json:"location_reference_assets,omitempty"`
	StyleReferences     []Asset            `json:"style_reference_assets,omitempty"`
	ContinuityContext   *ContinuityContext `json:"continuity_context,omitempty"`
	ShotPlan            []Shot             `json:"shot_plan,omitempty"`
	NarrationText       string             `json:"narration_text,omitempty"`
	SubtitleLanguage    string             `json:"subtitle_language,omitempty"`
	OutputFormat        string             `json:"output_format,omitempty"`
	CallbackURL         string             `json:"callback_url,omitempty"`
	IdempotencyKey      string             `json:"idempotency_key,omitempty"`
	MaxCostUSD          float64            `json:"max_cost_usd,omitempty"`
	DryRun              bool               `json:"dry_run,omitempty"`
	Priority            Priority           `json:"priority,omitempty"`
}

func (r VideoRequest) Normalized() VideoRequest {
	r.SchemaVersion = SchemaVersion
	r.ProjectID = strings.TrimSpace(r.ProjectID)
	r.EpisodeID = strings.TrimSpace(r.EpisodeID)
	r.Prompt = strings.TrimSpace(r.Prompt)
	if r.Mode == "" {
		r.Mode = ModeTextToVideo
	}
	if r.AspectRatio == "" {
		r.AspectRatio = Aspect16x9
	}
	if r.OutputFormat == "" {
		r.OutputFormat = "mp4"
	}
	if r.FPS == 0 {
		r.FPS = 24
	}
	if r.Priority == "" {
		r.Priority = PriorityNormal
	}
	return r
}
