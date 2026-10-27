# Compatibility matrix

Conformance status is **tested, not asserted**: the M5-2 suite
(`internal/httpapi/conformance_test.go`) runs five scenarios —
text, tool call, tool-result turn, streaming, upstream-error
propagation — for every ingress × upstream pair below against
dialect-faithful fakes that validate request shape and addressing.
All 12 pairs pass all five scenarios on the current tree. The suite
found three real translation defects on its first run (see
"Conformance history" below); any future red cell is a defect to
fix or a limit to document here — never a test to weaken.

## Conformance manifest (single evidence source, #436)

Per-dialect compatibility claims are recorded once, in the
versioned manifest at `internal/providers/conformance.go`
(`conformance.v1`). The table below is **generated from that
manifest** and contract-checked by
`TestCompatibilityDocMatchesManifest`: if the manifest and this
document drift apart, CI fails with the expected table as the
actionable diff. Evidence states: `FIXTURE-VERIFIED` (a named
hermetic test exercises the claim), `LIVE-VERIFIED` (a bounded,
opt-in live canary produced the evidence — none currently),
`PARTIAL` (some evidence, known gaps), `UNTESTED` (no evidence;
a preset or a `/models` response is never evidence), and
`UNSUPPORTED` (no equivalent exists; the claim asserts absence).
Verified and partial evidence older than 180 days computes to
`STALE` (`EffectiveState`) — a fixture pass proves the contract
on the day it ran, not forever. Native passthrough claims never
imply lossless translation to other dialects.

Refresh workflow when provider behavior changes: update the
fixture or adapter, re-run the cited evidence, then update the
manifest entry (state, evidence date, limitations) and
regenerate this table from `RenderConformanceTable`. Live
canaries stay opt-in, cost/rate bounded and secret-safe; they
are never required in public CI.

## Opt-in live canary (#436)

The live canary is implemented in
`internal/providers/livecanary` and exposed as
`nexaroute conformance canary`. It is **manual only**: it sends
real provider requests, spends real quota, and requires
`--acknowledge-cost`. It never runs during gateway startup, public
CI, or an ordinary `go test`, and it never edits the manifest or
promotes a claim to `LIVE-VERIFIED` by itself.

Preset mode needs no config file. Credentials come from
environment variables; the default names are
`NEXAROUTE_CANARY_API_KEY` (API key or AWS access key id),
`NEXAROUTE_CANARY_SECRET_KEY` (AWS secret access key), and
`NEXAROUTE_CANARY_SESSION_TOKEN` (optional AWS session token).
Secret values are redacted from scenario errors, the JSON report,
and the console summary. Use the protected `conformance-live`
GitHub environment and the manual
`.github/workflows/conformance-live-canary.yml` workflow (create the
environment, add those secrets, and restrict who may approve it),
or run locally:

```bash
export NEXAROUTE_CANARY_API_KEY=...   # never commit this value
go run ./cmd/gateway conformance canary \
  --acknowledge-cost --preset openai --model gpt-live-deployment \
  --scenarios core --max-calls 5 --max-output-tokens 512 \
  --max-cost-usd 1.00 --out .artifacts/conformance/live-canary.json
```

Config mode uses a configured provider and model (credentials may
already be wired through the config's `api_key_env`):

```bash
go run ./cmd/gateway conformance canary \
  --acknowledge-cost --config ~/.config/nexaroute/config.json \
  --provider my-provider --model my-model --scenarios extended \
  --max-calls 9 --out .artifacts/conformance/my-provider.json
```

Scenarios and the manifest dimensions they can support on a
successful run:

| Scenario | Dimensions exercised | Applies to |
|---|---|---|
| `text` | addressing, auth, non_stream (+ usage_cache_tokens or finish_reasons only when the response reports them) | all dialects |
| `stream` | addressing, auth, streaming_sse | all dialects |
| `tools` | addressing, auth, tool_calls | all dialects |
| `tool_result` | addressing, auth, tool_results | all dialects |
| `structured_output` | addressing, auth, structured_output | OpenAI Chat/Responses, Azure, Gemini, Vertex |
| `images` | addressing, auth, images_multimodal | all dialects |
| `parallel_tools` | addressing, auth, parallel_tools | OpenAI Chat/Responses, Azure, Anthropic |
| `reasoning` | addressing, auth, reasoning | all except Bedrock |
| `error_envelope` | error_envelopes | all dialects |

`--scenarios core` runs text, stream, tools, tool_result, and
error_envelope (5 calls). `extended`/`all` adds structured output,
images, parallel tools, and reasoning (9 calls), so raise
`--max-calls` accordingly. A scenario is skipped when the
manifest records any of its dimensions as `UNSUPPORTED` for that
dialect or when the scenario is not applicable to the dialect.
The CLI rejects a run whose call count or preflight cost estimate
exceeds `--max-calls`/`--max-cost-usd`; the runner also enforces
per-call and overall timeouts and per-request input/output token
ceilings.

Report semantics (honesty rules):

- `PASS` means every selected applicable scenario passed.
  `PARTIAL` preserves the scenarios that passed and names the failed
  dimensions next to the successes; it also covers a run stopped by
  an observed cost/call budget before the selected set finished.
  `FAIL` means no scenario passed. `NO-EVIDENCE` means everything
  was skipped. `CANCELED` records an operator (Ctrl-C/SIGTERM) or
  timeout cancellation; the report is still written.
- `verified_dimensions` are scoped to this provider, deployment,
  model, dialect, scenario, and evidence date. They are **not** a
  manifest update and **not** an automatic claim about any other
  provider using the same dialect.
- `retry_after`, `idempotency`, prompt-cache behavior, and
  provider-side cancellation are not fully scoped by this canary
  and are listed in `not_covered_dimensions`; do not upgrade them
  on the strength of a canary pass. Cache and finish-reason
  dimensions are only claimed when the live response actually
  reports the native signal.
- A canary report is an input to review. Retain it under
  `.artifacts/conformance/` (gitignored) or summarize it in the
  issue; only a reviewed, dated evidence entry may change the
  manifest, and a canary run older than 180 days computes to
  `STALE` like any other evidence.
- No live credentials are assumed to be configured in this
  repository. Fixture evidence remains the default and the table
  above stays `FIXTURE-VERIFIED`/`PARTIAL` until an actual live
  run is reviewed and recorded.

<!-- BEGIN GENERATED CONFORMANCE MANIFEST -->
| Dialect | Status | Evidence date | addressing | auth | non_stream | streaming_sse | tool_calls | tool_results | parallel_tools | reasoning | structured_output | images_multimodal | usage_cache_tokens | finish_reasons | error_envelopes | retry_after | cancellation | idempotency |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| anthropic_compatible | FIXTURE-VERIFIED | 2026-10-18 | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | PARTIAL | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | PARTIAL | UNTESTED |
| azure_openai | PARTIAL | 2026-10-18 | PARTIAL | PARTIAL | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED |
| bedrock | PARTIAL | 2026-10-18 | PARTIAL | PARTIAL | PARTIAL | PARTIAL | PARTIAL | PARTIAL | UNSUPPORTED | UNSUPPORTED | UNSUPPORTED | PARTIAL | PARTIAL | PARTIAL | PARTIAL | PARTIAL | UNTESTED | UNTESTED |
| gemini | FIXTURE-VERIFIED | 2026-10-18 | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | UNSUPPORTED | PARTIAL | FIXTURE-VERIFIED | FIXTURE-VERIFIED | PARTIAL | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | PARTIAL | UNTESTED |
| openai_compatible | FIXTURE-VERIFIED | 2026-10-18 | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | PARTIAL | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | PARTIAL | UNTESTED |
| openai_responses | FIXTURE-VERIFIED | 2026-10-18 | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | FIXTURE-VERIFIED | PARTIAL | UNTESTED |
| vertex | PARTIAL | 2026-10-18 | PARTIAL | PARTIAL | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED | UNTESTED |
<!-- END GENERATED CONFORMANCE MANIFEST -->

## Retry safety and idempotency (gateway policy, not a provider claim)

The `idempotency` column above stays `UNTESTED` for every dialect:
no provider mechanism has been verified by a fixture or a live
canary, and the gateway-side tests in this change do not promote
any dialect — they prove gateway behavior (never generating a key,
never forwarding a client key by default, never replaying a
committed stream, recording ambiguous outcomes), not provider
behavior. The only manifest changes for idempotency are the
dimension notes, which now state that policy and cite those
fixtures; states and evidence dates are unchanged. Cancellation
notes for `anthropic_compatible` and `openai_compatible` likewise
cite the new dedicated gateway cancellation fixture while keeping
the state at `PARTIAL`, because provider-side cancellation of
in-flight upstream work remains unverified. The full policy,
bounds, and evidence map are in `docs/RETRY_SAFETY.md`.

| Client ingress | Upstream | Text | Streaming | Tools | Tool-result turns | Reasoning | Images |
|---|---|---:|---:|---:|---:|---:|---:|
| Anthropic `/v1/messages` | Anthropic-compatible | Native | Native | Native | Native | Native | Provider-dependent |
| Anthropic `/v1/messages` | OpenAI Chat | Yes | Yes | Yes (reversible name mapping) | Yes | `thinking` → `reasoning_effort` | Data URL + URL |
| Anthropic `/v1/messages` | OpenAI Responses | Yes | Yes | Yes | Yes (function_call_output) | thinking → Responses reasoning | Yes |
| Anthropic `/v1/messages` | Gemini | Yes | Yes | Yes | Yes (functionResponse) | Budget hints only | Yes |
| OpenAI `/v1/chat/completions` | OpenAI-compatible | Native | Native | Native | Native | `reasoning_effort` passthrough | Provider-dependent |
| OpenAI `/v1/chat/completions` | Anthropic-compatible | Yes (role alternation guaranteed) | Yes (role chunk first) | Yes (merged parallel tool results) | Yes | `reasoning_effort` → `thinking` budget | Data URL + URL |
| OpenAI `/v1/chat/completions` | OpenAI Responses | Yes | Yes | Yes | Yes | Yes | Yes |
| OpenAI `/v1/chat/completions` | Gemini | Yes | Yes | Yes | Yes (functionResponse) | Effort hints only | Yes |
| OpenAI `/v1/responses` | OpenAI Responses | Native-shaped | Yes | Yes | Native-shaped | Native-shaped | Provider-dependent |
| OpenAI `/v1/responses` | OpenAI Chat | Yes | Yes | Yes | Yes | Yes | Yes |
| OpenAI `/v1/responses` | Anthropic-compatible | Yes | Yes | Yes | Yes (tool_result in user turn) | Responses reasoning → thinking budget | Yes |
| OpenAI `/v1/responses` | Gemini | Yes | Yes | Yes | Yes (functionResponse) | Budget hints only | Yes |

## Translation limits (per field, canonical paths)

The canonical IR is the ceiling: fields with no IR representation
do not cross protocols (native passthrough preserves them only on
same-protocol pairs). Per upstream encoder:

| Field | Anthropic | OpenAI Chat | OpenAI Responses | Gemini | Bedrock (Converse) |
|---|---|---|---|---|---|
| `temperature` / `top_p` | mapped | mapped | mapped | mapped (generationConfig) | mapped (inferenceConfig) |
| `top_k` | mapped | **dropped** (no such knob) | **dropped** | mapped | **dropped** (no such knob) |
| `max_output_tokens` | `max_tokens` (required; defaulted) | `max_tokens`/`max_completion_tokens` per dialect | `max_output_tokens` | `maxOutputTokens` | `maxTokens` (inferenceConfig; passed through, no defaulting) |
| stop sequences | mapped | mapped | mapped | **single stop only in practice** — list passed, Gemini honors up to 5 | mapped (stopSequences) |
| tools / tool results | tool_use / tool_result blocks | function / role:"tool" | function_call / function_call_output | functionCall / functionResponse | toolUse / toolResult blocks, toolConfig |
| `tool_choice` none | mapped (`{"type":"none"}`) | mapped | mapped | functionCallingConfig mode | withholds tools (no Converse none mode) |
| parallel tool calls flag | disable_parallel_tool_use | mapped | mapped | **dropped** (no equivalent knob) | **dropped** (no equivalent knob) |
| structured output (`response_format`) | **dropped** (no IR→Anthropic mapping in this build) | mapped | mapped (`text.format`) | mapped (responseMimeType/responseSchema) | **dropped** (no mapping) |
| reasoning | `thinking` budget | `reasoning_effort` | `reasoning` object | **hints only** — no faithful budget mapping | **dropped** (thinking blocks not encoded) |
| prompt-cache markers | native only | **not expressed cross-protocol** | **not expressed** | **not expressed** (see M5-5) | expressed as Converse `cachePoint` (system + message text/tool blocks, TTL `1h` carried; max 4/request, excess markers dropped, content kept) |
| seed / frequency & presence penalties | parsed into IR metadata by the OpenAI decoder; **not re-emitted as knobs** by the other encoders | same | same | same | same |
| provider-native unknown fields | preserved on native passthrough | preserved on native passthrough | — | **dropped** on translation | **dropped** on translation |
| built-in tools (e.g. `web_search`, `computer_use`) | **rejected at ingress** — no canonical representation; the request fails with an explicit error rather than silently dropping the tool | **rejected at ingress** | **rejected at ingress** (function tools only) | **rejected at ingress** | **rejected at ingress** |
| `previous_response_id` | — | — | **rejected at ingress** — the gateway does not retain upstream responses (`store` is not forwarded), so a chained id could never resolve; it is refused instead of silently discarded | — | — |
| audio / file content parts (`input_audio`, `input_file`) | **rejected at ingress** — no canonical representation; never dropped or forwarded as raw-JSON text | **rejected at ingress** | **rejected at ingress** | **rejected at ingress** | **rejected at ingress** |

Two IR shapes exist for tool results and both are supported by
every encoder (regression-pinned): dedicated `tool` messages
(OpenAI-family decoders) and tool-result parts inside user
messages (Anthropic decoder).

## Conformance history (defects found by the M5-2 suite, fixed)

1. The canonical Anthropic encoder emitted IR `tool` messages with
   role `"tool"` verbatim — conversations the real Anthropic API
   rejects (400). Tool results now fold into user turns.
2. The canonical Responses and OpenAI Chat encoders silently
   dropped tool results embedded in user messages (the Anthropic
   decoder's shape). They now emit sibling
   function_call_output / `role:"tool"` items.
3. The Responses ingress's repair dispatcher ignored the bundle
   path and always called the provider's chat path — Gemini
   upstreams received Gemini bodies at `/v1/chat/completions`.
   Model-in-path addressing now holds for every ingress.

Additional Anthropic behavior:

- native `/v1/messages/count_tokens` is used on eligible Anthropic-compatible providers when available;
- local estimated counting is the fallback;
- Anthropic ingress returns Anthropic-style error envelopes;
- `anthropic-beta` / `anthropic-version` can be forwarded through the provider allowlist;
- client auth is not reused as provider auth;
- translated streaming requests inject `stream_options: {"include_usage": true}` and retry once without it against providers that reject the option.

Cross-protocol guarantees (the "never dead-end" rules):

1. Role alternation — consecutive same-role OpenAI messages, including tool
   result runs, merge into valid Anthropic alternation; assistant-first
   histories receive a user placeholder.
2. Reasoning blocks never poison a conversation — unsigned thinking blocks are
   never fabricated toward Anthropic clients; Anthropic thinking history is
   dropped (not replayed) toward OpenAI upstreams; thinking requests over
   tool-using OpenAI-format histories are dropped instead of triggering the
   upstream's "Expected thinking" 400.
3. Tool names round-trip — MCP-style names that violate either protocol's
   grammar rename reversibly and restore in non-streaming and streaming
   responses.
4. Malformed tool arguments are preserved via `{"_raw": ...}` objects instead
   of failing the request or silently vanishing.
5. Unknown content blocks are preserved on native-protocol passthrough where possible. Cross-protocol translation rejects blocks without a canonical equivalent instead of silently discarding user content.
6. Usage propagation — include_usage injection, cache-token mapping in both
   directions, and `{}` padding for tools that stream no arguments.

The strongest intended test path is:

```text
Claude Code -> Anthropic /v1/messages -> NexaRoute -> OpenAI-compatible Chat2API -> model
```

plus native Anthropic-compatible passthrough.

## Prompt caching (M5-5)

| Mechanism | Anthropic | OpenAI Chat | OpenAI Responses | Gemini | Bedrock (Converse) |
|---|---|---|---|---|---|
| Block markers (`cache_control`) | Native; preserved through the canonical codec (system + text/image/tool blocks, TTL `5m`/`1h` carried) | No equivalent — dropped on encode | No equivalent — dropped | No equivalent — dropped | Expressed as `cachePoint` checkpoints after the marked system/message content (text + tool_use/tool_result; TTL `1h` carried, otherwise `{"type": "default"}`); max 4 per request — excess markers dropped, content kept; minimum prefix tokens enforced server-side by Bedrock |
| Request key (`prompt_cache_key`) | No equivalent — ignored | Mapped both ways (provider-dependent: honored by OpenAI and some compatibles) | Mapped both ways | Ignored | Ignored (no equivalent) |
| Cache usage, read | `cache_read_input_tokens` | `prompt_tokens_details.cached_tokens` | `input_tokens_details.cached_tokens` | `cachedContentTokenCount` | `cacheReadInputTokens` (note: Converse `inputTokens` excludes cached tokens) |
| Cache usage, write | `cache_creation_input_tokens` | — (no write accounting) | — | — (implicit) | `cacheWriteInputTokens` |

Rules: Anthropic block markers never translate into
`prompt_cache_key` (different semantics: breakpoints vs a routing
hint) and never leak into other dialects' payloads. Gemini prompt
caching is implicit — the gateway cannot request it, only report
its usage. Cache-token usage maps through the canonical IR in both
directions for all five kinds and is pinned by tests. Live-Bedrock
cache behavior is UNVERIFIED (no live AWS calls; mapping evidence
is adapter unit tests plus the cited AWS field shapes).
