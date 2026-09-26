# Phase C — Local Request Intelligence Foundation — Converged

Date: 2026-09-25 (convergence)
Branch: arena/01a0d825-nexaroute
Baseline: ca5e05d (Phase B PASS) → 3004540 → final convergence

## Objective

Build normalized, privacy-safe request feature extraction and deterministic task classification that is observational only — must NOT alter router candidate ordering, failover, health, session affinity, credential selection, or client response.

## Final Task Vocabulary (15 types)

Spec requested: SIMPLE_CHAT, CODING, CODE_EDIT, DEBUGGING, REPOSITORY_ANALYSIS, ARCHITECTURE_REASONING, DEEP_REASONING, TOOL_USE, AGENTIC_TASK, LONG_CONTEXT, VISION, STRUCTURED_OUTPUT, DATA_EXTRACTION, GENERAL, UNKNOWN

Implemented as:

```go
const (
    TaskSimpleChat           TaskType = "simple_chat"
    TaskCoding               TaskType = "coding"
    TaskCodeEdit             TaskType = "code_edit"
    TaskDebugging            TaskType = "debugging"
    TaskRepositoryAnalysis   TaskType = "repository_analysis"
    TaskArchitectureReasoning TaskType = "architecture_reasoning"
    TaskDeepReasoning        TaskType = "deep_reasoning"
    TaskToolUse              TaskType = "tool_use"
    TaskAgenticTask          TaskType = "agentic_task"
    TaskLongContext          TaskType = "long_context"
    TaskVision               TaskType = "vision"
    TaskStructuredOutput     TaskType = "structured_output"
    TaskDataExtraction       TaskType = "data_extraction"
    TaskGeneral              TaskType = "general"
    TaskUnknown              TaskType = "unknown"
)
```

Backward compat aliases kept:
- `TaskEditing = TaskCodeEdit`
- `TaskRepo = TaskRepositoryAnalysis`
- `TaskArchitecture = TaskArchitectureReasoning`
- `TaskExtraction = TaskDataExtraction`
- `TaskAgent = TaskAgenticTask`
- `TaskReasoning = TaskDeepReasoning`

**Distinctions ensured:**
- CODE_EDIT vs CODING: diff OR (edit keywords + code block/identifiers) OR (edit keywords + file path without URL) → code_edit; code block alone → coding
- REPOSITORY_ANALYSIS vs CODING: repo keywords + file path/code → repository_analysis
- ARCHITECTURE_REASONING: arch keywords (architecture, system design, component, diagram, etc.) — "Design a birthday card" does NOT trigger (requires architecture-specific terms)
- DATA_EXTRACTION: extraction keywords (extract, summarize, parse, key points, bullet points, json output/table as output request)
- AGENTIC_TASK: agent keywords (agent, tool use, autonomous, multi-step, plan and execute) OR tools + multi-turn (>3 messages)
- TOOL_USE: tools present + (tool_choice required OR tool count>0) and not agentic (single turn with tools → tool_use)
- SIMPLE_CHAT vs GENERAL: simple_chat when messageCount≤1, no code/tools/vision/stack/diff/file path, tokens<500; general fallback otherwise
- DEEP_REASONING / LONG_CONTEXT / STRUCTURED_OUTPUT:
  - DEEP_REASONING as primary when reasoning requested + tokens>2000 and no stronger type
  - LONG_CONTEXT and STRUCTURED_OUTPUT as secondary requirement flags (RequiresLongContext, RequiresStructuredOutput) that are orthogonal and can co-occur with any primary type. They are also primary types only when dominant and no other strong signals, to avoid false precision and combinatorial explosion. Documented as secondary flags in TaskProfile.

UNKNOWN remains valid for TooComplex or invalid.

## Final RequestFeatures

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

Bounds: model 256, session 256, relevant 64KiB, vision 100, tools 128, code blocks 100, messages 10k, total chars 10M, nodes 100k.

**Tool choice semantics:**
- ToolChoicePresent: tool_choice field exists (any value)
- ToolChoiceRequired: tool_choice forces a call — OpenAI "required" or forced function, Anthropic "any"/"tool"/"required", Responses same via canonical NeedsTool
- Parsed via parseToolChoice(root["tool_choice"]): string "required"/"any"/"tool" => required true; object with type "tool"/"any"/"required"/"function" or has name/function => required true; "auto"/"none" => present true, required false

## Final TaskProfile

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

Secondary requirements needed by DecisionProvider phases are retained: RequiresTools, RequiresVision, RequiresReasoning, RequiresStructuredOutput, RequiresLongContext, ToolChoiceRequired, EstimatedContextTokens.

No provider/model recommendations — request only.

## Lexical Scanning — Relevant Text Only

**Previous behavior:** collected every string in contentFields subtree (including tool results, image URLs, metadata, JSON).

**Converged behavior:** protocol-aware collection of only semantically relevant natural-language text:

- OpenAI Chat:
  - Top-level system/instructions if string
  - messages[]: include roles system/developer/user/assistant (exclude tool/function)
  - content string → collect
  - content []: collect only type=="text" → text, type=="input_text"/"output_text" → text
  - Exclude image_url, image, tool_use, tool_result, function, tool_calls[].arguments

- Anthropic:
  - system string or array of type=="text" blocks
  - messages[] content string or array: only type=="text" blocks
  - Exclude tool_use, tool_result, image

- Responses:
  - instructions string or array text
  - input string or array of items: each item content array only type=="input_text"/"text"/"output_text"
  - Exclude input_image, tool_call, tool_result

Preserves existing vision/reasoning/token-count behavior (token counting still uses all strings for conservative overestimate; vision detection via type==visionType).

**Tests proving non-relevant exclusion:**
- Keywords inside tool schema (description) do NOT trigger code/debug/arch/extraction/agent/file path
- Keywords inside tool result (role tool) do NOT trigger
- Keywords inside image_url URL do NOT trigger
- Anthropic tool_result block does NOT trigger

## False-Positive Suite (spec explicit)

1. "Can you edit this sentence?" → NOT code_edit
   - HasEditKeywords requires code context (code block/file path/diff) for generic "edit"; even "edit this" phrase requires code block/identifiers or file path without URL. Result: simple_chat or general.

2. "Tell me what an error means in statistics." → NOT debugging
   - HasDebugKeywords now requires code context (code block/file path/stack) for generic "error"; without, only "debug this"/"fix the bug"/"stack trace"/"traceback"/"panic"/"crash" trigger. Result: simple_chat/general.

3. "Design a birthday card." → NOT architecture_reasoning
   - Arch keywords require architecture, system design, component, diagram, etc.; "design" alone insufficient. Result: general/simple_chat.

4. "JSON is a data format." → NOT structured_output merely from sentence
   - StructuredOutput flag from response_format, not lexical. Extraction keywords require action words. Result: general.

5. "I saw an image yesterday." → NOT vision
   - Vision from type==visionType, not lexical "image". Result: general.

Additional negative cases: tool schema, tool result, metadata, URL, JSON schema containing misleading keywords do NOT trigger classification (see TestExtractor_RelevantTextOnly).

## Cross-Protocol — All Three

Test `TestTaskClassification_CrossProtocol_AllThree` uses equivalent semantic request:

- OpenAI: messages user content "fix this bug\n```go\nfunc foo() {}\n```\nStack trace: panic: nil\nsrc/main.go"
- Anthropic: messages content type text same
- Responses: instructions "You are a coding assistant", input user content input_text same

Results:
- Primary TaskType identical: debugging (stack trace) across all three
- Capability flags equivalent: HasCodeBlock true, HasStackTrace true, HasFilePath true across all
- Complexity similar: at least medium/high (not trivial)
- Documented differences: Responses includes instructions as system prompt, so HasSystemPrompt true and message count may differ slightly, but task type unchanged

## Virtual Endpoint + Provider Neutrality

- `TestTaskClassification_VirtualEndpointNeutrality`: same content with model "nexa-code" vs "different-model" → same task type
- `TestTaskClassification_ProviderNeutrality`: same content with 6 different model names (gpt-4, claude-3, nexa-code, custom-model-123, prov1/model-a, prov2/model-b) → identical task type, secondary flags independent of model
- Classification does not depend on: physical model name, provider ID, deployment ID, client API key, route profile, candidate pool, health state, cost/priority — request semantics only
- Evidence: feature extractor uses only raw JSON body + protocol opts, no config; analyzer uses only RequestFeatures; no imports of taskprofile/feature in router/route/health/providers (verified via grep)

## Routing Neutrality — Strengthened

- `TestTaskClassification_RoutingNeutrality`: same requirement before/after analysis → identical candidate count and ordering
- `TestTaskClassification_RoutingNeutrality_Strengthened`: fixed requirement with vision+reasoning+tools → identical IDs, ordering, scores before and after analysis
- Confirm TaskProfile not imported/consumed by:
  - internal/router: grep shows no taskprofile/feature import
  - internal/route: no import
  - health: no import
  - provider selection, credential selection, hedging: no import, verified via `grep -R "taskprofile\|feature" internal/router internal/route internal/health internal/providers`
- TaskProfile is observational only, emitted after candidatesForRequirement, never used for candidate filtering

## Metric Cardinality Fix

Previous claim: 10 types × 5 complexities = 50, but TaskType includes UNKNOWN (11 types).

Final: 15 types × 5 complexities = 75 max keys (derived from AllTaskTypes() and AllComplexities()).

Fixed in:
- `task_intelligence.go` comment: "Bounded cardinality: task types (len(AllTaskTypes)=15) x complexities (5) = 75 max keys"
- `task_test.go`: uses `AllTaskTypes()` and `AllComplexities()` to compute expected max, asserts ≤75, logs expected max
- Docs updated

## Analysis Duration

Event stores `LatencyMS = duration.Milliseconds()` with fix: if 0 and duration>0, store 1 to avoid 0ms for sub-ms classifications. Still bounded. Benchmarks remain primary performance evidence. No high-cardinality metrics added.

## Benchmarks (actual)

Machine: Intel Xeon 2.60GHz, linux amd64, Go 1.23.9

Feature extractor:
- TinyChat: 3231 ns/op, 1120 B/op, 27 allocs/op
- Coding: 6715 ns/op, 1680 B/op, 30 allocs/op
- ToolHeavy: 9473 ns/op, 5096 B/op, 89 allocs/op
- ScanLimit (70KiB): 676122 ns/op, 140368 B/op, 27 allocs/op

TaskProfile analyzer:
- SimpleChat: 433.9 ns/op, 344 B/op, 4 allocs/op
- Coding: 465.2 ns/op, 360 B/op, 4 allocs/op
- ToolHeavy: 640.1 ns/op, 392 B/op, 4 allocs/op
- Complex: 3017 ns/op, 1848 B/op, 6 allocs/op

Total typical overhead: ~3-7 µs + ~0.4-0.6 µs = ~4-8 µs per request, <0.5 ms.

## Privacy

- No raw prompt, user content, tool results, secrets in RequestFeatures, TaskProfile, events
- Lexical scan uses only 64KiB relevant text, then discards, keeping booleans
- BodySessionKey json:"-" and never appears in task telemetry (verified)
- Secret canary SECRET_CANARY_7f31a9 tested: does NOT appear in features serialized, profile, task_classified event, metrics, message fields

## Analyzer Failure Policy

- TooComplex → UNKNOWN with confidence 0.1, routing continues
- Empty/invalid features → GENERAL or SIMPLE_CHAT or UNKNOWN, never panic
- Invalid client JSON/protocol → existing protocol errors (400) retained, analyzer never turns 400 into 500
- Tested via `TestAnalyzer_FailurePolicy`

## Verification

- `./scripts/verify.sh` PASS (go version, shell syntax, formatting, unit/integration tests count=10 shuffle, vet, race count=3 shuffle, js syntax, fuzz 2s, linux amd64/arm64 builds)
- `./scripts/stress.sh` PASS (router scale, probe/recovery, event-state, admission, log rotation)
- `./scripts/smoke-local.sh` PASS (UI 200, hello 200, model list, admin snapshot, count_tokens fallback, provider CRUD, backup-free persistence)
- Race: `go test -race ./internal/feature ./internal/taskprofile ./internal/httpapi ./internal/events ./internal/router` PASS

## Remaining Limitations

- Lexical analysis is heuristic substring search, not NLP; may still have edge false positives/negatives, but false-positive suite guards critical cases
- File path detection is simple (src/, lib/, .go etc.) and may miss exotic paths, but bounded and privacy-safe
- Tool choice required detection relies on top-level tool_choice field; some providers may use different casing or nested structures, but present vs required distinction is best-effort where reliable
- Long context and structured output as primary types only when dominant; otherwise secondary flags — documented as intentional to avoid combinatorial explosion
- Analyzer does not yet use session affinity or virtual endpoint info (by design, request semantics only)

## Final Verdict

PHASE C: PASS
