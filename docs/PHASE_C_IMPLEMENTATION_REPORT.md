# Phase C — Local Request Intelligence Foundation — Implementation Report (Converged)

Date: 2026-09-25 (convergence)
Branch: arena/01a0d825-nexaroute
Baseline: ca5e05df96766ec403dfe8cd86ba354db37429da (Phase B PASS) → 3004540 → final convergence commit
Spec sections: Request/Task Feature Extraction, Local Classifier, Observational Only, Convergence Gaps

---

## A. Final Architecture (verified)

```
Client (OpenAI / Anthropic / Responses)
  ↓ raw JSON (bounded body)
Protocol decode (core.OpenAIRequest / AnthropicRequest / ResponsesRequest via canonical IR)
  ↓
Feature Extractor (internal/feature) — single bounded parse
  - Root map[string]any unmarshal
  - Session key: session_id / metadata.session_id / metadata.user_id.session_id bounded 256
  - Reasoning: top-level keys only case-insensitive (reasoning_effort, reasoning, thinking)
  - Structured output: response_format, text.format, json_schema
  - Tools: count from root["tools"] or hint, tool_choice present/required via parseToolChoice
  - Token estimate: all strings in contentFields subtree counted chars/4 + messageCount*8 +16
  - Vision: type == visionType (image_url, image, input_image) case-insensitive, count capped 100
  - Message count: presence of "role"
  - System prompt: role system/developer or top-level system/instructions
  - Relevant text: protocol-aware collection ONLY of semantically relevant natural-language text up to 64KiB
    * OpenAI: messages[] roles system/developer/user/assistant (exclude tool/function), content string or [] parts type text/input_text/output_text
    * Anthropic: system string or text blocks, messages content string or text blocks only, exclude tool_use/tool_result/image
    * Responses: instructions string or text, input string or array items content input_text/text/output_text only, exclude input_image/tool_call
  - Lexical signals from relevant text only: code block (```), inline code (`), stack trace (traceback, stack trace, at .java/.py/.go/.js/.ts + panic goroutine), diff (diff --git, @@ @@, --- +++), file path (src/, lib/, internal/, pkg/, / + .go/.py/.js/.ts/.java/.rs), URL (http:// https://), edit/debug/repo/arch/agent/extraction keywords, code identifiers
  ↓ RequestFeatures (privacy-safe, no raw content, bounded)
Router.Requirement from features (Vision, Reasoning, Tools, EstimatedPromptTokens, BodySessionKey, MaxOutputTokens, MinContextWindow)
  ↓
Task Analyzer (internal/taskprofile) — deterministic, stateless, no locks, no global state
  - Consumes only RequestFeatures
  - Handles TooComplex → UNKNOWN
  - TaskType precedence: debugging (stack trace or debug+code) > code_edit (diff or edit+code block/identifiers or edit+file path without URL) > repository_analysis (repo+file path/code) > architecture_reasoning (arch keywords) > coding (code block/identifiers) > data_extraction (extraction) > agentic_task (agent keywords or tools+multi-turn) > tool_use (tools+required/single turn) > vision (vision without code) > structured_output (structured without other signals) > long_context (tokens>8000 without other signals) > deep_reasoning (reasoning+tokens>2000) > simple_chat (messageCount≤1, no code/tools/vision/stack/diff/file path, tokens<500) > general > unknown
  - Complexity: score = tokens + toolCount*200 + imageCount*500 + messageCount*50 + code 500 + diff 800 + stack 600 → trivial <200, low <1000, medium <4000, high <16000, very_high else
  - Confidence: base 0.5 + signals per type (0.1-0.4), penalized for TooComplex (-0.1) and truncated (-0.05), clamped 0.1-1.0, simple_chat high confidence when no signals
  - ReasonCodes: sorted, deduped, bounded
  ↓ TaskProfile with secondary requirement flags
candidatesForRequirement (unchanged, routing neutrality)
  ↓
task_classified event (privacy-safe, latency_ms with min 1 if sub-ms) + bounded metrics (15*5=75 max)
  ↓
Existing execution (failover, health, session affinity, credential pool — unchanged)
```

## B. Exact Final Structs

### RequestFeatures (internal/feature/features.go)

```go
type RequestFeatures struct {
    Protocol       Protocol
    ModelRequested string
    Streaming      bool

    HasVision             bool
    VisionImageCount      int
    HasReasoning          bool
    HasTools              bool
    ToolCount             int
    ToolChoice            bool // deprecated alias
    ToolChoicePresent     bool
    ToolChoiceRequired    bool
    StructuredOutput      bool
    HasSystemPrompt       bool
    MessageCount          int
    MaxOutputTokens       int
    HasToolResult         bool
    HasImageURL           bool

    EstimatedPromptTokens int
    EstimatedTotalTokens  int
    TotalChars            int
    RelevantTextLength    int
    RelevantTruncated     bool

    SessionKeyPresent bool
    BodySessionKey    string `json:"-"`

    TooComplex bool

    HasCodeBlock          bool
    CodeBlockCount        int
    HasInlineCode         bool
    HasStackTrace         bool
    HasDiff               bool
    HasFilePath           bool
    HasURL                bool
    HasEditKeywords       bool
    HasDebugKeywords      bool
    HasRepoKeywords       bool
    HasArchKeywords       bool
    HasAgentKeywords      bool
    HasExtractionKeywords bool
    HasCodeIdentifiers    bool
}
```

### TaskType Enum (internal/taskprofile/tasktype.go)

```go
const (
    TaskSimpleChat            TaskType = "simple_chat"
    TaskCoding                TaskType = "coding"
    TaskCodeEdit              TaskType = "code_edit"
    TaskDebugging             TaskType = "debugging"
    TaskRepositoryAnalysis    TaskType = "repository_analysis"
    TaskArchitectureReasoning TaskType = "architecture_reasoning"
    TaskDeepReasoning         TaskType = "deep_reasoning"
    TaskToolUse               TaskType = "tool_use"
    TaskAgenticTask           TaskType = "agentic_task"
    TaskLongContext           TaskType = "long_context"
    TaskVision                TaskType = "vision"
    TaskStructuredOutput      TaskType = "structured_output"
    TaskDataExtraction        TaskType = "data_extraction"
    TaskGeneral               TaskType = "general"
    TaskUnknown               TaskType = "unknown"
)
func AllTaskTypes() []TaskType { return 15 types }
```

### TaskProfile (internal/taskprofile/profile.go)

```go
type TaskProfile struct {
    Type       TaskType
    Complexity Complexity
    Confidence float64
    ReasonCodes []ReasonCode

    EstimatedContextTokens int

    RequiresVision           bool
    RequiresReasoning        bool
    RequiresTools            bool
    RequiresStructuredOutput bool
    RequiresLongContext      bool
    ToolChoiceRequired       bool

    HasVision    bool
    HasReasoning bool
    HasTools     bool
    ToolCount    int
    ImageCount   int
    MessageCount int
}
```

### Complexity & ReasonCodes

Complexity: trivial, low, medium, high, very_high
ReasonCodes: vision_present, reasoning_requested, tools_present, tool_choice_required, structured_output, code_block, inline_code, stack_trace, diff_present, file_path, url_present, edit_keyword, debug_keyword, repo_keyword, arch_keyword, agent_keyword, extraction_keyword, code_identifiers, long_context, multi_turn, single_turn, complex_tools, system_prompt, streaming, high_image_count, simple_chat

## C. File Changes (vs Phase B)

New:
- internal/feature/features.go, extractor.go, extractor_test.go, bench_test.go
- internal/taskprofile/tasktype.go, profile.go, analyzer.go, analyzer_test.go, bench_test.go
- internal/httpapi/task_intelligence.go, task_test.go
- docs/PHASE_C_CURRENT_STATE_NOTE.md, PHASE_C_REQUEST_INTELLIGENCE.md, PHASE_C_IMPLEMENTATION_REPORT.md

Modified:
- internal/httpapi/routing_helpers.go — delegates to feature extractor
- internal/httpapi/openai.go, anthropic.go, canonical_path.go — feature extraction + tool choice required detection + event emission after final resolution
- internal/httpapi/server.go — task counters init
- internal/httpapi/metrics.go — task metrics with sorted keys, cardinality 15*5=75
- internal/events/bus.go — task fields, bounded strings

Unchanged intentionally:
- internal/router, internal/route, internal/health, internal/providers, internal/config, internal/compat, internal/protocol — no task awareness

## D. Verification

### Compile & Format
- go fmt ./... PASS (fixed after convergence)
- go build ./... PASS
- go vet ./... PASS

### Unit Tests
- go test ./... -count=1 PASS (17 packages)
  - feature: 13 tests (basic, vision 3 protocols, reasoning top-level only, session, TooComplex, lexical signals, truncation, privacy, tool count, tool choice semantics 6 cases, structured output, tokens, relevant text only, false positives spec 5 cases)
  - taskprofile: 8 tests (deterministic, task types 20 cases, complexity 6, confidence, reason codes, no raw prompt, secondary requirements)
  - httpapi: task tests 11 (routing neutrality, strengthened neutrality, false positives, false positives spec 5 + URL/tool schema, cross-protocol all three, privacy canary, event emission, VE neutrality, provider neutrality 6 models, bounded metrics, performance, handler integration, failure policy)
  - existing integration tests PASS

### Routing Neutrality Evidence
- TestTaskClassification_RoutingNeutrality: identical candidate count and ordering for identical requirement
- TestTaskClassification_RoutingNeutrality_Strengthened: fixed requirement vision+reasoning+tools → identical IDs, ordering, scores before and after analysis
- grep -R "taskprofile|feature" internal/router internal/route internal/health internal/providers → no matches except unrelated "feature-x" header test, proving analyzer cannot affect failover/health/session/credentials
- TaskProfile not imported in router/route/health/providers

### False-Positive Suite
- "Can you edit this sentence?" → NOT code_edit (requires code block/identifiers or file path without URL) → PASS (simple_chat/general)
- "Tell me what an error means in statistics." → NOT debugging (requires code context for generic error) → PASS
- "Design a birthday card." → NOT architecture_reasoning (requires architecture-specific terms) → PASS
- "JSON is a data format." → NOT structured_output (flag from response_format, not lexical) → PASS
- "I saw an image yesterday." → NOT vision (type==visionType) → PASS
- Tool schema, tool result, metadata, URL, JSON schema containing misleading keywords do NOT trigger → PASS (TestExtractor_RelevantTextOnly)

### Cross-Protocol
- TestTaskClassification_CrossProtocol_AllThree: OpenAI Chat, Anthropic Messages, Responses with equivalent semantic "fix this bug\n```go\nfunc foo() {}\n```\nStack trace: panic: nil\nsrc/main.go" + instructions → all produce debugging, same code block/stack/file path flags, complexity at least medium
- Documented differences: Responses includes instructions as system prompt, message count may differ, but task type identical

### VE/Provider Neutrality
- VE neutrality: nexa-code vs different-model same content → same type
- Provider neutrality: 6 models (gpt-4, claude-3, nexa-code, custom-model-123, prov1/model-a, prov2/model-b) same content → identical type, secondary flags independent
- Classification does not depend on: model name, provider ID, deployment ID, API key, route profile, pool, health, cost/priority

### Metric Cardinality
- Previous claim 10*5=50 inconsistent because TaskType includes UNKNOWN (11) or now 15
- Fixed: AllTaskTypes()=15, AllComplexities()=5, max 75
- TestTaskClassification_BoundedMetrics uses AllTaskTypes() and AllComplexities() to compute expected max, asserts ≤75, logs 75
- Comment updated

### Analysis Duration
- LatencyMS = duration.Milliseconds() with min 1 if duration>0 and <1ms, avoiding 0ms for sub-ms, still bounded
- Benchmarks primary evidence, no high-cardinality metrics

### Benchmarks (actual)

Feature extractor (linux amd64, Xeon 2.60GHz, Go 1.23.9):
- TinyChat: 3231 ns/op, 1120 B/op, 27 allocs/op
- Coding: 6715 ns/op, 1680 B/op, 30 allocs/op
- ToolHeavy: 9473 ns/op, 5096 B/op, 89 allocs/op
- ScanLimit 70KiB: 676122 ns/op, 140368 B/op, 27 allocs/op

TaskProfile analyzer:
- SimpleChat: 433.9 ns/op, 344 B/op, 4 allocs/op
- Coding: 465.2 ns/op, 360 B/op, 4 allocs/op
- ToolHeavy: 640.1 ns/op, 392 B/op, 4 allocs/op
- Complex: 3017 ns/op, 1848 B/op, 6 allocs/op

Total typical: ~4-8 µs per request.

### Privacy
- Canary SECRET_CANARY_7f31a9 placed in request → NOT in features JSON, profile JSON, task_classified event JSON, metrics keys, message fields
- BodySessionKey json:"-" and not leaked via telemetry (verified)

### Failure Policy
- TooComplex → UNKNOWN confidence 0.1, routing continues
- Empty/invalid features → GENERAL/SIMPLE_CHAT/UNKNOWN, Valid()
- Invalid JSON → existing 400, not 500 (TestAnalyzer_FailurePolicy)

### Full Mandatory Gates
- ./scripts/verify.sh PASS (60s: go version, shell syntax, formatting, unit/integration count=10 shuffle, vet, race count=3 shuffle, js syntax, fuzz 2s, linux amd64/arm64 builds)
- ./scripts/stress.sh PASS (router scale, probe/recovery, event-state, admission, log rotation)
- ./scripts/smoke-local.sh PASS (UI 200, hello 200, model list, admin snapshot, count_tokens fallback, provider CRUD, atomic persistence)
- Race targeted: go test -race ./internal/feature ./internal/taskprofile ./internal/httpapi ./internal/events ./internal/router PASS

## E. Acceptance Checklist

- [x] Task vocabulary matches intended semantic contract (15 types, distinguishes CODE_EDIT, REPOSITORY_ANALYSIS, ARCHITECTURE_REASONING, DATA_EXTRACTION, AGENTIC_TASK, TOOL_USE, SIMPLE_CHAT, DEEP_REASONING/LONG_CONTEXT/STRUCTURED_OUTPUT as secondary flags + primary when dominant)
- [x] Important secondary requirements remain in TaskProfile (RequiresVision, RequiresReasoning, RequiresTools, RequiresStructuredOutput, RequiresLongContext, ToolChoiceRequired, EstimatedContextTokens)
- [x] Tool-choice semantics normalized where reliable (ToolChoicePresent, ToolChoiceRequired, parsed for OpenAI auto/none/required/function and Anthropic auto/any/tool)
- [x] Lexical analysis scans relevant text only (protocol-aware, excludes tool schemas, tool results, image URLs, metadata, JSON schemas)
- [x] Explicit false-positive cases pass (5 spec cases + tool schema/result/URL/JSON schema)
- [x] OpenAI + Anthropic + Responses cross-protocol fixtures pass (identical primary type, equivalent flags)
- [x] VE/provider/model identity does not affect classification (VE neutrality + provider neutrality tests)
- [x] Routing candidate order remains unchanged (neutrality + strengthened tests)
- [x] Analyzer cannot affect failover/health/session/credentials (grep evidence, no imports)
- [x] Metric cardinality claim is correct (15*5=75, derived from AllTaskTypes())
- [x] Raw secret canary does not leak (SECRET_CANARY_7f31a9)
- [x] Actual benchmarks executed (ns/op, B/op, allocs/op reported)
- [x] ./scripts/verify.sh PASS
- [x] ./scripts/stress.sh PASS
- [x] ./scripts/smoke-local.sh PASS
- [x] Targeted race tests PASS
- [x] Documentation exactly matches code (final structs, enums, precedence, complexity, confidence, lexical semantics, privacy, cross-protocol, neutrality, benchmarks, verification)

## F. Remaining Limitations

- Heuristic substring search, not NLP; edge false positives/negatives possible but guarded by false-positive suite
- File path detection simple, may miss exotic paths
- Tool choice required detection best-effort where reliable, ambiguous cases treated as present not required
- Long context and structured output as primary only when dominant; otherwise secondary flags to avoid combinatorial explosion — documented as intentional
- No session affinity or VE info used (by design)

## G. Final Verdict

PHASE C: PASS
