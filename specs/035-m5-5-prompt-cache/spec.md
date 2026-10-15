# M5-5 — Provider prompt-cache passthrough / mapping

Cache-token USAGE already maps in both directions for all four
kinds (verified by inventory; pinned by tests here). The request
side is what this unit adds:

- **Anthropic cache_control markers through the canonical codec.**
  core.AnthBlock gains cache_control; the canonical Part gains a
  CacheControl marker ("ephemeral", or a TTL like "1h"/"5m").
  The Anthropic decoder captures markers on system and message
  blocks (previously discarded); the Anthropic encoder re-emits
  them — including emitting the system prompt as blocks when any
  system part carries a marker (the joined-string form cannot).
  Native anthropic->anthropic passthrough already preserved them;
  now canonical detours (Responses ingress, repair re-encode) do
  too.
- **prompt_cache_key mapping.** The IR gains Request.PromptCacheKey.
  Responses decode reads it, Responses encode emits it, OpenAI Chat
  decode reads it and Chat encode emits it (provider-dependent on
  Chat, documented). Anthropic/Gemini have no request-level key.
- **Non-mappings, documented**: Anthropic block markers do NOT
  translate to prompt_cache_key or to Gemini (no equivalent);
  Gemini caching is implicit (usage cachedContentTokenCount maps
  to CacheReadTokens). Bedrock cachePoint blocks join when PR #323
  lands.
