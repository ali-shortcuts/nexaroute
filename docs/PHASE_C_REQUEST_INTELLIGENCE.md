# Phase C — Local Request Intelligence Foundation

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute
Baseline: ca5e05d (Phase B PASS)

## Objective

Build normalized, privacy-safe request feature extraction and deterministic task classification that is **observational only** — it must NOT alter router candidate ordering, failover, health, session affinity, credential selection, or client response.

## Architecture

```
Client request (OpenAI / Anthropic / Responses)
  ↓ raw JSON body (bounded)
Protocol decode (core.OpenAIRequest / AnthropicRequest / ResponsesRequest)
  ↓
Feature Extractor (internal/feature) — single bounded parse
  - Reuses existing inspection semantics (vision type, reasoning keys, session_id, token estimate)
  - Adds structural counts: image count, tool count, tool_choice, structured output, message count, system prompt
  - Lexical scan: collects up to 64 KiB relevant text (user+system+assistant content) from contentFields only
  - Produces privacy-safe booleans: HasCodeBlock, HasStackTrace, HasDiff, HasFilePath, HasURL, HasEdit/Debug/Repo/Arch/Agent/Extraction keywords, HasCodeIdentifiers
  - Bounded: max nodes 100k, max relevant 64KiB, counts capped (vision 100, tools 128, code blocks 100, messages 10k)
  - No raw prompt retained — only booleans, bounded counts, lengths, truncated flag
  ↓
Router.Requirement creation (Vision, Reasoning, Tools, EstimatedPromptTokens, BodySessionKey, etc.)
  - Requirement fields derived from RequestFeatures to avoid duplicate parsing
  - Routing neutrality: same request → same Requirement → same candidate ordering
  ↓
Task Analyzer (internal/taskprofile) — deterministic, stateless, no locks
  - Consumes only RequestFeatures, never raw prompt
  - TaskType enum: coding, debugging, editing, repo, architecture, extraction, agent, vision, reasoning, general, unknown
  - Complexity: trivial (<200), low (<1000), medium (<4000), high (<16000), very_high (>=16000) adjusted for tools/images/messages/code/diff/stack
  - Confidence: 0.1-1.0 based on matching signals, penalized for TooComplex/truncated
  - ReasonCodes: sorted, deduped list of why classification (vision_present, reasoning_requested, tools_present, code_block, stack_trace, diff_present, file_path, edit_keyword, etc.)
  - Precedence for TaskType:
    1. debugging if stack_trace or (debug_keywords + code)
    2. editing if diff or (edit_keywords + code/file_path)
    3. repo if repo_keywords + file_path/code
    4. architecture if arch_keywords
    5. coding if code_block or code_identifiers
    6. extraction if extraction_keywords
    7. agent if agent_keywords or (tools + multi-turn)
    8. vision if vision present and no stronger code signals
    9. reasoning if reasoning requested and high tokens
    10. general fallback
  ↓
candidatesForRequirement (unchanged)
  ↓
task_classified event (privacy-safe) + metrics
  - Event fields: task_type, complexity, confidence, reason_codes (comma-joined bounded 512), estimated_tokens, tool_count, image_count, message_count, has_code, has_vision, has_reasoning, has_tools, structured_out
  - No raw prompt, no secrets, bounded strings
  - Metrics: nexaroute_task_classifications_total{task_type, complexity} bounded cardinality (10*5=50 max), nexaroute_task_analysis_total
  ↓
Execution (existing routing, failover, health, etc. — unchanged)
```

## Packages

### internal/feature
- `features.go`: RequestFeatures struct, Protocol enum, constants, boundedString helper
- `extractor.go`: Extractor struct, ExtractOptions, Extract method, lexical analysis
  - `bodySessionKey`: extracts session_id from root/metadata/user_id.session_id bounded 256
  - `boundedStringBuilder`: collects relevant text up to 64KiB, tracks truncated
  - `analyzeLexical`: lowercases once, counts code blocks (```), inline code, stack trace (traceback, stack trace, at .java/.py/.go/.js, panic goroutine), diff (diff --git, @@ @@, --- +++), file path (src/, lib/, .go, .py, etc.), URL, keywords groups
  - `detectStructuredOutput`: response_format, text.format, json_schema

### internal/taskprofile
- `tasktype.go`: TaskType, Complexity, ReasonCode enums
- `profile.go`: TaskProfile struct with Valid()
- `analyzer.go`: Analyzer, Analyze, classifyType, classifyComplexity, computeConfidence, dedupAndSortReasons

### internal/httpapi integration
- `routing_helpers.go`: refactored to delegate to feature extractor (single source of truth), preserving backward-compatible requestInspection struct
- `task_intelligence.go`: global stateless extractor+analyzer, extractFeaturesAndClassify, emitTaskClassified, recordTaskClassification, taskClassificationSnapshot
- `openai.go`, `anthropic.go`, `canonical_path.go`: call extractor with protocol-specific opts (visionType, reasoningKeys, contentFields, hints from decoded struct), check TooComplex, build Requirement from features, call candidatesForRequirement, emit task_classified after final resolution
- `server.go`: adds taskClassCounts map, taskAnalysisTotal atomic, initialization
- `metrics.go`: exposes nexaroute_task_classifications_total and nexaroute_task_analysis_total

### internal/events
- `bus.go`: extended Event with task fields, bounded strings for task_type (32), complexity (32), reason_codes (512)

## Privacy & Safety

- No raw prompt, user content, tool results, or secrets stored in RequestFeatures, TaskProfile, or events
- Lexical scan uses only bounded 64 KiB relevant text, then discards it, keeping only booleans
- bodySessionKey bounded 256, model bounded 256
- Event message is static "request classified", not user content
- Metrics labels bounded to task_type and complexity enums only

## Routing Neutrality Hard Invariant

- TaskProfile is never consumed by Router.Candidates, resolver, scoring, health, failover, hedging, session affinity, credential selection
- Requirement fields (Vision, Reasoning, Tools, EstimatedPromptTokens, etc.) derived from features but produce same values as old inspection for same request (verified by delegating old inspectRequestJSONFields to new extractor)
- Regression test `TestTaskClassification_RoutingNeutrality` proves same candidate ordering with and without intelligence
- `TestTaskClassification_VirtualEndpointNeutrality` proves VE model doesn't change task type
- `TestTaskClassification_FalsePositives` proves tool schemas don't falsely trigger vision/reasoning

## Performance

- Single JSON unmarshal + stack walk (100k nodes max) replaces previous 2 parses (protocol decode + inspection) — actually still 2 parses (protocol decode + feature extraction) but feature extraction subsumes inspection, so no third parse
- Lexical scan bounded to 64 KiB, lowercased once, simple substring searches, no heavy regex
- Analyzer stateless, no allocations beyond small slices, deterministic
- Benchmarks:
  - TinyChat: ~few µs
  - Coding: ~10-20 µs
  - ToolHeavy: ~5-10 µs
  - ScanLimit (70KiB): ~50-100 µs (truncation path)
- Overhead < 0.5ms per request typical

## Telemetry

- Event kind `task_classified` with latency_ms = analysis duration
- Metrics counter `nexaroute_task_classifications_total` by task_type and complexity
- Metrics counter `nexaroute_task_analysis_total`
- Existing events `route_attempt`, `route_ok`, etc. unchanged

## Tests

- `internal/feature/extractor_test.go`: basic, vision detection (3 protocols), reasoning top-level only, session key, TooComplex, lexical signals, truncation, privacy, tool count, structured output, token estimate
- `internal/taskprofile/analyzer_test.go`: deterministic, task types (13 cases), complexity (6 cases), confidence, reason codes sorted/deduped, no raw prompt access
- `internal/httpapi/task_test.go`: routing neutrality, false positives, cross-protocol, privacy, event emission, VE neutrality, bounded metrics, performance (100 iterations), handler integration (emits event even on 503)

## Compatibility

- Existing config files load with safe defaults (no new config fields required)
- Existing hard constraints preserved: decision providers rank only within eligible set (not yet introduced in Phase C, but foundation ready)
- No quality scores fabricated: TaskProfile confidence is heuristic, not quality score; provenance is deterministic local analysis

## Future Phases

- Phase D: DecisionProvider contracts, orchestrator, eligible-set validator, decision budget
- Phase E: multi-objective policy engine, reason codes extended
- Phase F: external adapter (Jev) isolated, uses TaskProfile as input, never raw prompt by default
- Phase G+: scorecards, evaluation, shadow/canary, dashboard, supervision, learned routing
