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

Deployments of other provider types are skipped. When a model is
served only by unsupported types, the gateway answers
`400 media endpoints are supported only for openai_compatible providers in this release`
rather than sending a mangled payload upstream. Per-cloud media
addressing — Azure deployment URLs (with M5-1a), Bedrock InvokeModel
translation, Gemini embedContent — are follow-ups at the
`mediaUpstreamPath` seam in `internal/httpapi/media.go`.

## Limits

- Request bodies: 32 MiB (audio uploads). JSON responses: the
  standard upstream cap (32 MiB). Binary (speech) responses: 64 MiB.
- No streaming variants in this unit (media APIs above are unary).
- Image *edits* / variations endpoints are not routed yet.
