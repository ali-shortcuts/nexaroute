package providers

// profileList is the data table for the Provider Profile registry.
// Every entry was reviewed against public documentation on 2026-11-01;
// see docs/PROVIDER_COMPATIBILITY_MATRIX.md for the per-provider evidence
// notes and for the services deliberately left WITHOUT a profile
// (no reliable documentation for the exact route was found).
//
// Path convention: canonical "/v1/..." paths, except Ashna, whose base
// ".../v1/api" takes client-appended "/chat/completions", "/v1/messages"
// and "/responses" (see profiles.go). Capabilities list only what the
// reviewed documentation explicitly states.

var profileList = []Profile{
	// Ashna — one host, three protocols (ashna.ai/api-docs). Chat and
	// Responses use Bearer; Messages clients send the same secret as
	// x-api-key. Custom agent ids may be passed as `model`.
	{ID: "ashna-chat", ProviderID: "ashna", ProviderName: "Ashna", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.ashna.ai/v1/api", ChatPath: "/chat/completions", ModelsPath: "/models",
		AuthMode: "bearer", DocsURL: "https://www.ashna.ai/api-docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming, ProfileCapStreamingSSE, ProfileCapStreamingTermination, ProfileCapUsageExtraction, ProfileCapToolCalls, ProfileCapToolResults, ProfileCapStructuredOutput, ProfileCapImageInput, ProfileCapErrorEnvelope, ProfileCapRateLimit},
		Limitations:  []string{"model may be a foundation id or an Ashna custom agent id (server-side agent tools run on the server and intermediate tool I/O is not forwarded as OpenAI chunks)", "streamType data is an Ashna UI-message stream, not OpenAI SSE", "PDF handling is provider-aware (native PDF vs text extract)"}},
	{ID: "ashna-messages", ProviderID: "ashna", ProviderName: "Ashna", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://api.ashna.ai/v1/api", MessagesPath: "/v1/messages", ModelsPath: "/models",
		AuthMode: "x-api-key", DocsURL: "https://www.ashna.ai/api-docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Messages documentation is Claude Code oriented; tool round-trips and streaming are documented for Chat, not separately for Messages", "model may be a foundation id, a sonnet/opus/haiku alias, or a custom agent id"}},
	{ID: "ashna-responses", ProviderID: "ashna", ProviderName: "Ashna", Category: "Gateway",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://api.ashna.ai/v1/api", ResponsesPath: "/responses", ModelsPath: "/models",
		AuthMode: "bearer", DocsURL: "https://www.ashna.ai/api-docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Responses documentation is Codex/OpenClaw oriented; streaming, tools and usage are documented for Chat, not separately for Responses"}},

	// AnyRouter (anyrouter.dev only — anyrouter.top is a different service).
	// OpenAI base https://anyrouter.dev/api/v1; the Anthropic base
	// https://anyrouter.dev/api plus /v1/messages lands on the same
	// .../api/v1/messages URL as the paths stored here.
	{ID: "anyrouter-chat", ProviderID: "anyrouter", ProviderName: "AnyRouter", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://anyrouter.dev/api/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://anyrouter.dev/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"anyrouter.top is a different service (Claude Code relay); this profile is anyrouter.dev only", "model ids use provider/model form", "streaming, tools and usage are not separately documented in the reviewed material"}},
	{ID: "anyrouter-messages", ProviderID: "anyrouter", ProviderName: "AnyRouter", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://anyrouter.dev/api/v1", MessagesPath: "/v1/messages", ModelsPath: "/v1/models",
		AuthMode: "x-api-key", DocsURL: "https://anyrouter.dev/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"anyrouter.top is a different service; this profile is anyrouter.dev only", "the Anthropic surface is documented as base https://anyrouter.dev/api with /v1/messages appended, which is the same final URL as stored here"}},
	{ID: "anyrouter-responses", ProviderID: "anyrouter", ProviderName: "AnyRouter", Category: "Gateway",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://anyrouter.dev/api/v1", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://anyrouter.dev/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"anyrouter.top is a different service; this profile is anyrouter.dev only", "Responses statefulness is not documented in the reviewed material"}},

	// AgentRouter (agentrouter.org only — not TheAgentRouter / Envoy AI
	// Gateway, a different project). Its onboarding pages document two
	// separate surfaces, and the vendor states they run "in parallel" and
	// must not be mixed: an Anthropic-compatible base of
	// https://agentrouter.org with no /v1 suffix (Claude Code sets
	// ANTHROPIC_BASE_URL this way and sends the token as
	// ANTHROPIC_AUTH_TOKEN -> Bearer), and an OpenAI-compatible base of
	// https://agentrouter.org/v1 (Codex CLI, and "other general models").
	// They are therefore TWO profiles with different bases and auth modes;
	// a Claude model served through the OpenAI-compatible base is still a
	// chat row, not native Anthropic Messages.
	{ID: "agentrouter-chat", ProviderID: "agentrouter", ProviderName: "AgentRouter", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://agentrouter.org/v1", ChatPath: "/v1/chat/completions",
		AuthMode: "bearer", DocsURL: "https://agentrouter.org/docs/codex.html", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"documented model ids on 2026-11-01 are gpt-5.6-sol and gpt-6-astra, plus claude-opus-5, claude-opus-4-8, glm-5.3 and deepseek-v4-flash which the vendor documents for both protocols; the vendor states the list changes with capacity and its console/pricing pages are authoritative", "the vendor documents this base as the OpenAI-compatible one (base URL plus /v1); other general models are served here rather than through the Messages base", "streaming, tool-call and usage shapes are not documented for this route in the reviewed material", "a misspelled or delisted model id returns 503 with no available channel", "the landing page also lists /v1/responses and /v1beta/models, but no Responses or native-Gemini profile is shipped here; those routes are unverified for this repository"}},
	{ID: "agentrouter-messages", ProviderID: "agentrouter", ProviderName: "AgentRouter", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://agentrouter.org", MessagesPath: "/v1/messages",
		AuthMode: "bearer", DocsURL: "https://agentrouter.org/docs/claude-code.html", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"the documented base is https://agentrouter.org with NO /v1 suffix (Claude Code ANTHROPIC_BASE_URL); the /v1 base belongs to the separate OpenAI-compatible profile and the vendor states the two must not be mixed", "only the token-style credential is documented for this base (ANTHROPIC_AUTH_TOKEN, sent as a Bearer token); an x-api-key credential is not documented for this route", "Claude Opus models are documented as preferring this Anthropic surface; claude-opus-5, claude-opus-4-8, glm-5.3 and deepseek-v4-flash are documented for both protocols", "streaming, tool round-trips and usage are not documented for Messages in the reviewed material", "a misspelled or delisted model id returns 503 with no available channel"}},

	// NaraRouter — router.bynara.id/docs. Images live on a different host
	// (api-images.bynara.id) and are a separate capability family, not
	// part of these chat profiles.
	{ID: "nararouter-chat", ProviderID: "nararouter", ProviderName: "NaraRouter", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://router.bynara.id/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://router.bynara.id/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"image generation/edits live on https://api-images.bynara.id/v1, a different host, and are not covered by this profile"}},
	{ID: "nararouter-messages", ProviderID: "nararouter", ProviderName: "NaraRouter", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://router.bynara.id/v1", MessagesPath: "/v1/messages", ModelsPath: "/v1/models",
		AuthMode: "x-api-key", DocsURL: "https://router.bynara.id/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Messages uses the native Anthropic request format with a Bearer token in the vendor docs; NexaRoute sends it as x-api-key per its anthropic dialect", "image endpoints live on a different host"}},
	{ID: "nararouter-responses", ProviderID: "nararouter", ProviderName: "NaraRouter", Category: "Gateway",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://router.bynara.id/v1", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://router.bynara.id/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Responses is documented as stateful; state handling is vendor-side and not separately verified here", "image endpoints live on a different host"}},

	// Cloudflare AI Gateway — account-scoped templates. The operator MUST
	// substitute {account_id} and {gateway_id}; ToProviderConfig refuses
	// the template. Compat is chat-only (no Responses). The provider-scoped
	// OpenAI base carries Chat and Responses; the Anthropic base carries
	// Messages with x-api-key. Authenticated gateways (unified billing)
	// expect the gateway token in cf-aig-authorization, a header NexaRoute
	// does not send, so these profiles cover the provider-key-forwarding /
	// stored-key mode only.
	{ID: "cloudflare-compat-chat", ProviderID: "cloudflare-ai-gateway", ProviderName: "Cloudflare AI Gateway", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/compat", ChatPath: "/chat/completions",
		AuthMode: "bearer", DocsURL: "https://developers.cloudflare.com/ai-gateway/usage/chat-completion/", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"base URL is a template: substitute your account_id and gateway_id", "compat endpoint is Chat Completions only; it does not expose Responses", "model ids use provider/model form", "authenticated gateways expect cf-aig-authorization, which NexaRoute does not send; use provider-key-forwarding or stored-key mode", "no models-list endpoint is documented for the compat base"}},
	{ID: "cloudflare-openai-chat", ProviderID: "cloudflare-ai-gateway", ProviderName: "Cloudflare AI Gateway", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/openai", ChatPath: "/chat/completions",
		AuthMode: "bearer", DocsURL: "https://developers.cloudflare.com/ai-gateway/", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"base URL is a template: substitute your account_id and gateway_id", "provider-scoped to OpenAI models only; other providers need their own scoped base or the compat profile", "authenticated gateways expect cf-aig-authorization, which NexaRoute does not send"}},
	{ID: "cloudflare-openai-responses", ProviderID: "cloudflare-ai-gateway", ProviderName: "Cloudflare AI Gateway", Category: "Gateway",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/openai", ResponsesPath: "/responses",
		AuthMode: "bearer", DocsURL: "https://developers.cloudflare.com/ai-gateway/", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"base URL is a template: substitute your account_id and gateway_id", "provider-scoped to OpenAI models only", "authenticated gateways expect cf-aig-authorization, which NexaRoute does not send"}},
	{ID: "cloudflare-anthropic-messages", ProviderID: "cloudflare-ai-gateway", ProviderName: "Cloudflare AI Gateway", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/anthropic", MessagesPath: "/v1/messages",
		AuthMode: "x-api-key", DocsURL: "https://developers.cloudflare.com/ai-gateway/usage/providers/anthropic/", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"base URL is a template: substitute your account_id and gateway_id", "the provider key is sent as x-api-key; the gateway token for authenticated gateways goes in cf-aig-authorization, which NexaRoute does not send (with stored keys or unified billing a placeholder provider key may be required)"}},

	// Cline — chat only. Cline's own docs document Cline as a CLIENT with
	// an OpenAI Compatible provider; no official docs page for a Cline
	// hosted API surface was found, and secondary reports disagree on the
	// base path (/v1 vs /api/v1). Shipped as unverified, chat only.
	{ID: "cline-chat", ProviderID: "cline", ProviderName: "Cline", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.cline.bot/v1", ChatPath: "/v1/chat/completions",
		AuthMode: "bearer", DocsURL: "https://docs.cline.bot/provider-config/openai-compatible", Evidence: EvidenceUnverified, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"no official Cline hosted-API docs page was found; secondary reports disagree on the base path (https://api.cline.bot/v1 vs https://api.cline.bot/api/v1)", "chat only: no Messages or Responses endpoint is documented", "no models-list endpoint is documented for this base"}},

	// Puter — official developer docs: OpenAI-compatible endpoint, Puter
	// auth token as Bearer. Claude/Gemini/etc. models served through this
	// endpoint are OpenAI Chat shaped; that is not native Messages.
	{ID: "puter-chat", ProviderID: "puter", ProviderName: "Puter", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.puter.com/puterai/openai/v1", ChatPath: "/v1/chat/completions",
		AuthMode: "bearer", DocsURL: "https://developer.puter.com/tutorials/use-openai-sdk-with-puter/", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"chat only: no Messages or Responses endpoint is documented", "a Claude model reached through this endpoint is OpenAI Chat shaped, not native Anthropic Messages", "no models-list endpoint is documented for this base"}},

	// KIE — proprietary task API, NOT an OpenAI/Anthropic surface. Model
	// families (video, image, audio, chat agents) share the job pattern
	// but differ in per-model parameters. No adapter dialect exists, so
	// ToProviderConfig fails closed for this profile.
	{ID: "kie-task", ProviderID: "kie", ProviderName: "KIE", Category: "Media",
		Protocol: ProfileProtocolProviderSpecific, Dialect: "",
		BaseURL: "https://api.kie.ai", ModelsPath: "/api/v1/models",
		CreateTaskPath: "/api/v1/jobs/createTask", RecordInfoPath: "/api/v1/jobs/recordInfo",
		AuthMode: "bearer", DocsURL: "https://kie.ai", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery},
		Limitations:  []string{"proprietary task API: createTask then poll recordInfo; it is not Chat Completions, Messages or Responses", "per-model parameters differ by model family; no single request shape covers all KIE models", "no NexaRoute adapter dialect exists for this API yet; this profile is inventory only and cannot be configured as a provider"}},

	// Mistral — chat exists as preset "mistral"; Responses is a public
	// preview (announced 2026-10-28) on the same base.
	{ID: "mistral-chat", ProviderID: "mistral", ProviderName: "Mistral AI", Category: "Direct",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.mistral.ai/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://docs.mistral.ai", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"existing preset mistral covers this chat surface; this profile adds the protocol view"}},
	{ID: "mistral-responses", ProviderID: "mistral", ProviderName: "Mistral AI", Category: "Direct",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://api.mistral.ai/v1", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://docs.mistral.ai/api/endpoint/responses", Evidence: EvidenceDocumentedPreview, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Responses API is in public preview; an older third-party guide claimed no Responses endpoint existed, the official endpoint page is the stronger source"}},

	// Groq — chat exists as preset "groq"; Responses is beta on the same
	// OpenAI-shaped base, without stateful conversations.
	{ID: "groq-chat", ProviderID: "groq", ProviderName: "Groq", Category: "Fast inference",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.groq.com/openai/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://console.groq.com/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"existing preset groq covers this chat surface; this profile adds the protocol view"}},
	{ID: "groq-responses", ProviderID: "groq", ProviderName: "Groq", Category: "Fast inference",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://api.groq.com/openai/v1", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://console.groq.com/docs/responses-api", Evidence: EvidenceDocumentedBeta, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Responses API is beta", "stateful conversations are not yet supported: the client must resend conversation history"}},

	// NVIDIA NIM — hosted and self-hosted are separate profiles. Hosted
	// capabilities are never transferred to a self-hosted endpoint.
	{ID: "nvidia-nim-hosted-chat", ProviderID: "nvidia-nim", ProviderName: "NVIDIA NIM", Category: "Inference",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://integrate.api.nvidia.com/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://docs.nvidia.com", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"existing preset nvidia-nim covers this hosted surface", "hosted catalog and rate limits do not apply to self-hosted deployments"}},
	{ID: "nvidia-nim-selfhosted-chat", ProviderID: "nvidia-nim", ProviderName: "NVIDIA NIM", Category: "Local",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "http://localhost:8000/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "none", Local: true, DocsURL: "https://docs.nvidia.com", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapNonStreaming, ProfileCapModelDiscovery},
		Limitations:  []string{"operator-supplied host: replace localhost:8000 with your NIM deployment host; the localhost URL is only the conventional default", "a self-hosted NIM without an auth proxy needs no credential; if your deployment fronts an auth proxy, configure bearer auth instead of none", "the served model set is whatever that single NIM container serves, not the hosted catalog"}},

	// Venice — chat plus Responses beta. Responses is native for OpenAI
	// models and translated for others.
	{ID: "venice-chat", ProviderID: "venice", ProviderName: "Venice", Category: "Direct",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.venice.ai/api/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://docs.venice.ai", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"unknown OpenAI-only top-level fields (for example logit_bias, service_tier) return HTTP 400", "Assistants and Batch are not available"}},
	{ID: "venice-responses", ProviderID: "venice", ProviderName: "Venice", Category: "Direct",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://api.venice.ai/api/v1", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://docs.venice.ai", Evidence: EvidenceDocumentedBeta, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Responses API is beta", "Responses is native for OpenAI models and translated for other models", "unknown OpenAI-only top-level fields return HTTP 400"}},

	// OpenCode Zen — separate from OpenCode Console/Go presets. One base
	// https://opencode.ai/zen/v1 serves Chat, Messages and Responses;
	// model ids in client config carry the opencode/ prefix while direct
	// API requests use the bare id. Jev (systemone decisions) is a
	// separate API and is not a chat profile.
	{ID: "opencode-zen-chat", ProviderID: "opencode-zen", ProviderName: "OpenCode Zen", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://opencode.ai/zen/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://open-code.ai/en/docs/zen", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"separate from OpenCode Console (opencode.ai/inference) and OpenCode Go (opencode.ai/inference/go) presets", "client model ids use the opencode/ prefix; direct API requests use the bare model id", "some models retained in reference tables have been retired; check the live catalog"}},
	{ID: "opencode-zen-messages", ProviderID: "opencode-zen", ProviderName: "OpenCode Zen", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://opencode.ai/zen/v1", MessagesPath: "/v1/messages", ModelsPath: "/v1/models",
		AuthMode: "x-api-key", DocsURL: "https://open-code.ai/en/docs/zen", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"separate from OpenCode Console/Go presets", "the Anthropic surface is also documented with base https://opencode.ai/zen plus /v1/messages, which is the same final URL as stored here"}},
	{ID: "opencode-zen-responses", ProviderID: "opencode-zen", ProviderName: "OpenCode Zen", Category: "Gateway",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://opencode.ai/zen/v1", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://open-code.ai/en/docs/zen", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"separate from OpenCode Console/Go presets; the mission asked for this Responses profile specifically", "Jev systemone decisions (https://opencode.ai/zen/v1/systemone) are a different API, not this profile"}},

	// OxionAI — documented Chat surface only. No reliable Anthropic
	// endpoint was found, so no Messages profile is shipped; do not
	// assume /v1/messages exists on this base.
	{ID: "oxionai-chat", ProviderID: "oxionai", ProviderName: "OxionAI", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://oxionai.com/v1", ChatPath: "/v1/chat/completions",
		AuthMode: "bearer", DocsURL: "https://oxionai.com/docs", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"chat only: no Anthropic Messages endpoint was found in the reviewed documentation, so no messages profile is shipped", "no models-list endpoint is documented for this base"}},

	// OrcaRouter — official docs: OpenAI base https://api.orcarouter.ai/v1
	// (chat and responses endpoints listed per model), Anthropic SDK base
	// https://api.orcarouter.ai (the SDK appends /v1/messages).
	{ID: "orcarouter-chat", ProviderID: "orcarouter", ProviderName: "OrcaRouter", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.orcarouter.ai/v1", ChatPath: "/v1/chat/completions", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://docs.orcarouter.ai", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"orcarouter/auto is a router model, not a foundation model", "model ids use provider/model form"}},
	{ID: "orcarouter-messages", ProviderID: "orcarouter", ProviderName: "OrcaRouter", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://api.orcarouter.ai", MessagesPath: "/v1/messages", ModelsPath: "/v1/models",
		AuthMode: "x-api-key", DocsURL: "https://docs.orcarouter.ai", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"the Anthropic base is the bare domain; the SDK appends /v1/messages", "the same sk-orca secret is sent as x-api-key on this surface"}},
	{ID: "orcarouter-responses", ProviderID: "orcarouter", ProviderName: "OrcaRouter", Category: "Gateway",
		Protocol: ProfileProtocolResponses, Dialect: "openai_responses",
		BaseURL: "https://api.orcarouter.ai/v1", ResponsesPath: "/v1/responses", ModelsPath: "/v1/models",
		AuthMode: "bearer", DocsURL: "https://docs.orcarouter.ai", Evidence: EvidenceDocumented, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapModelDiscovery, ProfileCapNonStreaming},
		Limitations:  []string{"Responses is listed as an endpoint per model page; statefulness is not documented in the reviewed material"}},

	// UnoRouter — only non-official reports agree on the endpoints
	// (OpenAI base https://api.unorouter.com/v1, Anthropic bare domain).
	// Shipped as unverified: official documentation was not confirmed.
	{ID: "unorouter-chat", ProviderID: "unorouter", ProviderName: "UnoRouter", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.unorouter.com/v1", ChatPath: "/v1/chat/completions",
		AuthMode: "bearer", DocsURL: "https://api.unorouter.com", Evidence: EvidenceUnverified, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"official documentation was not confirmed; two non-official reports agree on this base URL", "no models-list endpoint is documented in the reviewed material"}},
	{ID: "unorouter-messages", ProviderID: "unorouter", ProviderName: "UnoRouter", Category: "Gateway",
		Protocol: ProfileProtocolMessages, Dialect: "anthropic_compatible",
		BaseURL: "https://api.unorouter.com", MessagesPath: "/v1/messages",
		AuthMode: "x-api-key", DocsURL: "https://api.unorouter.com", Evidence: EvidenceUnverified, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"official documentation was not confirmed; the bare-domain Anthropic base comes from a non-official report", "no models-list endpoint is documented in the reviewed material"}},

	// TokenJuice — a single non-official mention (host api.tokenjuice.ai,
	// one model, Chat Completions) and no official docs. Chat only,
	// unverified, no discovery claim.
	{ID: "tokenjuice-chat", ProviderID: "tokenjuice", ProviderName: "TokenJuice", Category: "Gateway",
		Protocol: ProfileProtocolChat, Dialect: "openai_compatible",
		BaseURL: "https://api.tokenjuice.ai", ChatPath: "/v1/chat/completions",
		AuthMode: "bearer", DocsURL: "https://api.tokenjuice.ai", Evidence: EvidenceUnverified, ReviewedDate: "2026-11-01",
		Capabilities: []string{ProfileCapAuth, ProfileCapNonStreaming},
		Limitations:  []string{"only a single non-official mention was found (host api.tokenjuice.ai, Chat Completions); no official documentation, model list, or Messages/Responses endpoint was confirmed", "do not treat the host alone as proof of a working endpoint"}},
}
