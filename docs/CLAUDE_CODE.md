# Claude Code with NexaRoute

NexaRoute exposes an Anthropic Messages-compatible endpoint at `http://127.0.0.1:8080/v1/messages` by default. Point Claude Code at the NexaRoute base URL (`http://127.0.0.1:8080`) and use the public model name of an enabled NexaRoute virtual endpoint. Do not use a physical provider model name as the stable client identity.

Configure providers, physical deployments, capability metadata, virtual endpoint and candidate pool in the NexaRoute admin UI. Claude Code continues to send the same public model name while NexaRoute selects a compatible eligible deployment. An optional NexaRoute client API key is distinct from upstream provider keys; never configure provider credentials in Claude Code.

Example environment configuration (supported by common Anthropic-compatible clients; confirm variable support for your Claude Code version):

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN='<NexaRoute client key, only if client auth is enabled>'
```

Use a public model alias configured in NexaRoute in Claude Code's model selection. The gateway's default listener is loopback-only. Do not expose the admin surface to an untrusted network.

## Compatibility boundaries

- Native Anthropic Messages traffic can pass to Anthropic-compatible providers; common OpenAI Chat Completions cross-protocol text, tool, and stream traffic is translated through the canonical/request adapters.
- Tool names are sanitized reversibly when required. Tool IDs, JSON arguments, tool results, and common parallel tool calls are preserved with protocol-specific normalization.
- Capability filtering prevents selection of deployments that do not advertise required tools, streaming, vision, or reasoning support. OpenAI Responses support is implemented on the explicit `/v1/responses` path; it is not an undocumented substitute for every provider's Chat Completions endpoint.
- Provider-specific thinking, cache metadata, beta fields, audio/video and other extensions may not have a lossless equivalent. Unsupported or malformed structure can be rejected or fail over; NexaRoute does not promise fabricated provider features.
- Once streaming output has been committed to the client, the gateway cannot transparently restart the same answer on another model. Errors are emitted using the ingress protocol's error representation where possible.

See [KNOWN_GAPS](KNOWN_GAPS.md) and [COMPATIBILITY](COMPATIBILITY.md) for the implemented protocol boundary.
