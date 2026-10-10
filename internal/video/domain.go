package video

const SchemaVersion = "video.v1"

type Mode string

const (
	ModeTextToVideo  Mode = "text_to_video"
	ModeImageToVideo Mode = "image_to_video"
	ModeVideoToVideo Mode = "video_to_video"
)

type AspectRatio string

const (
	Aspect16x9   AspectRatio = "16:9"
	Aspect9x16   AspectRatio = "9:16"
	Aspect1x1    AspectRatio = "1:1"
	AspectCustom AspectRatio = "custom"
)

type Priority string

const (
	PriorityInteractive Priority = "interactive"
	PriorityNormal      Priority = "normal"
	PriorityBatch       Priority = "batch"
)

type Asset struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	URI         string            `json:"uri"`
	Filename    string            `json:"filename,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	SizeBytes   int64             `json:"size_bytes,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type ContinuityContext struct {
	PreviousFinalFrame   *Asset  `json:"previous_final_frame,omitempty"`
	CharacterReferences  []Asset `json:"character_references,omitempty"`
	WardrobeDescription  string  `json:"wardrobe_description,omitempty"`
	SignatureProp        string  `json:"signature_prop,omitempty"`
	LocationState        string  `json:"location_state,omitempty"`
	ObjectContinuity     string  `json:"object_continuity,omitempty"`
	TimeOfDayContinuity  string  `json:"time_of_day_continuity,omitempty"`
	ColorGradeContinuity string  `json:"color_grade_continuity,omitempty"`
	VoiceIdentity        string  `json:"voice_identity,omitempty"`
	PreviousManifest     *Asset  `json:"previous_episode_manifest,omitempty"`
}
