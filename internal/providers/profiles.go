package providers

// Provider Profile registry.
//
// A Profile is one (provider, protocol) pair. One provider brand may expose
// several independent profiles (for example Ashna exposes Chat, Messages
// and Responses on one host with different paths and, for Messages, a
// different auth header), and each profile maps to exactly one existing
// ProviderConfig dialect (Type) plus its path overrides. Profiles are a
// data layer on top of ProviderConfig: they introduce no new config format
// and no breaking change. An operator creates a deployment from a profile
// by supplying the credential as an environment-variable reference
// (APIKeyEnv); credential values never appear in this registry.
//
// Evidence honesty (read before trusting any profile):
//   - Evidence describes the documentation reviewed on ReviewedDate, NOT a
//     live call. No profile here has been verified against a live upstream
//     by this repository, and no paid live request was made to build it.
//   - "documented" means an official documentation page states the base
//     URL and the path for this protocol. "documented_preview" /
//     "documented_beta" mean the same, with the vendor's own preview/beta
//     qualifier. "secondary" means only non-official reports agree on the
//     endpoint. "unverified" means no reliable documentation was found for
//     the exact route; the profile is shipped only where the mission
//     explicitly asked for a chat-only entry, and its limitations say so.
//   - Capabilities lists ONLY capabilities the reviewed documentation
//     explicitly states for that profile. An absent capability means "not
//     documented in the reviewed material", never "proven unsupported".
//     Never infer full protocol compliance from one documented chat path.
//   - Paths are stored exactly as the HTTP adapter joins them to BaseURL
//     (see endpoint() in http_adapter.go): a base ending in "/v1" strips a
//     leading "/v1" from a canonical path, so most profiles store canonical
//     "/v1/..." paths. Ashna is the exception: its documented base is
//     ".../v1/api" and its clients append "/chat/completions",
//     "/v1/messages" and "/responses", so those relative paths are stored
//     verbatim. Changing a path without re-checking endpoint() joining
//     will silently build a wrong URL; profiles_test.go pins the joined
//     URL for every profile.
//   - Cloudflare profiles store a templated BaseURL containing
//     {account_id} and {gateway_id}. The template is NOT a usable endpoint:
//     ToProviderConfig refuses it until the operator substitutes their own
//     account and gateway ids. See the profile limitations.

import (
	"fmt"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Protocol identities for profiles. "provider_specific" is not an
// OpenAI/Anthropic wire protocol: it marks a vendor task API (KIE) that
// has no adapter dialect in this repository yet.
const (
	ProfileProtocolChat             = "chat"
	ProfileProtocolMessages         = "messages"
	ProfileProtocolResponses        = "responses"
	ProfileProtocolProviderSpecific = "provider_specific"
)

// Evidence states for profiles. See the package comment above.
const (
	EvidenceDocumented        = "documented"
	EvidenceDocumentedPreview = "documented_preview"
	EvidenceDocumentedBeta    = "documented_beta"
	EvidenceSecondary         = "secondary"
	EvidenceUnverified        = "unverified"
)

// Capability keys (Task 4 tracking list). A profile lists only the keys
// its reviewed documentation explicitly supports.
const (
	ProfileCapAuth                 = "auth"
	ProfileCapModelDiscovery       = "model_discovery"
	ProfileCapNonStreaming         = "non_streaming"
	ProfileCapStreamingSSE         = "streaming_sse"
	ProfileCapStreamingTermination = "streaming_termination"
	ProfileCapUsageExtraction      = "usage_extraction"
	ProfileCapToolCalls            = "tool_calls"
	ProfileCapToolResults          = "tool_results"
	ProfileCapParallelTools        = "parallel_tools"
	ProfileCapReasoningMetadata    = "reasoning_metadata"
	ProfileCapStructuredOutput     = "structured_output"
	ProfileCapImageInput           = "image_input"
	ProfileCapErrorEnvelope        = "error_envelope"
	ProfileCapRateLimit            = "rate_limit"
	ProfileCapRetryAfter           = "retry_after"
	ProfileCapCancellation         = "cancellation"
	ProfileCapIdempotency          = "idempotency"
)

var profileCapabilities = map[string]bool{
	ProfileCapAuth: true, ProfileCapModelDiscovery: true,
	ProfileCapNonStreaming: true, ProfileCapStreamingSSE: true,
	ProfileCapStreamingTermination: true, ProfileCapUsageExtraction: true,
	ProfileCapToolCalls: true, ProfileCapToolResults: true,
	ProfileCapParallelTools: true, ProfileCapReasoningMetadata: true,
	ProfileCapStructuredOutput: true, ProfileCapImageInput: true,
	ProfileCapErrorEnvelope: true, ProfileCapRateLimit: true,
	ProfileCapRetryAfter: true, ProfileCapCancellation: true,
	ProfileCapIdempotency: true,
}

// Profile is one provider's one protocol surface.
type Profile struct {
	ID           string `json:"id"`
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	Category     string `json:"category"`
	// Protocol is chat, messages, responses, or provider_specific.
	Protocol string `json:"protocol"`
	// Dialect is the existing ProviderConfig Type this profile maps to.
	// Empty means no adapter dialect exists yet (KIE task API); such a
	// profile cannot be turned into a working ProviderConfig.
	Dialect        string   `json:"dialect,omitempty"`
	BaseURL        string   `json:"base_url"`
	ChatPath       string   `json:"chat_path,omitempty"`
	MessagesPath   string   `json:"messages_path,omitempty"`
	ResponsesPath  string   `json:"responses_path,omitempty"`
	ModelsPath     string   `json:"models_path,omitempty"`
	CreateTaskPath string   `json:"create_task_path,omitempty"`
	RecordInfoPath string   `json:"record_info_path,omitempty"`
	AuthMode       string   `json:"auth_mode"`
	DocsURL        string   `json:"docs_url"`
	Capabilities   []string `json:"capabilities,omitempty"`
	Limitations    []string `json:"limitations,omitempty"`
	Evidence       string   `json:"evidence"`
	ReviewedDate   string   `json:"reviewed_date"`
	Local          bool     `json:"local,omitempty"`
}

// AuthHeader returns the HTTP header carrying the credential for the
// profile's auth mode, or "" when the profile needs no credential.
func (p Profile) AuthHeader() string {
	switch p.AuthMode {
	case "bearer":
		return "Authorization"
	case "x-api-key":
		return "x-api-key"
	case "x-goog-api-key":
		return "x-goog-api-key"
	case "api-key":
		return "api-key"
	default:
		return ""
	}
}

// IsTemplated reports whether BaseURL still contains an operator-supplied
// placeholder (Cloudflare account/gateway ids).
func (p Profile) IsTemplated() bool {
	return strings.Contains(p.BaseURL, "{")
}

// Supports reports whether the profile's reviewed documentation explicitly
// lists the capability. Absence means "not documented", not "unsupported".
func (p Profile) Supports(capability string) bool {
	for _, c := range p.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// ToProviderConfig builds the ProviderConfig an operator would save for
// this profile, with the credential supplied only as an environment
// variable name. It fails closed for profiles with no adapter dialect
// (KIE) and for templated base URLs that have not been substituted.
func (p Profile) ToProviderConfig(apiKeyEnv string) (config.ProviderConfig, error) {
	return p.toProviderConfigWithBase(p.BaseURL, apiKeyEnv)
}

// ToProviderConfigWithBase is ToProviderConfig with an operator-supplied
// base URL, used for templated profiles after substitution and for
// self-hosted profiles (NVIDIA NIM) pointed at the operator's own host.
func (p Profile) ToProviderConfigWithBase(baseURL, apiKeyEnv string) (config.ProviderConfig, error) {
	if strings.Contains(baseURL, "{") {
		return config.ProviderConfig{}, fmt.Errorf("profile %q base URL is a template; substitute account and gateway ids before use", p.ID)
	}
	return p.toProviderConfigWithBase(baseURL, apiKeyEnv)
}

func (p Profile) toProviderConfigWithBase(baseURL, apiKeyEnv string) (config.ProviderConfig, error) {
	if p.Dialect == "" {
		return config.ProviderConfig{}, fmt.Errorf("profile %q has no adapter dialect yet and cannot be configured as a provider", p.ID)
	}
	if strings.Contains(baseURL, "{") {
		return config.ProviderConfig{}, fmt.Errorf("profile %q base URL is a template; substitute account and gateway ids before use", p.ID)
	}
	label := p.Protocol
	if label == ProfileProtocolProviderSpecific {
		label = "task"
	}
	return config.ProviderConfig{
		ID:            p.ID,
		Name:          p.ProviderName + " " + label,
		Type:          p.Dialect,
		BaseURL:       baseURL,
		APIKeyEnv:     apiKeyEnv,
		AuthMode:      p.AuthMode,
		ChatPath:      p.ChatPath,
		MessagesPath:  p.MessagesPath,
		ResponsesPath: p.ResponsesPath,
		ModelsPath:    p.ModelsPath,
	}, nil
}

// Profiles returns a copy of the registry.
func Profiles() []Profile {
	out := make([]Profile, len(profileList))
	copy(out, profileList)
	return out
}

// ProfileByID returns the profile with the given ID.
func ProfileByID(id string) (Profile, bool) {
	for _, p := range profileList {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// ProfilesForProvider returns every profile for one provider brand,
// in protocol order chat, messages, responses, provider_specific.
func ProfilesForProvider(providerID string) []Profile {
	out := []Profile{}
	for _, p := range profileList {
		if p.ProviderID == providerID {
			out = append(out, p)
		}
	}
	return out
}
