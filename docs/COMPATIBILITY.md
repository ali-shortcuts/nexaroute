# Compatibility matrix — v0.3

| Client ingress | Upstream | Text | Text streaming | Tools | Tool streaming | Common image path | Unknown native fields |
|---|---|---:|---:|---:|---:|---:|---:|
| Anthropic `/v1/messages` | Anthropic-compatible | Yes | Yes | Yes | Native passthrough | Provider-dependent | Preserved where possible |
| Anthropic `/v1/messages` | OpenAI-compatible Chat Completions | Yes | Yes | Yes for common flows | Parallel common flows tested | Selected conversions | Only fields with a mapped meaning |
| OpenAI `/v1/chat/completions` | OpenAI-compatible | Yes | Yes | Yes | Native passthrough | Provider-dependent | Preserved where possible |
| OpenAI `/v1/chat/completions` | Anthropic-compatible | Yes | Yes | Common flows | Implemented but less mature than native | Selected conversions | Only fields with a mapped meaning |

Additional Anthropic behavior:

- native `/v1/messages/count_tokens` is used on eligible Anthropic-compatible providers when available;
- local estimated counting is the fallback;
- Anthropic ingress returns Anthropic-style error envelopes;
- `anthropic-beta` / `anthropic-version` can be forwarded through the provider allowlist;
- client auth is not reused as provider auth.

The strongest intended v0.3 test path is:

```text
Claude Code -> Anthropic /v1/messages -> NexaRoute -> OpenAI-compatible Chat2API -> model
```

plus native Anthropic-compatible passthrough.
