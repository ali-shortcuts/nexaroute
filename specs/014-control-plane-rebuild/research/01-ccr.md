# Claude Code Router (CCR): control-plane research

## Product pattern

CCR is a **local model gateway plus browser/desktop control plane**. It presents one stable local endpoint to several coding agents while keeping provider, model, credential, routing, tool, profile, and request-log configuration behind the control plane. The official repository describes the product as a place to “manage every agent and provider from one place,” and the current README lists Claude Code, Claude Design, Codex, Grok CLI, Kimi CLI, Kilo Code, OpenCode, Pi, ZCode, WorkBuddy, and compatible API clients. [1]

The strongest product pattern is the separation between **management plane** and **model gateway**. In the CLI distribution, the management UI defaults to `127.0.0.1:3458`, while the model gateway defaults to `127.0.0.1:3456`; the Server page makes this distinction explicit. A reachable UI therefore does not imply a usable gateway. [2] [3]

## Provider onboarding

CCR uses a progressively disclosed provider wizard:

1. Go to **Providers → Add Provider**.
2. Select a built-in preset, or choose **Other / custom API endpoint**.
3. For presets, common endpoint, protocol, model defaults, icon, website, and sometimes usage settings are filled in. The endpoint is hidden by default but can be overridden in **Advanced settings**.
4. For a custom endpoint, provide a display name and API base URL.
5. Add a provider API key, then let CCR detect supported protocols and models.
6. Select models and run **Check Connection** before saving. [4] [5]

This is a useful onboarding pattern for NexaRoute because it puts a low-friction happy path ahead of the full schema. It also makes the custom endpoint path first-class instead of treating the preset catalog as the product boundary.

The provider form exposes the conceptual fields users need: unique internal name, API endpoint, API key, exposed model IDs, searchable discovered models, manual custom models, and a connection check. CCR explicitly warns that the connection check is a real model request, can consume tokens or provider quota, and supports selecting only the models needed for validation. The check result reports availability, matched protocol, and upstream diagnostics; results do not automatically add models to the saved list. [5]

### Presets and custom endpoints

A preset is more than a branded dropdown item: it is a template containing endpoint, supported protocols, default models, icon, provider website, and optional account/usage configuration. A custom provider can target OpenAI Chat, OpenAI Responses, Anthropic Messages, Gemini Generate, or Gemini Interactions compatible services. [4] [5]

The public provider registry uses a deliberately small provider JSON contract: `name`, `api_base_url`, an empty `api_key` placeholder, `models`, and `transformer`. That is a useful mental model for a provider catalog: provider metadata and request adaptation are separate from secrets. [6]

### Protocol/model detection

CCR auto-detects protocols and models after endpoint and key entry. It uses the endpoint for protocol probing, model discovery, icon detection, and safety checks. If detection is wrong, Advanced settings lets the operator disable auto-detection and choose a protocol manually, followed by a connection check. [4] [5]

The model list has two deliberate sources: discovery/catalog results and **Custom models** for providers with no `/models` endpoint or newly released IDs. This avoids making upstream discovery a hard dependency. Model IDs are reused by routing, Agent Config selectors, the catalog, and client `/models` responses, so a single canonical model record should drive all surfaces. [5]

### Credentials

CCR distinguishes three credential classes:

- **Upstream provider credentials**: API keys or imported local logins stored by the control plane and used to call providers.
- **Credential pools**: multiple upstream keys with enable/disable, name, priority, weight, and local request/token/image limits. Lower priority is tried first; weight breaks ties. CCR skips a key when local limits would be exceeded and tries another key on that provider.
- **CCR client API keys**: keys issued to clients calling the gateway. They have a name, masked list display, expiration, optional request/token/image limits, copy-once creation, edit, and immediate revocation. They are separate from upstream keys and from the management token. [2] [5] [7]

This separation is a major lesson: never show or conceptualize “the API key” as one undifferentiated object. A key used by NexaRoute’s gateway client should not be confused with a provider secret or the admin/session credential for the control plane.

CCR also supports imported credentials where providers expose local login state, such as Kimi CLI and OpenCode. The import UI reports when login traces exist but no usable token is available and tells the user to complete login in the originating agent before rescanning. That is a strong recovery state: explain what was detected, why it is unusable, and the next action. [5]

### Usage and account health

Provider configuration can enable **Fetch usage** independently from request forwarding. Usage connectors can show balance, subscription quota, status, and messages in the provider list, tray, overview, and account widgets. Modes include a standard endpoint, custom HTTP JSON mapping, browser request using in-app login state, raw connector JSON, plugins, and local estimates. Usage failure does not prevent model requests. [5] [8]

The account widget documentation gives explicit diagnosis for empty states: no connector, failed usage test, invalid key/account endpoint, or a deleted/renamed account. NexaRoute should preserve the distinction between “no usage integration configured,” “usage integration failed,” and “provider has no usage data.” [8]

## Model tests and health

**Check Connection** is an end-to-end test, not a superficial URL ping. It uses the selected endpoint, credential, protocol, and model, sends a real request with length-limited output, and reports per-model diagnostics. The product warns users about cost before they run it. [4] [5]

Server health is likewise layered. The Server page asks the operator to add a provider/model, create a client key, start or restart the gateway, request `/health`, send a minimal model request, and inspect the resolved provider/model in Logs. Docker can return `502` from `/health` while the management service is available but the gateway is not. [3]

This produces a useful health ladder for NexaRoute:

- **Control plane reachable**: the UI/RPC is available.
- **Gateway running**: the model endpoint is listening.
- **Provider configured**: at least one usable provider/model exists.
- **Credential authorized**: key works for the endpoint.
- **Model routable**: selected model exists and the route resolves.
- **Request verified**: a real minimal request succeeds.

Do not collapse these into one green “healthy” badge.

## Routing: simple first, advanced second

The default route is intended to be easy to set: add usable provider models, choose a default model in Agent Config, confirm the built-in agent route is enabled, and use the agent. CCR then provides custom routing as an ordered rule list. [9]

Custom routes have recognizable names, conditions, request actions, status toggles, move up/down priority controls, edit/delete actions, and search across rule name, condition, action, and row text. Rules match in list order; the first enabled match rewrites the request. A disabled rule remains visible for auditability but is excluded from matching. [9]

The rule editor separates:

- **Condition**: request header or body, field path, operator, value.
- **Request rewrites**: one or more key/value operations.
- **Enabled**: whether it participates.
- **On failure**: fallback behavior for the matching rule.

Validation is concrete: required names, condition field/value, rewrite keys, and operation-specific values gate Save. For more complex decisions, CCR offers a Node.js script rule with validation, test input, timeouts, file-size limits, worker isolation, and a circuit breaker. Script failures are fail-open and continue to the next rule, with a diagnostic recorded. [9]

**What NexaRoute should learn:** offer a one-default-route mode that can be understood in seconds, then expose an advanced rule builder without forcing users to learn request-body paths, rewrites, or scripts on day one. Preserve rule order visibly and make disabled rules auditable.

**What not to copy:** do not reproduce CCR’s exact route names, field labels, script shape, visual treatment, or provider imagery. Use the same underlying principles with NexaRoute’s own domain language and data model.

### Fallbacks and aliases

CCR supports retries, ordered fallback models, and per-rule `On failure` behavior. Its troubleshooting guidance says to inspect rule order, match conditions, and the fallback rule when the resolved model is unexpected. [1] [9] [10]

The current public docs emphasize model IDs, provider/model references, and routing resolution, but do not expose a separate, fully documented “alias” feature in the pages reviewed. There is a related documented pattern in the overview dashboard: legacy size aliases (`small`, `medium`, `large`, `wide`, `full`) are accepted and normalized to current dimensions for backward compatibility. [8] NexaRoute should treat model aliases as a deliberate compatibility layer if it adds them: show the alias, canonical target, scope, and collision behavior rather than silently rewriting names.

## Agent connection

Agent connection is profile-based. In **Agent Config**, a profile names the agent, chooses effect scope, entry mode, model, optional small/fast model, environment variables, settings/config file, and sometimes a Bot. Profiles can be enabled/disabled, limited to **Only opened from CCR**, or set as a **System default**. The docs recommend “Only opened from CCR” during trials so direct launches are not accidentally changed. [11]

The profile card provides a terminal command and an app/play action. CLI commands are namespaced (`ccr <profile-name-or-id>` for npm CLI and `ccr-app` for desktop-generated commands), and agent arguments follow `--` so they cannot be confused with CCR options. Ambiguous profile names require an ID. App-only and CLI-only constraints are explicit per agent. [2] [11]

This is a strong connection pattern because configuration, scope, launch surface, and model are visible together. It also acknowledges that “connected” means more than an environment variable: service status, launch origin, applied profile, and scope all determine whether traffic actually passes through CCR. [10]

## API keys and access boundaries

The API Keys page lists searchable, masked client keys with name, expiration, limits, edit, and remove actions. Creation shows the full key once and explicitly warns that it may not be shown again. Expiration presets include Never, 7/30/90 days, and Custom. Advanced limits cover requests, tokens, and images per minute/hour/day. [7]

The management token protects the browser UI/RPC; client keys authenticate model requests to the gateway. The CLI documentation warns that the authenticated management URL contains a token and should be treated like a password. The Server docs warn that upstream provider credentials should never be distributed to gateway clients. [2] [3]

NexaRoute should copy the security **model**, not CCR’s token names or UI text: distinct admin/session, upstream, and client credentials; masked lists; copy-once secrets; expiration; revocation; local limits; and clear “this secret is not shown again” language.

## Observability and activity

CCR has two switches under **Settings → Logs & Observability**:

- **Request logs** record same-day request details.
- **Agent observability** records agent execution traces, steps, tool calls, tool results, and duration.

A request log contains request time, ID, client, path, requested model, resolved provider/model, credential, status, success state, duration, tokens, cost estimate, headers, bodies, and errors. The Logs page filters by status, provider, model, credential, request ID, model name, request body, or response body. This makes the key debugging distinction explicit: requested model versus resolved model/provider/credential. [12]

Regular request logs are retained locally for the current day; body capture can be all, errors-only, or none, with a success sampling rate. The product therefore balances deep local diagnostics with storage/privacy controls. Agent observability is intentionally opt-in and only records tasks started after the switch is enabled. [12]

The troubleshooting page provides a practical diagnostic sequence: if an agent bypasses CCR, check service status, launch path, applied Agent Config, and scope; if traces are absent, enable the observability switches and start a new task; if one key fails, filter logs by credential before changing routing. [10]

NexaRoute should learn to make activity useful for action, not merely telemetry: show “what the client asked for,” “what route selected,” “which credential was used,” “what failed,” and “what to try next.” Do not make long-term audit retention implicit; clearly state retention and sampling in the UI.

## Settings, advanced separation, and progressive disclosure

CCR’s recurring information architecture is **simple form → Advanced settings**. Presets hide endpoint overrides; provider credentials hide pools; routing starts with default routing and opens into conditions, rewrites, and scripts; API keys hide local limits; usage fetching expands into connector mapping; proxy mode exposes certificate controls only when enabled. [4] [5] [7] [9] [3]

This separation works because advanced controls are still present, discoverable, and reversible. Collapsing a credential section does not delete saved keys. Disabling a routing rule preserves it. Removing a dashboard widget does not delete logs or providers. Reset layout restores defaults without deleting underlying configuration. [5] [8] [9]

NexaRoute should use the same principle while adding clear dependency messages. For example: “Gateway can start, but no provider/model is configured”; “usage is optional and does not affect routing”; “advanced limits apply locally and do not change provider quota.”

## Empty, error, large-list, responsive, and keyboard UX

Verified CCR patterns include:

- Empty overview after removing all widgets: **No widgets configured**.
- Empty account data with a diagnosis checklist.
- No gateway/model path despite a reachable management UI, with Server recovery steps.
- Model-not-found diagnosis that compares provider model list, routing selection, and Agent Config selection.
- Search fields for routing rules and API keys; searchable model selection with Select all/Clear; custom models when discovery returns nothing.
- Desktop overview grid up to four columns that collapses automatically on narrow screens.
- Explicit minimum widget sizes so account lists and exported cards remain readable. [5] [7] [8] [10]

The reviewed public documentation does **not** verify detailed keyboard shortcuts, focus order, roving-tabindex behavior, or screen-reader semantics. It also does not document a dedicated virtualized large-list implementation. NexaRoute should not claim these as CCR patterns. Instead, treat them as gaps to solve independently: keyboard-first dialogs and reorder controls, visible focus, Escape/Enter behavior, accessible status text, virtualization for provider/model/log lists, and responsive table-to-card transformations. The evidence supports CCR’s search/filter and responsive-grid intent, not a specific implementation.

For large lists, CCR’s practical patterns are search, select all/clear, masked values, per-row actions, filters, and explicit model list boundaries. NexaRoute should add result counts, zero-result messaging that preserves the current query, bulk actions with confirmations, and stable keyboard navigation.

## What NexaRoute should learn

1. **One stable endpoint, many clients.** Keep provider changes behind a stable client connection.
2. **Separate planes and credentials.** Management access, gateway client keys, and upstream secrets have different owners and failure modes.
3. **Presets plus custom endpoints.** Accelerate common onboarding without making the catalog a gate.
4. **Real, scoped tests.** Let operators test selected models, warn about usage, and report protocol/model diagnostics.
5. **Health as a ladder.** Distinguish UI reachability, gateway running, provider configured, credential validity, model routing, and successful request.
6. **Simple routing first.** Make one default route obvious, then expose ordered rules, fallback, rewrites, and scripts.
7. **Profiles make agent connection legible.** Show model, scope, mode, enabled state, and launch command together.
8. **Observability must explain resolution.** Log requested versus resolved provider/model/credential, not only a final status.
9. **Progressive disclosure with non-destructive collapse/disable.** Advanced settings should be available without crowding the first-run path.
10. **Design empty states as recovery.** State what is empty, why it is empty, and the next safe action.
11. **Make lists operable.** Search, filter, bulk selection, masked secrets, stable row actions, counts, and responsive layouts are foundational.
12. **Document security and retention in the product.** Call out copy-once keys, token handling, bind addresses, local limits, body capture, sampling, and retention.

## What NexaRoute should not copy

- CCR’s exact branding, name, iconography, sponsor/promotional placements, screenshots, or prose.
- Proprietary implementation details or source code; the lessons above are interaction and architecture patterns, not a request to reproduce internals.
- CCR’s exact endpoint names, CLI command names, profile syntax, database paths, JSON schemas, or script contract.
- A broad multi-agent catalog before NexaRoute’s core provider, routing, health, and observability flows are coherent.
- A single overloaded “healthy” status that masks gateway/provider/model/credential failures.
- Silent model alias normalization or implicit provider/model renaming. Aliases need visible mapping and conflict rules.
- Storing or exposing upstream keys to clients merely because the gateway itself has them.
- Claiming verified keyboard/virtualization/accessibility behavior when public CCR documentation does not establish it.
- Real connection tests that run every model by default; CCR’s own docs warn that checks can consume quota.

## Sources

[1]: https://github.com/musistudio/claude-code-router "Claude Code Router official GitHub repository and README"
[2]: https://www.npmjs.com/package/@musistudio/claude-code-router "Claude Code Router official npm CLI package documentation"
[3]: https://ccrdesk.top/en/configuration/server/ "CCR official Server configuration"
[4]: https://ccrdesk.top/en/guides/provider/ "CCR official Add a provider guide"
[5]: https://ccrdesk.top/en/configuration/providers/ "CCR official Provider configuration reference"
[6]: https://github.com/musistudio/claude-code-router-provider-registry "CCR official Provider Registry"
[7]: https://ccrdesk.top/en/configuration/api-keys/ "CCR official API keys configuration"
[8]: https://ccrdesk.top/en/configuration/overview/ "CCR official Overview dashboard configuration"
[9]: https://ccrdesk.top/en/configuration/routing/ "CCR official Routing configuration"
[10]: https://ccrdesk.top/en/troubleshooting/ "CCR official troubleshooting Q&A"
[11]: https://ccrdesk.top/en/configuration/profiles/ "CCR official Agent Config profiles"
[12]: https://ccrdesk.top/en/configuration/observability/ "CCR official Logs and observability configuration"
[13]: https://code.claude.com/docs/en/llm-gateway "Anthropic Claude Code official LLM gateway documentation"
