// Package canonical defines NexaRoute's protocol-neutral request/response
// intermediate representation (IR) and the canonical stream event model.
//
// Design rules (see ARCHITECTURE.md "Universal Compatibility Engine"):
//
//	Provider   != Protocol
//	Protocol   != Dialect
//	Provider   != Model capability
//	Health     != Compatibility
//
// Client protocols (Anthropic Messages, OpenAI Chat Completions, OpenAI
// Responses) and upstream protocols (those plus Gemini GenerateContent) are
// decoded into this IR and re-encoded to the target dialect. Direct N x N
// translators must not be added; every new protocol family plugs into the IR
// once and becomes reachable from every other family.
package canonical

import (
	"encoding/json"
	"strings"
)

// PartType enumerates canonical content part kinds.
const (
	PartText       = "text"
	PartImage      = "image"
	PartToolCall   = "tool_call"
	PartToolResult = "tool_result"
	PartThinking   = "thinking"
)

// Roles understood by the IR. The tool role is normalized away during
// encoding; it exists so tool results can be represented losslessly.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Stop reasons form the canonical vocabulary (superset of Anthropic's).
const (
	StopEndTurn      = "end_turn"
	StopMaxTokens    = "max_tokens"
	StopToolUse      = "tool_use"
	StopStopSequence = "stop_sequence"
	StopRefusal      = "refusal"
	StopPauseTurn    = "pause_turn"
)

// Image carries either a remote/data URL or inline base64 data.
type Image struct {
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"` // base64, no data: prefix
}

// ToolCall is one requested tool invocation. Arguments is a JSON object
// serialized as a string; a malformed upstream payload is preserved verbatim
// so the conversation stays recoverable.
type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolResult answers a prior ToolCall.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Thinking carries reasoning content plus (when the upstream produced one) a
// signature. Unsigned thinking is never re-encoded toward Anthropic clients.
type Thinking struct {
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// Part is one canonical content part.
type Part struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	Image      *Image      `json:"image,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	Thinking   *Thinking   `json:"thinking,omitempty"`
}

// Message is one canonical conversation turn.
type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts,omitempty"`
}

// ToolDef describes a callable tool.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolChoice modes: auto, none, required, tool.
type ToolChoice struct {
	Mode string `json:"mode"`
	Name string `json:"name,omitempty"` // required when Mode == "tool"
}

// Reasoning normalizes thinking controls across protocols.
type Reasoning struct {
	Effort       string `json:"effort,omitempty"` // low | medium | high
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
}

// ResponseFormatKind values.
const (
	FormatText       = "text"
	FormatJSONObject = "json_object"
	FormatJSONSchema = "json_schema"
)

// ResponseFormat normalizes structured-output controls.
type ResponseFormat struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// Request is the canonical request IR.
type Request struct {
	Model             string          `json:"model"`
	System            []Part          `json:"system,omitempty"`
	Messages          []Message       `json:"messages"`
	Tools             []ToolDef       `json:"tools,omitempty"`
	ToolChoice        *ToolChoice     `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *Reasoning      `json:"reasoning,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	TopK              *float64        `json:"top_k,omitempty"`
	MaxOutputTokens   int             `json:"max_output_tokens,omitempty"`
	Stop              []string        `json:"stop,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Metadata          map[string]any  `json:"metadata,omitempty"`
	// ClientProtocol records the ingress family the canonical request was
	// decoded from (anthropic | openai_chat | openai_responses). Response
	// encoders use it as the default target format.
	ClientProtocol string `json:"client_protocol,omitempty"`
}

// Usage is canonical token accounting.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
}

// Block is one canonical response content block.
type Block struct {
	Type     string    `json:"type"` // text | tool_call | thinking
	Text     string    `json:"text,omitempty"`
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	Thinking *Thinking `json:"thinking,omitempty"`
}

// Response is the canonical non-streaming response IR.
type Response struct {
	ID         string          `json:"id,omitempty"`
	Model      string          `json:"model,omitempty"`
	Blocks     []Block         `json:"blocks"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      Usage           `json:"usage"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// HasToolCalls reports whether the response contains any tool invocation.
func (r *Response) HasToolCalls() bool {
	for _, b := range r.Blocks {
		if b.Type == PartToolCall {
			return true
		}
	}
	return false
}

// Text concatenates all text blocks with newlines.
func (r *Response) Text() string {
	var sb strings.Builder
	for _, b := range r.Blocks {
		if b.Type == PartText && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// Requirements expresses what a request genuinely needs versus what is
// optional decoration. The sanitizer may drop optional unsupported fields but
// must never drop semantics-critical ones, and the router must treat a
// required-but-unsupported capability as ineligibility.
type Requirements struct {
	Tools     bool `json:"tools"`      // request carries tool definitions
	NeedsTool bool `json:"needs_tool"` // tool_choice forces a call (any/required/tool)
	Vision    bool `json:"vision"`
	Streaming bool `json:"streaming"`
	Reasoning bool `json:"reasoning"`
	Stop      bool `json:"stop"`
	Seed      bool `json:"seed"`
}

// DetectRequirements derives the requirement profile from a canonical request.
func (r *Request) DetectRequirements() Requirements {
	out := Requirements{Tools: len(r.Tools) > 0, Streaming: r.Stream}
	for _, p := range r.System {
		if p.Type == PartImage {
			out.Vision = true
		}
	}
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			switch p.Type {
			case PartImage:
				out.Vision = true
			case PartToolCall, PartToolResult:
				out.Tools = true
			}
		}
	}
	if r.ToolChoice != nil {
		switch r.ToolChoice.Mode {
		case "any", "required", "tool":
			out.NeedsTool = true
			out.Tools = true
		case "auto":
			out.Tools = true
		}
	}
	if r.Reasoning != nil && !r.Reasoning.Disabled {
		out.Reasoning = true
	}
	out.Stop = len(r.Stop) > 0
	return out
}

// NormalizeStop flattens the stop payload shapes accepted by OpenAI-style
// upstreams (string, []string, []any) into a canonical slice.
func NormalizeStop(v any) []string {
	switch s := v.(type) {
	case string:
		if s == "" {
			return nil
		}
		return []string{s}
	case []string:
		out := make([]string, 0, len(s))
		for _, x := range s {
			if x != "" {
				out = append(out, x)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]string, 0, len(s))
		for _, x := range s {
			if str, ok := x.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// MapStopReason maps any upstream stop/finish vocabulary onto the canonical
// vocabulary. Unknown values degrade to end_turn rather than empty.
func MapStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool_use", "tool_calls", "function_call":
		return StopToolUse
	case "max_tokens", "length":
		return StopMaxTokens
	case "stop_sequence":
		return StopStopSequence
	case "refusal", "content_filter":
		return StopRefusal
	case "pause_turn":
		return StopPauseTurn
	default:
		return StopEndTurn
	}
}
