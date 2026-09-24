package compat

import (
	"strings"
)

// DialectProfile captures how one provider family deviates from the plain
// OpenAI/Anthropic shapes. Code must never switch on provider IDs; dialects
// carry the differences (spec section 15: Provider != Protocol != Dialect).
type DialectProfile struct {
	// Name identifies the profile in config, evidence and UI.
	Name string `json:"name"`
	// Protocol is the wire protocol family: openai_chat | anthropic |
	// openai_responses | gemini.
	Protocol string `json:"protocol"`
	// MaxTokensKey is the preferred token-limit key for this dialect:
	// max_tokens | max_completion_tokens | max_output_tokens.
	MaxTokensKey string `json:"max_tokens_key"`
	// ParameterAliases renames canonical/OpenAI parameter names to the
	// dialect's accepted names (e.g. max_completion_tokens -> max_tokens).
	ParameterAliases map[string]string `json:"parameter_aliases,omitempty"`
	// StreamOptionsSupport is the dialect prior for stream_options.
	StreamOptionsSupport Support `json:"stream_options_support"`
	// ReasoningStyle describes how reasoning output is exposed:
	// "" | reasoning_content (OpenAI-style field) | anthropic (thinking blocks).
	ReasoningStyle string `json:"reasoning_style,omitempty"`
	// ModelListPath is the model discovery path (relative to base URL).
	ModelListPath string `json:"model_list_path,omitempty"`
	// AuthStyle: bearer | x-api-key | x-goog-api-key | none.
	AuthStyle string `json:"auth_style,omitempty"`
	// RejectsUnknownFields marks dialects known to 400 on any unrecognized
	// top-level request field; the sanitizer is stricter for them.
	RejectsUnknownFields bool `json:"rejects_unknown_fields,omitempty"`
	// TemperatureRange, when set, bounds the accepted temperature.
	TemperatureMax *float64 `json:"temperature_max,omitempty"`
	// Priors are default capability assumptions applied to every model of
	// the dialect until evidence overrides them. They are seeds only:
	// never treated as verified.
	Priors ModelCapabilities `json:"priors,omitempty"`
}

// Well-known dialect names.
const (
	DialectGenericOpenAI  = "generic_openai"
	DialectGenericAnthro  = "generic_anthropic"
	DialectNvidiaNIM      = "nvidia_nim"
	DialectDeepSeek       = "deepseek"
	DialectOpenRouter     = "openrouter"
	DialectGroq           = "groq"
	DialectTogether       = "together"
	DialectOpenAI         = "openai"
	DialectAnthropic      = "anthropic"
	DialectGemini         = "gemini"
	DialectOpenAIResponse = "openai_responses"
)

func f64(v float64) *float64 { return &v }

// Dialects returns the built-in dialect registry.
func Dialects() map[string]DialectProfile {
	return map[string]DialectProfile{
		DialectGenericOpenAI: {
			Name: DialectGenericOpenAI, Protocol: "openai_chat",
			MaxTokensKey: "max_tokens", AuthStyle: "bearer",
			ModelListPath: "/v1/models", StreamOptionsSupport: UnknownSupport,
		},
		DialectGenericAnthro: {
			Name: DialectGenericAnthro, Protocol: "anthropic",
			MaxTokensKey: "max_tokens", AuthStyle: "x-api-key",
			ModelListPath: "/v1/models", StreamOptionsSupport: UnknownSupport,
		},
		DialectOpenAI: {
			Name: DialectOpenAI, Protocol: "openai_chat",
			MaxTokensKey: "max_completion_tokens", AuthStyle: "bearer",
			ModelListPath: "/v1/models", StreamOptionsSupport: Supported,
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, ToolChoiceAuto: Supported, ToolChoiceRequired: Supported,
				ParallelToolCalls: Supported, Temperature: Supported, TopP: Supported,
				Stop: Supported, MaxTokens: Supported, MaxCompletionTokens: Supported,
			},
		},
		DialectAnthropic: {
			Name: DialectAnthropic, Protocol: "anthropic",
			MaxTokensKey: "max_tokens", AuthStyle: "x-api-key",
			ModelListPath: "/v1/models", StreamOptionsSupport: UnknownSupport,
			ReasoningStyle: "anthropic",
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, ToolChoiceAuto: Supported, ToolChoiceRequired: Supported,
				ParallelToolCalls: Supported, Temperature: Supported, TopP: Supported,
				Stop: Supported, MaxTokens: Supported,
			},
		},
		DialectNvidiaNIM: {
			Name: DialectNvidiaNIM, Protocol: "openai_chat",
			MaxTokensKey: "max_tokens", AuthStyle: "bearer",
			ModelListPath: "/v1/models",
			// NVIDIA NIM is a multi-tenant zoo: host-level endpoints accept
			// OpenAI fields, individual models reject various subsets (notably
			// temperature on reasoning builds and stream_options on several
			// paths). Priors stay conservative; probes decide per model.
			StreamOptionsSupport: UnknownSupport,
			ReasoningStyle:       "reasoning_content",
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported, MaxTokens: Supported,
			},
		},
		DialectDeepSeek: {
			Name: DialectDeepSeek, Protocol: "openai_chat",
			MaxTokensKey: "max_tokens", AuthStyle: "bearer",
			ModelListPath:        "/v1/models",
			StreamOptionsSupport: Supported,
			ReasoningStyle:       "reasoning_content",
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, ToolChoiceAuto: Supported, Temperature: Supported,
				TopP: Supported, Stop: Supported, MaxTokens: Supported,
			},
		},
		DialectOpenRouter: {
			Name: DialectOpenRouter, Protocol: "openai_chat",
			MaxTokensKey: "max_tokens", AuthStyle: "bearer",
			ModelListPath: "/v1/models", StreamOptionsSupport: Supported,
			ReasoningStyle: "reasoning_content",
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, ToolChoiceAuto: Supported, Temperature: Supported,
				TopP: Supported, Stop: Supported, MaxTokens: Supported,
			},
		},
		DialectGroq: {
			Name: DialectGroq, Protocol: "openai_chat",
			MaxTokensKey: "max_completion_tokens", AuthStyle: "bearer",
			ModelListPath: "/openai/v1/models", StreamOptionsSupport: Supported,
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, ToolChoiceAuto: Supported, Temperature: Supported,
				TopP: Supported, Stop: Supported, MaxTokens: Supported,
				MaxCompletionTokens: Supported,
			},
		},
		DialectTogether: {
			Name: DialectTogether, Protocol: "openai_chat",
			MaxTokensKey: "max_tokens", AuthStyle: "bearer",
			ModelListPath: "/v1/models", StreamOptionsSupport: UnknownSupport,
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, Temperature: Supported, TopP: Supported,
				Stop: Supported, MaxTokens: Supported,
			},
		},
		DialectGemini: {
			Name: DialectGemini, Protocol: "gemini",
			MaxTokensKey: "maxOutputTokens", AuthStyle: "x-goog-api-key",
			ModelListPath: "/v1beta/models", StreamOptionsSupport: Unsupported,
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, Temperature: Supported, TopP: Supported,
				Stop: Supported, MaxTokens: Supported, Vision: Supported,
			},
		},
		DialectOpenAIResponse: {
			Name: DialectOpenAIResponse, Protocol: "openai_responses",
			MaxTokensKey: "max_output_tokens", AuthStyle: "bearer",
			StreamOptionsSupport: Unsupported,
			Priors: ModelCapabilities{
				Text: Supported, Streaming: Supported, SystemMessage: Supported,
				Tools: Supported, Reasoning: Supported, MaxTokens: Supported,
			},
		},
	}
}

// DetectDialect resolves the dialect for a provider configuration. The
// explicit config field wins; otherwise known host fingerprints apply; the
// provider type provides the last fallback.
func DetectDialect(providerID, providerType, baseURL, explicit string) DialectProfile {
	all := Dialects()
	if explicit != "" {
		if d, ok := all[explicit]; ok {
			return d
		}
	}
	host := hostOf(baseURL)
	switch {
	case strings.Contains(host, "integrate.api.nvidia.com"), strings.Contains(host, "nvidia.com"), strings.Contains(host, "nvapi"):
		return all[DialectNvidiaNIM]
	case strings.Contains(host, "api.deepseek.com"):
		return all[DialectDeepSeek]
	case strings.Contains(host, "openrouter.ai"):
		return all[DialectOpenRouter]
	case strings.Contains(host, "api.groq.com"):
		return all[DialectGroq]
	case strings.Contains(host, "api.together.xyz"):
		return all[DialectTogether]
	case strings.Contains(host, "generativelanguage.googleapis.com"):
		return all[DialectGemini]
	case strings.Contains(host, "api.openai.com"):
		return all[DialectOpenAI]
	case strings.Contains(host, "api.anthropic.com"):
		return all[DialectAnthropic]
	}
	switch providerType {
	case "anthropic_compatible":
		return all[DialectGenericAnthro]
	case "gemini":
		return all[DialectGemini]
	case "openai_responses":
		return all[DialectOpenAIResponse]
	default:
		return all[DialectGenericOpenAI]
	}
}

func hostOf(baseURL string) string {
	u := strings.TrimSpace(baseURL)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexAny(u, "/"); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(u)
}

// AliasParameter translates a parameter name into the dialect's accepted key.
func (d DialectProfile) AliasParameter(name string) string {
	if d.ParameterAliases != nil {
		if v, ok := d.ParameterAliases[name]; ok {
			return v
		}
	}
	return name
}

// CapabilityForDialect derives the static seed contract for a deployment
// from its dialect plus operator-configured capability flags.
func SeedFromDialect(d DialectProfile, static ModelCapabilities) ModelCapabilities {
	out := d.Priors.Clone()
	// Static config overrides dialect priors (both are seeds; evidence wins).
	for _, cap := range AllCapabilities {
		if v := static.Get(cap); v != UnknownSupport {
			out.Set(cap, v)
		}
	}
	if d.Protocol != "" {
		out.NativeProtocol = d.Protocol
	}
	return out
}
