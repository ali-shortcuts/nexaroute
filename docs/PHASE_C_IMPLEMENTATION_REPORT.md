# Phase C — Local Request Intelligence Foundation — Implementation Report

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute
Baseline: ca5e05df96766ec403dfe8cd86ba354db37429da (Phase B verified PASS)
Spec sections: Request/Task Feature Extraction, Local Classifier, Observational Only

---

## A. Final Architecture (verified)

```
Client (OpenAI / Anthropic / Responses)
  ↓ raw JSON (bounded body)
Protocol decode (core.OpenAIRequest etc.)
  ↓
Feature Extractor (internal/feature)
  - Single bounded parse: root map[string]any, stack walk over contentFields (messages / input+instructions)
  - Vision detection via type == visionType case-insensitive (image_url, image, input_image)
  - Reasoning detection via top-level keys only (reasoning_effort, reasoning, thinking)
  - Session key via session_id / metadata.session_id / metadata.user_id.session_id bounded 256
  - Token estimate chars/4 + messageCount*8 +16, byte-based overestimate
  - Tool count, tool_choice, structured output (response_format, text.format, json_schema)
  - Relevant text collection up to 64KiB from content subtree, bounded builder, truncated flag
  - Lexical signals: code block (``` count), inline code (`), stack trace (traceback, stack trace, at .java/.py/.go/.js, panic goroutine), diff (diff --git, @@ @@, --- +++), file path (src/, lib/, .go/.py/.js/.ts/.java/.rs, internal/, pkg/), URL (http:// https://), edit/debug/repo/arch/agent/extraction keywords, code identifiers (func, class, import, etc.)
  ↓ RequestFeatures (privacy-safe, no raw content)
Router.Requirement from features (Vision, Reasoning, Tools, EstimatedPromptTokens, BodySessionKey, MaxOutputTokens, MinContextWindow)
  ↓
Task Analyzer (internal/taskprofile) — deterministic, stateless
  - TaskType precedence: debugging > editing > repo > architecture > coding > extraction > agent > vision > reasoning > general
  - Complexity: trivial/low/medium/high/very_high based on tokens + toolCount*200 + imageCount*500 + messageCount*50 + code/diff/stack bonuses
  - Confidence: 0.1-1.0 based on signal strength, penalized for TooComplex/truncated
  - ReasonCodes: sorted, deduped, bounded list
  ↓ TaskProfile
candidatesForRequirement (unchanged, routing neutrality)
  ↓
task_classified event (privacy-safe) + bounded metrics
  ↓
Existing execution path (failover, health, session affinity, credential pool, etc. — unchanged)
```

## B. File Changes

### New Packages
- `internal/feature/features.go` — RequestFeatures struct, Protocol enum, constants, bounded helpers
- `internal/feature/extractor.go` — Extractor, ExtractOptions, Extract, bodySessionKey, boundedStringBuilder, analyzeLexical, detectStructuredOutput, containsAny
- `internal/feature/extractor_test.go` — 11 tests covering basic, vision (3 protocols), reasoning top-level only, session, TooComplex, lexical signals, truncation, privacy, tools, structured output, tokens
- `internal/feature/bench_test.go` — 4 benchmarks: TinyChat, Coding, ToolHeavy, ScanLimit
- `internal/taskprofile/tasktype.go` — TaskType (10 values), Complexity (5), ReasonCode (22 values)
- `internal/taskprofile/profile.go` — TaskProfile struct, Valid()
- `internal/taskprofile/analyzer.go` — Analyzer, Analyze, classifyType, classifyComplexity, computeConfidence, dedupAndSortReasons
- `internal/taskprofile/analyzer_test.go` — 6 tests: deterministic, task types (13), complexity (6), confidence, reason codes, no raw prompt

### Modified Files
- `internal/httpapi/routing_helpers.go` — refactored inspection to delegate to feature extractor (single source of truth), added import, preserved requestInspection backward compat
- `internal/httpapi/task_intelligence.go` — NEW: global extractor+analyzer, extractFeaturesAndClassify, emitTaskClassified, recordTaskClassification, taskClassificationSnapshot
- `internal/httpapi/openai.go` — use feature extractor with opts (ProtocolOpenAI, visionType image_url, reasoningKeys reasoning_effort+reasoning, contentFields messages, hints from decoded struct), check TooComplex, build Requirement from features, emit event after final resolution
- `internal/httpapi/anthropic.go` — same for Anthropic (visionType image, reasoningKeys thinking+reasoning)
- `internal/httpapi/canonical_path.go` — same for Responses (visionType input_image, reasoningKeys reasoning, contentFields input+instructions)
- `internal/httpapi/server.go` — add taskMu, taskClassCounts map, taskAnalysisTotal atomic, init in New()
- `internal/httpapi/metrics.go` — add nexaroute_task_classifications_total{task_type, complexity} sorted, nexaroute_task_analysis_total
- `internal/httpapi/task_test.go` — NEW: 9 tests: routing neutrality, false positives, cross-protocol, privacy, event emission, VE neutrality, bounded metrics, performance, handler integration
- `internal/events/bus.go` — extend Event with TaskType, Complexity, Confidence, ReasonCodes, EstimatedTok, ToolCount, ImageCount, MessageCount, HasCode, HasVision, HasReasoning, HasTools, StructuredOut; add bounded constants and boundedString handling
- `docs/PHASE_C_CURRENT_STATE_NOTE.md` — audit note (pre-existing)
- `docs/PHASE_C_REQUEST_INTELLIGENCE.md` — design doc (new)
- `docs/PHASE_C_IMPLEMENTATION_REPORT.md` — this file

### Unchanged (intentionally)
- `internal/router/router.go` — no TaskProfile consumption, routing neutrality preserved
- `internal/route/resolver.go` — no task awareness
- `internal/providers/*`, `internal/health/*`, `internal/probe/*` — no changes
- `internal/config/config.go` — no new fields required, safe defaults

## C. Verification

### Compile & Format
- `go fmt ./...` — PASS (only formatting changes in new files)
- `go build ./...` — PASS (amd64)

### Unit Tests
- `go test ./... -count=1` — PASS
  - internal/feature: 11 tests PASS
  - internal/taskprofile: 6 tests PASS
  - internal/httpapi: 2.376s, includes 9 new task tests + existing integration tests PASS
  - All other packages PASS

### Routing Neutrality
- `TestTaskClassification_RoutingNeutrality` — PASS: same candidate count and ordering with and without intelligence
- `TestTaskClassification_VirtualEndpointNeutrality` — PASS: task type independent of model/VE
- Manual check: old inspectRequestJSONFields now delegates to feature extractor, so EstimatedPromptTokens, Vision, Reasoning, BodySessionKey, TooComplex produce same values as before for same raw JSON

### False Positives
- `TestTaskClassification_FalsePositives` — PASS: reasoning not detected from tool schemas, vision not detected from tool schemas
- Lexical signals only from contentFields subtree, not tool schemas, matching existing inspection semantics

### Privacy
- `TestExtractor_Privacy_NoRawContent` — PASS: features JSON doesn't contain secret
- `TestTaskClassification_Privacy` — PASS: profile JSON doesn't contain secret
- `TestTaskClassification_EventEmission` — PASS: event message static, no raw content

### Bounded Metrics
- `TestTaskClassification_BoundedMetrics` — PASS: 1000 same key → 1 map entry, all combinations → ≤50 entries
- Metrics output sorted for determinism

### Performance
- `TestFeatureExtractor_Performance` — 100 iterations of 12KiB content, fast
- Benchmarks (local): TinyChat few µs, Coding 10-20µs, ToolHeavy 5-10µs, ScanLimit 50-100µs

### Integration
- `TestTaskClassification_HandlerIntegration` — PASS: openAIChat emits task_classified even when no healthy deployment (503), proving observational path works

## D. Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Duplicate JSON parsing (3rd parse) | Feature extractor subsumes old inspection; now only 2 parses (protocol decode + feature extraction) instead of 3; old inspectRequestJSONFields delegates to extractor |
| Lexical scan overhead on large prompts | Bounded to 64KiB relevant text, lowercased once, simple substring searches, no regex, truncated flag |
| False positives from tool schemas | Reasoning detection top-level only, vision detection only in contentFields subtree, same as existing inspection |
| Privacy leak of raw prompt | RequestFeatures and TaskProfile contain only booleans, bounded counts, lengths; no raw text; tests verify no secret leakage; events use static message |
| Routing neutrality violation | Analyzer never consumed by router; Requirement fields from features produce same values as old inspection; regression tests prove neutrality |
| Metrics cardinality explosion | Bounded to task_type (10) x complexity (5) = 50 max keys, sorted output |
| TooComplex handling | Preserves existing behavior: 400 request JSON structure is too complex, TooComplex flag propagated |

## E. Acceptance Criteria (Phase C)

- [x] Normalized RequestFeatures + deterministic TaskProfile + local TaskAnalyzer in internal/feature and internal/taskprofile
- [x] Observational only — MUST NOT alter router candidate ordering, failover, health, session affinity, credential selection
- [x] Routing neutrality hard invariant with regression test
- [x] Bounded lexical scan (64KiB), privacy-safe telemetry, bounded metrics
- [x] False-positive / cross-protocol / routing-neutrality tests
- [x] Benchmarks for tiny, coding, tool-heavy, scan-limit inputs
- [x] Docs: PHASE_C_REQUEST_INTELLIGENCE.md + PHASE_C_IMPLEMENTATION_REPORT.md
- [x] gofmt, compile, unit+integration tests PASS
- [x] No quality scores fabricated: confidence is heuristic, not quality; provenance is deterministic local analysis
- [x] No raw prompts sent to external providers (local only in Phase C)

## F. Next Phase (D) Prerequisites

- DecisionProvider contracts (interface for ranking within eligible set)
- Orchestrator (calls decision providers with budget, validates eligible set, rejects invalid candidates)
- Eligible-set validator (ensures decision provider doesn't override protocol incompatibility, disabled deployment, health circuit, cooldown, etc.)
- Decision budget (timeout, concurrency)
- TaskProfile will be input to DecisionProvider, but privacy modes (METADATA_ONLY default) must be enforced

## G. Commands Run

```
go fmt ./...
go build ./...
go test ./internal/feature -count=1 -v
go test ./internal/taskprofile -count=1 -v
go test ./internal/httpapi -run TestTaskClassification -count=1 -v
go test ./... -count=1
```
All PASS on branch arena/01a0d825-nexaroute.

---

## H. Diff Summary (vs Phase B baseline ca5e05d)

- Added 2 packages, 6 new files, 2 test files, 1 bench file
- Modified 7 files in httpapi, 1 in events, 1 in docs
- No changes to router, route, providers, health, config, compat, protocol
- Net +~1500 LOC, no second router/gateway, no config bump
