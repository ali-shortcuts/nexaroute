package video

type Capabilities struct {
	Modes              []Mode        `json:"modes"`
	Models             []string      `json:"models,omitempty"`
	MaxDurationSeconds float64       `json:"max_duration_seconds,omitempty"`
	Resolutions        []string      `json:"resolutions,omitempty"`
	AspectRatios       []AspectRatio `json:"aspect_ratios,omitempty"`
	SupportsAudio      bool          `json:"supports_audio"`
	SupportsWebhooks   bool          `json:"supports_webhooks"`
	SupportsCancel     bool          `json:"supports_cancel"`
	MinPollIntervalMS  int           `json:"min_poll_interval_ms,omitempty"`
}
