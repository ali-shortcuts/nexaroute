# Helicone AI Gateway: control-plane and gateway patterns

Helicone’s current public material describes two related surfaces: a hosted/credits gateway at `ai-gateway.helicone.ai`, and a self-hosted Rust gateway configured through YAML. The documentation is not fully synchronized: the general overview and quick-start describe hosted credits and a cloud endpoint, while the newer AI Gateway quickstart says the current product is self-hosted and labels cloud as “coming soon.” Treat deployment mode as an explicit product state in NexaRoute rather than assuming one universal onboarding flow. [1] [2] [3]

## Provider onboarding and credentials

Helicone models the domain as **authors → models → providers → endpoints**. An endpoint is a model/provider combination with deployment configuration. Provider integration requires a base URL, authentication mode, model list, pricing/model metadata, and provider-specific request/response transforms when the upstream is not OpenAI-compatible. The repository separates provider definitions, model metadata, endpoint combinations, helper mappings, usage processors, priorities, and test fixtures. This is a strong control-plane decomposition: a credential is not the same thing as a provider, and a provider is not the same thing as a model deployment. [4]

The hosted path offers two credential modes. Pass-through billing uses a Helicone key and Helicone-managed provider keys, while BYOK stores the customer’s provider keys and tries those first. The docs explicitly position credits as the low-friction path and BYOK as the path for provider credits, compliance, or direct relationships. [1] [5] The self-hosted path uses provider-specific environment variables such as `OPENAI_API_KEY` and `ANTHROPIC_API_KEY`; gateway authentication is separately controlled with `HELICONE_CONTROL_PLANE_API_KEY`. [3] [6]

**What NexaRoute should learn:** make onboarding progressively disclose three separate decisions: (1) provider identity and protocol, (2) one or more endpoint/model deployments, and (3) credentials and credential scope. Offer a managed-credentials path and a BYOK path without conflating them. Show exactly which requests can use each key, whether a key is primary or fallback, and whether it is stored, referenced, or injected at runtime. Support a provider preset that pre-fills protocol, base URL, common headers, and model discovery, followed by a custom endpoint escape hatch.

**What not to copy:** do not copy Helicone’s exact provider names, URL structure, logo/icon treatment, or its hosted-versus-self-hosted wording. Also avoid exposing raw provider configuration as the only onboarding UX; YAML/environment-variable configuration is useful for operators but is not a substitute for a guided control plane.

## Presets, custom endpoints, protocol and model detection

The embedded provider registry is intentionally declarative. Each provider has a `models` array and `base-url`; non-default protocol details such as Anthropic’s version header are declared alongside it. The examples include OpenAI-compatible providers whose base URL may already include an `/openai/` path, and providers with namespaced model IDs such as Bedrock or Hyperbolic. This demonstrates why NexaRoute should treat protocol, base URL, path prefix, authentication, and model identifier as independent fields rather than infer everything from a provider name. [7]

The provider-integration guide recommends OpenAI compatibility as the simplest case and custom `buildBody`/`buildHeaders` transforms for non-compatible providers. It also requires supported-parameter metadata, context limits, pricing thresholds, regions/deployments, and usage processing. [4] A good NexaRoute preset should therefore detect protocol from a short connection test, but let the user override the result and preserve the raw diagnostic (status, response shape, and detected capabilities).

The public materials do not document a customer-facing “model test” screen or a detailed protocol-detection UI. What is verified is the underlying pattern: a known model registry plus explicit provider/model endpoint mappings, and a request path that can target a provider, a deployment, or a manual fallback chain. [4] [5] NexaRoute should implement testing as a first-class action—“test credentials,” “test endpoint,” and “test model”—with separate results, timing, and actionable remediation instead of one ambiguous green check.

## Aliases, simple routing, and advanced separation

The hosted gateway intentionally makes the simple case minimal: request a model name and the gateway discovers providers, chooses a route, and fails over. Advanced model syntax adds progressively more control: `model/provider` locks a provider; `model/provider/deployment-id` targets a configured deployment; comma-separated values define an ordered fallback chain; and `!provider,model` excludes providers. Unknown models are allowed through BYOK deployments even when they are absent from the registry. [5]

The self-hosted gateway separates independently named routers. A router has its own URL path, provider set, and load-balancing policy. The configuration reference documents latency routing and weighted routing, with weights required to sum to exactly `1.0`; the repository also advertises model-latency, provider-latency, weighted, and cost-oriented strategies. [6] [8]

This is a useful **simple/advanced separation**: default routing is zero-configuration, while router-level policies are explicit and isolated. NexaRoute should use a two-tier UI: a simple route builder for “one model, optional fallback,” and an advanced editor for provider sets, weights, latency/cost policy, exclusions, regions, rate limits, caching, and deployment IDs. Keep the generated route preview visible in both tiers so users can understand what their application will call.

A caution: aliases are only partially visible in the public evidence. The provider registry comments mention upstream aliases that require an explicit `-latest` suffix, while the routing docs describe friendly model names and provider/deployment syntax. [7] [5] NexaRoute should maintain a durable internal alias object with display label, canonical model ID, provider model ID, version/deprecation state, and compatibility notes. Do not silently rewrite a customer’s model string without showing the resolved target and a migration warning.

## Health, failover, and errors

Hosted routing is documented as selecting the cheapest available provider, load balancing equal-cost options, and trying the next provider on rate limits, authentication errors, context errors, timeouts, and server errors. The failure table includes 429, 401, 400, 408, and 500+. [5] The self-hosted configuration adds rolling health monitoring: a configurable error ratio over a time window, with a minimum-request grace period before a provider is marked unhealthy. [6]

Helicone’s error-handling docs distinguish PTB and BYOK failures and return the most actionable error when all attempts fail. The stated priority is 403, 401, 400, 500, then 429; examples explain why an invalid BYOK key should be shown instead of hiding the problem behind an insufficient-credit error. [9]

**NexaRoute pattern:** expose health as evidence, not a decorative status dot. For every provider/deployment show last test, rolling success rate, latency percentile, last failure code, cooldown/unhealthy state, and whether the route currently excludes it. In a fallback test, show an ordered attempt timeline and the final surfaced error. Separate “credential invalid,” “model unsupported,” “upstream unavailable,” “policy excluded,” and “no credits” states. Preserve raw upstream diagnostics behind a disclosure control.

**Do not copy:** do not reproduce Helicone’s exact error-priority order as a universal rule. NexaRoute should make the precedence configurable by workspace policy and distinguish a customer-actionable error from an internal retry event. Also avoid implying that any 401 or 400 is safe to fail over; some errors are request-specific and retrying another provider can waste money or obscure the real problem.

## Agent connection and observability/activity

Helicone treats agent connection as an integration layer rather than a separate gateway protocol. The Claude Agent SDK guide uses an MCP server with an API key, an explicit allowlist of tools, and tools for gateway calls plus historical request/session queries. The OpenAI Agents guide changes the global OpenAI client base URL and API key, after which existing agent code continues to work. [10] [11]

The agent-facing observability model is concrete: session ID/name, user ID, custom properties, request/response bodies, latency, tokens, costs, model analytics, tool usage, reasoning steps, error tracking, and session tracking. The MCP tools expose filtered, paginated, sorted request queries and session queries. [10] This is a strong pattern for NexaRoute: make an “agent connection” card generate the correct SDK/MCP snippet, list required permissions, and provide a copyable verification command. Give every connection a test result and a link from the connection to its activity.

Observability is enabled by a control-plane API key and a feature setting. It logs gateway traffic to the dashboard and supports request/response omission headers for sensitive data. [3] [12] The quick-start promises that a request appears in the Requests tab within seconds. [2] NexaRoute should make the activity surface useful before volume exists: an empty state should explain how to send a test request, which environment is selected, and what filters will become available. For non-empty activity, use server-side filtering, pagination, sorting, and URL-persisted filters; these are especially important because Helicone’s documented agent query tools explicitly require filtering and pagination.

## Settings, advanced separation, and operational UX

Verified settings concepts include API key generation, provider settings, credits, feature toggles, control-plane authentication, provider base URLs/models, router policies, caching, rate limits, and privacy controls. [2] [5] [6] [12] The docs place provider keys at a dedicated provider-settings route and Helicone keys at a dedicated API-keys route. That separation is worth preserving: gateway access keys, upstream provider credentials, and observability/control-plane credentials have different blast radii.

NexaRoute should use a left-to-right information architecture such as **Connections → Providers → Models/aliases → Routes → Activity → Settings**, with advanced controls inside the relevant object rather than a single dense settings page. Advanced sections should be collapsed by default but deep-linkable. Every destructive or high-impact action—deleting a credential, changing fallback order, disabling a provider, changing a region—should show affected routes and require a deliberate confirmation.

The public documentation does not verify Helicone’s responsive breakpoints, keyboard shortcuts, focus treatment, or exact empty/error-state visuals. Do not claim those as observed. For NexaRoute, treat them as acceptance criteria: keyboard-reachable tabs and dialogs, visible focus, escape-to-close, no hover-only actions, responsive route editing that becomes stacked on narrow screens, and list virtualization/server pagination for hundreds of providers/models. Provide stable loading, empty, partial-error, and permission-denied states; never render a blank table while a request is still pending.

## Concrete patterns NexaRoute can reuse

1. **One familiar protocol, explicit escape hatches.** Start with an OpenAI-compatible contract, but expose provider-specific transforms, headers, versions, and deployment IDs when needed. [1] [4]
2. **Credential precedence is visible.** Show BYOK-first versus managed fallback as an ordered policy, not an undocumented implementation detail. [5]
3. **Route syntax maps to user intent.** Friendly model-only routing, provider lock, deployment targeting, ordered fallback, and provider exclusion cover common needs without requiring a policy editor. [5]
4. **Router isolation.** Separate production, development, and experiment policies by named routers and URLs. [6]
5. **Health explains decisions.** Use rolling error ratios, windows, grace periods, and attempt histories to explain why a provider is or is not receiving traffic. [6]
6. **Actionable final errors.** Surface the error that helps the operator fix the configuration, while retaining the full attempt chain for diagnosis. [9]
7. **Agents are first-class clients.** Generate SDK/MCP connection snippets and carry session/user/custom metadata into activity. [10] [11]
8. **Observability has privacy controls.** Make request/response logging opt-in by feature mode and provide per-request omission controls. [12]
9. **Large-list readiness.** Use searchable model/provider registries, server-side pagination, sorting, and filters; these are consistent with the documented request/session query model. [10]

## Patterns NexaRoute should not copy

- Do not copy Helicone branding, product names, exact copy, icons, dashboard layout, or code. The recommendations above are behavioral patterns derived from public documentation and repository configuration.
- Do not make cloud credits and self-hosted provider secrets look like one credential type. Helicone’s own docs expose a deployment-mode inconsistency; NexaRoute should make the mode and source of truth explicit. [1] [3]
- Do not force users to learn provider-specific model IDs before they can make a first request. Keep friendly aliases, but show the canonical resolution and provider deployment behind them.
- Do not hide fallback attempts, provider health decisions, or the final error. A green response after several retries should remain inspectable in activity.
- Do not put provider secrets, gateway API keys, and control-plane keys in one undifferentiated settings list.
- Do not rely on YAML/environment variables as the only UX for presets, custom endpoints, tests, or routing. Keep declarative export/import for operators, but pair it with a safe UI.
- Do not claim responsive or keyboard behavior based on the public sources; those details were not verifiable from the accessible documentation.

## References

[1]: https://docs.helicone.ai/gateway/overview "AI Gateway Overview"
[2]: https://docs.helicone.ai/getting-started/quick-start "Quickstart"
[3]: https://docs.helicone.ai/ai-gateway/quickstart "AI Gateway Quickstart"
[4]: https://docs.helicone.ai/references/provider-integration "How to Integrate a Model Provider to the AI Gateway"
[5]: https://docs.helicone.ai/gateway/provider-routing "Provider Routing"
[6]: https://docs.helicone.ai/ai-gateway/config "Configuration Reference"
[7]: https://github.com/Helicone/ai-gateway/blob/main/ai-gateway/config/embedded/providers.yaml "Embedded Provider Configuration"
[8]: https://github.com/Helicone/ai-gateway "Helicone AI Gateway repository"
[9]: https://docs.helicone.ai/gateway/concepts/error-handling "Error Handling & Fallback"
[10]: https://docs.helicone.ai/gateway/integrations/claude-agent-sdk "Claude Agent SDK Integration"
[11]: https://docs.helicone.ai/gateway/integrations/openai-agents "OpenAI Agents Integration"
[12]: https://docs.helicone.ai/ai-gateway/observability "Observability with Helicone"
