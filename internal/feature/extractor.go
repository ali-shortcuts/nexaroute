package feature

import (
	"encoding/json"
	"strings"
)

// ExtractOptions controls extraction per protocol.
type ExtractOptions struct {
	Protocol        Protocol
	Model           string
	Streaming       bool
	VisionType      string   // e.g. "image_url", "image", "input_image"
	ReasoningKeys   []string // top-level keys that indicate reasoning
	ContentFields   []string // fields to walk for conversation content (e.g. "messages", "input", "instructions")
	MaxOutputTokens int
	// Optional hints from already-decoded protocol structs to avoid double counting.
	ToolCountHint          int
	ToolChoiceHint         bool
	ToolChoiceRequiredHint *bool
	HasSystemPromptHint    *bool
}

type Extractor struct{}

func NewExtractor() *Extractor { return &Extractor{} }

// Extract performs a single bounded parse of raw JSON and produces RequestFeatures.
// It reuses the existing inspection logic (vision, reasoning, session, token estimate)
// but also collects lexical signals from up to 64 KiB of *relevant* text only.
func (e *Extractor) Extract(raw []byte, opts ExtractOptions) RequestFeatures {
	out := RequestFeatures{
		Protocol:  opts.Protocol,
		Streaming: opts.Streaming,
	}
	if out.Protocol == "" {
		out.Protocol = ProtocolUnknown
	}
	out.ModelRequested = boundedString(strings.TrimSpace(opts.Model), maxModelIDLen)
	out.MaxOutputTokens = opts.MaxOutputTokens

	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return out
	}

	// Session key extraction (bounded, header-preferred handling done by caller)
	out.BodySessionKey = bodySessionKey(root)
	out.SessionKeyPresent = out.BodySessionKey != ""

	// Reasoning detection: top-level keys only, case-insensitive, avoids tool schemas
	for _, wanted := range opts.ReasoningKeys {
		for k := range root {
			if strings.EqualFold(k, wanted) {
				out.HasReasoning = true
				break
			}
		}
		if out.HasReasoning {
			break
		}
	}

	// Structured output detection
	out.StructuredOutput = detectStructuredOutput(root, opts.Protocol)

	// Tools count
	if opts.ToolCountHint > 0 {
		out.ToolCount = boundInt(opts.ToolCountHint, 0, maxToolCount)
		out.HasTools = out.ToolCount > 0
	} else {
		if v, ok := root["tools"]; ok {
			if arr, ok := v.([]any); ok {
				out.ToolCount = boundInt(len(arr), 0, maxToolCount)
				out.HasTools = out.ToolCount > 0
			}
		}
	}

	// Tool choice semantics
	present, required := parseToolChoice(root["tool_choice"])
	// Apply hints if provided
	if opts.ToolChoiceHint {
		present = true
	}
	if opts.ToolChoiceRequiredHint != nil {
		required = *opts.ToolChoiceRequiredHint
		if required {
			present = true
		}
	}
	out.ToolChoicePresent = present
	out.ToolChoiceRequired = required
	out.ToolChoice = present // deprecated alias

	if opts.HasSystemPromptHint != nil {
		out.HasSystemPrompt = *opts.HasSystemPromptHint
	} else {
		if _, ok := root["system"]; ok {
			out.HasSystemPrompt = true
		}
		if _, ok := root["instructions"]; ok {
			out.HasSystemPrompt = true
		}
	}

	// Content subtree walk for token counting, vision, message count (all strings)
	contentFields := opts.ContentFields
	if len(contentFields) == 0 {
		contentFields = []string{"messages"}
	}

	stack := make([]any, 0, len(contentFields))
	for _, f := range contentFields {
		if v, ok := root[f]; ok {
			stack = append(stack, v)
		}
	}
	if len(stack) == 0 {
		out.EstimatedPromptTokens = defaultEstimatedTokens
		out.EstimatedTotalTokens = out.EstimatedPromptTokens + out.MaxOutputTokens
		// Still do relevant text collection (may be empty)
	} else {
		const maxNodes = 100000
		nodes := 0
		chars := 0
		messageCount := 0
		visionCount := 0
		hasVision := false
		hasToolResult := false
		hasImageURL := false

		for len(stack) > 0 {
			last := len(stack) - 1
			v := stack[last]
			stack = stack[:last]
			nodes++
			if nodes > maxNodes {
				out.TooComplex = true
				break
			}
			switch x := v.(type) {
			case string:
				chars += len(x)
				if chars > maxTotalChars {
					chars = maxTotalChars
				}
			case map[string]any:
				if typ, _ := x["type"].(string); opts.VisionType != "" && strings.EqualFold(typ, opts.VisionType) {
					hasVision = true
					visionCount++
					if visionCount > maxVisionImageCount {
						visionCount = maxVisionImageCount
					}
				}
				// Detect tool_result and image URL presence for observability
				if typ, _ := x["type"].(string); strings.EqualFold(typ, "tool_result") || strings.EqualFold(typ, "tool") {
					hasToolResult = true
				}
				if _, ok := x["tool_call_id"]; ok {
					// OpenAI tool role has tool_call_id
					// Could be tool result
				}
				if _, ok := x["image_url"]; ok {
					hasImageURL = true
				}
				if _, ok := x["role"]; ok {
					messageCount++
					if messageCount > maxMessageCount {
						messageCount = maxMessageCount
					}
				}
				if !out.HasSystemPrompt {
					if role, _ := x["role"].(string); strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
						out.HasSystemPrompt = true
					}
				}
				if len(x) > maxNodes-nodes-len(stack) {
					out.TooComplex = true
					break
				}
				for _, child := range x {
					stack = append(stack, child)
				}
			case []any:
				if len(x) > maxNodes-nodes-len(stack) {
					out.TooComplex = true
					break
				}
				stack = append(stack, x...)
			}
		}
		out.HasVision = hasVision
		out.VisionImageCount = boundInt(visionCount, 0, maxVisionImageCount)
		out.MessageCount = boundInt(messageCount, 0, maxMessageCount)
		out.TotalChars = boundInt(chars, 0, maxTotalChars)
		out.EstimatedPromptTokens = chars/4 + messageCount*perMessageOverhead + defaultEstimatedTokens
		if out.EstimatedPromptTokens < 0 {
			out.EstimatedPromptTokens = defaultEstimatedTokens
		}
		out.EstimatedTotalTokens = out.EstimatedPromptTokens + out.MaxOutputTokens
		out.HasToolResult = hasToolResult
		out.HasImageURL = hasImageURL
	}

	// Phase C convergence: collect ONLY semantically relevant text for lexical analysis
	relevantBuilder := &boundedStringBuilder{limit: maxRelevantTextBytes}
	collectRelevantText(root, opts, relevantBuilder)
	out.RelevantTextLength = relevantBuilder.Len()
	out.RelevantTruncated = relevantBuilder.truncated

	lex := analyzeLexical(relevantBuilder.String())
	out.HasCodeBlock = lex.HasCodeBlock
	out.CodeBlockCount = boundInt(lex.CodeBlockCount, 0, maxCodeBlockCount)
	out.HasInlineCode = lex.HasInlineCode
	out.HasStackTrace = lex.HasStackTrace
	out.HasDiff = lex.HasDiff
	out.HasFilePath = lex.HasFilePath
	out.HasURL = lex.HasURL
	out.HasEditKeywords = lex.HasEditKeywords
	out.HasDebugKeywords = lex.HasDebugKeywords
	out.HasRepoKeywords = lex.HasRepoKeywords
	out.HasArchKeywords = lex.HasArchKeywords
	out.HasAgentKeywords = lex.HasAgentKeywords
	out.HasExtractionKeywords = lex.HasExtractionKeywords
	out.HasCodeIdentifiers = lex.HasCodeIdentifiers

	return out
}

func parseToolChoice(v any) (present bool, required bool) {
	if v == nil {
		return false, false
	}
	present = true
	switch x := v.(type) {
	case string:
		lower := strings.ToLower(strings.TrimSpace(x))
		switch lower {
		case "required", "any", "tool":
			required = true
		case "auto", "none":
			required = false
		default:
			// Unknown string, treat as present but not required unless contains required
			if strings.Contains(lower, "required") {
				required = true
			}
		}
	case map[string]any:
		// Check type field
		if typ, _ := x["type"].(string); typ != "" {
			lower := strings.ToLower(typ)
			switch lower {
			case "tool", "any", "required", "function":
				required = true
			case "auto", "none":
				required = false
			default:
				// If has name, it's forced
				if _, hasName := x["name"]; hasName {
					required = true
				}
			}
		} else {
			// No type, but has function name => forced
			if _, ok := x["function"]; ok {
				required = true
			}
			if _, ok := x["name"]; ok {
				required = true
			}
		}
	default:
		// Other types, present but not required
		required = false
	}
	return present, required
}

func detectStructuredOutput(root map[string]any, proto Protocol) bool {
	if _, ok := root["response_format"]; ok {
		return true
	}
	if txt, ok := root["text"].(map[string]any); ok {
		if _, ok := txt["format"]; ok {
			return true
		}
	}
	if _, ok := root["json_schema"]; ok {
		return true
	}
	// Responses API: text.format already handled
	// Also check output_config etc? Keep simple
	return false
}

func boundInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func bodySessionKey(root map[string]any) string {
	if v, _ := root["session_id"].(string); boundedSessionValue(v) != "" {
		return boundedSessionValue(v)
	}
	meta, _ := root["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	if v, _ := meta["session_id"].(string); boundedSessionValue(v) != "" {
		return boundedSessionValue(v)
	}
	if user, ok := meta["user_id"].(map[string]any); ok {
		if v, _ := user["session_id"].(string); boundedSessionValue(v) != "" {
			return boundedSessionValue(v)
		}
	}
	return ""
}

func boundedSessionValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > maxSessionKeyLen {
		v = v[:maxSessionKeyLen]
	}
	return v
}

type boundedStringBuilder struct {
	sb        strings.Builder
	limit     int
	truncated bool
}

func (b *boundedStringBuilder) WriteString(s string) {
	if b.truncated {
		return
	}
	if b.limit <= 0 {
		b.limit = maxRelevantTextBytes
	}
	remaining := b.limit - b.sb.Len()
	if remaining <= 0 {
		b.truncated = true
		return
	}
	if len(s) > remaining {
		b.sb.WriteString(s[:remaining])
		b.truncated = true
		return
	}
	b.sb.WriteString(s)
}

func (b *boundedStringBuilder) Len() int {
	return b.sb.Len()
}

func (b *boundedStringBuilder) String() string {
	return b.sb.String()
}

type lexicalResult struct {
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

// collectRelevantText extracts only semantically relevant natural-language text
// for lexical analysis, excluding tool schemas, tool results, image URLs, metadata, etc.
func collectRelevantText(root map[string]any, opts ExtractOptions, builder *boundedStringBuilder) {
	switch opts.Protocol {
	case ProtocolAnthropic:
		collectRelevantAnthropic(root, builder)
	case ProtocolResponses:
		collectRelevantResponses(root, builder)
	default:
		// Default to OpenAI Chat semantics (also for unknown)
		collectRelevantOpenAI(root, builder)
	}
}

func collectRelevantOpenAI(root map[string]any, b *boundedStringBuilder) {
	// system as top-level string? Rare, but handle
	if sys, ok := root["system"].(string); ok && sys != "" {
		b.WriteString(sys)
		b.WriteString("\n")
	}
	// instructions as top-level (for Responses compat, but also OpenAI)
	if instr, ok := root["instructions"].(string); ok && instr != "" {
		b.WriteString(instr)
		b.WriteString("\n")
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		// Exclude tool and function roles (tool results)
		if strings.EqualFold(role, "tool") || strings.EqualFold(role, "function") {
			continue
		}
		// Only include system, developer, user, assistant for lexical
		// We include assistant text as it may contain relevant context in multi-turn,
		// but exclude its tool_calls
		content := mm["content"]
		if content == nil {
			continue
		}
		switch c := content.(type) {
		case string:
			if c != "" {
				b.WriteString(c)
				b.WriteString("\n")
			}
		case []any:
			for _, part := range c {
				switch p := part.(type) {
				case string:
					if p != "" {
						b.WriteString(p)
						b.WriteString("\n")
					}
				case map[string]any:
					typ, _ := p["type"].(string)
					// Only text types
					if strings.EqualFold(typ, "text") {
						if txt, _ := p["text"].(string); txt != "" {
							b.WriteString(txt)
							b.WriteString("\n")
						}
					} else if strings.EqualFold(typ, "input_text") || strings.EqualFold(typ, "output_text") {
						if txt, _ := p["text"].(string); txt != "" {
							b.WriteString(txt)
							b.WriteString("\n")
						}
						// Some Responses compat uses "text" field
						if txt, _ := p["input_text"].(string); txt != "" {
							b.WriteString(txt)
							b.WriteString("\n")
						}
					}
					// Exclude image_url, image, tool_use, tool_result, etc.
				}
			}
		}
		// Explicitly exclude tool_calls[].function.arguments (JSON payload)
		// We do not walk into mm["tool_calls"] at all
	}
}

func collectRelevantAnthropic(root map[string]any, b *boundedStringBuilder) {
	// system can be string or array of text blocks
	if sys, ok := root["system"]; ok {
		switch s := sys.(type) {
		case string:
			if s != "" {
				b.WriteString(s)
				b.WriteString("\n")
			}
		case []any:
			for _, blk := range s {
				if m, ok := blk.(map[string]any); ok {
					if typ, _ := m["type"].(string); strings.EqualFold(typ, "text") {
						if txt, _ := m["text"].(string); txt != "" {
							b.WriteString(txt)
							b.WriteString("\n")
						}
					}
				}
			}
		}
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		// role check, but Anthropic roles are user, assistant
		content := mm["content"]
		if content == nil {
			continue
		}
		switch c := content.(type) {
		case string:
			if c != "" {
				b.WriteString(c)
				b.WriteString("\n")
			}
		case []any:
			for _, blk := range c {
				bm, ok := blk.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := bm["type"].(string)
				// Only include text blocks
				if strings.EqualFold(typ, "text") {
					if txt, _ := bm["text"].(string); txt != "" {
						b.WriteString(txt)
						b.WriteString("\n")
					}
				}
				// Exclude tool_use, tool_result, image, etc.
			}
		}
	}
}

func collectRelevantResponses(root map[string]any, b *boundedStringBuilder) {
	// instructions
	if instr, ok := root["instructions"]; ok {
		switch v := instr.(type) {
		case string:
			if v != "" {
				b.WriteString(v)
				b.WriteString("\n")
			}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && s != "" {
					b.WriteString(s)
					b.WriteString("\n")
				} else if m, ok := item.(map[string]any); ok {
					if txt, _ := m["text"].(string); txt != "" {
						b.WriteString(txt)
						b.WriteString("\n")
					}
				}
			}
		}
	}
	// input can be string or array
	inp, ok := root["input"]
	if !ok {
		return
	}
	switch v := inp.(type) {
	case string:
		if v != "" {
			b.WriteString(v)
			b.WriteString("\n")
		}
	case []any:
		for _, item := range v {
			switch it := item.(type) {
			case string:
				if it != "" {
					b.WriteString(it)
					b.WriteString("\n")
				}
			case map[string]any:
				// Each input item may have role and content
				content, ok := it["content"]
				if !ok {
					// Sometimes input item itself is a content block
					typ, _ := it["type"].(string)
					if strings.EqualFold(typ, "input_text") || strings.EqualFold(typ, "text") || strings.EqualFold(typ, "output_text") {
						if txt, _ := it["text"].(string); txt != "" {
							b.WriteString(txt)
							b.WriteString("\n")
						}
					}
					continue
				}
				switch c := content.(type) {
				case string:
					if c != "" {
						b.WriteString(c)
						b.WriteString("\n")
					}
				case []any:
					for _, part := range c {
						if pm, ok := part.(map[string]any); ok {
							typ, _ := pm["type"].(string)
							if strings.EqualFold(typ, "input_text") || strings.EqualFold(typ, "text") || strings.EqualFold(typ, "output_text") {
								if txt, _ := pm["text"].(string); txt != "" {
									b.WriteString(txt)
									b.WriteString("\n")
								}
							}
							// Exclude input_image, tool_call, etc.
						}
					}
				}
			}
		}
	}
}

func analyzeLexical(s string) lexicalResult {
	var r lexicalResult
	if s == "" {
		return r
	}
	lower := strings.ToLower(s)

	// Code blocks: count ```
	r.CodeBlockCount = strings.Count(s, "```")
	if r.CodeBlockCount > 0 {
		r.HasCodeBlock = true
		r.CodeBlockCount = r.CodeBlockCount / 2
		if r.CodeBlockCount == 0 {
			r.CodeBlockCount = 1
		}
	}
	if strings.Contains(s, "`") {
		tmp := strings.ReplaceAll(s, "```", "")
		if strings.Contains(tmp, "`") {
			r.HasInlineCode = true
		}
	}

	// Stack trace signals — require stronger evidence than just "error"
	// To avoid false positives like "Tell me what an error means"
	if strings.Contains(lower, "traceback") ||
		strings.Contains(lower, "stack trace") ||
		strings.Contains(lower, "stacktrace") ||
		strings.Contains(lower, "exception in thread") ||
		(strings.Contains(lower, " at ") && (strings.Contains(lower, ".java:") || strings.Contains(lower, ".py:") || strings.Contains(lower, ".go:") || strings.Contains(lower, ".js:") || strings.Contains(lower, ".ts:"))) ||
		(strings.Contains(lower, "panic:") && strings.Contains(lower, "goroutine")) {
		r.HasStackTrace = true
	}

	// Diff signals
	if strings.Contains(s, "diff --git") ||
		(strings.Contains(s, "@@ ") && strings.Contains(s, " @@")) ||
		(strings.Contains(s, "--- ") && strings.Contains(s, "+++ ")) {
		r.HasDiff = true
	}

	// File path signals — require slash or dot + extension and not just generic words
	// Avoid false positives from URLs or generic mentions
	if strings.Contains(s, "src/") ||
		strings.Contains(s, "lib/") ||
		strings.Contains(s, "internal/") ||
		strings.Contains(s, "pkg/") ||
		(strings.Contains(s, "/") && (strings.Contains(s, ".go") || strings.Contains(s, ".py") || strings.Contains(s, ".js") || strings.Contains(s, ".ts") || strings.Contains(s, ".java") || strings.Contains(s, ".rs"))) {
		r.HasFilePath = true
	}

	// URL — but we already filter relevant text to exclude image URLs, so this is user-provided URLs
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		r.HasURL = true
	}

	// Keyword groups — avoid false positives by requiring stronger context
	// Edit keywords: require code context or explicit edit phrases
	r.HasEditKeywords = containsAny(lower, []string{"refactor", "rename", "edit this", "fix this", "update the", "modify ", "change the", "patch ", "apply patch"})
	if !r.HasEditKeywords {
		if r.HasCodeBlock || r.HasFilePath || r.HasDiff {
			// Only if code context present, allow broader edit words
			if containsAny(lower, []string{"edit", "fix", "update", "refactor", "rename", "modify"}) {
				// Additional check: avoid "Can you edit this sentence?" -> no code context, so not counted
				// Since we already require code context, this is safer
				r.HasEditKeywords = true
			}
		}
	}

	// Debug keywords: require stronger signals than just "error"
	// "Tell me what an error means" should NOT trigger debugging
	if r.HasStackTrace || r.HasCodeBlock || r.HasFilePath {
		r.HasDebugKeywords = containsAny(lower, []string{"debug", "bug", "error", "fail", "exception", "traceback", "stack", "panic", "crash"})
	} else {
		// Without code context, require more specific debug phrases
		r.HasDebugKeywords = containsAny(lower, []string{"debug this", "fix the bug", "stack trace", "traceback", "panic", "crash"})
	}

	r.HasRepoKeywords = containsAny(lower, []string{"repository", "repo ", "git ", "commit", "branch", "pull request", " pr ", "merge request", "github", "gitlab"})
	// Architecture: avoid "Design a birthday card" false positive
	// Require architecture-specific terms, not just "design"
	if containsAny(lower, []string{"architecture", "system design", "component", "sequence diagram", "microservice", "scalability", "tradeoff", "trade-off"}) {
		r.HasArchKeywords = true
	} else if strings.Contains(lower, "design doc") || strings.Contains(lower, "design document") {
		r.HasArchKeywords = true
	}
	// "design" alone should not trigger arch unless combined with system/component/diagram etc.

	r.HasAgentKeywords = containsAny(lower, []string{"agent", "tool use", "function call", "autonomous", "multi-step", "multi step", "plan and execute"})

	// Extraction: avoid "JSON is a data format" -> not extraction
	// Require action words + format words
	if containsAny(lower, []string{"extract", "summarize", "parse", "key points", "bullet points"}) {
		// If mentions JSON output/table as requested output, it's extraction
		if containsAny(lower, []string{"json output", "structured output", "table", "extract", "summarize", "parse"}) {
			r.HasExtractionKeywords = true
		}
	} else if containsAny(lower, []string{"json output", "structured output"}) {
		// Structured output request alone is not necessarily extraction unless combined
		// But keep as extraction signal if strong
		if strings.Contains(lower, "output") {
			r.HasExtractionKeywords = true
		}
	}

	r.HasCodeIdentifiers = containsAny(lower, []string{"func ", "function ", "class ", "import ", "package ", "def ", "const ", "let ", "var ", "struct ", "interface ", "pub fn", "fn "})

	return r
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
