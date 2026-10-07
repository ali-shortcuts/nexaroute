# Claude Code with NexaRoute

NexaRoute exposes an Anthropic Messages-compatible endpoint at `http://127.0.0.1:8080/v1/messages` by default. Point Claude Code at the NexaRoute **Client/Public Gateway Base URL** and use the public model name of an enabled NexaRoute virtual endpoint. Do not use a physical provider model name as the stable client identity.

The Client/Public Gateway Base URL is separate from every provider upstream URL. Configure it in **Settings → Claude Code client access** or with `NEXAROUTE_CLIENT_BASE_URL`; environment configuration wins over the persisted setting. If it is empty, Connect shows the browser origin only as an explicitly labelled same-origin fallback. The value must be an absolute HTTP(S) URL without userinfo, query, or fragment; HTTP is accepted only for loopback development, while remote use requires HTTPS.

The exact request URL is `POST {client_base_url}/v1/messages`. Token counting, when requested by the client, is `POST {client_base_url}/v1/messages/count_tokens`; it is not `/v1/v1/messages`.

Configure providers, physical deployments, capability metadata, virtual endpoint and candidate pool in the NexaRoute admin UI. Claude Code continues to send the same public model name while NexaRoute selects a compatible eligible deployment. An optional NexaRoute client API key is distinct from upstream provider keys; never configure provider credentials in Claude Code.

Example environment configuration (supported by common Anthropic-compatible clients; confirm variable support for your Claude Code version):

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN='<NexaRoute client key, only if client auth is enabled>'
```

When client authentication is enabled, Claude Code's `ANTHROPIC_AUTH_TOKEN` maps to `Authorization: Bearer ...`. Clients using `ANTHROPIC_API_KEY` use `x-api-key`. NexaRoute never returns provider keys or an existing client key to the browser. For a temporary shell session, prefer a hidden read such as `read -s ANTHROPIC_AUTH_TOKEN; export ANTHROPIC_AUTH_TOKEN`; do not put secrets in tracked settings, URLs, browser storage, or command history.

After launching `claude` from the same shell, use `/status` to confirm the configured base URL. A minimal direct check is:

```bash
export NEXA_BASE_URL='http://127.0.0.1:8080'
export NEXA_MODEL='your-public-model-alias'
curl --fail-with-body -sS "$NEXA_BASE_URL/v1/messages" \
  -H 'content-type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  ${ANTHROPIC_AUTH_TOKEN:+-H "authorization: Bearer $ANTHROPIC_AUTH_TOKEN"} \
  -d "{\"model\":\"$NEXA_MODEL\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}"
```

Use a public model alias configured in NexaRoute in Claude Code's model selection. The gateway's default listener is loopback-only. Do not expose the admin surface to an untrusted network.

## Compatibility boundaries

- Native Anthropic Messages traffic can pass to Anthropic-compatible providers; common OpenAI Chat Completions cross-protocol text, tool, and stream traffic is translated through the canonical/request adapters.
- Tool names are sanitized reversibly when required. Tool IDs, JSON arguments, tool results, and common parallel tool calls are preserved with protocol-specific normalization.
- Capability filtering prevents selection of deployments that do not advertise required tools, streaming, vision, or reasoning support. OpenAI Responses support is implemented on the explicit `/v1/responses` path; it is not an undocumented substitute for every provider's Chat Completions endpoint.
- Provider-specific thinking, cache metadata, beta fields, audio/video and other extensions may not have a lossless equivalent. Unsupported or malformed structure can be rejected or fail over; NexaRoute does not promise fabricated provider features.
- Once streaming output has been committed to the client, the gateway cannot transparently restart the same answer on another model. Errors are emitted using the ingress protocol's error representation where possible.
- A custom gateway base URL does not imply full parity with Anthropic-hosted Claude Code. Remote Control and MCP tool search may be unavailable or require separate configuration; NexaRoute only claims the HTTP contracts it implements and tests.

See [KNOWN_GAPS](KNOWN_GAPS.md) and [COMPATIBILITY](COMPATIBILITY.md) for the implemented protocol boundary.
