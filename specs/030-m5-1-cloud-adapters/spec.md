# M5-1 — Cloud provider adapters (series)

Checklist row M5-1 bundles Bedrock/Vertex/Azure adapters and the
embeddings/rerank/image/audio/TTS endpoints. It ships as focused PRs:

- **M5-1a (PR #322): Azure OpenAI + Vertex AI provider types.**
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
- **M5-1b (this PR): Amazon Bedrock (Converse API).** A genuinely
  different wire protocol, integrated through the canonical IR as a
  new upstream kind `bedrock`:
  - `internal/protocol/canonical/bedrock.go`: Converse request
    encoding (messages/system/inferenceConfig/toolConfig), response
    decoding, and stream-chunk decoding.
  - Auth is AWS SigV4, hand-rolled on stdlib crypto and tested
    against AWS's published documentation test vector. Credentials:
    the access key ID via the normal provider key fields, the secret
    access key ONLY via the env var named by the new
    `secret_key_env` provider field (never a literal), optional
    session token via `session_token_env`; region via `aws_region`
    (default us-east-1). One credential pair per provider in this
    unit — the key-rotation pool does not apply (documented limit).
  - Streaming: ConverseStream answers in the AWS binary event
    stream format. The adapter decodes it (CRC-verified frames) and
    re-emits SSE frames carrying {eventType, payload} envelopes that
    the canonical stream pump decodes — one pump, no protocol leak.
  - Limits (documented): tool_choice "none" drops toolConfig
    (Converse has no none mode); thinking/reasoning parts are not
    encoded (Converse reasoning is model-specific); document blocks
    are not mapped.
- M5-1c: media endpoints as scoped (separate PR).

Existing protocol fidelity is preserved: the Azure OpenAI and Vertex types flow through
the canonical translation paths unchanged; only kind mapping, path
construction, auth defaults, probe requests and config validation
are type-aware.
