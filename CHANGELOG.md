# Changelog

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
