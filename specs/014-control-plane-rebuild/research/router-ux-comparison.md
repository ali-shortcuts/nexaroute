# NexaRoute v0.14.0 — Router UX Comparison and Control-Plane Recommendations

**Purpose.** This report synthesizes the supplied public-product research into a UX and information-architecture recommendation for the NexaRoute v0.14.0 control-plane rebuild. It borrows **interaction principles and operational models**, not product branding, wording, visual design, source code, configuration schemas, URLs, or scripts.

## Evidence and confidence

- **Documented pattern** means a behavior described in the linked public docs/repository material supplied with this research.
- **NexaRoute recommendation** is a product decision inferred from repeated patterns; it is **not** a claim that every compared product implements it.
- **Uncertain / validate** means the reviewed public materials did not establish the point. It must be specified, implemented, and usability/accessibility tested by NexaRoute rather than attributed to a comparator.
- Names below identify research inputs only. They are not proposed NexaRoute navigation labels or data-model names.

## Executive decision

NexaRoute should be designed as an **inspectable routing control plane** around a stable gateway endpoint:

> **Credential vault → connection → deployment/model capability → saved route → consumer key or agent connection → request attempt trace**

The pivotal design choice is to keep these objects—and their permissions—separate. A provider secret authorizes an upstream connection; a runtime/client key authorizes gateway use; a management credential administers the control plane. A successful dashboard health check is not evidence that a model route works. A friendly model name is not proof of the canonical upstream deployment. A successful request is not proof that the first target succeeded.

Ship a very short first-run path—**choose a preset or custom endpoint → add a named upstream credential → select/pin a model → run a bounded test → enable a simple route → create a scoped consumer connection/key**—while treating routing rules, fallback policy, transformations, health policy, catalog details, and data retention as progressive disclosure.

## Comparison at a glance

| Research input | Strongest reusable pattern | Evidence that should influence v0.14.0 | Important limit / uncertainty | NexaRoute adaptation |
|---|---|---|---|---|
| Claude Code Router (CCR) | Local gateway/control-plane split; provider wizard and agent profiles | Preset or custom endpoint; protocol/model discovery with manual override; bounded real connection check; default route before ordered rules; request logs show requested vs. resolved target and credential. [CCR provider guide](https://ccrdesk.top/en/guides/provider/) · [routing](https://ccrdesk.top/en/configuration/routing/) · [observability](https://ccrdesk.top/en/configuration/observability/) | Public docs do not establish detailed keyboard behavior, screen-reader semantics, or list virtualization. | Make the gateway, management plane, upstream secret, runtime key, and agent profile visibly distinct. Use a health ladder instead of one status. |
| LiteLLM Proxy | Separate public model identity, deployment, reusable credentials, health, routing, and usage | Named reusable credentials; config-vs-database source markers; test connection separate from health checks; route order/retry/cooldown/fallback are separate concepts; effective-permission and content-retention controls. [model management](https://docs.litellm.ai/docs/proxy/model_management) · [health](https://docs.litellm.ai/docs/proxy/health) · [load balancing](https://docs.litellm.ai/docs/proxy/load_balancing) | Documentation does not prove full responsive or keyboard quality. | Give every resource an owner/source and make the resolved protocol, model capability, and policy precedence inspectable. |
| Portkey Gateway | Credential/integration → workspace provider → reusable route configuration | Centrally stored credentials are provisioned as workspace-facing provider connections; readable provider/model references; configs and API-key defaults have explicit precedence; activity supports detail, deep link, replay, and redacted metrics-only logging. [model catalog integrations](https://docs.portkey.ai/docs/product/model-catalog/integrations) · [configs](https://docs.portkey.ai/docs/product/ai-gateway/configs) · [logs](https://docs.portkey.ai/docs/product/observability/logs) | No reviewed public evidence of a standalone model-test screen, full empty-state system, keyboard shortcuts, or breakpoints. | Model a connection and its scope separately from the secret vault; make route resolution and attempt status operationally visible. |
| OpenRouter | Versioned saved route configuration with inspectable provider selection and request history | Typed model/endpoint metadata and lifecycle; stable, versioned presets; credential priority; canonical IDs and visible alias resolution; request waterfall plus aggregate activity; opt-in content logging. [provider guide](https://openrouter.ai/docs/guides/community/for-providers) · [presets](https://openrouter.ai/docs/guides/features/presets) · [provider routing](https://openrouter.ai/docs/guides/routing/provider-selection) · [logs](https://openrouter.ai/docs/guides/features/logs) | Public sources do not verify a complete keyboard interaction contract or every responsive breakpoint. | Use versioned saved routes with a resolved-policy diff and a test-before-activation workflow; distinguish attempts, requests, estimates, and settled spend. |
| Helicone AI Gateway | Minimal familiar gateway contract with explicit provider/deployment escape hatches | Separate provider, model, endpoint/deployment, and credential; model-only route with provider/deployment/fallback escalation; rolling health and attempt-aware final errors; agent integration metadata. [provider integration](https://docs.helicone.ai/references/provider-integration) · [provider routing](https://docs.helicone.ai/gateway/provider-routing) · [error handling](https://docs.helicone.ai/gateway/concepts/error-handling) | **Uncertain:** supplied docs conflict on hosted versus self-hosted availability; no verified keyboard/responsive contract. | Make deployment mode and source of truth explicit; never make any 400/401 automatically eligible for fallback. |
| Bifrost | Catalog-assisted identity plus distinct retry, credential rotation, fallback, and agent/MCP safety | Preset-to-custom onboarding; provider-qualified pinning when a bare model is ambiguous; separate resilience stages; governed virtual keys; MCP state and approval; live filterable request activity. [provider configuration](https://docs.getbifrost.ai/quickstart/gateway/provider-configuration) · [fallbacks](https://docs.getbifrost.ai/features/fallbacks) · [MCP overview](https://docs.getbifrost.ai/mcp/overview) | Repository states accessibility/responsive goals, but exact interactions and breakpoints remain **uncertain**. | Make every stage of resolution explainable, and treat a connected MCP server as available for approval—not automatically safe to execute. |

## Cross-product patterns

### 1. The product is a graph of governed resources, not a provider table

All six inputs separate some version of secret ownership, provider connection, model/deployment metadata, route policy, consumer access, and observability. The most durable shared shape is:

1. **Credential** — an upstream authorization material with a name, scope, last use, rotation/revocation state, and no routine plaintext display.
2. **Connection** — a provider/protocol/base endpoint that references one or more credentials and exposes models/deployments to a workspace or environment.
3. **Model/deployment** — canonical identifier, provider-qualified identifier, capabilities, limits, catalog source/freshness, and lifecycle.
4. **Route** — stable public name, primary target, optional fallback policy, and a resolved effective version.
5. **Consumer access** — runtime key, client connection, agent profile, or tool connection with allowed routes/models and limits.
6. **Attempt trace** — the request, route decision, individual target attempts, final result, redaction state, and remediation path.

This prevents the frequent failure mode in which a user believes an “API key” both connects an upstream provider, calls the gateway, and administrates the console. The separate key classes are documented in CCR, LiteLLM, OpenRouter, and Bifrost sources. [CCR API keys](https://ccrdesk.top/en/configuration/api-keys/) · [LiteLLM virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys) · [OpenRouter management keys](https://openrouter.ai/docs/guides/overview/auth/management-api-keys) · [Bifrost API keys](https://docs.getbifrost.ai/api/procuring-api-keys)

### 2. Preset plus escape hatch beats a catalog-only or raw-config-only setup

Each strong onboarding model offers a common-provider happy path and a custom/self-hosted path. The custom path must expose protocol/base URL and capability validation rather than assuming any endpoint is OpenAI-compatible. CCR supports protocol/model detection with override; LiteLLM pass-through makes path, target, headers, methods, timeout, and pricing progressively available; Helicone and Bifrost make provider-specific transforms/base-provider type explicit. [CCR providers](https://ccrdesk.top/en/configuration/providers/) · [LiteLLM pass-through](https://docs.litellm.ai/docs/proxy/pass_through) · [Helicone provider integration](https://docs.helicone.ai/references/provider-integration) · [Bifrost provider configuration](https://docs.getbifrost.ai/quickstart/gateway/provider-configuration)

### 3. Identity, capability, and aliasing must be explicit

A public route/model identity is a compatibility promise; it must be distinct from a provider deployment identifier. Strong inputs show canonical IDs, readable qualified references, and/or aliases, but also reveal the risk of ambiguity. NexaRoute should resolve a typed input into a visible canonical target and ask the operator to pin a provider when resolution is ambiguous. Do not silently normalize model aliases or rename providers. [LiteLLM model aliases](https://docs.litellm.ai/docs/completion/model_alias) · [OpenRouter model API](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties) · [Bifrost overview](https://docs.getbifrost.ai/overview)

### 4. Reliability is an execution sequence, not a switch

Across LiteLLM, Portkey, OpenRouter, Helicone, and Bifrost, routing can have priority/order, retries, credential rotation, cooldown/circuit behavior, and cross-target fallback. These are related but not interchangeable. The useful shared design is a visual execution sequence:

`primary target → same-target retry policy → credential-pool selection/rotation → next fallback target → final surfaced error`

A successful final response should still show prior failed attempts. A target’s circuit state and cooldown should explain current behavior. Tests, passive routing health, and gateway liveness should remain separate signals. [LiteLLM health](https://docs.litellm.ai/docs/proxy/health) · [Portkey circuit breaker](https://docs.portkey.ai/docs/product/ai-gateway/circuit-breaker) · [OpenRouter fallbacks](https://openrouter.ai/docs/guides/routing/model-fallbacks) · [Helicone error handling](https://docs.helicone.ai/gateway/concepts/error-handling) · [Bifrost fallbacks](https://docs.getbifrost.ai/features/fallbacks)

### 5. Activity is useful only when it closes the diagnostic loop

The consistent operational loop is **configure → test → run → inspect → replay/diagnose → change**. Good activity answers: What did the client request? Which route/version was resolved? Which target and credential were tried? Which attempt failed, why, and whether it was retryable? What happened next? Logs then link to an editable resource, a safe replay/test, and a concrete next step. [CCR observability](https://ccrdesk.top/en/configuration/observability/) · [Portkey logs](https://docs.portkey.ai/docs/product/observability/logs) · [OpenRouter activity](https://openrouter.ai/docs/guides/features/activity) · [Bifrost observability](https://docs.getbifrost.ai/features/observability/default)

### 6. Privacy, scope, source, and precedence are product-state—not footnotes

Every decision needs visible scope and ownership: organization/workspace/environment; UI/database/config/GitOps; request/client/runtime-key/route setting; managed/BYOK/host-injected secret; metrics-only/content capture/export destination. LiteLLM documents UI/config precedence and prompt retention; OpenRouter distinguishes workspace, inference/management key, content logging, and external broadcast; Portkey documents request/client/key configuration precedence. [LiteLLM configs](https://docs.litellm.ai/docs/proxy/configs) · [LiteLLM UI spend/log settings](https://docs.litellm.ai/docs/proxy/ui_spend_log_settings) · [OpenRouter input/output logging](https://openrouter.ai/docs/guides/features/input-output-logging) · [OpenRouter broadcast](https://openrouter.ai/docs/guides/features/broadcast) · [Portkey configs](https://docs.portkey.ai/docs/product/ai-gateway/configs)

## Prioritized v0.14.0 recommendations

### P0 — required for a coherent first release

| Area | Recommendation | Acceptance signal |
|---|---|---|
| Provider onboarding | Build a five-step wizard: **choose preset/custom → name and scope connection → add/select named upstream credential → select/discover or manually enter model → bounded test and enable**. Show protocol detection, but always allow an explicit override. Put endpoint headers, paths, network controls, timeouts, pricing, and transforms under Advanced. | A user can connect one common provider or a custom compatible endpoint without raw configuration; each can be saved in an untested state with an honest status. |
| Credentials | Create three visibly distinct resource types: **upstream provider credential**, **runtime/client key**, and **management/control-plane credential**. Use masked lists, one-time reveal, expiry/rotation/revoke, scope, last-used, and clear impact copy. | No screen calls all three simply “API keys”; a runtime key never reveals or grants provider-secret administration. |
| Model and route identity | Store canonical provider/model/deployment IDs separately from a public route/model name and optional alias. Display the resolved canonical target and collision rule before save and in activity. | Ambiguous bare input produces a pin-or-correct choice, never silent selection. |
| Simple routing | Make **one primary target plus optional ordered fallbacks** the default builder. Offer one visible failure-policy summary. Render an execution preview before activation. | An operator can explain target order and trigger behavior without opening expert controls. |
| Tests and health | Offer three distinct actions/states: **validate configuration** (no inference when possible), **run a bounded active request test** (model/protocol/auth/capability evidence), and **observe passive/ongoing health** (real-traffic or scheduled evidence). Active tests require explicit model/scope and disclose expected quota/token use. | A green configuration state cannot obscure a failed model test; a healthy route states its time window and denominator. |
| Connect | Add a **Connect** surface for runtime client and agent setup: choose a route, scope, expiry/limit, and client style; generate a copyable configuration template plus a verification request; link directly to its Activity filter. Treat tool/MCP connection separately with transport, auth, discovered tools, and approval policy. | “Connected” identifies endpoint, route, scope, credential class, last verification, and activity—not merely a code snippet. |
| Activity | Ship a request ledger plus detail drawer. The drawer must show request ID/time, requested model/route, resolved route version, attempt waterfall, actual provider/model/deployment, credential identity (not secret), status/error class, latency, tokens/cost estimate labeling, content-redaction state, and next diagnostic action. | A fallback success includes failed child attempts; an operator can deep-link to a request and filter by route/provider/key/status. |
| Observability safety | Default to metadata/metrics rather than prompt or completion storage. Make content capture, sampling, retention, and any external export independently opt-in, scoped, and explicit about whether a setting applies only to new data. | The first test request cannot accidentally store raw content without an informed choice. |
| Resource states and accessibility | Define loading, empty, no-match, denied, failed, stale, disabled, partially configured, checking, tested, ready, degraded, and unavailable states per resource. Keyboard operation, focus return, accessible async status, and responsive layouts are release acceptance criteria. | No blank list conflates no data, filter mismatch, permission denial, or load failure. |

### P1 — high-value after P0’s simple loop works

| Area | Recommendation | Why now |
|---|---|---|
| Reusable connections and scoped policy | Separate a credential vault from workspace/environment connections, model exposure, budgets, rate limits, and allow-lists. Label ownership and source. | Reuses secrets safely without accidental cross-environment access. |
| Saved route profiles | Make routes versioned resources with a human name, active version, change note, resolved inherited/overridden/added policy diff, test-before-activate, rollback, and activity linkage. | Lets clients hold a stable identifier while the implementation changes safely. |
| Reliability detail | Split retry, credential-pool rotation, cooldown/circuit behavior, fallback, and load balancing into individual controls. Add testable policy preview and per-attempt trace. | Avoids the opaque “reliability” toggle and supports incident diagnosis. |
| Capability catalog | Add model cards/details with protocol, modalities/endpoints, supported parameters, token/context limits, pricing provenance, region/deployment, discovery status, and last refresh. | Prevents a user from discovering capability mismatch only at request time. |
| Effective policy | Provide a read-only effective-policy view for a request/route/key/agent: inherited source, local overrides, limiting allow-list, and final permitted targets. | Makes multi-scope governance understandable. |
| Activity at scale | Use server-side filter/sort/page or virtualization, debounced search, stable sorting, bulk actions with confirmation, URL-persisted filter state, and a distinct aggregate Activity view. | Preserves operability for large model catalogs and request volume. |
| Settings and declarative mode | Show configuration source (UI/API/config/GitOps), read-only status, drift, precedence, and restart/apply implications. Provide export/import only as a secondary automation surface. | Avoids invisible configuration conflict and raw config as the normal UX. |

### P2 — only after the core contract and evidence model are stable

| Area | Recommendation | Guardrail |
|---|---|---|
| Advanced routes | Add ordered conditions, scoped rules, target transformations, policy constraints, weighted/load-based selection, and an expert expression mode. Keep visual builder first and preserve disabled rules for audit. | Show first-match/order semantics and a dry run; never require expression syntax for the common path. |
| Organization/workspace administration | Add account → workspace/project → connection/route → request scope, membership, role policy, eligibility previews, and audit trails. | Make inherited scope visible at every level. |
| Agent and tool governance | Add SDK/MCP connection templates, tool namespace/transport negotiation, health/reauth state, tool allow-list, and explicit host-controlled/manual/allowlisted approval. | Connection does not equal authorization to execute tools. |
| Advanced analytics and export | Add aggregate trends, top routes/keys/users, configurable charts, CSV/PDF export, and safe external trace destinations. | Keep request counts, upstream attempts, cost estimates, and settled charges distinctly labeled. |

## Product-surface guidance

### Provider onboarding

**Recommended flow**

1. **Select connection type.** Cards for vetted presets and “custom endpoint.” A preset pre-fills only operational metadata; it does not hide the resulting protocol or endpoint.
2. **Name and locate it.** Require a human label, machine-safe unique reference, workspace/environment scope, and source/owner.
3. **Authenticate.** Choose an existing named upstream credential or create one. Dynamic fields adapt to provider/protocol; secret is never put into a model row.
4. **Discover or declare.** Run a non-billing metadata check when possible; otherwise explain why discovery/test will make an upstream call. Display detected protocol, capability evidence, and catalog freshness. Offer manual model/deployment entry when discovery is unavailable.
5. **Verify deliberately.** Let the user select the specific model(s) to test, choose a bounded test, see cost/quota notice, then examine model/protocol/auth/upstream diagnostics before enabling.

**States to show:** Draft; configured; discovery unavailable; test required; checking; tested; ready; disabled; stale; credential needs attention; policy-limited; failed. Do not automatically make “test passed” mean “enabled”; activation has a different operational effect.

**Source patterns:** CCR documents selected-model real connection checks and manual custom models; LiteLLM documents provider-specific credential fields and a per-model test; Bifrost documents provider-qualified resolution and custom compatible endpoints. [CCR provider guide](https://ccrdesk.top/en/guides/provider/) · [LiteLLM model management](https://docs.litellm.ai/docs/proxy/model_management) · [Bifrost setup](https://docs.getbifrost.ai/quickstart/gateway/setting-up)

### Simple routing

**Default page:** “Where should requests for this route go?”

- Choose a single primary model/deployment from known eligible targets.
- Add an optional fallback list by explicit order.
- Display a compact policy sentence, e.g., “Use Primary; retry according to this route’s retry policy; then evaluate fallback targets for eligible failure classes.”
- Include a read-only “Resolved execution” panel with primary, credential selection/pool, retry rule, fallback triggers, and final error behavior.
- Add **Test this route** and show an attempt waterfall. Keep raw transforms or weights out of the default surface.

**Advanced escalation:** policy constraints (region/data policy/capability), target transforms, circuit/cooldown controls, weighted/load-based choice, route conditions, override precedence, and expert expressions. Advanced must remain explainable in the same resolved panel.

**Safety:** do not silently retry non-idempotent operations; do not treat policy/moderation, credential, malformed-request, and transient upstream errors as universally equivalent failover triggers. This follows the documented need to distinguish fallback semantics, including caution around errors, rather than copying any one vendor’s error priority. [OpenRouter fallbacks](https://openrouter.ai/docs/guides/routing/model-fallbacks) · [Helicone error handling](https://docs.helicone.ai/gateway/concepts/error-handling) · [Bifrost routing](https://docs.getbifrost.ai/providers/provider-routing)

### Connect: client, agent, and tool setup

Use **Connect** as the bridge from control-plane configuration to an actual client. Its purpose is not another kind of upstream provider credential.

| Connect resource | Must show | Must not imply |
|---|---|---|
| Runtime/client connection | Gateway endpoint, selected route/public model, consumer key scope, expiry/limits, environment, last verification, copyable non-secret configuration, verification request, Activity link | That it can administer the control plane or reveal upstream secrets |
| Agent profile/connection | Agent/client type, route/model, launch or environment scope, enabled state, required permissions, trace/session metadata, verification, revoke | That the system default applies to direct launch unless scope says so |
| Tool/MCP connection | Transport, URL/command, auth class, discovered tools/namespace, last sync, health/reauth, user/host approval policy, Activity link | That discovery or connection grants auto-execution authority |

CCR’s profile scope, LiteLLM’s explicit MCP transport/namespace/headers, Helicone’s agent integration metadata, and Bifrost’s approval distinction support this separation. [CCR profiles](https://ccrdesk.top/en/configuration/profiles/) · [LiteLLM MCP](https://docs.litellm.ai/docs/mcp) · [Helicone Claude Agent SDK](https://docs.helicone.ai/gateway/integrations/claude-agent-sdk) · [Bifrost MCP](https://docs.getbifrost.ai/mcp/overview)

### Activity

Use two deliberately different surfaces:

- **Requests:** the operational ledger. Server-side filters should include time, route, requested model, resolved provider/model/deployment, consumer key, status/error class, region, content-logging state, and request ID. A row opens a keyboard-accessible detail drawer/sheet.
- **Activity:** aggregate usage and reliability. Counts, success rate, p95 latency, tokens, estimated/settled cost distinctions, cache ratio where relevant, top route/key/client, and trends. Every aggregate can drill into its requests.

The request detail must retain the difference between **one inbound request** and **multiple upstream attempts**. It should show each attempt’s target, credential label, route decision, retry/fallback/circuit event, timing, response class, and redaction. This follows LiteLLM’s distinction between gateway traffic and spend attribution, Portkey’s status/replay flow, and OpenRouter’s provider-attempt waterfall. [LiteLLM endpoint activity](https://docs.litellm.ai/docs/proxy/endpoint_activity) · [Portkey logs](https://docs.portkey.ai/docs/product/observability/logs) · [OpenRouter logs](https://openrouter.ai/docs/guides/features/logs)

### Settings and advanced separation

Keep Settings for cross-cutting administration, while object-specific advanced controls live with their owning resource.

| Group by operator intent | Contents | Cross-link from |
|---|---|---|
| Workspace & access | members, roles, management credentials, sessions, runtime-key defaults | consumer-key and Connect details |
| Providers & models | presets, credential vault, connections, catalog refresh, custom endpoints, aliases | route target picker |
| Routing & reliability | route versions, retry/circuit defaults, fallbacks, budgets, rate/allow policies | route resolved-policy panel |
| Activity & data | metadata/content capture, redaction, sampling, retention, exports/destinations | request detail redaction state |
| Agents & tools | agent connections, MCP connections, tool approvals, sync/health | Connect cards and activity traces |
| Deployment & source | config/GitOps source, environment references, storage, network restrictions, read-only/restart effects | each resource source badge |

Settings must never quietly merge sources. Every editable item needs a source badge (for example, console-managed, declaratively managed, externally synchronized), effective precedence, and what a change will affect. The document patterns for config/database precedence and file-only read-only behavior support this priority. [LiteLLM configs](https://docs.litellm.ai/docs/proxy/configs) · [Bifrost config JSON](https://docs.getbifrost.ai/deployment-guides/config-json)

### Accessibility, responsive UX, and large-list contract

**These are NexaRoute requirements, not comparator claims.** The supplied public docs often establish data-model and workflow patterns but do **not** establish complete keyboard interaction, focus management, screen-reader announcements, list virtualization, or all breakpoints. CCR, LiteLLM, Portkey, OpenRouter, and Helicone findings explicitly leave these gaps; Bifrost’s public repository describes accessibility/responsiveness goals but not sufficient implementation detail to treat them as verified behavior. [CCR repository](https://github.com/musistudio/claude-code-router) · [LiteLLM UI](https://docs.litellm.ai/docs/proxy/ui) · [Portkey feature overview](https://docs.portkey.ai/docs/introduction/feature-overview) · [OpenRouter models](https://openrouter.ai/docs/guides/overview/models) · [Bifrost repository](https://github.com/maximhq/bifrost)

| Contract | v0.14.0 requirement |
|---|---|
| Keyboard | All navigation, tabs, comboboxes, row actions, filters, route reordering, test actions, drawers, dialogs, menus, copy actions, and destructive confirmations work without a pointer. Support Escape to close transient UI, Enter/Space according to native semantics, and return focus to the invoking control. Never make a needed action hover-only. |
| Focus and announcements | Strong visible focus; logical reading/tab order; focus trap only in modal dialogs; live announcement for start/finish/failure of an async test; accessible text equivalents for health/status iconography and attempt timelines. |
| Tables/lists | Server-side filter/sort/page **or** virtualization for models, requests, keys, and providers. Keep stable row identity and selection under refresh. Announce filtered and total counts. URL-persist filters where shareability is useful. |
| Responsive layout | On narrow screens, preserve title, current status, and primary action; transform dense tables into stacked cards or a list + full-screen detail sheet. Do not make horizontal scrolling the sole way to identify a failed route. |
| States | Distinguish loading, no configured resources, no search matches, permission denied, failed load, partial result, stale data, disabled item, and pending test. Preserve unsaved input after recoverable errors. |
| Color/motion | Never use color alone for status; meet contrast requirements; respect reduced motion; do not convey a failed or active health check only through animation. |

## Explicit anti-patterns

1. **One generic “API key” abstraction.** Do not merge upstream provider secrets, runtime/client access keys, and control-plane authentication in language, storage, permissions, or UI.
2. **Single green health badge.** Do not let a reachable console, gateway liveness, valid credential, available model, passive recent traffic, and successful active request collapse into “Healthy.”
3. **Unbounded or automatic paid tests.** Do not run connection/model probes for every discovered model by default. Require scope and consent; disclose token/quota impact.
4. **Silent identity mutation.** Do not silently normalize model aliases, silently pick one provider for a collision, or rename provider references. Show canonical target, source, scope, and collision rule.
5. **Opaque resilience control.** Do not flatten priority, retry, credential rotation, cooldown/circuit behavior, fallback, and load balancing into one “reliable” setting or conceal failed attempts after final success.
6. **Universal-protocol assumption.** Do not treat all upstream APIs as compatible with one protocol, endpoint family, modality, parameter set, or streaming behavior. Validate the selected operation and expose capability gaps.
7. **Raw configuration as the primary UI.** Do not force operators to author YAML/JSON for common setup. Offer inspectable/exportable declarative forms after a guided, validated workflow.
8. **Invisible source-of-truth/precedence.** Do not silently merge UI, API, database, and declarative configuration or hide request/client/key/route override order. Show effective policy and drift.
9. **Secrets in operational surfaces.** Do not place secrets in rows, URLs, client/browser storage, exports, error messages, copied activity payloads, or diagnostic screenshots. Mask values and redact raw evidence.
10. **Prompt retention by default.** Do not capture request/response content merely because activity exists. Metadata-first, opt-in capture, retention, sampling, and per-destination privacy controls are required.
11. **Conflating state cases.** Do not render the same empty table for no resources, no matching filter, loading, permission denial, network failure, or partial provider failure.
12. **Connection equals tool execution.** Do not infer that an MCP/tool connection is authorized, safe, or configured for auto-execution. Make approval mode explicit.
13. **Keyboard/accessibility by assumption.** Do not claim the comparison products verify keyboard behavior, screen-reader support, responsive breakpoints, or list virtualization where public evidence did not. Test NexaRoute’s own contract.
14. **Copying comparators.** Do not replicate comparator branding, sponsor content, screenshots, visual layout, prose, proprietary code, exact schemas, CLI syntax, database paths, or script contracts.

## Open decisions and uncertainty register

| Topic | Current recommendation | Confidence / follow-up |
|---|---|---|
| Canonical route/model naming | Maintain immutable internal IDs plus user-facing public route names, explicit aliases, and provider-qualified canonical targets. | High as a cross-product principle; exact NexaRoute field names and collision policy need product/API design. |
| Default active-test budget | Use a selected-model, bounded request with preflight cost/quota notice and cancel option. | High direction; **uncertain** default token cap, test payload, and provider cost model—validate with supported providers and billing policy. |
| Passive health checks | Scope checks by connection/route, show schedule and cost, prefer recent real traffic where appropriate. | High direction; **uncertain** polling cadence, denominator, retryable classes, and whether health may invoke paid inference—define SRE policy. |
| Fallback eligibility | Distinguish transient service failures from invalid request, policy, auth, and non-idempotent operations. | High direction; **uncertain** exact per-protocol status/error taxonomy—define with API semantics and safety review. |
| Content logging default | Metadata-only with explicit opt-in content capture and retention. | High privacy principle; exact legal/compliance requirements and retention defaults need organizational policy. |
| Responsive/keyboard mechanics | Implement the contract above, including virtualization/pagination as needed. | **Uncertain as comparator evidence.** Validate through automated accessibility checks and manual keyboard/screen-reader testing in NexaRoute. |
| Helicone deployment interpretation | Treat hosted/self-hosted status as an external research inconsistency, not an architecture dependency. | **Uncertain:** supplied Helicone overview/quick-start material is not fully synchronized. No NexaRoute decision should rely on its availability claim. |

## Source index (supplied public sources)

### Claude Code Router (CCR)

- [CCR GitHub repository](https://github.com/musistudio/claude-code-router)
- [CCR npm package](https://www.npmjs.com/package/@musistudio/claude-code-router)
- [Provider guide](https://ccrdesk.top/en/guides/provider/)
- [Provider configuration](https://ccrdesk.top/en/configuration/providers/)
- [Provider registry repository](https://github.com/musistudio/claude-code-router-provider-registry)
- [Server configuration](https://ccrdesk.top/en/configuration/server/)
- [Routing configuration](https://ccrdesk.top/en/configuration/routing/)
- [Profiles configuration](https://ccrdesk.top/en/configuration/profiles/)
- [API key configuration](https://ccrdesk.top/en/configuration/api-keys/)
- [Observability configuration](https://ccrdesk.top/en/configuration/observability/)
- [Configuration overview](https://ccrdesk.top/en/configuration/overview/)
- [Troubleshooting](https://ccrdesk.top/en/troubleshooting/)
- [Claude Code LLM gateway documentation](https://code.claude.com/docs/en/llm-gateway)

### LiteLLM Proxy

- [Proxy UI](https://docs.litellm.ai/docs/proxy/ui)
- [Model management](https://docs.litellm.ai/docs/proxy/model_management)
- [Proxy configuration](https://docs.litellm.ai/docs/proxy/configs)
- [Pass-through endpoints](https://docs.litellm.ai/docs/proxy/pass_through)
- [OpenAI-compatible providers](https://docs.litellm.ai/docs/providers/openai_compatible)
- [Health](https://docs.litellm.ai/docs/proxy/health)
- [Load balancing](https://docs.litellm.ai/docs/proxy/load_balancing)
- [Model aliases](https://docs.litellm.ai/docs/completion/model_alias)
- [AI Hub](https://docs.litellm.ai/docs/proxy/ai_hub)
- [MCP](https://docs.litellm.ai/docs/mcp)
- [Virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys)
- [Endpoint activity](https://docs.litellm.ai/docs/proxy/endpoint_activity)
- [UI spend/log settings](https://docs.litellm.ai/docs/proxy/ui_spend_log_settings)

### Portkey Gateway

- [Feature overview](https://docs.portkey.ai/docs/introduction/feature-overview)
- [AI Gateway](https://docs.portkey.ai/docs/product/ai-gateway)
- [Universal API](https://docs.portkey.ai/docs/product/ai-gateway/universal-api)
- [Configs](https://docs.portkey.ai/docs/product/ai-gateway/configs)
- [Model Catalog](https://docs.portkey.ai/docs/product/model-catalog)
- [Model Catalog integrations](https://docs.portkey.ai/docs/product/model-catalog/integrations)
- [Virtual keys](https://docs.portkey.ai/docs/product/ai-gateway/virtual-keys)
- [Observability logs](https://docs.portkey.ai/docs/product/observability/logs)
- [Circuit breaker](https://docs.portkey.ai/docs/product/ai-gateway/circuit-breaker)
- [LangChain agents integration](https://docs.portkey.ai/docs/integrations/agents/langchain-agents)
- [Portkey gateway product page](https://portkey.ai/features/ai-gateway)
- [Portkey gateway repository](https://github.com/portkey-ai/gateway)

### OpenRouter

- [Provider guide](https://openrouter.ai/docs/guides/community/for-providers)
- [Presets](https://openrouter.ai/docs/guides/features/presets)
- [Authentication](https://openrouter.ai/docs/api_reference/authentication)
- [Management API keys](https://openrouter.ai/docs/guides/overview/auth/management-api-keys)
- [BYOK](https://openrouter.ai/docs/guides/overview/auth/byok)
- [Models overview](https://openrouter.ai/docs/guides/overview/models)
- [Models API reference](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties)
- [Provider selection](https://openrouter.ai/docs/guides/routing/provider-selection)
- [Model fallbacks](https://openrouter.ai/docs/guides/routing/model-fallbacks)
- [Logs](https://openrouter.ai/docs/guides/features/logs)
- [Activity](https://openrouter.ai/docs/guides/features/activity)
- [Activity export](https://openrouter.ai/docs/cookbook/administration/activity-export)
- [Input/output logging](https://openrouter.ai/docs/guides/features/input-output-logging)
- [Broadcast](https://openrouter.ai/docs/guides/features/broadcast)
- [Workspaces](https://openrouter.ai/docs/guides/features/workspaces)
- [Guardrails](https://openrouter.ai/docs/guides/features/guardrails)
- [Codex CLI integration](https://openrouter.ai/docs/cookbook/coding-agents/codex-cli)

### Helicone AI Gateway

- [Gateway overview](https://docs.helicone.ai/gateway/overview)
- [Quick start](https://docs.helicone.ai/getting-started/quick-start)
- [AI Gateway quickstart](https://docs.helicone.ai/ai-gateway/quickstart)
- [Provider integration reference](https://docs.helicone.ai/references/provider-integration)
- [Provider routing](https://docs.helicone.ai/gateway/provider-routing)
- [Gateway configuration](https://docs.helicone.ai/ai-gateway/config)
- [Embedded provider configuration](https://github.com/Helicone/ai-gateway/blob/main/ai-gateway/config/embedded/providers.yaml)
- [Helicone AI Gateway repository](https://github.com/Helicone/ai-gateway)
- [Error handling](https://docs.helicone.ai/gateway/concepts/error-handling)
- [Claude Agent SDK integration](https://docs.helicone.ai/gateway/integrations/claude-agent-sdk)
- [OpenAI Agents integration](https://docs.helicone.ai/gateway/integrations/openai-agents)
- [Observability](https://docs.helicone.ai/ai-gateway/observability)

### Bifrost

- [Bifrost overview](https://docs.getbifrost.ai/overview)
- [Bifrost repository](https://github.com/maximhq/bifrost)
- [Provider configuration](https://docs.getbifrost.ai/quickstart/gateway/provider-configuration)
- [Gateway setup](https://docs.getbifrost.ai/quickstart/gateway/setting-up)
- [Provider routing](https://docs.getbifrost.ai/providers/provider-routing)
- [Default observability](https://docs.getbifrost.ai/features/observability/default)
- [MCP overview](https://docs.getbifrost.ai/mcp/overview)
- [Config JSON / deployment](https://docs.getbifrost.ai/deployment-guides/config-json)
- [API reference](https://docs.getbifrost.ai/api-reference)
- [Governance routing](https://docs.getbifrost.ai/features/governance/routing)
- [Fallbacks](https://docs.getbifrost.ai/features/fallbacks)
- [Routing rules](https://docs.getbifrost.ai/providers/routing-rules)
- [Procuring API keys](https://docs.getbifrost.ai/api/procuring-api-keys)

## Implementation checklist for the rebuild

- [ ] Navigation/resource model distinguishes credential vault, connection, model/deployment, route, Connect, Activity, and Settings.
- [ ] Wizard supports preset and custom endpoint, explicit protocol override, discovery fallback, manual model entry, and selected bounded tests.
- [ ] All key classes have distinct UI language, scopes, permission checks, one-time secret handling, and lifecycle controls.
- [ ] Simple route builder and resolved execution preview ship before advanced rule/expression authoring.
- [ ] Health ladder and active/passive test evidence are separate; no generic all-green health summary.
- [ ] Activity trace preserves inbound request versus upstream attempts and makes route/credential resolution inspectable.
- [ ] Metadata-only observability is default; content capture, retention, sampling, and export are explicit and reversible.
- [ ] Source-of-truth/preference/effective-policy views exist before supporting more than one configuration source.
- [ ] Keyboard, focus, assistive-status, responsive detail, empty/error/partial/loading, and large-list requirements have automated plus manual acceptance tests.
- [ ] Advanced controls are deep-linkable and reversible; disabled policies remain auditable; destructive actions show impact and require confirmation.
