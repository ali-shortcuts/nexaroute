# Portkey Gateway — control-plane research

**Product researched:** Portkey AI Gateway (official docs, official product pages, and the official open-source gateway repository)  
**Research date:** 2026-10-02  
**Scope:** Provider onboarding; presets/custom endpoints; credentials; protocol/model detection; model tests; health; aliases; simple/fallback routing; agent connection; API keys; observability/activity; settings; advanced separation; empty/error states; large lists; responsive and keyboard UX.

## Executive summary

Portkey’s strongest control-plane pattern is a **two-layer resource model**:

1. **Integrations** are centrally managed credential vaults, with workspace provisioning, model allow-lists, budgets, and rate limits.
2. **Providers** are workspace-facing, named/sluggable views over an integration. Applications reference a provider/model with `@provider-slug/model-name`.

That model keeps secrets out of application code while making environment separation and routing explicit. A second strong pattern is a **universal request surface**: Chat Completions, Responses, and Anthropic Messages formats are accepted at stable gateway endpoints, while the provider/model is selected by a single model string or provider header. Routing behavior is expressed as reusable JSON Configs and attached either per request, at client level, or as an API-key default.

The docs substantiate reliability and observability behavior (fallbacks, retries, load balancing, circuit breakers, status indicators, raw request/response inspection, replay, cost/tokens/latency, traces). They do **not** substantiate several UI details requested for NexaRoute—such as a documented model-test workflow, a standalone provider-health screen, alias CRUD, keyboard shortcuts, responsive breakpoints, or explicit empty/error-state designs. Those should be treated as NexaRoute opportunities, not copied assumptions.

## Verified product patterns

### 1. Provider onboarding and credential separation

**Observed flow:** Model Catalog → Add Provider → select an AI service or Self-hosted/Custom → choose existing credentials or create new credentials → name and slug the provider → save. The official guide explicitly separates credentials from the provider object. Existing org-managed credentials can be selected without re-entering keys; creating credentials in the provider flow makes a workspace-linked integration. Org admins can create reusable Integrations for cross-workspace sharing.

**Why it works:** The user makes the service choice before confronting provider-specific credential fields, and the system gives a safe reuse path. A human-readable name and machine-safe slug make the object understandable in the UI and addressable in code.

**NexaRoute lesson:** Make onboarding a progressive, provider-aware wizard. Keep `credential`, `provider connection`, and `model exposure` as distinct entities. Show scope (workspace vs organization), endpoint type (public/private), and the resulting reference slug before save.

### 2. Integrations as advanced separation

Integrations store credentials once and can be provisioned to selected workspaces. Per-workspace budgets, rate limits, and model access can differ even when the underlying credential is shared. The docs recommend separate integrations for development, staging, and production and least-privilege provisioning. A three-tab organization dashboard is documented: **All**, **Connected**, and **Workspace-Created**.

**Why it works:** It separates secret ownership from usage policy. One vendor key can support multiple environment/provider identities without duplicating secrets, while workspaces receive only approved models and quotas.

**NexaRoute lesson:** Use explicit “credential vault → connection/provider → workspace policy” boundaries. Include an audit-friendly source label (org-created vs workspace-created), provisioning status, model allow-list, and per-scope limits.

### 3. Presets / custom endpoints / model aliases

Portkey calls reusable routing definitions **Configs**. Configs are JSON objects that control fallbacks, load balancing, retries, caching, conditional routing, parameter defaults/overrides/drops, and other gateway behavior. They can be passed as an SDK option, an `x-portkey-config` header, or attached as an API-key default. Request-specific config overrides the client/default config.

Custom/self-hosted models and custom hosts are supported. The universal API docs say the custom host URL includes a version identifier; Portkey appends endpoint paths automatically. Model-level custom headers and hosts can override integration-level headers. Provider/model references use `@provider-slug/model-name`; this is effectively a stable, human-readable address rather than a UI-only alias.

**NexaRoute lesson:** Treat “preset” as a first-class, versionable object with previewable execution semantics. Support fixed targets, custom endpoint + protocol selection, per-target parameter transforms, and explicit precedence: request > client > API-key default. If NexaRoute adds aliases, make alias → immutable target/version visible and auditable.

### 4. Protocol and model detection / normalization

Portkey documents three accepted API formats, all translated across providers: OpenAI Chat Completions (`POST /v1/chat/completions`), OpenAI Responses (`POST /v1/responses`), and Anthropic Messages (`POST /v1/messages`). The same provider/model reference can be used through each format, and switching providers is described as changing only `@provider/model` while keeping the API shape.

Supported endpoint families include chat, responses, messages, images, audio, OCR, fine-tuning, batches, files, moderations, assistants, legacy completions, and gateway-to-other-APIs. The docs caution that not every provider supports every endpoint/modality and point to a compatibility matrix.

**NexaRoute lesson:** Detect protocol from endpoint and request schema, but show the resolved protocol and capability warnings before saving. Model records should expose supported modalities/endpoints, context/token limits, pricing (when available), and provider slug. Never silently imply universal capability when a provider lacks an endpoint.

### 5. Simple and fallback routing

Configs support a `single` strategy and richer strategies such as `fallback` and `loadbalance`. Fallback targets can specify a provider or use a passthrough target that resolves provider information from the incoming provider header or `@slug/model` request model. Targets can inject defaults, overwrite values, or drop fields; the documented execution order is `default_params` → `override_params` → `drop_params`.

Automatic retries, timeouts, conditional routing, caching, and load balancing are documented as gateway features. Circuit breakers remove unhealthy targets from routing and close after a cooldown; if all targets are open, the breaker is bypassed so the request is not permanently stranded.

**NexaRoute lesson:** Make the common path a small, readable route builder: one target first, then “add fallback”. Show target order, trigger conditions, transformed model, and parameter precedence in a dry-run summary. Expose circuit state and last transition reason without making users understand the full JSON schema.

### 6. Health and failure semantics

The circuit-breaker docs provide concrete semantics: failure threshold or failure-rate threshold, minimum requests, cooldown interval (minimum 30 seconds), and selectable failure status codes. Runtime state includes counts, first-failure time, and failure rate. Routing prefers healthy targets; all-open behavior is explicitly defined.

This is **health as routing state**, not merely a green/red dashboard indicator. The public docs do not describe a separate provider-health page, active probing UI, or a model test console.

**NexaRoute lesson:** Represent health as a time-stamped state with cause, scope, and recovery behavior. Provide “test connection” and “send sample request” actions separately from passive health; make the test non-destructive and redact payloads.

### 7. Agent connection

The official LangChain agent integration requires pointing a compatible client at the gateway base URL and supplying Portkey headers. The guide says the same setup provides access to many providers; reliability is added through fallback, load balancing, retries, and timeouts. Agent observability uses trace IDs, metadata, logs, and traces; documented metrics include cost, token usage, and latency. The broader feature overview lists integrations for AutoGen, CrewAI, LangChain, LlamaIndex, PhiData, ControlFlow, and LangGraph.

The repository README also describes MCP Gateway capabilities: centralized authentication, access control, identity forwarding, and per-tool-call observability.

**NexaRoute lesson:** Agent connection should be a copyable integration recipe, not a bespoke runtime. Include base URL, API key, provider/model reference, trace ID/metadata conventions, and a “verify connection” request. Treat tool/MCP activity as a trace child span, not a separate uncorrelated log stream.

### 8. API keys and access

Configs can be attached to an API key as a default, useful for clients that cannot send custom headers. The documented behavior is: the default applies when no request config is supplied; a request-specific config overrides it. Model Catalog describes one Portkey API key accessing multiple providers/models while provider credentials remain hidden. The product page describes virtual keys/access controls, rotation, revocation, and usage monitoring; the current docs say Virtual Keys have migrated to Model Catalog.

**NexaRoute lesson:** Distinguish **consumer/API keys** from **upstream provider credentials** in navigation, forms, masking, and permissions. Show what a consumer key is allowed to call, its default route/preset, last used time, and revocation state. Make precedence explicit in the key detail view.

### 9. Observability and activity

Logs are described as a chronological list of gateway requests. Each entry can show timestamp, request type, model, token counts (including thinking tokens), and cost; multimodal logs can include images. Selecting a row opens a side panel with raw request and response objects. Each log has a shareable URL.

A status column summarizes gateway behavior with explicit states: cache disabled/miss/refreshed/hit, retry not triggered or success/failure on N tries, fallback disabled/active, and load balancer disabled/active. Config and prompt IDs are linked from log details. Replay opens a request in a prompt playground, with documented disabled cases. A `DO NOT TRACK` mode preserves high-level tokens/cost/latency while omitting request/response content.

**Why it works:** The list is an operational activity feed, while the side panel is a deep-debug surface. Status labels compress complex routing behavior into scan-friendly, human-readable evidence. Shareable URLs and replay reduce handoffs between operators and developers.

**NexaRoute lesson:** Build list → detail drawer → shareable deep link → replay/test as a coherent workflow. Make state labels explicit and filterable; support privacy-preserving logging by design.

## Requested UX topics: evidence and gaps

| Topic | Officially verified | NexaRoute implication |
|---|---|---|
| Model tests | No standalone model-test UI or documented test matrix found in the reviewed official pages. Replay exists for eligible logs. | Add a first-class test action: protocol-aware request, masked payload, latency/status/cost, and capability checks. |
| Health | Circuit-breaker health semantics and automatic recovery are documented. | Surface current state, failure evidence, cooldown, and last successful probe/request. |
| Aliases | `@provider-slug/model-name` is the documented stable reference. No separate alias CRUD workflow verified. | If aliases are added, include collision rules, version pinning, and “resolved target” display. |
| Empty states | No official empty-state copy or behavior found. | Design intentional first-run states for no providers, no models, no routes, and no activity; provide one primary next action. |
| Error states | Error/fallback status and replay-ineligible cases are documented; no complete UI error taxonomy found. | Give actionable errors with provider, protocol, target, request ID, redaction, retryability, and next step. |
| Large lists | Docs describe 200+/250+/1600+ model catalogs and 50+ providers, but do not specify UI virtualization/pagination. | Use server-side search/filter, list virtualization or pagination, sticky columns, and bulk policy operations. |
| Responsive UX | No public responsive breakpoint specification found. | Prioritize table-to-card transformation and persistent filter/search on narrow screens. |
| Keyboard UX | No public keyboard-shortcut/accessibility specification found. | Implement standard focus order, visible focus, Escape-to-close drawers, keyboard menus, and accessible status text; verify with automated and manual tests. |
| Settings | Organization/workspace provisioning and admin Integrations are documented; a complete settings IA is not. | Keep security, workspace scope, credentials, provider policy, and API keys distinct but cross-linked. |

## What NexaRoute should learn

1. **Model the control plane as relationships, not a flat provider table:** credential/integration → provider connection → models → workspace policy → consumer API key → route/preset.
2. **Use stable, readable references:** a provider slug plus model name is easier to copy, debug, and audit than opaque IDs alone; still retain immutable internal IDs.
3. **Make precedence visible:** request-level route/preset should override key/client defaults, and the UI should show the effective config before execution.
4. **Turn reliability into evidence:** expose fallback target order, retry count, circuit state, and exact status in activity records.
5. **Make capability boundaries explicit:** protocol, endpoint, modality, context limits, pricing, and model availability should be data—not assumptions.
6. **Support both centralized governance and local autonomy:** org admins can share credentials and restrict models; workspace admins can create scoped connections when allowed.
7. **Design the operational loop end to end:** configure → test → run → inspect → replay → change config.
8. **Build privacy controls into observability:** content logging and high-level metrics should be independently controllable.

## What NexaRoute should not copy

1. **Do not copy Portkey’s branding, naming, visual assets, iconography, screenshot layout, or proprietary UI code.** Recreate the underlying interaction patterns with NexaRoute’s own vocabulary and visual system.
2. **Do not copy claims such as exact model/provider counts, latency, uptime, token volume, or plan limits without independently verified NexaRoute evidence.** The official materials themselves vary between 200+, 250+, 1600+, and 3000+ depending on page and scope.
3. **Do not assume a provider supports every protocol or modality.** Portkey explicitly qualifies this; NexaRoute should validate and warn.
4. **Do not hide routing behavior behind a single “reliable” badge.** Users need target order, trigger conditions, transformed parameters, and recovery state.
5. **Do not expose upstream credentials as if they were consumer API keys.** Keep vault secrets masked and permissions separate.
6. **Do not treat a log as an immutable raw-data dump by default.** Support redaction/no-content modes, retention controls, and least-privilege access.
7. **Do not infer that an undocumented model test, alias manager, health dashboard, responsive layout, or keyboard shortcut exists simply because the platform has the underlying capability.** Treat these as NexaRoute design opportunities and verify them with product testing if Portkey access is available.

## Source URLs

### Primary official documentation

- [Portkey feature overview](https://docs.portkey.ai/docs/introduction/feature-overview)
- [AI Gateway overview](https://docs.portkey.ai/docs/product/ai-gateway)
- [Universal API](https://docs.portkey.ai/docs/product/ai-gateway/universal-api)
- [Configs](https://docs.portkey.ai/docs/product/ai-gateway/configs)
- [Model Catalog](https://docs.portkey.ai/docs/product/model-catalog)
- [Integrations](https://docs.portkey.ai/docs/product/model-catalog/integrations)
- [Virtual Keys migration / Model Catalog](https://docs.portkey.ai/docs/product/ai-gateway/virtual-keys)
- [Observability logs](https://docs.portkey.ai/docs/product/observability/logs)
- [Circuit Breaker](https://docs.portkey.ai/docs/product/ai-gateway/circuit-breaker)
- [LangChain agents](https://docs.portkey.ai/docs/integrations/agents/langchain-agents)

### Official product and repository pages

- [Portkey AI Gateway product page](https://portkey.ai/features/ai-gateway)
- [Portkey-AI/gateway GitHub repository](https://github.com/portkey-ai/gateway)

## Verification notes

This report uses only publicly accessible official Portkey documentation/product/repository pages listed above. The docs were sufficient to verify architecture, routing semantics, credentials, protocol formats, logging, agent integration, and circuit-breaker behavior. Public pages did not provide enough evidence to assert details for standalone model tests, alias CRUD, a dedicated health screen, empty/error-state copy, responsive breakpoints, or keyboard shortcuts; those items are explicitly marked as gaps rather than presented as facts.
