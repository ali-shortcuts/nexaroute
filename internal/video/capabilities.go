package video

// ProviderKind classifies where and how a video provider executes. It is
// the honest label behind "local vs commercial" questions: operators must
// be able to tell at a glance whether a provider runs on their own
// machine, executes deterministically from a model-authored plan, bills a
// third-party API, or is only a test double.
const (
	// ProviderKindLocal runs on the operator's own machine (for example
	// the ComfyUI adapter). No third-party video API is involved and no
	// provider API credentials are required.
	ProviderKindLocal = "local"
	// ProviderKindProgrammatic executes deterministically on local
	// infrastructure from a model-authored plan (for example code-render:
	// a text/coding model plans, the local renderer executes).
	ProviderKindProgrammatic = "programmatic"
	// ProviderKindCommercial is a billed third-party video generation
	// API. No commercial provider adapter is bundled with NexaRoute; the
	// kind exists so a future real adapter can declare itself honestly.
	ProviderKindCommercial = "commercial"
	// ProviderKindTest is a non-production test double (for example the
	// fake provider). It must never be presented to operators or counted
	// as a production provider.
	ProviderKindTest = "test"
)

// Capabilities describes exactly what a video provider can do. Every
// field is a declared, testable claim: the adapter contract conformance
// fixture (internal/video/providers/provider_contract_test.go) verifies
// that each provider's ValidateRequest rejects requests exercising
// capabilities it does not declare, so unsupported features fail closed
// instead of being silently ignored.
type Capabilities struct {
	Modes              []Mode        `json:"modes"`
	Models             []string      `json:"models,omitempty"`
	MaxDurationSeconds float64       `json:"max_duration_seconds,omitempty"`
	Resolutions        []string      `json:"resolutions,omitempty"`
	AspectRatios       []AspectRatio `json:"aspect_ratios,omitempty"`
	// ProviderKind classifies the execution model: local, programmatic,
	// commercial, or test. Additive.
	ProviderKind string `json:"provider_kind,omitempty"`
	// ProductionReady reports whether the provider is a real, supported
	// execution backend. Test doubles always report false and must never
	// be counted as production providers. Always present (no omitempty)
	// so unready providers are explicitly marked false rather than
	// silently omitted.
	ProductionReady bool `json:"production_ready"`
	// SupportsAudio is the legacy coarse flag for provider-side generic
	// audio generation. New integrations should consult the granular
	// flags below; admission treats SupportsAudio as additionally
	// covering dialogue and music/sound requests for backwards
	// compatibility.
	SupportsAudio     bool `json:"supports_audio"`
	SupportsWebhooks  bool `json:"supports_webhooks"`
	SupportsCancel    bool `json:"supports_cancel"`
	MinPollIntervalMS int  `json:"min_poll_interval_ms,omitempty"`
	// SupportsReferenceFrames covers reference / first-frame / last-frame
	// / character / location / style image inputs and continuity context.
	// A provider that cannot consume them must reject such requests.
	SupportsReferenceFrames bool `json:"supports_reference_frames,omitempty"`
	// SupportsClipExtension covers extending an existing clip beyond its
	// current duration.
	SupportsClipExtension bool `json:"supports_clip_extension,omitempty"`
	// SupportsVideoEditing covers transforming or restyling an existing
	// video (video-to-video editing beyond plain generation).
	SupportsVideoEditing bool `json:"supports_video_editing,omitempty"`
	// SupportsMultiShot covers accepting a multi-shot plan in a single
	// provider submission. NexaRoute's orchestrator can still run
	// multi-shot episodes against single-shot providers by submitting
	// each shot separately and composing locally.
	SupportsMultiShot bool `json:"supports_multi_shot,omitempty"`
	// SupportsComposition covers provider-side composition of segments
	// into a single output.
	SupportsComposition bool `json:"supports_composition,omitempty"`
	// SupportsSubtitles covers provider-side subtitle/caption generation
	// or burn-in. Gateway-side SRT composition via the local narration
	// pipeline is separate and gated by the orchestrator.
	SupportsSubtitles bool `json:"supports_subtitles,omitempty"`
	// SupportsNarration covers spoken narration synthesis, whether by a
	// local pipeline (for example Piper) or by the provider itself.
	SupportsNarration bool `json:"supports_narration,omitempty"`
	// SupportsMusicGeneration covers provider-side music and sound-effect
	// generation.
	SupportsMusicGeneration bool `json:"supports_music_generation,omitempty"`
	// SupportsDialogueGeneration covers provider-side dialogue and voice
	// generation.
	SupportsDialogueGeneration bool `json:"supports_dialogue_generation,omitempty"`
}

// KnownProviderKind reports whether kind is one of the declared
// ProviderKind* values. Unknown kinds fail closed: they are never treated
// as production backends.
func KnownProviderKind(kind string) bool {
	switch kind {
	case ProviderKindLocal, ProviderKindProgrammatic, ProviderKindCommercial, ProviderKindTest:
		return true
	default:
		return false
	}
}
