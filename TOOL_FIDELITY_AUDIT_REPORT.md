# NexaRoute Tool-Call Fidelity Audit — Final Report

**Date:** 2026-09-27
**Branch:** arena/01a0e281-nexaroute
**Auditor:** Arena Agent
**Go toolchain:** bootstrapped 1.4 → 1.17.13 → 1.21.13 → 1.23.0 (GitHub egress only)

## Executive Summary

**Verdict: PROTECTED — No string → unknown coercion bug found. Implementation correctly preserves schema type fidelity across all protocol combos. Defense-in-depth validation now enforced in production path.**

The observed failure class elsewhere (Bash.command, Read.file_path InputValidationError where string expected but got object/null/array) does **NOT** reproduce in NexaRoute. All 16 gates + A-K acceptance criteria pass.

- Existing canonical IR stores `ToolCall.Arguments` as **raw JSON string**, never as `any`/`unknown`. This prevents implicit coercion.
- Translation layers (anthropic_to_openai, openai_to_anthropic, canonical encoders/decoders) use `json.Marshal` / `json.RawMessage` round-trips, preserving type.
- Streaming assembly concatenates `ArgsDelta` fragments **before** JSON parsing, tested with hostile splits (including unicode, escaped quotes, backslashes, multiline, nested JSON, 1KB-1MB boundaries).
- Malformed payloads are detected via `ValidateToolCallWithMeta` which returns structured diagnostic: `tool=%s field=%s expected=%s actual=%s stage=%s protocol=%s streaming=%t provider=%s: message`
- **New defense-in-depth (2026-09-27 continuation):**
  - `internal/httpapi/canonical_path.go`: `canonicalStreamPump` now tracks assembled args per tool index, enforces 1MB size boundary, validates raw args on `tool_end` via `ValidateRawArguments`, emits `StreamError` with structured diagnostic and fail-closes.
  - `handleCanonicalResponse`: validates all response blocks via `ValidateResponseBlocks` (raw + critical field checks for Bash/Read) before emitting to client, returning error for failover if validation fails.
  - `validation.go`: added `ValidateRawArguments` (detects double-encoding, null, empty, non-object) and `ValidateResponseBlocks` (enforces Bash requires `command` string, Read requires `file_path` string, plus critical-field type checks).

## 1. Complete Tool-Call Path Trace (All Protocol Combos)

**Ingress:** `anthropic`, `openai_chat`, `openai_responses`, `gemini`
**Egress:** same 4 families
**IR:** `internal/protocol/canonical/*`

| Path | Encoder | Decoder | Result |
|------|---------|---------|--------|
| anthropic → openai_chat | EncodeAnthropicRequest | DecodeOpenAIChatResponse | PASS |
| openai_chat → anthropic | EncodeOpenAIChatRequest | DecodeAnthropicResponse | PASS |
| anthropic → anthropic | EncodeAnthropicRequest | DecodeAnthropicResponse | PASS |
| openai → openai | EncodeOpenAIChatRequest | DecodeOpenAIChatResponse | PASS |
| openai → gemini | EncodeGeminiRequest | DecodeGeminiResponse | PASS |
| gemini → anthropic | EncodeAnthropicResponse | DecodeGeminiResponse | PASS |
| gemini → openai | EncodeOpenAIChatResponse | DecodeGeminiResponse | PASS |
| Responses API | EncodeResponsesRequest | DecodeResponsesResponse | PASS |

Verified via `internal/httpapi/protocol_matrix_e2e_test.go` (4×5 matrix, non-streaming, streaming, failure, cancellation, deadline) + custom fidelity tests.

## 2. Schema Type Preservation

- `ToolDef.Parameters` is `json.RawMessage` containing JSON Schema.
- `ToolCall.Arguments` is string containing JSON object.
- No `map[string]any` → `string` coercion, no `interface{}` unwrapping.
- Tests in `canonical/tool_fidelity_audit_test.go: TestToolFidelity_SchemaTypePreservation` verify string, integer, number, boolean, array, object, enum.

## 3. Streaming Argument Assembly with Hostile Splits

**Test:** `TestToolFidelity_StreamingFragmentation` and `TestToolFidelityAudit_StreamingFragmentation`

Hostile cases:
- `{"command":"git status"}` split at 2,5,10
- `{"command":"echo \"hello\""}` split at 5,12,18
- `{"command":"echo café 🚀"}` split at 4,10,15 (unicode multi-byte)
- `{"command":"line1\nline2"}` split at 5,12
- Nested JSON, 1000-char string

Implementation:
- `canonical/stream.go` `DecodeOpenAIStreamChunk` emits `StreamToolDelta` per fragment.
- `canonicalStreamPump` in `httpapi/canonical_path.go` concatenates deltas before emitting to client emitter.
- Anthropic emitter reassembles via `partial_json` accumulation.
- **Fixed test bug:** Original httpapi test searched body for contiguous "echo" / "command" which fails when split across SSE events. Fixed to reassemble `partial_json` from SSE events and validate final JSON type.

## 4. Long-Context Stress

Sizes: 1KB, 16KB, 64KB, 256KB, 1MB (in canonical test) + 1KB/16KB/64KB in httpapi test.
- Large system prompt + user message (repeat "x").
- Tool call after large context still preserves `command`/`file_path` as string.
- No contamination, no truncation.

## 5. Multiple Tool-Call Stress

- 100 sequential Bash calls: no state leakage.
- Mixed sequence Bash→Read→Bash→Read→Bash: verifies no contamination (`file_path` doesn't leak into Bash, etc.).
- Parallel tool calls (2 simultaneous in one assistant turn): both preserved.
- Concurrent goroutine stress in httpapi: `TestToolFidelityAudit_ConcurrentToolCalls` launches parallel requests.

## 6. Provider Translation Matrix

Covered in `TestToolFidelity_ProviderTranslationMatrix` (canonical) and `TestToolFidelityAudit_ProviderTranslationMatrix` (httpapi):
- Bash `pwd`, `git status --short`, Read `/tmp/example.txt`
- Round-trip: OpenAI upstream → Anthropic client, Anthropic upstream → OpenAI client, etc.
- Asserts `typeof(command) == string` and `typeof(file_path) == string`.

## 7. Canonical Tool Model Audit

- `canonical.go`: `ToolCall.Arguments` is string, not `any`.
- `GeminiCall.Args` is `json.RawMessage`, fallback to `"{}"` only when empty, never to object wrapper.
- `EncodeAnthropicResponse` handles malformed args by preserving in `{"_raw": args}` only for observability, not coercing type.
- `stripSchemaKeywords` removes `$`-prefixed keys but preserves `type: string` etc.
- No hidden `unknown` type in IR.

## 8. Never Silently Coerce Dangerous Types

`validation.go` enforces:
- If schema says `type: string` but value is object/array/number/bool/null → **fail closed** with `ToolCallValidationError`.
- `typeMatches` checks strict: string must be Go string, integer must be float64 with integer value, etc.
- No `fmt.Sprint` coercion, no `json.Number` → string conversion.

## 9. Malformed Fail-Closed

Test cases in `TestToolFidelity_MalformedFailClosed`:
- `{"command":null}` → fail
- `{"command":{}}` → fail
- `{"command":[]}` → fail
- `{"command":42}` → fail
- `{"command":true}` → fail
- `{}` (missing required) → fail
- `{"command":` (invalid JSON) → fail
- `{"command":"pwd"` (truncated) → fail
- `"{\"command\":\"pwd\"}"` (double-encoded) → fail
- `{"value":"pwd"}` (wrapper) → fail
- Duplicate keys `{"command":"pwd","command":"ls"}` → allowed (last wins, still string) — matches JSON spec.

## 10. Double-Encoding Audit

- Single encoding: `{"command":"pwd"}` → JSON string `"{\"command\":\"pwd\"}"` → decode → original.
- Double encoding: `json.Marshal(json.Marshal(original))` produces extra quotes, detected via `strings.HasPrefix(args, "\"")`.
- Canonical path never double-encodes: `ToolCall.Arguments` is stored as raw string, passed directly to `json.RawMessage`, not via `Marshal(Marshal(...))`.

## 11. Unknown/Any Audit

- Schema without `type` → skip strict check (allow).
- `type: object` with arbitrary props → validated as object, not coerced.
- No `any`/`unknown` in Go types; all args are `string` → `map[string]any` only for validation, not storage.

## 12. Auto-Mode / Classifier Path

- `TestToolFidelityAudit_ClassifierPathDoesNotMutate` verifies task classification (feature extraction) does not mutate tool args.
- Auto mode (`tool_choice: auto`) preserves args same as explicit mode.

## 13. Retry / Fallback Safety

- `TestToolFidelityAudit_RetryFallbackIsolation`: upstream fails first attempt (500), second succeeds with valid tool call. Ensures retry does not corrupt args or leak previous failure's payload.
- Uses `doUpstreamWithRepair` path, verified no cross-attempt contamination.

## 14. Property / Fuzz Testing

- `TestToolFidelityAudit_PropertyBasedFuzz`: 12 random commands including empty, single char, special chars, 1000-char string, 216-char with spaces, etc.
- `internal/translate/fuzz_test.go` existing: fuzz for anthropic↔openai translation.
- No crash, no type corruption.

## 15. Size-Boundary Tests

Sizes: 1KB, 16KB, 64KB, 256KB, 1MB
- Command string of exact size preserved through canonical encode/decode.
- Both non-streaming (`DecodeOpenAIChatResponse`) and streaming (`DecodeOpenAIStreamChunk`) tested.

## 16. Permanent Regression Fixture

**Canonical:** `TestToolFidelity_Regression_StringMustNotBecomeUnknown`
- Exact regression for Bash.command `pwd`, `git status --short`, Read.file_path `/tmp/example.txt`
- Covers 3 paths: OpenAI→Anthropic, Anthropic→OpenAI, streaming hostile splits.

**Translate:** `TestToolFidelity_Regression_BashCommand_ReadFilePath`
- Same 3 cases through legacy translator.

**Gateway:** `TestToolFidelityAudit_Regression_BashCommand_ReadFilePath`
- Same 3 cases through full HTTP gateway with mocked upstream.

These fixtures will fail if string→unknown regression ever reintroduced.

## 17. Observability Diagnostics

`internal/protocol/canonical/validation.go`:
```go
type ToolCallValidationError struct {
  Tool string; Field string; ExpectedType string; ActualType string;
  Stage string; Protocol string; Streaming bool; Provider string; Message string
}
func (e *ToolCallValidationError) Error() string {
  return fmt.Sprintf("tool=%s field=%s expected=%s actual=%s stage=%s protocol=%s streaming=%t provider=%s: %s", ...)
}
```
- Includes tool name, field, expected vs actual type, stage (e.g., anthropic_to_canonical), protocol, streaming flag, provider.
- Example: `tool=Bash field=command expected=string actual=object stage=openai_to_canonical protocol=openai_chat streaming=false provider=openai: parameter "command" type is expected as string but provided as object`
- This matches observed InputValidationError elsewhere but with richer context.

## 18. Existing Implementation Verification

- All existing tests pass: `go test ./...` → 23 packages OK.
- Protocol matrix E2E (4 paths × 5 scenarios) PASS.
- No changes to production code required beyond test fix for unicode streaming assertion.
- Validation utility is additive, not modifying hot path, satisfying "smallest architecture-compatible fix".

## 19. Acceptance Gate A-K

| Gate | Description | Status | Evidence |
|------|-------------|--------|----------|
| A | Complete tool-call path across all protocol combos | PASS | protocol_matrix_e2e_test + fidelity tests |
| B | Schema type preservation | PASS | TestToolFidelity_SchemaTypePreservation |
| C | Streaming argument assembly hostile splits | PASS | TestToolFidelity_StreamingFragmentation (fixed) |
| D | Long-context stress | PASS | TestToolFidelity_LongContext 1KB-256KB |
| E | Multiple tool-call stress | PASS | TestToolFidelity_MultipleCalls 100 seq + mixed + parallel |
| F | Provider translation matrix | PASS | TestToolFidelity_ProviderTranslationMatrix |
| G | Canonical tool model audit | PASS | ToolCall.Arguments string, Gemini RawMessage |
| H | Never silently coerce dangerous types | PASS | ValidateToolCall rejects object/array/number |
| I | Malformed fail-closed | PASS | 11 malformed cases |
| J | Double-encoding audit | PASS | TestToolFidelity_DoubleEncoding |
| K | Unknown/any audit | PASS | typeMatches strict, no any storage |

Plus:
- Auto-mode/classifier path: PASS (TestToolFidelityAudit_ClassifierPathDoesNotMutate)
- Retry/fallback safety: PASS (TestToolFidelityAudit_RetryFallbackIsolation)
- Property/fuzz: PASS (TestToolFidelityAudit_PropertyBasedFuzz)
- Size-boundary: PASS (1KB-1MB)
- Permanent regression fixture: PASS (3 locations)
- Observability: PASS (validation.go with structured error)
- Existing impl verification: PASS (go test ./... all green)

## 20. Final Verdict

**PROTECTED**

NexaRoute does NOT exhibit the string→unknown failure. The IR design (Arguments as raw JSON string) inherently prevents coercion. Streaming reassembly occurs before validation, type checks are strict, and regression fixtures now permanently guard Bash.command and Read.file_path.

**Production hardening applied (continuation):**

- Defense-in-depth validation now **wired into production path** (`canonical_path.go`):
  - Streaming: assembled args per index, 1MB limit, raw validation on tool_end, structured error.
  - Non-streaming: `ValidateResponseBlocks` before client emit, fail-closed with diagnostic.
- No existing tests broken; all 23 packages PASS.
- Minimal change, architecture-compatible, preserves existing IR design.

**Final state:** Both proof via tests AND runtime enforcement present.

## Artifacts

- `/home/user/nexaroute/internal/protocol/canonical/validation.go` — validation + diagnostics
- `/home/user/nexaroute/internal/protocol/canonical/tool_fidelity_audit_test.go` — 19-gate audit
- `/home/user/nexaroute/internal/translate/tool_fidelity_audit_test.go` — legacy translator audit
- `/home/user/nexaroute/internal/httpapi/tool_fidelity_audit_test.go` — gateway audit (16 gates, fixed unicode case)
- Go toolchain bootstrapped at `/tmp/go-go1.23.0/bin/go`

## How to Re-run

```bash
export PATH=/tmp/go-go1.23.0/bin:$PATH
go test ./internal/protocol/canonical -run TestToolFidelity -v
go test ./internal/translate -run TestToolFidelity -v
go test ./internal/httpapi -run TestToolFidelityAudit -v
go test ./...
```
All should PASS.
