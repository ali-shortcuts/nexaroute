# M5-1 — Cloud provider adapters (series)

Checklist row M5-1 bundles Bedrock/Vertex/Azure adapters and the
embeddings/rerank/image/audio/TTS endpoints. It ships as focused PRs:

- **M5-1a (this PR): Azure OpenAI + Vertex AI provider types.**
  Both reuse existing wire protocols (OpenAI chat JSON; Gemini JSON)
  and differ in URL construction, auth and configuration:
  - `azure_openai`: per-deployment URL
    `/openai/deployments/{deployment}/chat/completions?api-version=…`,
    `api-key` header auth, `api_version` provider field
    (default 2024-10-21).
  - `vertex`: Gemini wire format at
    `/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent|:streamGenerateContent?alt=sse`,
    Bearer auth with an operator-supplied OAuth access token
    (`vertex_project` required, `vertex_location` default
    us-central1). Token minting/refresh (ADC/service-account JWT
    exchange) is NOT built in — documented boundary.
- M5-1b: Bedrock Converse adapter + SigV4 (separate PR).
- M5-1c: media endpoints as scoped (separate PR).

Existing protocol fidelity is preserved: both types flow through
the canonical translation paths unchanged; only kind mapping, path
construction, auth defaults, probe requests and config validation
are type-aware.
