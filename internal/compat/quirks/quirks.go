package quirks

import "strings"

// Dialect profiles capture per-provider differences without scattering
// `if provider == "x"` checks through the codebase. See spec section 15.
//
// Provider != Protocol, Protocol != Dialect: several providers share the
// OpenAI Chat protocol while applying only their own quirk deltas.
type DialectProfile struct {
	Name string `json:"name"`
	// AuthStyle is bearer | x-api-key | x-goog-api-key | none.
	AuthStyle string `json:"auth_style"`
	// EndpointStyle documents path conventions (openai | anthropic).
	EndpointStyle string `json:"endpoint_style"`
	// DropTemperature means the dialect rejects temperature outright.
	DropTemperature bool `json:"drop_temperature"`
	// MaxTokensField selects the output-token field: max_tokens |
	// max_completion_tokens | either.
	MaxTokensField string `json:"max_tokens_field"`
	// SupportsStreamOptions reports stream_options/include_usage support.
	SupportsStreamOptions bool `json:"supports_stream_options"`
	// SupportsParallelTools reports parallel_tool_calls support.
	SupportsParallelTools bool `json:"supports_parallel_tools"`
	// ReasoningField selects the reasoning control: none | reasoning_effort | thinking.
	ReasoningField string `json:"reasoning_field"`
	// StrictToolNames requires sanitized tool names.
	StrictToolNames bool `json:"strict_tool_names"`
	// Notes carry human-readable operator guidance.
	Notes string `json:"notes"`
}

// Version identifies the dialect implementation for capability-cache
// invalidation (spec section 18).
const Version = "v1"

var registry = map[string]DialectProfile{
	"generic_openai": {
		Name: "generic_openai", AuthStyle: "bearer", EndpointStyle: "openai",
		MaxTokensField: "either", SupportsStreamOptions: true,
		SupportsParallelTools: true, ReasoningField: "reasoning_effort",
		Notes: "Baseline OpenAI Chat Completions behavior.",
	},
	"generic_anthropic": {
		Name: "generic_anthropic", AuthStyle: "x-api-key", EndpointStyle: "anthropic",
		MaxTokensField: "max_tokens", SupportsStreamOptions: false,
		SupportsParallelTools: true, ReasoningField: "thinking",
		Notes: "Baseline Anthropic Messages behavior.",
	},
	"generic_gemini": {
		Name: "generic_gemini", AuthStyle: "x-goog-api-key", EndpointStyle: "gemini",
		MaxTokensField: "max_tokens", SupportsStreamOptions: false,
		SupportsParallelTools: false, ReasoningField: "none",
		Notes: "Google Gemini API (v1beta generateContent): x-goog-api-key auth, per-model action paths.",
	},
	"nvidia_nim": {
		Name: "nvidia_nim", AuthStyle: "bearer", EndpointStyle: "openai",
		MaxTokensField: "max_tokens", SupportsStreamOptions: false,
		SupportsParallelTools: true, ReasoningField: "reasoning_effort",
		StrictToolNames: true,
		Notes:           "NVIDIA NIM speaks OpenAI Chat but varies per model: some models reject temperature, stream_options, or reasoning controls. Prefer max_tokens over max_completion_tokens.",
	},
	"deepseek": {
		Name: "deepseek", AuthStyle: "bearer", EndpointStyle: "openai",
		MaxTokensField: "max_tokens", SupportsStreamOptions: false,
		SupportsParallelTools: false, ReasoningField: "none",
		Notes: "DeepSeek chat models accept OpenAI Chat; reasoning models expose thinking via content, not reasoning_effort.",
	},
	"openrouter": {
		Name: "openrouter", AuthStyle: "bearer", EndpointStyle: "openai",
		MaxTokensField: "either", SupportsStreamOptions: true,
		SupportsParallelTools: true, ReasoningField: "reasoning_effort",
		Notes: "OpenRouter routes to heterogeneous backends; per-model capability variance is expected.",
	},
	"together": {
		Name: "together", AuthStyle: "bearer", EndpointStyle: "openai",
		MaxTokensField: "max_tokens", SupportsStreamOptions: false,
		SupportsParallelTools: true, ReasoningField: "none",
		Notes: "Together inference endpoints vary in tool/stream support per model.",
	},
	"groq": {
		Name: "groq", AuthStyle: "bearer", EndpointStyle: "openai",
		MaxTokensField: "either", SupportsStreamOptions: true,
		SupportsParallelTools: true, ReasoningField: "reasoning_effort",
		Notes: "Groq is broadly OpenAI-compatible with per-model reasoning differences.",
	},
	"custom": {
		Name: "custom", AuthStyle: "bearer", EndpointStyle: "openai",
		MaxTokensField: "either", SupportsStreamOptions: false,
		SupportsParallelTools: true, ReasoningField: "none",
		Notes: "Conservative defaults for unknown OpenAI-compatible endpoints.",
	},
}

// Get returns the dialect profile by name, defaulting to generic_openai.
func Get(name string) DialectProfile {
	if p, ok := registry[strings.TrimSpace(name)]; ok {
		return p
	}
	return registry["generic_openai"]
}

// Names lists registered dialect names.
func Names() []string {
	return []string{"generic_openai", "generic_anthropic", "generic_gemini", "nvidia_nim", "deepseek", "openrouter", "together", "groq", "custom"}
}

// Infer guesses the dialect from provider identity signals (explicit config,
// preset ID, base URL). Explicit configuration always wins; fingerprints are
// conservative and fall back to the generic profile of the provider type.
func Infer(explicit, providerID, baseURL, providerType string) DialectProfile {
	if p, ok := registry[strings.TrimSpace(explicit)]; ok {
		return p
	}
	hay := strings.ToLower(providerID + " " + baseURL)
	switch {
	case strings.Contains(hay, "nvidia"):
		return registry["nvidia_nim"]
	case strings.Contains(hay, "deepseek"):
		return registry["deepseek"]
	case strings.Contains(hay, "openrouter"):
		return registry["openrouter"]
	case strings.Contains(hay, "together"):
		return registry["together"]
	case strings.Contains(hay, "groq"):
		return registry["groq"]
	case strings.Contains(hay, "gemini"), strings.Contains(hay, "generativelanguage"):
		return registry["generic_gemini"]
	}
	if strings.TrimSpace(providerType) == "anthropic_compatible" {
		return registry["generic_anthropic"]
	}
	if strings.TrimSpace(providerType) == "gemini" {
		return registry["generic_gemini"]
	}
	return registry["generic_openai"]
}
