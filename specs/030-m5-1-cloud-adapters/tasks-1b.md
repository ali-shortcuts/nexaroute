# M5-1b tasks

- [x] T1 canonical bedrock.go: encode/decode/stream + fixture tests
- [x] T2 SigV4 signer (AWS doc vector) + eventstream codec/SSE translator + tests
- [x] T3 Config: bedrock type, aws_region, secret_key_env, session_token_env, validation
- [x] T4 Adapter wiring: signing hook, no bearer for bedrock, probe/eval builders, stream wrap
- [x] T5 httpapi wiring: kind mapping, attempt cases, bundle path, pump/decode cases; preset
- [x] T6 E2E tests (chat, tools, streaming via synthetic eventstream, SigV4 headers asserted)
- [x] T7 Docs (CONFIGURATION bedrock + limits, UNRELEASED), PR green; stop for owner
