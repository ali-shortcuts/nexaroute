# M5-1 — Cloud provider adapters (series)

Checklist row M5-1 bundles Bedrock/Vertex/Azure adapters and the
embeddings/rerank/image/audio/TTS endpoints. It ships as focused PRs:

- M5-1a (PR #322): Azure OpenAI + Vertex AI provider types.
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
