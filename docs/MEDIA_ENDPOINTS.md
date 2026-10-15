# Media endpoints (M5-1c)

The gateway serves the OpenAI-wire media APIs as a model-routed
passthrough. Requests are authenticated with the same client keys as
chat, routed by `model` to a deployment, failed over across
candidates in routing order, and recorded in deployment health.
Responses carry the usual `X-Gateway-Deployment` / `X-Gateway-Provider`
/ `X-Gateway-Upstream-Model` headers; upstream error bodies pass
through (bounded) when every candidate fails, exactly like the
native chat passthrough.

| Endpoint | Request | Response | Notes |
|---|---|---|---|
| `POST /v1/embeddings` | JSON | JSON | `usage.prompt_tokens` is recorded as input-token usage |
| `POST /v1/images/generations` | JSON | JSON | Large base64 payloads pass through within the response cap |
| `POST /v1/audio/speech` | JSON | **binary** | Audio bytes and the upstream content type are preserved exactly |
| `POST /v1/audio/transcriptions` | multipart/form-data | JSON | The `model` form field is rewritten to the deployment model; file parts pass through byte-exact |
| `POST /v1/rerank` | JSON | JSON | Cohere/Jina wire shape (`query`, `documents`, `top_n`) |

The client's model name is always substituted with the deployment's
upstream model before forwarding.

## Supported providers

The passthrough is defined for **`openai_compatible`** providers —
the wire shape all five APIs above follow (OpenAI itself, plus the
many compatible media servers: Voyage/Jina-style endpoints, vLLM,
TEI, LocalAI, … configured with their own `base_url`). Per-kind
upstream paths are provider fields with OpenAI defaults:
`embeddings_path`, `images_path`, `speech_path`,
`transcriptions_path`, `rerank_path`.

**`azure_openai`** deployments use native per-deployment addressing,
mirroring chat (`azureDeploymentPath`): the deployment name is the
upstream model and `api_version` (default `2024-10-21`) rides in the
query — `/openai/deployments/{deployment}/embeddings`,
`/images/generations`, `/audio/speech`, `/audio/transcriptions`.
Azure has no rerank equivalent, so rerank via Azure is refused. The
OpenAI wire shape is preserved byte-for-byte on both families.

Deployments of other provider types (`vertex`, `bedrock`, `gemini`,
`anthropic_compatible`, `openai_responses`) are skipped: their native
media wires (Gemini `embedContent`, Bedrock model-specific
`InvokeModel` JSON) are not the OpenAI shape this passthrough
preserves, so forwarding would mangle the payload. When a model is
served only by unsupported types, the gateway answers
`400 media endpoints are supported only for openai_compatible and azure_openai providers in this release`
rather than guessing a URL shape.

Live Azure/Vertex/Bedrock media behavior is UNVERIFIED (no live cloud
calls in CI); addressing is pinned by fixture/config tests.

## Limits

- Request bodies: 32 MiB (audio uploads). JSON responses: the
  standard upstream cap (32 MiB). Binary (speech) responses: 64 MiB.
- No streaming variants in this unit (media APIs above are unary).
- Image *edits* / variations endpoints are not routed yet.
