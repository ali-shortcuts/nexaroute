# OpenRouter control-plane research

OpenRouter is a useful reference for NexaRoute because its product surface is a control plane around model/provider discovery, credentialed routing, reusable configuration, and request observability. The strongest verified pattern is **separating stable configuration from per-request code while exposing the routing decision and its consequences**. The notes below are based on current official OpenRouter documentation and public product pages; no proprietary code or branding is copied.

## What OpenRouter does, verified

### Provider onboarding and endpoint metadata

OpenRouter's provider onboarding starts with a public form and a machine-readable model-list endpoint. A provider publishes typed input and output modality objects, with each modality owning capabilities, constraints, passthrough parameters, pricing, and capacity. The schema includes identity, tokenizer, context limits, modality support, generation parameters, pricing, and capacity. This is a better contract than a flat “model name + URL” record because it prevents capabilities from being implied globally when they vary by modality or service tier. [1]

The onboarding contract also has explicit lifecycle controls. `is_ready: false` lets a provider stage a model without exposing it or running baseline tests, or temporarily hide a live model. When ready, OpenRouter's monitor stages new endpoints, runs baseline tests, and unhides them after tests pass and pricing is configured. Service tiers are separate endpoint documents with their own pricing, context length, limits, and capabilities. Deprecation dates can trigger warnings and eventual marketplace hiding. [1]

**What NexaRoute should learn:** make provider onboarding a typed, validated manifest, not a loose form. Show a preflight checklist: credentials valid, endpoint reachable, protocol understood, model metadata valid, pricing/capacity present, test passed, and launch visibility. Distinguish “configured,” “tested,” “ready,” “hidden,” “deprecated,” and “failed.”

**What not to copy:** do not reproduce OpenRouter's marketplace taxonomy, provider names, exact schema, or visual presentation. NexaRoute should use its own domain model and only borrow the general idea that each endpoint declares capabilities and lifecycle state.

### Presets and custom endpoints

Presets separate LLM configuration from application code. A preset can hold model selection, an ordered fallback list, provider routing, system prompt, generation parameters, provider inclusion/exclusion, and tools. It is referenced as `@preset/{slug}`, via a `preset` field, or in a combined model/preset reference. Requests can override preset fields through a shallow merge; preset versions are retained and the latest designated version is used for inference. A preset can also be captured from a known-good inference request, while transient input such as messages is ignored for stored configuration. [2]

This is a strong pattern for **custom endpoints**: give a route a stable semantic name while allowing its implementation to change. It also supports safe iteration without redeploying clients. The tool merge behavior is explicit: request tools override preset tools with the same identity, while other request-only tools are appended. [2]

**NexaRoute pattern:** offer “saved route” or “endpoint profile” resources with a human name, immutable version history, a designated active version, and a “test this configuration” action. Make the effective configuration inspectable before save. Keep transient request payloads out of saved definitions by default.

**Caution:** shallow merge is easy to explain but can create surprising nested behavior. NexaRoute should show a resolved diff (“inherited,” “overridden,” “added”) and reject ambiguous conflicts rather than silently applying an opaque merge.

### Credentials, API keys, and provider connections

OpenRouter separates ordinary inference API keys from management API keys. Inference keys have names and credit limits, can be disabled or deleted, and are intended for completion calls. Management keys are administrative only, have a fixed expiration chosen at creation, are shown once, and can create, update, paginate, disable, or delete inference keys. The documentation explicitly recommends limits and rotation because a leaked unlimited key can spend the account balance. [3] [4]

Provider credentials are handled as BYOK integrations. Keys are encrypted, scoped to a workspace/provider, and arranged into **Prioritized** and **Fallback** sections. A prioritized key is tried first; fallback keys are attempted after shared OpenRouter capacity. Each key has a “shared capacity fallback” policy, a model filter, and a provider-agreement/ZDR declaration. BYOK can override ordinary provider ordering for the initial attempts, so the UI must explain the effective order rather than only showing the user-authored order. [5]

**NexaRoute should learn:** use separate credential classes and least-privilege scopes; show “secret displayed once”; require or strongly encourage expiration and spend limits; provide rotate, disable, revoke, and last-used metadata. For upstream connections, make priority/fallback order visible as an executable sequence and expose the policy that applies when a credential fails or does not match a model.

**Do not copy:** do not use OpenRouter's exact key labels, BYOK fees, provider-agreement wording, or URLs. Do not imply that encrypted storage makes a credential risk-free; preserve clear ownership and audit history.

### Protocol and model detection

The public model API exposes canonical IDs, human names, descriptions, context length, architecture, tokenizer, pricing, top-provider settings, supported parameters, default parameters, expiration, and optional benchmarks. Models can be filtered by output modality and supported parameter, and sorted by price, context, throughput, latency, popularity, or recency. Single-model lookup resolves aliases and supports catalog/routing variants; nonexistent IDs return 404. [6]

This gives NexaRoute a verified model-detection pattern: identify protocol/format from an explicit adapter declaration, then validate the model against capability metadata. The model record should answer: what inputs and outputs are supported, which parameters are legal, what context limit applies, what endpoint/provider actually served, and whether the ID is canonical or an alias.

**NexaRoute should learn:** maintain canonical IDs plus alias resolution, and show the canonical target when a user enters an alias. Validate capabilities before a test run. Use server-side filter/sort and pagination for large model lists; OpenRouter's API supports `offset`, `limit` up to 1000, and a ready-to-use `links.next` URL. [6] [7]

**Do not copy:** do not promise that an alias is permanent unless NexaRoute owns that compatibility contract. Avoid hiding variant semantics behind cryptic suffixes; explain whether a variant changes catalog identity or only routing behavior.

### Model tests and health

OpenRouter's provider monitor automatically stages and baseline-tests new endpoint records, only making them live after tests pass and pricing is configured. It also calculates uptime from successful requests divided by total requests, excludes user errors from the uptime denominator, tracks rate limits and geographic restrictions separately, and adjusts traffic based on reliability. Provider response detail includes retries, fallbacks, latency, moderation time, and overhead. [1] [8]

**NexaRoute pattern:** separate deterministic connection tests from real-traffic health. A test result should include timestamp, protocol check, authentication check, model availability, capability checks, latency, error class, and the exact remediation. Health should be a time-windowed signal with a visible denominator and explicit treatment of 4xx, 429, 5xx, timeouts, and mid-stream failures. Never reduce health to one green dot.

For test UX, include a safe sample prompt, a cost estimate, an abort action, and a “do not save until test passes” option. Preserve the raw request/response and provider attempt timeline for debugging, while redacting secrets.

### Simple routing and fallback routing

Provider routing accepts explicit order, allow/deny fallback, parameter compatibility, data-collection policy, ZDR, provider allow/ignore lists, quantization, sorting by price/throughput/latency, throughput/latency thresholds, and maximum price. Default routing load-balances by price while weighting recent stability; an explicit sort or order disables load balancing. [8]

Model fallback is deliberately simple: pass a priority-ordered `models` array and try the next model when the first errors. Default triggers include context-length errors, moderation flags, rate limits, and downtime. The response identifies the model ultimately used, and billing uses that model's price. The Anthropic-compatible fallback form is limited to three entries and cannot be combined with `models`. [9]

**NexaRoute should learn:** offer two modes. “Simple fallback” should be a short ordered list with an understandable trigger policy. “Advanced routing” should expose provider constraints, data policy, performance thresholds, and cost ceilings. In both modes, preview the execution graph and show the actual attempts afterward. A fallback is not just a second dropdown; it is a policy with failure semantics.

**Do not copy:** do not copy OpenRouter's inverse-square price weighting or exact trigger set without validating NexaRoute's own economics and safety requirements. Never silently retry non-idempotent operations or hide moderation/policy failures behind a different model.

### Agent connection

OpenRouter's core integration model is a standard Bearer-key API and OpenAI-compatible base URL, with optional site metadata headers. Its documentation also publishes dedicated coding-agent integration recipes that place the API key in environment variables and configure the agent's base URL/model. [3] [10]

**NexaRoute pattern:** provide a connection wizard that generates an adapter snippet for an agent, but keep secret values out of copied client code where possible. Give the agent a named connection, selected model/route, scope, expiry, and spend cap. Add a one-click “copy configuration,” “test connection,” and “revoke connection” path. Separate human dashboard credentials from machine credentials.

### Observability and activity

OpenRouter has two complementary observability surfaces. Logs is request-level: generations, upstream requests, sessions, videos, and batches. Filters cover user, model, provider, status, region, API key, modality, and generation ID; filters are stored in the URL. A generation detail sheet exposes model/provider, cost, tokens, finish reason, streaming, latency, throughput, region, fallbacks, provider-response waterfall, usage breakdown, prompt/completion (when enabled), and raw JSON. A request can be opened in Chatroom or replayed in Playground. [11]

Activity is aggregate: spend, requests, tokens, blended cost, cache-hit rate, trends, top keys/apps/users, and configurable exploration by metric, group, subgroup, rollup, rank limit, and chart type. Filters persist across tabs and saved charts can be private or organization-wide. CSV/PDF export is available. [12] [13]

Input/output logging is opt-in, beta, scoped through Observability settings, filterable by included/excluded API keys, and only affects generations created after enablement. By default, metadata is stored without prompt/completion content. Broadcast separately sends traces to external destinations, supports per-destination credentials, API-key filters, sampling, privacy mode, and test/configuration in Settings > Observability. [14] [15]

**NexaRoute should learn:** pair a searchable, filterable request ledger with aggregate analytics; link every chart back to its underlying requests. Persist filters in the URL. Make raw JSON and attempt waterfalls first-class debugging tools. Make content capture opt-in and distinguish “internal private logging” from “external export.” Support per-destination sampling and privacy redaction.

**Do not copy:** do not default to storing prompts and completions. Do not conflate cost estimates, BYOK spend, and settled charges. Clearly label derived/estimated values and retention/access rules.

### Settings and advanced separation

Workspaces provide a verified separation boundary: each workspace can own API keys, guardrails, BYOK keys, routing defaults, presets, plugins, observability, members, and budgets. Account-level activity/logs, billing, organization, management keys, privacy, and preferences remain global, with workspace filters available. Guardrails can restrict models/providers, budgets, regions, ZDR, prompt-injection, sensitive information, and custom filters; eligibility preview shows effective providers/models before assignment. [16] [17]

**NexaRoute pattern:** use a clear scope hierarchy: account > workspace/project > connection/route > request. Put inherited settings beside local overrides. Provide an eligibility preview for effective model/provider access. Ensure an “advanced” panel is a progressive disclosure layer, not a separate hidden product: users should always see the resulting policy in plain language.

### Empty/error states, large lists, responsive and keyboard UX

The official docs verify several useful states and affordances: loading placeholders and an error message for provider terms retrieval; 404 for unknown model IDs; hidden/staged models through `is_ready`; explicit disabled/expired keys; and filterable lists with pagination and URL-preserved state. [8] [6] [4] The docs also show tables, detail sheets, tabs, dropdowns, and responsive screenshots, but they do not document a complete keyboard interaction contract.

For NexaRoute, implement these states deliberately:

- **Empty:** explain why there are no connections/models/logs, offer the next action, and preserve search/filter context.
- **Loading:** skeleton the shape of the eventual content; do not replace the whole page with a spinner.
- **Error:** state which operation failed, whether retry is safe, the provider/request ID, and a recovery action.
- **Partial:** show healthy items even when one provider or analytics query fails.
- **Large list:** server-side search/filter/sort, pagination or virtualization, stable row keys, counts, bulk actions with confirmation, and a URL/shareable query.
- **Responsive:** keep primary actions and status visible at narrow widths, move secondary metadata into a drawer, and avoid tables that require horizontal scrolling for the only way to identify health.
- **Keyboard:** every filter, tab, row, drawer, menu, and test action must be reachable by keyboard; preserve visible focus, use logical tab order, support Escape to close drawers/menus, and announce async test/health results to assistive technology.

These are **NexaRoute recommendations**, not claims that OpenRouter's public pages satisfy every requirement. The public documentation is strong on data model and workflow semantics but does not provide enough evidence to grade OpenRouter's full keyboard behavior or all responsive breakpoints.

## Why the patterns work

- **Typed endpoint metadata** reduces configuration ambiguity because capability, limits, and cost travel with the modality/tier that owns them.
- **Presets and versioning** let teams improve routing without changing every client and create a rollback/audit point.
- **Separate inference and management credentials** limits blast radius and makes automation safe to rotate.
- **Explicit priority/fallback semantics** prevent a visually simple list from hiding expensive or privacy-relevant behavior.
- **Attempt-level logs plus aggregate activity** connect operational debugging to financial control.
- **Workspace and guardrail separation** lets one account support staging, production, teams, and agents without mixing keys or policy.
- **Opt-in content capture and per-destination privacy** preserve observability while reducing accidental data exposure.

## Sources

[1]: https://openrouter.ai/docs/guides/community/for-providers "Provider Integration"
[2]: https://openrouter.ai/docs/guides/features/presets "Presets"
[3]: https://openrouter.ai/docs/api_reference/authentication "Authentication"
[4]: https://openrouter.ai/docs/guides/overview/auth/management-api-keys "Management API Keys"
[5]: https://openrouter.ai/docs/guides/overview/auth/byok "BYOK"
[6]: https://openrouter.ai/docs/guides/overview/models "Models"
[7]: https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties "List all models and their properties"
[8]: https://openrouter.ai/docs/guides/routing/provider-selection "Provider Routing"
[9]: https://openrouter.ai/docs/guides/routing/model-fallbacks "Model Fallbacks"
[10]: https://openrouter.ai/docs/cookbook/coding-agents/codex-cli "Codex CLI Integration"
[11]: https://openrouter.ai/docs/guides/features/logs "Logs"
[12]: https://openrouter.ai/docs/guides/features/activity "Activity"
[13]: https://openrouter.ai/docs/cookbook/administration/activity-export "Activity Export"
[14]: https://openrouter.ai/docs/guides/features/input-output-logging "Input & Output Logging"
[15]: https://openrouter.ai/docs/guides/features/broadcast "Broadcast"
[16]: https://openrouter.ai/docs/guides/features/workspaces "Workspaces"
[17]: https://openrouter.ai/docs/guides/features/guardrails "Guardrails"
