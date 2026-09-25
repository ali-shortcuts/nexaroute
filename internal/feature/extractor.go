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
	// If zero, extractor will count from raw JSON itself.
	ToolCountHint       int
	ToolChoiceHint      bool
	HasSystemPromptHint *bool
}

type Extractor struct{}

func NewExtractor() *Extractor { return &Extractor{} }

// Extract performs a single bounded parse of raw JSON and produces RequestFeatures.
// It reuses the existing inspection logic (vision, reasoning, session, token estimate)
// but also collects lexical signals from up to 64 KiB of relevant text.
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

	// Fast path: invalid JSON => minimal features
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		// No structural info, but preserve model/streaming hints
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

	// Tools / tool_choice / system prompt detection from root (if hints not provided)
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
	if opts.ToolChoiceHint {
		out.ToolChoice = true
	} else {
		if _, ok := root["tool_choice"]; ok {
			out.ToolChoice = true
		}
	}
	if opts.HasSystemPromptHint != nil {
		out.HasSystemPrompt = *opts.HasSystemPromptHint
	} else {
		// Heuristic: presence of "system" role in messages or top-level "system" or "instructions"
		if _, ok := root["system"]; ok {
			out.HasSystemPrompt = true
		}
		if _, ok := root["instructions"]; ok {
			out.HasSystemPrompt = true
		}
	}

	// Content subtree walk
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
	// If no content fields found, still count for token estimate = 0, but not too complex
	if len(stack) == 0 {
		out.EstimatedPromptTokens = defaultEstimatedTokens
		out.EstimatedTotalTokens = out.EstimatedPromptTokens + out.MaxOutputTokens
		return out
	}

	const maxNodes = 100000
	nodes := 0
	chars := 0
	messageCount := 0
	visionCount := 0
	hasVision := false
	relevantBuilder := &boundedStringBuilder{limit: maxRelevantTextBytes}

	for len(stack) > 0 {
		last := len(stack) - 1
		v := stack[last]
		stack = stack[:last]
		nodes++
		if nodes > maxNodes {
			out.TooComplex = true
			// Preserve what we have, mark truncated
			out.RelevantTruncated = relevantBuilder.truncated || true
			break
		}
		switch x := v.(type) {
		case string:
			// Count chars for token estimate (bytes, overestimate for multibyte = safe)
			chars += len(x)
			if chars > maxTotalChars {
				chars = maxTotalChars
			}
			// Collect relevant text for lexical scan, but only if string is likely user/system content
			// We collect all strings from contentFields subtree, bounded to 64 KiB
			relevantBuilder.WriteString(x)
			relevantBuilder.WriteString("\n")
		case map[string]any:
			// Vision detection
			if typ, _ := x["type"].(string); opts.VisionType != "" && strings.EqualFold(typ, opts.VisionType) {
				hasVision = true
				visionCount++
				if visionCount > maxVisionImageCount {
					visionCount = maxVisionImageCount
				}
			}
			// Message count heuristic: presence of "role"
			if _, ok := x["role"]; ok {
				messageCount++
				if messageCount > maxMessageCount {
					messageCount = maxMessageCount
				}
			}
			// System prompt detection if not already via hint: role == "system" or "developer"
			if !out.HasSystemPrompt {
				if role, _ := x["role"].(string); strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
					out.HasSystemPrompt = true
				}
			}
			// Push children
			if len(x) > maxNodes-nodes-len(stack) {
				out.TooComplex = true
				out.RelevantTruncated = relevantBuilder.truncated || true
				break
			}
			for _, child := range x {
				stack = append(stack, child)
			}
		case []any:
			if len(x) > maxNodes-nodes-len(stack) {
				out.TooComplex = true
				out.RelevantTruncated = relevantBuilder.truncated || true
				break
			}
			// Append in reverse to preserve order? Not needed for counting, but for text collection order we push as is.
			stack = append(stack, x...)
		default:
			// ignore numbers, bools, nil
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
	out.RelevantTextLength = relevantBuilder.Len()
	out.RelevantTruncated = out.RelevantTruncated || relevantBuilder.truncated

	// Lexical scan on collected relevant text (privacy-safe, only booleans)
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

func detectStructuredOutput(root map[string]any, proto Protocol) bool {
	// OpenAI: response_format present
	if _, ok := root["response_format"]; ok {
		return true
	}
	// Responses API: text.format
	if txt, ok := root["text"].(map[string]any); ok {
		if _, ok := txt["format"]; ok {
			return true
		}
	}
	// Anthropic: not standard, but some clients use response_format
	// Generic: json_schema present
	if _, ok := root["json_schema"]; ok {
		return true
	}
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

// bodySessionKey extracts session_id from root/metadata/user_id.session_id bounded to 256
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

// boundedStringBuilder collects up to limit bytes, tracks truncation
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

// lexicalResult holds privacy-safe booleans
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

func analyzeLexical(s string) lexicalResult {
	var r lexicalResult
	if s == "" {
		return r
	}
	lower := strings.ToLower(s)

	// Code blocks: count ```
	r.CodeBlockCount = strings.Count(s, "```")
	// Each pair is one block, but count occurrences/2 roughly
	if r.CodeBlockCount > 0 {
		r.HasCodeBlock = true
		// Number of blocks = occurrences / 2 (open+close), but keep count of occurrences bounded
		r.CodeBlockCount = r.CodeBlockCount / 2
		if r.CodeBlockCount == 0 {
			r.CodeBlockCount = 1
		}
	}
	// Inline code: single backtick not part of triple
	// Heuristic: presence of `...` but not ``` already counted
	if strings.Contains(s, "`") {
		// Avoid false positive from only triple backticks
		// If there is a backtick that is not part of triple, mark inline
		// Simple: remove triple and check remaining
		tmp := strings.ReplaceAll(s, "```", "")
		if strings.Contains(tmp, "`") {
			r.HasInlineCode = true
		}
	}

	// Stack trace signals
	if strings.Contains(lower, "traceback") ||
		strings.Contains(lower, "stack trace") ||
		strings.Contains(lower, "stacktrace") ||
		strings.Contains(lower, "exception in thread") ||
		strings.Contains(lower, " at ") && (strings.Contains(lower, ".java:") || strings.Contains(lower, ".py") || strings.Contains(lower, ".go:") || strings.Contains(lower, ".js:")) ||
		strings.Contains(lower, "panic:") && strings.Contains(lower, "goroutine") {
		r.HasStackTrace = true
	}

	// Diff signals
	if strings.Contains(s, "diff --git") ||
		strings.Contains(s, "@@ ") && strings.Contains(s, " @@") ||
		(strings.Contains(s, "--- ") && strings.Contains(s, "+++ ")) {
		r.HasDiff = true
	}

	// File path signals: look for common patterns
	if strings.Contains(s, "src/") ||
		strings.Contains(s, "lib/") ||
		strings.Contains(s, ".go") ||
		strings.Contains(s, ".py") ||
		strings.Contains(s, ".js") ||
		strings.Contains(s, ".ts") ||
		strings.Contains(s, ".java") ||
		strings.Contains(s, ".rs") ||
		strings.Contains(s, "internal/") ||
		strings.Contains(s, "pkg/") {
		// Require slash or dot to reduce false positives
		r.HasFilePath = true
	}

	// URL
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		r.HasURL = true
	}

	// Keyword groups — case-insensitive substring search, bounded list
	r.HasEditKeywords = containsAny(lower, []string{"refactor", "rename", "edit this", "fix this", "update the", "modify ", "change the", "patch ", "apply patch"})
	// More generic edit words but avoid too broad: check with word boundaries via simple contains
	if !r.HasEditKeywords {
		// Count if edit/fix appears near code context? For now broad but limited to avoid false positives on general chat
		// We use stronger signals: "edit", "fix", "update", "modify", "change" are too common, so require code context already? Instead, check combined
		if r.HasCodeBlock || r.HasFilePath {
			r.HasEditKeywords = containsAny(lower, []string{"edit", "fix", "update", "refactor", "rename", "modify"})
		}
	}

	r.HasDebugKeywords = containsAny(lower, []string{"debug", "bug", "error", "fail", "exception", "traceback", "stack", "panic", "crash"})
	r.HasRepoKeywords = containsAny(lower, []string{"repository", "repo ", "git ", "commit", "branch", "pull request", " pr ", "merge request", "github", "gitlab"})
	r.HasArchKeywords = containsAny(lower, []string{"architecture", "design doc", "system design", "component", "diagram", "sequence diagram", "microservice", "scalability", "tradeoff", "trade-off"})
	r.HasAgentKeywords = containsAny(lower, []string{"agent", "tool use", "function call", "autonomous", "multi-step", "multi step", "plan and execute"})
	r.HasExtractionKeywords = containsAny(lower, []string{"extract", "summarize", "parse", "table", "json output", "structured output", "key points", "bullet points"})
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
