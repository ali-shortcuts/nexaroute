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

| Field | Anthropic | OpenAI Chat | OpenAI Responses | Gemini |
|---|---|---|---|---|
| `temperature` / `top_p` | mapped | mapped | mapped | mapped (generationConfig) |
| `top_k` | mapped | **dropped** (no such knob) | **dropped** | mapped |
| `max_output_tokens` | `max_tokens` (required; defaulted) | `max_tokens`/`max_completion_tokens` per dialect | `max_output_tokens` | `maxOutputTokens` |
| stop sequences | mapped | mapped | mapped | **single stop only in practice** — list passed, Gemini honors up to 5 |
| tools / tool results | tool_use / tool_result blocks | function / role:"tool" | function_call / function_call_output | functionCall / functionResponse |
| `tool_choice` none | mapped (`{"type":"none"}`) | mapped | mapped | functionCallingConfig mode |
| parallel tool calls flag | disable_parallel_tool_use | mapped | mapped | **dropped** (no equivalent knob) |
| structured output (`response_format`) | **dropped** (no IR→Anthropic mapping in this build) | mapped | mapped (`text.format`) | mapped (responseMimeType/responseSchema) |
| reasoning | `thinking` budget | `reasoning_effort` | `reasoning` object | **hints only** — no faithful budget mapping |
| prompt-cache markers | native only | **not expressed cross-protocol** | **not expressed** | **not expressed** (see M5-5) |
| seed / frequency & presence penalties | parsed into IR metadata by the OpenAI decoder; **not re-emitted as knobs** by the other encoders | same | same | same |
| provider-native unknown fields | preserved on native passthrough | preserved on native passthrough | — | **dropped** on translation |

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
