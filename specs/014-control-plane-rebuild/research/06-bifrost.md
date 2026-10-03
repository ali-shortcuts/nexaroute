# Bifrost AI Gateway — control-plane research for NexaRoute

## Product pattern

Bifrost is an open-source, self-hostable AI gateway and control plane. Its official docs describe a unified OpenAI-compatible API over 20+ providers, with a built-in web UI for provider configuration, real-time monitoring, governance, routing, fallbacks, and model discovery. The public repository is Apache-2.0 licensed. [1] [2]

The strongest product pattern is **one operational surface over two distinct planes**:

- **Inference plane:** unified and provider-compatible endpoints, provider/model resolution, retries, key rotation, and fallback execution.
- **Control plane:** providers and credentials, model catalog, virtual keys, routing rules, MCP clients, logs, plugins, configuration stores, and settings.

NexaRoute should keep those concepts separate in navigation and permissions even though users experience them in one console.

## Provider onboarding, presets, and custom endpoints

Bifrost exposes provider setup in a sidebar area named **Model Providers**, where the user selects a provider and configures keys. The documented setup supports multiple keys per provider, key-level model assignments, weights, network configuration, timeouts, retries, and provider-specific options. The UI is intended to apply changes immediately through a database-backed configuration store. [3] [4]

A useful onboarding shape is:

1. Select a known provider preset.
2. Add one or more named credentials.
3. Choose models (`*` or an explicit list).
4. Optionally set weight, endpoint/network settings, and request capabilities.
5. Save and immediately use the model or inspect it in the catalog.

Bifrost also supports custom/self-hosted OpenAI-compatible endpoints. A provider can be given a custom name such as `vllm-local`, a dummy credential when the upstream does not require one, a `base_url`, a timeout, and a `base_provider_type` of `openai`. This is a good example of **preset + escape hatch** rather than forcing every endpoint into a bespoke integration. [3]

The docs distinguish provider-level network configuration from key-level configuration. Base URLs can override the standard provider endpoint, and private-network access is explicit (`allow_private_network: true`), with link-local addresses still blocked. NexaRoute should expose these as an advanced network section with clear security warnings, not as a hidden text field. [3]

### What NexaRoute should learn

- Make the first-run path short, visual, and reversible: provider preset → credential → models → test → enable.
- Offer a clearly labeled **Custom OpenAI-compatible endpoint** path with protocol/base-provider selection rather than pretending all custom endpoints are the same.
- Keep endpoint, timeout, proxy, private-network, and retry controls grouped under **Advanced network settings**.
- Give every credential a human-readable name. Bifrost requires unique key names and uses them in rotation and configuration examples. [3]
- Allow model scoping per credential. This supports separate billing, quotas, and access policies for premium or specialized models. [3]

### What not to copy

Do not copy Bifrost’s provider names, logo, exact labels, screenshots, wording, or visual styling. Reuse the interaction logic only. Do not expose a raw configuration blob as the primary onboarding experience; Bifrost’s `config.json` is valuable for GitOps, but its own docs position the Web UI/database as the normal interactive path. [4] [8]

## Credentials, API keys, and separation of concerns

Bifrost has two different credential concepts that NexaRoute should not conflate:

- **Upstream provider credentials:** keys used to call OpenAI, Anthropic, vLLM, and other providers. The docs show environment-variable references such as `env.OPENAI_API_KEY` rather than putting secrets directly in `config.json`. [3] [8]
- **Bifrost management API keys / virtual keys:** credentials used by automation or inference consumers. Management API keys are created under **Settings → API Keys**, named, scoped, shown once, and stored in a secret manager. A key’s scopes—not the creator’s role—determine access, and scope grants cannot exceed the creator’s own permissions. [6]

The management API documentation explicitly lists required permissions and returns `403 Forbidden` for missing permissions. It also documents public health/version/session endpoints and separate management, inference, and virtual-key authentication paths. [6] [9]

NexaRoute should use a credential lifecycle with: masked values, one-time reveal, rotation/revoke, last-used timestamp, validation state, and a clear distinction between **provider secret**, **control-plane API key**, and **runtime consumer key**. Do not imply that a consumer key can be used to administer the control plane.

Bifrost also demonstrates valuable **advanced separation** in storage modes. Its config store holds runtime configuration; its logs store holds request traces; its vector store is optional for semantic caching. UI/API edits remain available in DB-backed mode, while file-only mode is intentionally read-only and restart-based. [4] [8] NexaRoute should preserve this distinction in Settings and explain the consequences before enabling GitOps/read-only mode.

## Protocol and model detection

Bifrost uses a central Model Catalog. It combines pricing/provider mappings with each provider’s `/v1/models` response, refreshes provider model lists when providers are added or updated, and enriches the catalog with provider-specific models and aliases. [5]

Model requests can be explicit (`provider/model`) or bare (`model`). Bare names are resolved through the catalog; explicit prefixes are deterministic. The docs warn that a bare name errors when the catalog cannot resolve it, so the UI should show the provider-resolution status and give users a deterministic “pin provider” action. [4] [5]

The supported-provider matrix is unusually useful as a product pattern. It makes operation capabilities visible across models/providers: chat, streaming, responses, images, embeddings, audio, files, batch, rerank, and more. It also distinguishes native support from Bifrost-emulated compatibility. [2]

The API model schema exposes `supported_methods`, reasoning metadata, provider, requested/deployed model, latency, and provider-specific extra fields. This is a strong basis for a model detail panel rather than a flat model name list. [9]

### NexaRoute model UX

- Show **protocol detected**, **capabilities**, **provider**, **deployment/model ID**, and **last catalog refresh**.
- Make aliases explicit: display canonical ID, provider-qualified ID, and user-facing alias as separate fields.
- Validate a model against the selected provider before enabling Save.
- Show “catalog unavailable,” “provider returned no models,” and “model exists in pricing data but not upstream discovery” as different states.
- Allow a user to pin `provider/model` when automatic detection is ambiguous.

Do not copy Bifrost’s exact catalog terminology, URL structure, or response schema. The lesson is the separation of canonical identity, provider identity, capabilities, and runtime deployment—not the field names.

## Model tests, health, and error states

The official material verifies health behavior strongly for MCP clients: Bifrost pings each connected client on a configurable interval, applies a timeout and consecutive-failure threshold, tracks connected/disconnected/unstable state, and provides manual reconnect. OAuth clients can become a sticky `needs_reauth` state. [7]

For provider inference, the docs verify health/version endpoints and robust request error semantics, but they do not document a single universal “Test provider” button or a complete provider-health UI contract. NexaRoute should therefore treat a model test as an explicit product decision, not claim it is copied from Bifrost.

A good test action should:

- use a bounded, low-cost request or a provider list-model call;
- report credential, network, protocol, model, and permission failures separately;
- never silently enable a provider after a successful test;
- retain the last test timestamp, latency, response class, and tested model;
- offer retry and copyable diagnostic details without exposing secrets.

Health should be multi-dimensional: configuration validity, credential validity, endpoint reachability, model discovery freshness, recent request success, and policy availability. Avoid a single green dot that hides partial failure.

## Simple routing and fallback routing

Bifrost has two routing methods. Governance-based routing is explicit and user-defined through Virtual Keys. Adaptive load balancing is performance-based and enterprise-only. Governance takes precedence when both apply. [5]

For a simple route, a Virtual Key can allow providers/models, assign numeric weights, and optionally pin specific provider keys. Weights are normalized for the requested model. Providers with no weight can remain available for direct requests or fallbacks without participating in weighted selection. [10]

Fallback behavior is deliberately layered:

- **Retry:** stay with the provider; transient server failures reuse the same key with exponential backoff and jitter.
- **Key rotation:** credential-bound 401/402/403/429 errors rotate across a key pool.
- **Fallback:** after the primary provider’s retry budget is exhausted, move to the next provider; each fallback receives its own retry budget.
- **First success wins:** the response includes the actual provider used. [11]

This is a good NexaRoute UI model: a route builder should show **primary**, **same-provider retry policy**, **credential pool**, and **fallback chain** as distinct stages. Provide a compact “simple route” mode for weighted providers and an “advanced rules” mode for conditions, scopes, and chaining.

Bifrost’s dynamic routing rules use CEL, scope hierarchy, first-match-wins evaluation, numeric priority, optional chain rules, weighted targets, and optional fallbacks. The dashboard supports list/filter/create/edit/delete, enable/disable without deletion, drag-to-reorder priority, and a visual condition builder with manual expression mode and real-time validation. [12]

NexaRoute should learn from the progressive disclosure: visual builder for common cases, expert expression editor for advanced cases, and a route preview explaining which rule wins. It should not copy CEL syntax, the routing-tree imagery, or Bifrost’s exact priority/scope names unless those are independently chosen for NexaRoute.

## Agent connection and MCP

Bifrost’s MCP design covers client connections to external servers over STDIO, HTTP, or SSE; multiple authentication modes; per-user sessions; tool filtering; explicit approval; Agent Mode; and Code Mode. [7]

The critical safety pattern is **suggestion versus execution**. In gateway mode, Bifrost exposes tools and the host application controls approval. `tools_to_auto_execute` is meaningful only when Bifrost itself runs the Agent Mode loop; it is ignored in pure gateway mode. [7]

For NexaRoute, an agent connection should show protocol, URL/command, authentication type, discovered tools, last sync, connection state, health-check policy, and approval policy. Make “manual approval,” “allowlisted auto-execution,” and “host-controlled approval” explicit. Never infer that a connected tool is safe to run.

## Observability and activity

Bifrost’s built-in observability captures request/response content when enabled, model parameters, provider/model context, output/tool calls, latency, tokens, cost, and status. Logging is asynchronous and documented as having negligible request overhead. The UI provides live streaming, advanced filters, request/response inspection, and analytics. [1] [6]

The log API exposes filters for providers, models, status, object/request type, time range, latency, tokens, cost, content search, tool-call names, request ID, pagination, and aggregate stats. WebSocket updates support live activity. Request IDs have a special exact lookup behavior, which is useful for an operations console. [6]

NexaRoute should make the activity page useful at both scales:

- top-level counters for volume, success rate, p95 latency, tokens, and cost;
- a virtualized/paginated event list with filters in a persistent toolbar;
- a detail drawer with request ID, route decisions, retries, fallback children, provider/model actually used, and redaction state;
- real-time pause/resume and reconnect status;
- content logging and redaction controls that are visible, not buried.

Do not copy Bifrost’s live-log animation, screenshot layout, or exact filter names. Copy the operational principle: every routing decision and fallback attempt should be explainable from an event trace.

## Settings and advanced separation

Bifrost’s documented settings surface spans client behavior, authentication, configuration store, logs store, vector store, providers, plugins, governance, MCP, and deployment. The config docs support schema validation, environment references, DB-backed reconciliation, and file-only GitOps mode. [4] [8]

NexaRoute should group settings by operator intent rather than by internal package names:

- **Workspace and access:** users, roles, API keys, sessions.
- **Providers and models:** presets, credentials, endpoints, catalog refresh.
- **Routing and reliability:** routes, aliases, retries, fallbacks, budgets.
- **Activity and data:** logging, redaction, retention, exports.
- **Agents and tools:** MCP connections, approvals, tool sync.
- **Deployment:** storage, environment references, network restrictions, advanced flags.

Put dangerous or rarely changed controls behind an Advanced section with an explanation of operational impact. Preserve a read-only/declarative mode, but make its edit lock and restart requirement unmissable.

## Large lists, empty/error states, responsive and keyboard UX

The repository UI README verifies React/Vite, TanStack Router, Tailwind, Radix UI components, Redux Toolkit/RTK Query, WebSockets, dark/light mode, responsive design, accessible components, typed service layers, and consistent error handling. It also describes a page/component split for logs, configuration, and reusable UI. [2]

The public docs do not provide enough evidence to claim exact Bifrost behavior for every empty state, loading skeleton, keyboard shortcut, or breakpoint. Treat those as **implementation requirements for NexaRoute**, informed by the stated accessibility/responsive goals rather than copied Bifrost behavior.

Concrete requirements for NexaRoute:

- Large provider/model/key/activity lists need server-side filtering, pagination or virtualization, stable sorting, and URL-persisted filters.
- Empty states must distinguish “nothing configured,” “filter returned no results,” and “data unavailable.” Each needs a next action.
- Error states must preserve user input, identify whether failure is validation/network/auth/upstream, and provide retry without a full-page reset.
- Loading states should preserve layout and show which scope is loading; avoid a global spinner for independent panels.
- Responsive layouts should collapse navigation and move detail panels to full-screen sheets without losing context.
- Keyboard navigation must cover sidebar, tabs, route builders, table rows, drawers, comboboxes, dialogs, and reorder controls. Focus should return to the launching control after a dialog closes.
- Every destructive action needs a confirm step, an undo where safe, and a visible success/error announcement.
- Do not rely on color alone for health/capability/error status.

## Verified patterns to adopt

1. **Unified API plus native protocol adapters:** one gateway can preserve existing SDK contracts while providing common routing and governance. [2] [9]
2. **Preset plus custom endpoint:** common providers are fast to configure; self-hosted OpenAI-compatible services remain possible. [3]
3. **Named, model-scoped credential pools:** credentials can be rotated and weighted without forcing a provider-wide key. [3] [11]
4. **Catalog-assisted model resolution:** model discovery, pricing, aliases, and provider support are first-class data, not free text. [5]
5. **Separated retries, key rotation, and fallbacks:** each failure class has an explainable policy and trace. [11]
6. **Scoped weighted routing with advanced rules:** common routes stay simple; CEL-like rules handle runtime conditions. [10] [12]
7. **Live activity with deep filters and request IDs:** operators can move from aggregate health to one failed request. [6]
8. **Agent safety through explicit approval and health state:** connection does not equal execution permission. [7]
9. **DB-backed interactive config plus declarative/file-only mode:** different deployment workflows can coexist, with clear tradeoffs. [4] [8]
10. **Accessible, responsive component foundation:** Radix primitives, typed API state, WebSockets, and consistent error handling are sensible foundations. [2]

## Patterns to avoid copying

- Bifrost/Maxim branding, naming, screenshots, iconography, copy, and exact information architecture.
- A single undifferentiated “health” status for credentials, endpoint, discovery, and traffic.
- Making raw config files the default for dashboard users.
- Hiding model/provider ambiguity behind automatic resolution with no pinning or explanation.
- Treating retries, key rotation, and fallbacks as one generic “resilience” toggle.
- Assuming MCP auto-execution is safe or host-controlled in every mode.
- Claiming exact keyboard, empty-state, or breakpoint behavior that the public sources do not verify.
- Copying implementation code, schemas, or proprietary assets. This report uses only public, high-level interaction and architecture patterns.

## References

[1]: https://docs.getbifrost.ai/overview "Bifrost AI Gateway overview"
[2]: https://github.com/maximhq/bifrost "Bifrost official GitHub repository and README"
[3]: https://docs.getbifrost.ai/quickstart/gateway/provider-configuration "Bifrost provider configuration"
[4]: https://docs.getbifrost.ai/quickstart/gateway/setting-up "Bifrost gateway setup and configuration modes"
[5]: https://docs.getbifrost.ai/providers/provider-routing "Bifrost provider routing and Model Catalog"
[6]: https://docs.getbifrost.ai/features/observability/default "Bifrost built-in observability"
[7]: https://docs.getbifrost.ai/mcp/overview "Bifrost MCP overview, agent mode, and health monitoring"
[8]: https://docs.getbifrost.ai/deployment-guides/config-json "Bifrost declarative configuration and storage separation"
[9]: https://docs.getbifrost.ai/api-reference "Bifrost API reference and authentication/model schemas"
[10]: https://docs.getbifrost.ai/features/governance/routing "Bifrost governance routing through Virtual Keys"
[11]: https://docs.getbifrost.ai/features/fallbacks "Bifrost retries, key rotation, and fallbacks"
[12]: https://docs.getbifrost.ai/providers/routing-rules "Bifrost dynamic routing rules and visual builder"
[13]: https://docs.getbifrost.ai/api/procuring-api-keys "Bifrost management API keys and scopes"
