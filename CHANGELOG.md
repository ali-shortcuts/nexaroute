# Changelog

## v0.5.2 — in-flight quota reservation

### Added

- **In-flight request reservations**: every tagged data-plane upstream leg temporarily reserves one request against the latest provider-reported request headroom.
- **In-flight token reservations**: each leg also reserves the request's conservative prompt estimate plus explicit output-token ceiling when available.
- **Effective quota telemetry**: Provider/Admin/Prometheus surfaces now expose raw remaining quota, locally reserved request/token amounts, and effective remaining headroom.
- **Reservation-aware routing**: quota pressure and exhausted-provider signals use effective remaining quota so concurrent requests do not all route against the same stale quota snapshot.
- **Hedging-safe accounting**: primary and hedge legs reserve independently. Fresh resource-specific remaining headers release that resource immediately; otherwise the reservation survives until response-body completion.

### Boundaries

- Reservations are advisory and local to one NexaRoute process. They do not hard-block requests.
- Probe, discovery, provider-test and other untagged control-plane calls do not create data-plane reservations.
- Completed requests without fresh quota headers do not create durable rolling-window debt; hard RPM/TPM throttling remains future work.


## v0.5.1 — price-aware routing and proactive quota headroom

### Added

- **Opt-in `cost_aware` routing**: verified-ready selection keeps configured priority tiers authoritative, prefers lower bounded request cost among known-priced peers, never treats unknown pricing as free, and falls back to ordinary score ordering when a caller omits an output-token ceiling.
- **Proactive quota pressure**: common OpenAI/Anthropic request/token limit headers are captured in addition to remaining/reset values. While a resource-specific reset is still in the future, routing pressure begins below 25% remaining headroom and rises smoothly toward the existing exhausted-provider ceiling.
- **Separate request/token reset deadlines**: request and token quota windows are tracked independently so one resource cannot borrow the other's reset clock.
- Dashboard/Admin/Prometheus surfaces now show quota ceilings and resource-specific reset deadlines.

### Fixed

- Runtime version reporting is consistent: binary and HTTP/dashboard version now both report v0.5.1.
- Provider-card edit button binding uses the multi-element selector correctly.


## v0.5.0 — hedging, response cache, client keys, context pre-routing, usage accounting

### Added

- **Request hedging** (`routing.hedging_enabled`, `routing.hedging_delay_ms`): bounded first-attempt race against the next eligible deployment when the primary is slow to response headers. Loser legs are abandoned with zero health signal; `hedge_launch` / `hedged_abandoned` events make every race observable. Guardrails: first attempt only, one partner maximum, attempt-budget aware, route-context bounded.
- **Exact-match response cache** (`cache.*`): opt-in, LRU + TTL + entry/byte bounded, deterministic non-streaming requests only, wholesale invalidation on config swap, `x-nexaroute-no-cache` per-request bypass, `X-NexaRoute-Cache` header, Prometheus counters, dashboard KPI.
- **Client API keys** (`client_auth.*`): opt-in data-plane auth for `/v1/messages`, `/v1/chat/completions`, `/v1/messages/count_tokens` and `/v1/models`; constant-time digest comparison; optional per-key RPM token bucket with `Retry-After`; protocol-shaped errors.
- **Context-window pre-routing** (`models[].context_window`): deployments advertising a too-small window are skipped when the estimated prompt plus `max_tokens` cannot fit; unknown windows are never filtered.
- **Usage and cost accounting**: per-deployment cumulative prompt/completion tokens from all four response paths (including native SSE tail chunks), optional per-model pricing with snapshot-time cost resolution, Prometheus `nexaroute_deployment_tokens_total` / `nexaroute_estimated_cost_usd_total`, admin snapshot `usage`, dashboard Tokens/Spend KPI card.
- **Provider incident circuits, quota hints, TTFT** (merged from `codex/provider-incident-intelligence-v03`): provider-level evidence-windowed circuits with half-open reopen, OpenAI/Anthropic rate-limit header observation with quota-exhausted deprioritization until reset, error-class-aware failure policy (400/422 health-neutral, 409/425 failover without poisoning, 404 deployment-scoped), separate streaming TTFT EWMA, admin controls, metrics and UI columns.

### Fixed

- `stream_options` 400-retry now rebinds correctly when the hedged winner is the translated leg (stream options injection state follows the winning bundle).
- Usage parsing tolerates providers that emit `usage: null` in the include_usage tail chunk.

## v0.4.1 — concurrent-correctness, admin hardening and stream failover

### Fixed

- **Circuit breaker: concurrent observations can no longer downgrade an active
  cooldown.** A half-open stampede or an in-flight request racing a 429
  `ForceCooldown` used to flip the state back to `Degraded`, immediately
  re-admitting the failing deployment. `RecordFailure` keeps an active
  cooldown (and its deadline), `RecordSuccess` no longer re-admits a
  deployment whose cooldown deadline is still in the future, and degraded
  states no longer carry stale `cooldown_until` values.
- **Stream failover before response commit.** The OpenAI-facing translated
  stream deferred its role chunk until the first valid upstream event, so an
  upstream that answers 200 and then dies before streaming now fails over to
  the next candidate instead of committing a truncated response.
- **Terminal error frames on mid-stream failures.** Both translation
  directions emit a final error event (OpenAI-style `data: {"error": ...}`
  chunk or Anthropic `event: error`) instead of silently cutting the stream,
  and `[DONE]` is withheld after an error.
- **Implicit response commits are now visible.** `statusWriter` overrides
  `Write`, so `responseCommitted()` observes handlers that stream bytes
  without an explicit `WriteHeader` (prevents double-stream concatenation).
- **Admin API DNS-rebinding defense.** The keyless loopback trust mode now
  validates the `Host` header, so a rebounded browser origin cannot read
  admin data (including `reveal=1` resolved provider API keys).
- **Admin API rate limiting.** `/admin/api/` requests pass through a per-IP
  token bucket (capacity 90, 1.5/s refill); each unauthorized attempt burns
  the full bucket, collapsing online key brute force while the dashboard's
  own polling is unaffected.
- **`POST /admin/api/probe` requires a JSON body**, closing the cross-site
  simple-POST probe-trigger path that bypassed the JSON content-type gate.
- **Dashboard Health tab and footer are live.** The snapshot now emits
  `provider_pressure`, `scope_health`, `request_total` and `version` (the
  UI previously read four fields the backend never sent, so provider
  pressure, capability evidence and the request counter were permanently
  empty).
- **Console filters match real event kinds.** The Probes filter now matches
  `probe_ready`/`probe_fail`/`probe_quarantine`/`recovery_*` (it previously
  matched zero real kinds and always showed an empty list) and the Errors
  filter includes stream and recovery failure kinds.
- **Graceful shutdown drains real in-flight work.** The drain window is
  derived from the request timeout and the largest provider stream idle
  timeout instead of a fixed 10 s that SIGTERM-cut long streams.
- **`NEXAROUTE_ADMIN_KEY=` (empty) no longer disables key auth** over a
  file-provided key; config validation rejects non-numeric/out-of-range
  listen ports.
- **Event truncation is UTF-8 safe**, admin one-shot adapters release idle
  connections, and editing a provider whose secret comes from an env var no
  longer persists the resolved literal into `config.json` (UI keeps the key
  field blank; the server additionally drops a submitted literal identical
  to the env-resolved value).
- `routeSnapshot` strips map-bearing fields from the hot-path config view so
  a future handler cannot race the admin config swap.

## v0.4 — bulletproof cross-protocol translation and 9router-class dashboard

### OpenAI ↔ Anthropic translation hardening

This release rebuilds the cross-protocol translation layer around one rule:
a valid conversation must never dead-end because of a protocol mismatch.
Patterns were studied from LiteLLM, 9router (open-sse), new-api, one-api,
claude-code-proxy and y-router, then implemented natively with zero
dependencies.

Request direction (Anthropic client → OpenAI upstream):

- `thinking` / `redacted_thinking` blocks in replayed history are dropped
  instead of failing the request, so Claude Code extended-thinking sessions
  survive against OpenAI-compatible upstreams.
- Unknown Anthropic block types (`server_tool_use`, `web_search_tool_result`,
  documents, future additions) are dropped instead of hard-failing.
- `thinking {type:enabled, budget_tokens}` maps onto a conservative
  `reasoning_effort` level; `metadata.user_id` maps onto the OpenAI `user`
  parameter; `disable_parallel_tool_use` maps onto `parallel_tool_calls`.
- `tool_result` blocks normalize fully: block arrays become ordered
  text/image parts, `is_error` keeps a visible `[tool error]` prefix, and tool
  messages always precede trailing user text so they directly follow the
  assistant `tool_calls` they answer.

Request direction (OpenAI client → Anthropic upstream):

- Consecutive same-role messages are merged into single multi-block messages.
  The Anthropic API enforces strict role alternation and rejects naive relays;
  NexaRoute now guarantees valid alternation for every input.
- Parallel OpenAI tool results coalesce into one user message with multiple
  `tool_result` blocks (the shape Claude Code itself produces).
- Conversations opening with an assistant turn receive a user placeholder;
  empty content becomes `...` instead of an API-rejected empty text block.
- `stop` / `stop_sequences` translate in both directions (previously dropped).
- `reasoning_effort` maps onto Anthropic `thinking` budgets with the
  `max_tokens > budget_tokens` invariant enforced, and the thinking request is
  dropped over tool-using histories that cannot carry signed thinking blocks
  (prevents Anthropic's "Expected thinking or redacted_thinking" 400).
- Tool names sanitize reversibly: MCP-style names with dots/colons or excess
  length (OpenAI 64-char limit, Anthropic 128-char limit) rename through a
  bidirectional map and restore on every response path.
- `input_schema` defaults to `{"type":"object"}` when missing; image media
  types normalize (`image/jpg` → `image/jpeg`); `user` maps to
  `metadata.user_id`.

Response and streaming:

- Real token usage now flows end to end: translated streaming requests inject
  `stream_options: {"include_usage": true}`, providers that reject the option
  are retried once without it, and usage (including cache-read tokens)
  surfaces in Anthropic `message_delta` usage and OpenAI final usage chunks.
- The Anthropic→OpenAI stream emits the role-first chunk OpenAI clients
  expect, maps `thinking_delta` onto the widely-supported `reasoning_content`
  field, pads empty tool argument streams with `{}` (no empty JSON parses),
  and maps `refusal`/`pause_turn` stop reasons onto OpenAI finish reasons.
- The OpenAI→Anthropic stream handles content-part array deltas, emits a
  `ping` frame after `message_start`, and maps `function_call` /
  `content_filter` finish reasons onto Anthropic stop reasons.
- Non-stream response translation handles content-part arrays, surfaces
  Anthropic thinking text as `reasoning_content` (safe direction; unsigned
  thinking blocks are never fabricated in the poison-prone direction), and
  preserves malformed tool arguments in a `{"_raw": ...}` object instead of
  failing the whole turn.
- A spec-correct SSE reader replaces line-scanning: multi-line `data:` frames
  join, comments and CRLF are tolerated, and unterminated final frames flush.

### Routing

- Reasoning-marked requests relax their provider-class preference when no
  Anthropic-compatible (or OpenAI-compatible) deployment is healthy, falling
  back to any reasoning-capable deployment instead of returning 503.

### Web dashboard

- Fully rebuilt embedded dashboard (no external assets, no build step):
  KPI cards with latency sparkline, fleet-health donut, live model ring with
  orbit animation, latency timeline chart, 9router-style live console with
  severity-colored routing lines and filters, provider cards with gradient
  marks and health bars, deployment table with inline latency bars and search,
  capability evidence panel, tabbed CLI Tools with copy-to-clipboard, and a
  polished provider drawer.

### Bug fixes

- Fixed a pre-existing test-infrastructure deadlock: fake upstream handlers
  that never read the request body defeated the HTTP server's disconnect
  detection after the remaining single background read consumed buffered body
  bytes; cancellation tests could hang forever. Handlers now drain the body,
  matching real upstream behavior.
- Fixed `streamAnthropicToOpenAI` ending without a finish chunk when the
  upstream ended with `message_stop` but no `message_delta` stop reason.
- Usage chunks with an empty `choices` array are no longer discarded during
  stream translation.

## v0.3 — baseline

- Ready Mesh routing with session affinity, capacity-aware power-of-two
  selection, supervised bounded recovery, credential pools with key-level
  cooldown, bounded global admission, self-rotating logs, and the first
  embedded control-plane dashboard.
