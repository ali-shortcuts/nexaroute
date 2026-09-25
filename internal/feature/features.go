package feature

// Protocol indicates ingress protocol family.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolResponses Protocol = "responses"
	ProtocolUnknown   Protocol = "unknown"
)

// RequestFeatures is the normalized, privacy-safe representation of a request.
// It MUST NOT contain raw prompts, user content, or tool results.
// Only bounded booleans, counts, enums, and truncated lengths are allowed.
type RequestFeatures struct {
	// Identity / structural
	Protocol       Protocol `json:"protocol"`
	ModelRequested string   `json:"model_requested,omitempty"` // bounded to 256
	Streaming      bool     `json:"streaming"`

	// Capability signals (mirrors router.Requirement fields)
	HasVision        bool `json:"has_vision"`
	VisionImageCount int  `json:"vision_image_count"` // bounded
	HasReasoning     bool `json:"has_reasoning"`
	HasTools         bool `json:"has_tools"`
	ToolCount        int  `json:"tool_count"` // capped
	ToolChoice       bool `json:"tool_choice"`
	StructuredOutput bool `json:"structured_output"`

	HasSystemPrompt bool `json:"has_system_prompt"`
	MessageCount    int  `json:"message_count"`
	MaxOutputTokens int  `json:"max_output_tokens"`

	// Token / length estimates
	EstimatedPromptTokens int  `json:"estimated_prompt_tokens"`
	EstimatedTotalTokens  int  `json:"estimated_total_tokens"` // prompt + max_output
	TotalChars            int  `json:"total_chars"`            // chars counted in relevant subtree
	RelevantTextLength    int  `json:"relevant_text_length"`   // bytes collected for lexical scan (capped)
	RelevantTruncated     bool `json:"relevant_truncated"`

	// Session
	SessionKeyPresent bool   `json:"session_key_present"`
	BodySessionKey    string `json:"-"` // internal, bounded 256, not exposed in JSON logs by default

	// Complexity guard
	TooComplex bool `json:"too_complex"`

	// Lexical signals — all derived from bounded 64 KiB relevant text, no raw content retained
	HasCodeBlock          bool `json:"has_code_block"`
	CodeBlockCount        int  `json:"code_block_count"`
	HasInlineCode         bool `json:"has_inline_code"`
	HasStackTrace         bool `json:"has_stack_trace"`
	HasDiff               bool `json:"has_diff"`
	HasFilePath           bool `json:"has_file_path"`
	HasURL                bool `json:"has_url"`
	HasEditKeywords       bool `json:"has_edit_keywords"`
	HasDebugKeywords      bool `json:"has_debug_keywords"`
	HasRepoKeywords       bool `json:"has_repo_keywords"`
	HasArchKeywords       bool `json:"has_arch_keywords"`
	HasAgentKeywords      bool `json:"has_agent_keywords"`
	HasExtractionKeywords bool `json:"has_extraction_keywords"`
	HasCodeIdentifiers    bool `json:"has_code_identifiers"`
}

const (
	maxModelIDLen          = 256
	maxSessionKeyLen       = 256
	maxRelevantTextBytes   = 64 * 1024 // 64 KiB lexical scan budget
	maxVisionImageCount    = 100
	maxToolCount           = 128
	maxCodeBlockCount      = 100
	maxMessageCount        = 10000
	maxTotalChars          = 10_000_000
	defaultEstimatedTokens = 16
	perMessageOverhead     = 8
)

func boundedString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
