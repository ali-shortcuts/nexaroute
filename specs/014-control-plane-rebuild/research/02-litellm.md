# LiteLLM Proxy — control-plane research for NexaRoute

**Research scope:** exactly one product, LiteLLM Proxy. Findings below are based on current public LiteLLM documentation and the public repository links surfaced by those docs. This is a product-pattern study, not a code or branding reproduction.

## What the product is doing well

LiteLLM treats the proxy as an operational control plane, not only as a request forwarder. The strongest pattern is a deliberate split between **public model identity**, **provider/deployment details**, **reusable credentials**, **routing policy**, **health**, and **usage**. The UI makes those objects discoverable while the same operations remain available through APIs and configuration. [1] [2] [3]

### Provider onboarding and credentials

The model-management flow is organized around **Models + Endpoints** and an **LLM Credentials** area. A model row shows public model name, underlying provider/model, and mapped input/output pricing. Search and provider filters are explicitly provided for long lists. A model detail page has an Overview and Raw JSON view, plus Edit Settings, Test Connection, and Delete Model actions. [2]

Reusable named credentials are an important abstraction. An operator can add a credential once, choose a provider, name it, and reuse it from the Add Model form. Provider-specific fields adapt to the selected provider; the docs use Vertex as an example where project, location, and credentials replace a generic API-key-only form. Existing model credentials can also be promoted into a reusable named credential. [2]

The database-backed model mode is designed for day-two operations: adding, editing pricing, rotating provider keys, and retiring deployments without config edits or a proxy restart. Credentials for database models are encrypted at rest using `LITELLM_SALT_KEY` (falling back to the master key if unset). This is a concrete pattern for separating **secret lifecycle** from **model lifecycle**. [2]

LiteLLM also exposes a clear source-of-truth distinction. Config-file models are marked as config and cannot be edited or deleted in the UI; UI/API models are stored in the database and can be changed without restart. The docs warn that using both as active management systems creates two sources of truth, even though both can be served simultaneously. [2] [3]

### Presets and custom endpoints

The pass-through endpoint flow is a useful preset-like onboarding pattern for non-native or non-chat APIs. The UI path is Models + Endpoints → Pass Through Endpoints → Add Pass Through Endpoint. Required fields are **Path Prefix** and **Target URL**. Optional/advanced controls cover forwarded or custom headers, default query parameters, HTTP methods, exact-path versus subpath forwarding, timeout, and per-request pricing. The route is immediately testable with a generated curl example. [4]

This separates a simple first-run form from advanced protocol details. It also centralizes authentication, spend tracking, and budgets while keeping upstream provider keys away from developers. Query precedence is explicit: client parameters override target URL parameters, which override defaults. That is a good model for an advanced drawer with visible precedence rather than a flat, opaque form. [4]

LiteLLM’s OpenAI-compatible provider docs show another onboarding pattern: provider selection is not purely cosmetic. The `openai/` prefix tells the system which adapter/protocol to use, and the endpoint base is kept clean so the client adds the relevant route. The docs warn that the OpenAI client requires an API key even for some compatible endpoints, and recommend direct provider adapters where available. [5]

### Protocol/model detection and model tests

Protocol selection is encoded in explicit model metadata and prefixes rather than inferred only from display names. Examples include `openai/<model>` for OpenAI-compatible chat/embedding calls and `text-completion-openai/<model>` for completion calls. Health checks use `model_info.mode` to select the operation: chat, completion, embedding, image generation, transcription, speech, rerank, batch, realtime, OCR, video generation, or image edit. If mode is unset, LiteLLM auto-detects capabilities and falls back to chat completion. [5] [6]

The product has two distinct verification patterns:

1. **Test Connection** on a model detail page re-verifies the provider using stored credentials, useful after key rotation or an edit. [2]
2. **Health Status → Run All Checks** tests configured models and shows status, error details, last-check time, and last-success time. The API equivalent, `GET /health`, performs real test requests and therefore consumes tokens. [6]

The health docs make this cost explicit. They also support per-model timeout, max tokens, reasoning effort, test voice, concrete model for wildcard routes, and background checks. This gives NexaRoute a strong lesson: a “test” must show **what was tested, which protocol was used, when, with which endpoint, and whether it costs usage**.

### Simple routing, aliases, and fallback

LiteLLM starts with a simple mental model: multiple deployments can share one public `model_name`, and the router distributes requests among them. The default routing strategy is `simple-shuffle`; alternatives include least-busy, usage-based, latency-based, and cost-based routing. Deployment `order` adds deterministic priority. [7]

Fallback is layered rather than hidden. An order-1 deployment is tried before order-2; each order level gets retries before the next level; model-level fallbacks can then be used after all deployment orders fail. Rate-limit failures place a deployment on cooldown. This is a compact way to express “simple routing” while retaining an explainable reliability path. [7]

Aliases provide a stable public contract while backends change. A user-facing model name can map to a different LiteLLM model or provider deployment. Router aliases can be visible through model discovery endpoints or hidden when intended for typos, case normalization, or minor version compatibility. [7] [8]

### Agent and tool connection

LiteLLM extends the same control-plane pattern beyond LLM deployments. AI Hub lets admins publish selected models and agents for organizational discovery. Agents follow A2A, and MCP servers can also be exposed. The public hub separates “what is available” from the admin management surface. [9]

The MCP Gateway provides one fixed endpoint for tools while controlling access by key, team, or organization. It supports Streamable HTTP, SSE, and stdio. The gateway negotiates protocol version during initialization, namespaces tool names by server, and supports server-specific client headers such as `x-mcp-{server_alias}-{header_name}`. These details demonstrate a good agent-connection UX principle: make **transport, namespace, credential scope, and capability negotiation** explicit instead of treating every tool as an undifferentiated URL. [10]

### API keys, observability, and activity

Virtual keys are a first-class control-plane resource. They require a database and a master key for administration, then can be generated with allowed models and metadata. Keys can carry model access, management-route restrictions, budgets, rate limits, owners, teams, and aliases. Keys can be blocked/unblocked, and spend is tracked by key, user, and team. [11]

The docs are especially useful on inheritance: model access and MCP access are evaluated differently from management-route access, and owner/team constraints can be ceilings rather than grants. NexaRoute should present these policies in a human-readable effective-permissions view rather than forcing operators to mentally merge key, user, and team rows. [11]

Endpoint Activity automatically aggregates endpoint-level usage, tokens, spend, success/failure, timestamps, and trends. The UI includes an endpoint usage table, success-versus-failure visualization, and trend data. LiteLLM carefully distinguishes gateway request counts from spend-derived breakdowns: edge counts include rejected requests and count one inbound request once even if retries/fallbacks cause multiple upstream attempts; spend logs are attribution data and may record upstream attempts separately. [12]

Spend-log settings are runtime controls in the UI. Operators can toggle prompt/response storage and set retention without editing config or restarting. The docs state that the toggle affects new logs only, and that UI settings override config. This is an effective safety pattern for sensitive data: make content capture an explicit, reversible setting with clear historical scope. [13]

### Settings and advanced separation

The docs separate settings into `general_settings`, `router_settings`, `litellm_settings`, and `environment_variables`, while models remain a separate resource. UI-written settings are overlaid from the database and can override YAML on restart. The documentation repeatedly distinguishes a quick path from advanced configuration, and it gives both UI and API/config equivalents. [2] [3]

The product’s best advanced-separation pattern is progressive disclosure: start with provider, model, credential, base URL, and test; keep raw JSON, headers, query precedence, protocol mode, routing strategy, retries, timeouts, health tuning, and custom adapters behind detail/advanced surfaces. This is an inference from the documented object model and flow, not a claim about every current screen layout.

## What NexaRoute should learn

1. **Model identity must be stable and separate from deployment identity.** Let users choose a public model name while allowing provider, region, base URL, credentials, version, and deployment order to change underneath. [2] [3] [7]
2. **Make credentials reusable objects.** A named credential reduces repetition, supports rotation, and lets operators filter usage by credential. Provider-specific fields should adapt after provider selection. [2]
3. **Offer a safe “Add endpoint” wizard with a real test.** Require only the minimum viable fields, then reveal headers, query defaults, methods, path matching, timeouts, pricing, and protocol mode. Provide a test that reports exact failure stage and last success. [4] [6]
4. **Make source of truth visible.** Every row should say whether it is managed by config/GitOps, database/UI, or an external system. Warn before enabling two competing sources. [2] [3]
5. **Use explicit protocol metadata.** Provider prefixes, endpoint modes, and capability flags are more reliable than guessing from names. Surface the resolved protocol in the model detail and test result. [5] [6]
6. **Separate connection test from health monitoring.** A manual test is an operator action; background health is an ongoing signal. Show test cost and test scope, and let users disable or scope expensive checks. [6]
7. **Keep routing explainable.** Show deployment order, strategy, retry count, cooldown, and fallback chain as a readable route preview. Do not make “simple” routing mean “mysterious.” [7]
8. **Treat aliases as a compatibility contract.** Show public name, backend target, and whether the alias is discoverable. Support hidden aliases for migrations and typo/case compatibility. [7] [8]
9. **Provide effective-permission views for keys.** Operators need to understand the combined result of key, team, user, route, model, and MCP permissions. [11]
10. **Separate traffic volume from spend attribution.** Use distinct labels and definitions for gateway requests, spend logs, provider attempts, retries, and fallbacks. [12]
11. **Make sensitive observability controls reversible and scoped to new data.** Prompt storage and retention should have warnings, defaults, immediate effect, and clear historical behavior. [13]
12. **Treat agents/MCP as governed resources.** Expose transport, server alias, tool namespace, auth headers, protocol negotiation, and access scope instead of hiding them in a generic “agent connection” field. [9] [10]
13. **Design long lists around search and filters from the start.** LiteLLM explicitly calls out search and provider filters for the model list; NexaRoute should add provider, status, source, credential, protocol, region, and last-check filters. [2]
14. **Use resilient states.** Documented health and troubleshooting behavior suggests useful states: never tested, checking, healthy, degraded, unhealthy, disabled, blocked, stale, and configuration error. Each should include last-known timestamp and next action. [6] [10]

## What NexaRoute should not copy

- **Do not copy LiteLLM’s names, visual identity, wording, URL paths, or exact navigation labels.** Reuse the information architecture only after adapting it to NexaRoute’s own domain language.
- **Do not copy raw configuration as the primary UX.** Raw JSON/YAML is valuable for inspection and automation, but it is not a substitute for guided forms, validation, secret handling, and readable diffs.
- **Do not silently merge config and database state.** LiteLLM’s own docs warn about two sources of truth. NexaRoute should show precedence, ownership, and drift directly in the UI. [2] [3]
- **Do not imply that a successful liveness probe means an LLM is usable.** Process health, dependency readiness, model health, and request-level success are separate signals. [6]
- **Do not run paid model health checks without clear consent and cost context.** A health check that makes real calls needs a visible scope, budget warning, and opt-out/background policy. [6]
- **Do not flatten routing and fallback into one opaque “reliability” toggle.** Operators need to see priority, retry, cooldown, and fallback behavior. [7]
- **Do not expose secrets in list rows, raw exports, URLs, browser storage, or error messages.** LiteLLM documents masked API keys and encrypted-at-rest credentials; NexaRoute should follow the principle without copying implementation details. [2]
- **Do not store prompts by default merely because observability exists.** Content capture should be off or explicitly consented, with retention and compliance controls. [13]
- **Do not treat every upstream API as OpenAI-compatible.** Protocol and capability mismatches are a major source of failed tests; make compatibility explicit and validate the selected operation. [5] [6]
- **Do not infer that a short result list means there are no resources.** Large-list and empty states need distinct “no resources,” “no matches,” “loading,” “permission denied,” and “failed to load” states.
- **Do not assume desktop-only interaction.** The cited docs describe functions, not complete responsive or keyboard behavior. NexaRoute should independently require keyboard-visible focus, logical tab order, dialog escape behavior, table navigation, responsive detail panes, and mobile-safe secret handling rather than treating LiteLLM documentation as evidence that these are solved.

## UX implications for responsive, keyboard, empty/error, and large-list behavior

The official documentation verifies the object and workflow model more strongly than pixel-level interaction details. There is explicit evidence for search/filtering, tabs, detail pages, modal creation, health-status badges, warnings, and error details. [1] [2] [4] [6] [13]

For NexaRoute, preserve those semantics while adding a deliberate interaction contract: every modal and drawer should be keyboard operable; focus should return to the invoking control; destructive actions need confirmation; tables should expose row actions without hover-only affordances; filters should be URL/shareable where appropriate; and responsive layouts should switch from wide tables to stacked resource cards with a persistent status and primary action. Large lists should use server-side pagination or virtualization, debounced search, stable sorting, and a count of filtered versus total items.

Recommended empty/error taxonomy:

- **No resources yet:** explain the first setup action and provide a primary CTA.
- **No matches:** preserve the query and offer clear-filter.
- **Loading:** show table skeletons that preserve column shape.
- **Permission denied:** explain which role or scope is missing without revealing secrets.
- **Connection/test failure:** show provider, operation, endpoint, timestamp, sanitized error class, and remediation.
- **Stale health:** show the last successful check and a “Run check” action.
- **Partially configured:** identify missing credential, base URL, protocol mode, or model capability.

These are recommendations for NexaRoute, not claims that the current LiteLLM UI implements every one.

## References

[1]: https://docs.litellm.ai/docs/proxy/ui "LiteLLM Admin UI Quick Start"
[2]: https://docs.litellm.ai/docs/proxy/model_management "LiteLLM Model Management"
[3]: https://docs.litellm.ai/docs/proxy/configs "LiteLLM Config Overview"
[4]: https://docs.litellm.ai/docs/proxy/pass_through "LiteLLM Pass-through Endpoints"
[5]: https://docs.litellm.ai/docs/providers/openai_compatible "LiteLLM OpenAI-Compatible Endpoints"
[6]: https://docs.litellm.ai/docs/proxy/health "LiteLLM Health Checks"
[7]: https://docs.litellm.ai/docs/proxy/load_balancing "LiteLLM Proxy Load Balancing"
[8]: https://docs.litellm.ai/docs/completion/model_alias "LiteLLM Model Alias"
[9]: https://docs.litellm.ai/docs/proxy/ai_hub "LiteLLM AI Hub"
[10]: https://docs.litellm.ai/docs/mcp "LiteLLM MCP Overview"
[11]: https://docs.litellm.ai/docs/proxy/virtual_keys "LiteLLM Virtual Keys"
[12]: https://docs.litellm.ai/docs/proxy/endpoint_activity "LiteLLM Endpoint Activity"
[13]: https://docs.litellm.ai/docs/proxy/ui_spend_log_settings "LiteLLM UI Spend Log Settings"
