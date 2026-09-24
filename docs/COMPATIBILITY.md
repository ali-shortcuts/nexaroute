# Compatibility matrix — v0.5 + Compatibility Engine

| Client ingress | Upstream | Text | Text streaming | Tools | Tool streaming | Reasoning | Images | Unknown native fields |
|---|---|---:|---:|---:|---:|---:|---:|---|
| Anthropic `/v1/messages` | Anthropic-compatible | Yes | Yes | Yes | Native passthrough | Native passthrough | Provider-dependent | Preserved where possible |
| Anthropic `/v1/messages` | OpenAI-compatible Chat Completions | Yes | Yes | Yes (reversible name mapping) | Parallel flows, real usage | `thinking` → `reasoning_effort` | Data URL + URL sources | Fields with a mapped meaning |
| OpenAI `/v1/chat/completions` | OpenAI-compatible | Yes | Yes | Yes | Native passthrough | `reasoning_effort` passthrough | Provider-dependent | Preserved where possible |
| OpenAI `/v1/chat/completions` | Anthropic-compatible | Yes (role alternation guaranteed) | Yes (role chunk first) | Yes (merged parallel tool results) | Yes (`{}`-padded args, reasoning deltas) | `reasoning_effort` → `thinking` budget | Data URL + URL sources | Fields with a mapped meaning |
| OpenAI `/v1/responses` | OpenAI-compatible | Yes (via Canonical IR) | Non-streaming in this phase | Yes (function tools) | Planned | Via `reasoning_effort` mapping | Input image parts | Fields with a mapped meaning |
| OpenAI `/v1/responses` | Anthropic-compatible | Yes (via Canonical IR) | Non-streaming in this phase | Yes | Planned | Via thinking mapping | Provider-dependent | Fields with a mapped meaning |

Universal Compatibility Engine (per-deployment, separate from health):

- tri-state capability contracts (SUPPORTED / UNSUPPORTED / UNKNOWN) with
  provenance and conservative runtime learning — see `GET /admin/api/compat`;
- structured error taxonomy: capability failures (unsupported parameter, tool
  calling, reasoning, vision, structured output, context overflow) are
  health-neutral and teach the contract instead of quarantining the model;
- bounded repair (≤2 attempts): `max_completion_tokens → max_tokens`, drop
  optional unsupported fields; semantics-critical fields are never dropped;
- dialect registry (`generic_openai`, `nvidia_nim`, `deepseek`, `openrouter`,
  `together`, `groq`, `generic_anthropic`, `custom`) selected per provider via
  optional `dialect` / `protocol` fields (`auto` by default);
- router filters REQUIRED capabilities verified UNSUPPORTED before any upstream
  attempt; UNKNOWN stays eligible but loses score to verified alternatives;
- Quick / Full / Claude Code agent compatibility probes at
  `POST /admin/api/compat/probe`, plus the Compat dashboard tab.

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
5. Unknown content block types are dropped, never fatal.
6. Usage propagation — include_usage injection, cache-token mapping in both
   directions, and `{}` padding for tools that stream no arguments.

The strongest intended v0.5 test path is:

```text
Claude Code -> Anthropic /v1/messages -> NexaRoute -> OpenAI-compatible Chat2API -> model
```

plus native Anthropic-compatible passthrough.
