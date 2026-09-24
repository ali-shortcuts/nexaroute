package core

import "encoding/json"

// Anthropic-compatible wire types (subset focused on Claude Code compatibility).
type AnthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []AnthMessage   `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	Tools         []AnthTool      `json:"tools,omitempty"`
	ToolChoice    any             `json:"tool_choice,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          int             `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Thinking      json.RawMessage `json:"thinking,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

type AnthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type AnthResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Content    []AnthContentBlock `json:"content"`
	Model      string             `json:"model"`
	StopReason *string            `json:"stop_reason"`
	StopSeq    *string            `json:"stop_sequence"`
	Usage      AnthUsage          `json:"usage"`
}

type AnthContentBlock struct {
	Type             string         `json:"type"`
	Text             string         `json:"text,omitempty"`
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Input            map[string]any `json:"input,omitempty"`
	Source           map[string]any `json:"source,omitempty"`
	Thinking         string         `json:"thinking,omitempty"`
	Signature        string         `json:"signature,omitempty"`
	RedactedThinking string         `json:"redacted_thinking,omitempty"`
}

type AnthUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// OpenAI-compatible chat completion wire types.
type OpenAIRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       json.RawMessage `json:"stream_options,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                any             `json:"stop,omitempty"`
	Tools               []OpenAITool    `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Reasoning           json.RawMessage `json:"reasoning,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	User                string          `json:"user,omitempty"`
	N                   int             `json:"n,omitempty"`
	// ResponseFormatRaw preserves response_format verbatim for upstreams
	// that support structured output; canonical traffic sets it through
	// the compatibility engine.
	ResponseFormatRaw any `json:"response_format,omitempty"`
}

type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content,omitempty"`
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type OpenAIToolCall struct {
	Index    int                `json:"index,omitempty"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason *string       `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens            int                `json:"prompt_tokens"`
	CompletionTokens        int                `json:"completion_tokens"`
	TotalTokens             int                `json:"total_tokens"`
	PromptTokensDetails     *OpenAIUsageDetail `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *OpenAIUsageDetail `json:"completion_tokens_details,omitempty"`
}

type OpenAIUsageDetail struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}
