# Provider Compatibility Matrix

Source of truth for the data: `internal/providers/profiles.go` and
`internal/providers/profiles_data.go`. This document is generated from
that registry by hand on 2026-11-01 and is pinned by
`TestProfilesDocumentedInMatrix` (every profile ID must appear here).
If the registry and this document disagree, the registry wins and this
document must be corrected — never the other way round.

## How to read this matrix (evidence honesty)

- **Evidence** describes documentation reviewed on **2026-11-01**, not a
  live call. No profile in this matrix has been verified against a live
  upstream by this repository, and **no paid live request was made** to
  build it. Nothing here is "live verified", "verified live", or "production verified" — all three are explicitly disclaimed.
- `documented` — an official documentation page states the base URL and
  the path for this protocol.
- `documented_preview` / `documented_beta` — the same, with the vendor's
  own preview/beta qualifier (Mistral Responses, Groq Responses, Venice
  Responses).
- `unverified` — no reliable documentation for the exact route was
  found. The profile is shipped only where the task explicitly asked
  for a chat-only entry, and its limitations say so in plain text.
- **Capabilities** list only what the reviewed documentation explicitly
  states for that profile. An absent capability means **not documented
  in the reviewed material**, not "proven unsupported" and not "tested
  and failed". Never infer full protocol compliance from one
  documented chat path.
- **A Claude model served through OpenAI Chat is not native Anthropic
  Messages.** Several gateways below serve Claude (and Gemini, GPT, …)
  models through an OpenAI Chat-shaped endpoint. Those rows are Chat
  rows. A Messages row exists only where the documentation states a
  Messages endpoint.
- **Model branding, URL suffixes and API-key prefixes were never used
  as classifiers.** A `sk-ar-v1-` style prefix, a `/v1` suffix, or a
  `claude-` model id proves nothing about the wire protocol.
- **No credential values** appear in the registry or in this document.
  Operators supply credentials as environment-variable references
  (`APIKeyEnv`) when creating a deployment from a profile.

## Profile registry

One provider brand may expose several independent profiles. Each
profile maps to exactly one existing `ProviderConfig` dialect (Type)
plus path overrides — there is no new configuration format, and no
breaking change. An operator creates one deployment per profile; a
chat deployment does not also serve Messages or Responses traffic.

Paths are stored exactly as the HTTP adapter joins them to the base URL
(see `endpoint()` in `internal/providers/http_adapter.go`). Most
profiles store canonical `/v1/...` paths. Ashna is the exception: its
base is `https://api.ashna.ai/v1/api` and clients append
`/chat/completions`, `/v1/messages` and `/responses`, so those relative
paths are stored verbatim. `profiles_test.go` pins the joined URL for
every profile.

| Profile ID | Protocol | Base URL | Main path | Auth | Evidence | Documented capabilities |
|---|---|---|---|---|---|---|
| ashna-chat | chat | https://api.ashna.ai/v1/api | /chat/completions | Bearer | documented | auth, model_discovery, non_streaming, streaming_sse, streaming_termination, usage_extraction, tool_calls, tool_results, structured_output, image_input, error_envelope, rate_limit |
| ashna-messages | messages | https://api.ashna.ai/v1/api | /v1/messages | x-api-key | documented | auth, model_discovery, non_streaming |
| ashna-responses | responses | https://api.ashna.ai/v1/api | /responses | Bearer | documented | auth, model_discovery, non_streaming |
| anyrouter-chat | chat | https://anyrouter.dev/api/v1 | /v1/chat/completions | Bearer | documented | auth, non_streaming |
| anyrouter-messages | messages | https://anyrouter.dev/api/v1 | /v1/messages | x-api-key | documented | auth, non_streaming |
| anyrouter-responses | responses | https://anyrouter.dev/api/v1 | /v1/responses | Bearer | documented | auth, non_streaming |
| nararouter-chat | chat | https://router.bynara.id/v1 | /v1/chat/completions | Bearer | documented | auth, model_discovery, non_streaming |
| nararouter-messages | messages | https://router.bynara.id/v1 | /v1/messages | x-api-key | documented | auth, model_discovery, non_streaming |
| nararouter-responses | responses | https://router.bynara.id/v1 | /v1/responses | Bearer | documented | auth, model_discovery, non_streaming |
| cloudflare-compat-chat | chat | https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/compat | /chat/completions | Bearer | documented | auth, non_streaming |
| cloudflare-openai-chat | chat | https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/openai | /chat/completions | Bearer | documented | auth, non_streaming |
| cloudflare-openai-responses | responses | https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/openai | /responses | Bearer | documented | auth, non_streaming |
| cloudflare-anthropic-messages | messages | https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/anthropic | /v1/messages | x-api-key | documented | auth, non_streaming |
| cline-chat | chat | https://api.cline.bot/v1 | /v1/chat/completions | Bearer | unverified | auth, non_streaming |
| puter-chat | chat | https://api.puter.com/puterai/openai/v1 | /v1/chat/completions | Bearer | documented | auth, non_streaming |
| kie-task | provider_specific | https://api.kie.ai | /api/v1/jobs/createTask (create), /api/v1/jobs/recordInfo (status) | Bearer | documented | auth, model_discovery |
| mistral-chat | chat | https://api.mistral.ai/v1 | /v1/chat/completions | Bearer | documented | auth, model_discovery, non_streaming |
| mistral-responses | responses | https://api.mistral.ai/v1 | /v1/responses | Bearer | documented_preview | auth, model_discovery, non_streaming |
| groq-chat | chat | https://api.groq.com/openai/v1 | /v1/chat/completions | Bearer | documented | auth, model_discovery, non_streaming |
| groq-responses | responses | https://api.groq.com/openai/v1 | /v1/responses | Bearer | documented_beta | auth, model_discovery, non_streaming |
| nvidia-nim-hosted-chat | chat | https://integrate.api.nvidia.com/v1 | /v1/chat/completions | Bearer | documented | auth, model_discovery, non_streaming |
| nvidia-nim-selfhosted-chat | chat | http://localhost:8000/v1 | /v1/chat/completions | none | documented | model_discovery, non_streaming |
| venice-chat | chat | https://api.venice.ai/api/v1 | /v1/chat/completions | Bearer | documented | auth, model_discovery, non_streaming |
| venice-responses | responses | https://api.venice.ai/api/v1 | /v1/responses | Bearer | documented_beta | auth, model_discovery, non_streaming |
| opencode-zen-chat | chat | https://opencode.ai/zen/v1 | /v1/chat/completions | Bearer | documented | auth, model_discovery, non_streaming |
| opencode-zen-messages | messages | https://opencode.ai/zen/v1 | /v1/messages | x-api-key | documented | auth, model_discovery, non_streaming |
| opencode-zen-responses | responses | https://opencode.ai/zen/v1 | /v1/responses | Bearer | documented | auth, model_discovery, non_streaming |
| oxionai-chat | chat | https://oxionai.com/v1 | /v1/chat/completions | Bearer | documented | auth, non_streaming |
| orcarouter-chat | chat | https://api.orcarouter.ai/v1 | /v1/chat/completions | Bearer | documented | auth, model_discovery, non_streaming |
| orcarouter-messages | messages | https://api.orcarouter.ai | /v1/messages | x-api-key | documented | auth, model_discovery, non_streaming |
| orcarouter-responses | responses | https://api.orcarouter.ai/v1 | /v1/responses | Bearer | documented | auth, model_discovery, non_streaming |
| unorouter-chat | chat | https://api.unorouter.com/v1 | /v1/chat/completions | Bearer | unverified | auth, non_streaming |
| unorouter-messages | messages | https://api.unorouter.com | /v1/messages | x-api-key | unverified | auth, non_streaming |
| tokenjuice-chat | chat | https://api.tokenjuice.ai | /v1/chat/completions | Bearer | unverified | auth, non_streaming |

Notes per provider (limitations live in the registry; the sharpest ones
are repeated here so a reader of this document alone is not misled):

- **Ashna:** `model` may be a foundation id or an Ashna custom agent id
  (server-side agent tools run on the server; intermediate tool I/O is
  not forwarded as OpenAI chunks). `streamType: "data"` is an Ashna
  UI-message stream, not OpenAI SSE. PDF handling is provider-aware.
  Messages documentation is Claude Code oriented; tool round-trips and
  streaming are documented for Chat, not separately for Messages.
- **AnyRouter:** this profile is **anyrouter.dev only**. `anyrouter.top`
  is a different service (a Claude Code relay) and must not be confused
  with it. Model ids use `provider/model` form. The Anthropic surface is
  documented as base `https://anyrouter.dev/api` plus `/v1/messages`,
  which is the same final URL as the stored profile.
- **NaraRouter:** image generation/edits live on a different host,
  `https://api-images.bynara.id/v1`, and are a separate capability
  family, not part of these chat profiles. Responses is documented as
  stateful (vendor-side).
- **Cloudflare AI Gateway:** every base URL is a **template** — the
  operator must substitute `{account_id}` and `{gateway_id}`.
  `ToProviderConfig` refuses the template until substituted. The
  compat endpoint is **Chat Completions only; it does not expose
  Responses** (the provider-scoped `/openai` base carries Chat and
  Responses; the provider-scoped `/anthropic` base carries Messages).
  Compat model ids use `provider/model` form. Authenticated gateways
  (unified billing) expect the gateway token in `cf-aig-authorization`,
  a header NexaRoute does not send, so these profiles cover the
  provider-key-forwarding / stored-key mode only. No models-list
  endpoint is documented for the compat base.
- **Cline:** chat only, **unverified**. Cline's own docs document Cline
  as a client with an OpenAI Compatible provider; no official docs page
  for a Cline hosted API surface was found, and secondary reports
  disagree on the base path (`/v1` vs `/api/v1`). No Messages,
  Responses, or models-list endpoint is documented.
- **Puter:** chat only. A Claude model reached through this endpoint is
  OpenAI Chat shaped, not native Anthropic Messages. The credential is
  a Puter auth token sent as Bearer.
- **KIE:** a proprietary task API, not Chat/Messages/Responses. Jobs are
  created with `POST /api/v1/jobs/createTask` and polled with
  `GET /api/v1/jobs/recordInfo`; per-model parameters differ by model
  family (video, image, audio, chat agents). **No NexaRoute adapter
  dialect exists for this API yet** — the profile is inventory only
  and `ToProviderConfig` fails closed for it. A future adapter is
  required before KIE can be routed.
- **Mistral:** Responses is in **public preview** (announced
  2026-10-28). An older third-party guide claimed no Responses
  endpoint existed; the official endpoint page is the stronger source.
- **Groq:** Responses is **beta** and **stateful conversations are not
  yet supported** — the client must resend conversation history.
- **NVIDIA NIM:** hosted and self-hosted are separate profiles and
  their capabilities are never transferred between them. The
  self-hosted profile's `localhost:8000` URL is only the conventional
  default; the operator must point it at their own NIM host, and the
  served model set is whatever that single container serves, not the
  hosted catalog. A self-hosted NIM without an auth proxy needs no
  credential; with an auth proxy, configure bearer auth instead.
- **Venice:** Responses is **beta**, native for OpenAI models and
  translated for other models. Unknown OpenAI-only top-level fields
  (for example `logit_bias`, `service_tier`) return HTTP 400.
  Assistants and Batch are not available. Models discovery is readable
  without a key per the vendor docs; key types are INFERENCE vs ADMIN.
- **OpenCode Zen:** separate from the OpenCode Console preset
  (`https://opencode.ai/inference`) and the OpenCode Go preset
  (`https://opencode.ai/inference/go`). Client model ids carry the
  `opencode/` prefix; direct API requests use the bare id. Some models
  retained in reference tables have been retired — check the live
  catalog. Jev decisions (`/zen/v1/systemone`) are a different API, not
  a chat profile.
- **OxionAI:** chat only. No Anthropic Messages endpoint was found in
  the reviewed documentation, so **no messages profile is shipped** —
  do not assume `/v1/messages` exists on this base.
- **OrcaRouter:** `orcarouter/auto` is a router model, not a foundation
  model. Model ids use `provider/model` form. The Anthropic base is the
  bare domain; the SDK appends `/v1/messages`. The same `sk-orca-`
  secret is sent as `x-api-key` on the Messages surface.
- **UnoRouter:** chat and messages, both **unverified** — official
  documentation was not confirmed; two non-official reports agree on
  the OpenAI base `https://api.unorouter.com/v1` and the bare-domain
  Anthropic base.
- **TokenJuice:** chat only, **unverified** — only a single
  non-official mention was found (host `api.tokenjuice.ai`, Chat
  Completions). No official documentation, model list, or
  Messages/Responses endpoint was confirmed. Do not treat the host
  alone as proof of a working endpoint.

## Services deliberately without a profile (UNVERIFIED)

No profile was shipped for these names because no reliable
documentation for the exact API routes and behaviour was found in the
reviewed material. Shipping a profile with an invented base URL or path
would be worse than shipping none. If official documentation appears,
add a profile with its evidence state — do not backfill from a model
name, a key prefix, or a homepage.

| Service | Status on 2026-11-01 |
|---|---|
| APInex | UNVERIFIED as a hosted endpoint. The official APInexGate repository documents paths (`/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1/models`) for a self-hosted gateway, but no hosted base URL was established from a reliable source, so no profile is shipped. |
| BeatAPI | UNVERIFIED — no reliable API documentation found. |
| Inception Labs | UNVERIFIED as a provider endpoint. The name appears in the Ashna catalog as a model family (`inkling`); a model name is not a provider endpoint. |
| LarpRouter | UNVERIFIED — no reliable API documentation found. |
| Refuseless | UNVERIFIED — no reliable API documentation found. |
| ExperientialLabs | UNVERIFIED — no reliable API documentation found. |
| Atria | UNVERIFIED — no reliable API documentation found. |
| Agnes | UNVERIFIED — no reliable API documentation found. |
| InferX | UNVERIFIED — no reliable API documentation found. |
| Faceb | UNVERIFIED — no reliable API documentation found. |
| Routeway | UNVERIFIED — no reliable API documentation found. |
| Apivale | UNVERIFIED — no reliable API documentation found. |
| CompanyFabric | UNVERIFIED — no reliable API documentation found. |
| BluesMinds | UNVERIFIED — no reliable API documentation found. |

Existing presets that already cover a surface in this matrix (so the
profile is a protocol view, not a new endpoint): `mistral` (chat),
`groq` (chat), `nvidia-nim` (hosted chat), `opencode-console` and
`opencode-go` (Console/Go, distinct from Zen).

## Capability tracking (per capability, per profile)

The registry tracks each capability independently: auth, model
discovery, non-streaming responses, SSE streaming and termination
events, usage extraction, tool calls and tool results, parallel tools,
reasoning metadata, structured output, image input, error envelopes,
rate limits, Retry-After, cancellation, and idempotency where
supported. At this release only Ashna Chat documents a wide set
(streaming + `[DONE]` termination, usage, tool calls/results,
structured output, image input, OpenAI error envelope, 429 rate
limits); every other profile documents only the subset in its table
row above. Parallel tools, reasoning metadata, Retry-After,
cancellation and idempotency are **not claimed for any profile** in
this release, because no reviewed documentation stated them for these
endpoints — they are tracked as unknown, not as supported and not as
failed.

## Testing (fake upstreams only)

`internal/providers/profiles_fake_test.go` points each sampled
profile's mapped `ProviderConfig` at a local fake upstream and asserts
the exact request: HTTP method, URL path, auth header, request body,
and the response envelope. Sampled: `ashna-chat` (POST
`/chat/completions`, Bearer), `ashna-messages` (POST `/v1/messages`,
`x-api-key`, `anthropic-version: 2023-06-01`),
`groq-responses` (POST `/v1/responses`, Bearer),
`cloudflare-compat-chat` (template substituted, then POST
`/chat/completions`, Bearer), `orcarouter-messages` (POST
`/v1/messages`, `x-api-key`). These tests verify NexaRoute's wire
shape for the profile. They are **not** provider verification and must
never be quoted as such.

## Smart routing audit

Audited against the existing canonical protocol layer and router
without redesigning unrelated components:

- **Eligibility:** profiles are eligible only for their own protocol.
  Routing selects operator-configured deployments (provider/model),
  not profiles directly, so an operator must create one deployment per
  profile they intend to use. A deployment built from `ashna-chat`
  (`openai_compatible`) is not eligible for an inbound Anthropic
  Messages request; that needs a separate `ashna-messages` deployment
  (`anthropic_compatible`). Profiles do not auto-expand across
  protocols, and model capabilities are not invented by the registry:
  a profile lists only documented capabilities.
- **Explicit failures:** an unsupported feature fails at configuration
  or request time through the existing adapter errors — a profile with
  no adapter dialect (KIE) fails `ToProviderConfig`, a templated base
  URL (Cloudflare) fails until substituted, and a protocol path a
  dialect does not implement is not advertised for that dialect. No
  silent fallback to another protocol is introduced.
- **Failover:** unchanged. The existing retry-safety discipline
  (`internal/retrysafety`) forbids replay after client-visible output
  is committed; profiles add no mid-stream failover and no new retry
  path. Failover across deployments remains an operator-configured
  chain/pool decision before response bytes are committed.
- **Usage and cost accounting:** unchanged. Usage extraction still
  depends on the adapter's canonical usage handling for the deployment
  dialect; a profile's `usage_extraction` capability is claimed only
  where the vendor documentation states usage is returned (Ashna
  Chat). Gateway cache behaviour (for example Cloudflare cache hits
  that never reach the provider) is vendor-side and is not modelled
  here; operators must not bill a cached response as provider usage.
- **Credential pools:** unchanged. `ToProviderConfig` sets a single
  `APIKeyEnv` reference; operators may still attach a `Credentials`
  pool to the deployment exactly as before. Multiple profiles of one
  provider share the provider's credential pool by operator choice —
  the registry itself stores no credential and introduces no new pool
  semantics.

## Admin UI

`GET /admin/api/provider-presets` now returns `profiles` alongside the
existing `presets`, `categories` and `total` fields (additive; existing
callers are unaffected). The admin provider/model management UI should
render, per profile: provider, protocol, base URL, authentication mode
and header, environment-variable reference, model discovery (only
where `ModelsPath` is set), supported capability indicators (only the
documented set), compatibility evidence, and known limitations — all
from this registry, never from fabricated status indicators. Templated
Cloudflare profiles must be rendered with a substitution prompt, not
as usable endpoints, and the KIE profile must be rendered as
inventory-only (not configurable) until an adapter exists.

## Incomplete work (explicit)

- No live upstream call was made for any profile; live acceptance for
  every documented profile still requires operator credentials and
  authorization, and is out of scope for this change.
- KIE needs a new adapter dialect before it can be routed; the profile
  is inventory only.
- Cloudflare `cf-aig-authorization` dual-header auth is not supported
  by NexaRoute's auth modes; authenticated-gateway mode is therefore
  not covered by these profiles.
- Capabilities not documented in the reviewed material (parallel
  tools, reasoning metadata, Retry-After, cancellation, idempotency,
  per-protocol streaming/tools for most multi-protocol gateways) remain
  unknown until vendor documentation or authorized live probes state
  them.
- Media and auxiliary APIs (embeddings, image, audio, video,
  reranking) are separate capability families and are not covered by
  these chat/message/response profiles, except for the noted
  NaraRouter images host and the KIE media task API.
- This change does not merge, close any issue, publish a tag or a
  release, lower any threshold, or mark any undocumented integration
  as verified.
