# Protocol Compatibility and Claude Code Reliability Report

Date: 2026-09-26. Audit base: `7bc67662118d56120da841c1e552e8a0c5e60396` (`arena/01a0dbf9-nexaroute`). Work branch in this Arena session is fixed as `arena/01a0dd9d-nexaroute`; no branch switch was made.

## Audit summary

The repository already implements protocol handlers for Anthropic Messages, OpenAI Chat Completions and OpenAI Responses ingress, with a shared canonical request/response and stream path in `internal/protocol/canonical`, and a legacy `internal/translate` path used by protocol-specific handlers. The adapter architecture handles provider protocol selection, capability eligibility, streaming conversion and bounded failover. Existing tests include:

- `TestClaudeCodeLikeToolRoundTripThroughOpenAIProvider` (Anthropic-style client/tool flow through OpenAI-compatible provider)
- `TestAnthropicStreamToOpenAIIncludesToolArguments`
- `TestOpenAIStreamToAnthropicParallelTools`
- `TestAnthropicNativePreservesUnknownFieldsAndBetaHeader`
- `TestOpenAINativePreservesUnknownFields`
- `TestOpenAINativeInvalid2xxFailsOverBeforeCommit` and `TestAnthropicNativeInvalid2xxFailsOverBeforeCommit`
- stream stop/usage/role/error regression tests in `internal/httpapi/helpers_test.go`
- OpenAI Responses ingress and stream regression tests in `internal/httpapi/compat_engine_test.go` and `canonical_path_regression_test.go`

Privacy/security regression tests already exercise provider credential redaction and prompt/privacy isolation, including `TestProviderErrorDoesNotLeakCredential`, `TestRemote...` cases in `jev_integration_test.go`, and the `TestTask...` prompt-canary test in `task_test.go`. Existing error handling classifies common status families and sanitizes/redacts upstream error payloads. The audit did not establish a single exhaustive automated matrix covering every requested combination and error class.

## Changes in this work

- Added fuzz targets `FuzzAnthropicToOpenAI` and `FuzzOpenAIToAnthropic` with valid and malformed seeds. Arbitrary JSON that decodes into protocol request types must not panic translation; invalid structures may return errors.
- Added `docs/CLAUDE_CODE.md` with stable public endpoint/model guidance and documented translation boundaries.
- Added this report; no installer or probe/recovery engine changes were made for this protocol-only task.

## Compatibility coverage and remaining gaps

| Path | Existing support | Status |
|---|---|---|
| Anthropic client → Anthropic upstream | Native request passthrough and Anthropic stream handling | Implemented; no exhaustive new matrix added |
| Anthropic client → OpenAI Chat Completions | Translation, tool-call round trip and streamed tool arguments | Implemented for common flows; fidelity bounded by provider capabilities |
| OpenAI Chat Completions client → OpenAI upstream | Native passthrough plus canonical adapter | Implemented; exact provider extension support varies |
| OpenAI client → Anthropic upstream | Translation and parallel tool-result normalization | Implemented for common flows; not exhaustive |
| OpenAI Responses | Explicit `/v1/responses` ingress and canonical parsing/encoding | Partial/provider-dependent; not a promise of universal Responses upstream support |
| Stable Claude Code route | Virtual endpoint public model and route/candidate pools | Implemented in existing architecture; no physical model changes required client-side |
| Malformed response/error classes | Bounded parser and classified error/failover behavior with existing tests | Partial; no complete requested status-by-status strict E2E matrix added |

Structured content and images are translated where corresponding canonical blocks and adapter support exist. Reasoning/thinking controls are intentionally capability/protocol constrained; cache/beta metadata and multimodal formats beyond supported image/text/tool forms are not universally lossless. A stream already committed to the client cannot safely be replayed from another deployment.

## Test execution

- `bash -n` / `git diff --check`: protocol fuzz additions were not separately syntax-checked with Go tooling.
- `go test ./...`: BLOCKED, Go is unavailable (`go: command not found`).
- `go test -race ./...`: BLOCKED, Go is unavailable.
- `./scripts/verify.sh`: BLOCKED, Go is unavailable.
- `./scripts/stress.sh`: BLOCKED, Go is unavailable.
- `./scripts/smoke-local.sh`: BLOCKED, expected Linux binary is not present in this workspace.
- Fuzz campaigns: NOT RUN (Go unavailable).

No claim of test pass is made. The exact four-path × streaming/non-streaming × plain/tool strict local E2E matrix and exhaustive cancellation/context/rate-limit/quota/auth/model-not-found/5xx/malformed-response cases remain to be completed and run in a Go-enabled environment.
