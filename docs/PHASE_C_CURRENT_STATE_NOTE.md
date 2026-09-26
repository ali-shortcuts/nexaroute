# Phase C Current-State Note — Request Feature Extraction

Date: 2026-09-25
Branch: arena/01a0d825-nexaroute (Phase B baseline ca5e05d)

## Existing feature extraction

- `internal/httpapi/routing_helpers.go`:
  - `requestInspection` struct: Vision bool, Reasoning bool, TooComplex bool, BodySessionKey string, EstimatedPromptTokens int
  - `inspectRequestJSONFields(raw, visionType, reasoningKeys, contentFields)`:
    - Parses body via `json.Unmarshal` into `map[string]any`, walks stack of relevant fields only (e.g. `messages` for chat, `input`+`instructions` for Responses)
    - Vision detection: looks for `type == visionType` case-insensitive (`image_url` for OpenAI chat, `image` for Anthropic, `input_image` for Responses)
    - Reasoning detection: top-level keys equalFold reasoningKeys (`reasoning_effort`, `reasoning` for OpenAI; `thinking`, `reasoning` for Anthropic; `reasoning` for Responses)
    - Token estimate: chars/4 + messageCount*8 +16, byte-based overestimate
    - Session extraction: `session_id` top-level, or `metadata.session_id`, or `metadata.user_id.session_id`, bounded 256
    - Node budget: maxRequestInspectionNodes 100k, marks TooComplex if exceeded
  - `sessionKeyFromRequestParts` prefers headers `x-claude-code-session-id`, `x-litellm-session-id`, `x-litellm-trace-id`, `x-session-id` over body key
  - `prepareRequirement`: sets SessionKey, SelectionKey (x-request-id), LoadForProvider hook with quota pressure

- Ingress handlers:
  - `openai.go`: `inspectRequestJSON(raw, "image_url", [reasoning_effort, reasoning])` → Requirement{Model, Tools=len(Tools)>0, Vision, Streaming, Reasoning, EstimatedInputTokens, MaxOutputTokens, MinContextWindow}
  - `anthropic.go`: `inspectRequestJSON(raw, "image", [thinking, reasoning])` → similar
  - `canonical_path.go` (Responses): `inspectResponsesRequestJSON` → visionType `input_image`, reasoning `reasoning`, fields `input`, `instructions`
  - Tools detection is boolean only (len>0), no count, no tool_choice
  - Vision is boolean only, no image count
  - Structured output not detected (response_format, json_schema)
  - Message count not preserved beyond token estimate
  - System/user text chars not separated

- Compatibility requirement extraction:
  - `profileFromRequirement` in compat layer maps router.Requirement to compat.RequirementProfile for capability checks

- Session affinity extraction already bounded and header-preferring

- Phase B integration:
  - `candidatesForRequirement` snapshots resolver+router under RLock, resolves VE, protocol check exact, then `rt.Candidates(reqAll)` where Model="" for VE, then `AllFilteredCandidates` (pool ∩ eligible)
  - `reqEligible` with Model="" used for `currentRouteCandidate` to avoid virtual model mismatch

## Duplicated parsing currently present

- Each ingress does:
  1. `readJSON` → decodes into protocol-specific struct (OpenAIRequest, AnthropicRequest, ResponsesRequest) via json.Decoder (bounded body)
  2. `inspectRequestJSONFields` → second `json.Unmarshal` into `map[string]any` + stack walk
- That's 2 parses per request. Adding Task Analyzer as third parse would be wasteful.
- Goal: generalize inspection pass into feature extractor, so we have protocol decode + one bounded feature extraction pass, not three.

## Protocol-specific differences

- Vision keys: OpenAI `image_url`, Anthropic `image`, Responses `input_image`
- Reasoning keys: OpenAI `reasoning_effort`/`reasoning`, Anthropic `thinking`/`reasoning`, Responses `reasoning`
- Content fields: OpenAI/Anthropic `messages`, Responses `input`+`instructions`
- Tools: OpenAI `tools`, Anthropic `tools`, Responses `tools` (but Responses tools structure differs)
- Structured output: OpenAI `response_format`, Anthropic not standard, Responses `text.format` etc.
- Session: same header preference, body key same extraction across protocols

## Selected extraction seam

- New packages:
  - `internal/feature` — normalized RequestFeatures, bounded lexical analysis, structural signals, single-pass extractor that reuses existing inspection logic (vision, reasoning, token estimate, session, message counts, image count, tool count, structured output, etc.)
  - `internal/taskprofile` — TaskType enum, Complexity, ReasonCode enum, TaskProfile, deterministic Analyzer that consumes RequestFeatures only (no raw prompt)

- Integration point in each ingress:
```
read body (raw)
→ protocol decode (existing)
→ feature.Extractor.Extract(raw, protocol, protoStruct) → RequestFeatures (one pass, reusing inspection)
→ router.Requirement creation (reuse features: Vision, Reasoning, EstimatedInputTokens, BodySessionKey, Tools)
→ taskprofile.Analyzer.Analyze(features) → TaskProfile
→ task_classified event (bounded, privacy-safe)
→ candidatesForRequirement (unchanged)
→ execution
```

- `RequestFeatures` must contain no raw prompt, only booleans, bounded counts, enums.
- Lexical scan budget: 64 KiB relevant text (user+system+instructions), truncate beyond, mark truncated.

## Expected files to move/change

- `internal/httpapi/routing_helpers.go` — refactor inspection into `internal/feature`, keep backward-compatible wrappers or remove duplicate parsing
- `internal/httpapi/openai.go`, `anthropic.go`, `canonical_path.go` — call feature extractor, create TaskProfile, emit event, keep routing unchanged
- `internal/feature/` NEW — extractor, RequestFeatures struct, lexical signals, code/debug/edit/repo/architecture/extraction/agent signals, bounds
- `internal/taskprofile/` NEW — TaskType, Complexity, ReasonCode, TaskProfile, Analyzer with precedence, complexity, confidence
- `internal/events/bus.go` — add task_classified fields (task_type, complexity, confidence, reason_codes, estimated_context, tools, vision, reasoning, structured_output) bounded
- `internal/httpapi/metrics.go` — add bounded task classification counter + duration histogram (optional)
- Tests: `internal/feature/*_test.go`, `internal/taskprofile/*_test.go`, `internal/httpapi/task_test.go` (routing neutrality, cross-protocol, VE neutrality, privacy)

## Performance risks

- Current inspection already walks up to 100k nodes; adding lexical scanning could increase CPU if scanning large prompts. Must bound scan to 64 KiB relevant text and avoid regex heavy.
- Duplicate JSON parsing risk: must avoid third unmarshal. Solution: extractor does single unmarshal + stack walk, produces both structural and lexical signals.
- Analyzer must be stateless, no global locks, no allocations per request beyond small slices.
- Benchmarks needed for tiny chat, coding, tool-heavy, scan-limit inputs to prove overhead < few hundred µs.
- Memory: must not retain raw text beyond extraction; only aggregate booleans/counts.
