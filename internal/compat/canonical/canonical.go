package canonical

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// Canonical IR: a single normalized representation between client protocols
// and provider dialects. Translators convert client -> CanonicalRequest and
// CanonicalRequest -> provider payload, avoiding N x N direct translators.
//
// This package implements Phase 1 of the Universal API Compatibility Engine.
// It is additive: the existing translate package keeps serving the hot path
// while new protocol families (Responses, Gemini) enter through here.

// ContentKind enumerates normalized message content part types.
type ContentKind string

const (
	ContentText       ContentKind = "text"
	ContentImage      ContentKind = "image"
	ContentToolCall   ContentKind = "tool_call"
	ContentToolResult ContentKind = "tool_result"
	ContentReasoning  ContentKind = "reasoning"
)

// ContentPart is one normalized unit inside a message.
type ContentPart struct {
	Kind ContentKind `json:"kind"`
	// Text for text/reasoning parts.
	Text string `json:"text,omitempty"`
	// ImageURL carries a normalized image reference (URL or data URL).
	ImageURL string `json:"image_url,omitempty"`
	// MediaType hints the image encoding when known (e.g. image/png).
	MediaType string `json:"media_type,omitempty"`
	// Tool call fields.
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolInput  map[string]any `json:"tool_input,omitempty"`
	// Tool result fields.
	ToolUseID   string `json:"tool_use_id,omitempty"`
	ToolError   bool   `json:"tool_error,omitempty"`
	ToolContent string `json:"tool_content,omitempty"`
}

// Message is one normalized conversation turn.
type Message struct {
	Role  string        `json:"role"` // system | user | assistant | tool
	Parts []ContentPart `json:"parts"`
}

// Text concatenates text parts for convenience.
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Kind == ContentText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// HasImages reports whether the message carries image parts.
func (m Message) HasImages() bool {
	for _, p := range m.Parts {
		if p.Kind == ContentImage {
			return true
		}
	}
	return false
}

// HasToolCalls reports whether the message carries tool calls.
func (m Message) HasToolCalls() bool {
	for _, p := range m.Parts {
		if p.Kind == ContentToolCall {
			return true
		}
	}
	return false
}

// HasToolResults reports whether the message carries tool results.
func (m Message) HasToolResults() bool {
	for _, p := range m.Parts {
		if p.Kind == ContentToolResult {
			return true
		}
	}
	return false
}

// ToolDef is a normalized function/tool definition.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Reasoning carries normalized reasoning controls.
type Reasoning struct {
	Enabled bool   `json:"enabled"`
	Effort  string `json:"effort,omitempty"` // none | low | medium | high | xhigh
	Budget  int    `json:"budget,omitempty"` // thinking budget tokens when set
}

// ResponseFormat carries normalized structured-output controls.
type ResponseFormat struct {
	Type       string         `json:"type,omitempty"` // text | json_object | json_schema
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

// Request is the canonical normalized request.
type Request struct {
	Model          string          `json:"model"`
	System         string          `json:"system,omitempty"`
	Messages       []Message       `json:"messages"`
	Tools          []ToolDef       `json:"tools,omitempty"`
	ToolChoice     string          `json:"tool_choice,omitempty"` // auto | none | required | named:<tool>
	ParallelTools  *bool           `json:"parallel_tools,omitempty"`
	Reasoning      *Reasoning      `json:"reasoning,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	Metadata       map[string]any  `json:"metadata,omitempty"`
}

// HasTools reports whether the request defines tools.
func (r Request) HasTools() bool { return len(r.Tools) > 0 }

// HasImages reports whether any message carries images.
func (r Request) HasImages() bool {
	for _, m := range r.Messages {
		if m.HasImages() {
			return true
		}
	}
	return false
}

// HasReasoning reports whether reasoning controls are present.
func (r Request) HasReasoning() bool { return r.Reasoning != nil && r.Reasoning.Enabled }

// HasStructuredOutput reports whether structured output was requested.
func (r Request) HasStructuredOutput() bool {
	return r.ResponseFormat != nil && (r.ResponseFormat.Type == "json_object" || r.ResponseFormat.Type == "json_schema")
}

// ToolCall is a normalized model-emitted tool invocation.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	RawArgs   string         `json:"raw_args,omitempty"`
}

// Response is the canonical normalized response.
type Response struct {
	Text         string     `json:"text,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	Reasoning    string     `json:"reasoning,omitempty"`
	StopReason   string     `json:"stop_reason,omitempty"`
	InputTokens  int        `json:"input_tokens,omitempty"`
	OutputTokens int        `json:"output_tokens,omitempty"`
}

// CapabilityName enumerates capability keys shared with the capabilities package.
type CapabilityName string

const (
	CapText               CapabilityName = "text"
	CapStreaming          CapabilityName = "streaming"
	CapSystemMessage      CapabilityName = "system_message"
	CapTools              CapabilityName = "tools"
	CapToolChoiceAuto     CapabilityName = "tool_choice_auto"
	CapToolChoiceRequired CapabilityName = "tool_choice_required"
	CapParallelTools      CapabilityName = "parallel_tools"
	CapReasoning          CapabilityName = "reasoning"
	CapReasoningEffort    CapabilityName = "reasoning_effort"
	CapVision             CapabilityName = "vision"
	CapStructuredOutput   CapabilityName = "structured_output"
	CapJSONSchema         CapabilityName = "json_schema"
	CapTemperature        CapabilityName = "temperature"
	CapTopP               CapabilityName = "top_p"
	CapStop               CapabilityName = "stop"
	CapMaxTokens          CapabilityName = "max_tokens"
)

// Requirements separates REQUIRED capabilities from OPTIONAL ones.
// See spec section 11: optional unsupported fields may be sanitized away,
// while missing required capabilities make a deployment ineligible.
type Requirements struct {
	Required []CapabilityName `json:"required"`
	Optional []CapabilityName `json:"optional"`
}

// Requirements derives capability requirements from the canonical request.
// Text is always required. Streaming/tools/vision/reasoning/structured
// output become required only when the request actually uses them; sampling
// parameters stay optional so they can be sanitized per policy.
func (r Request) Requirements() Requirements {
	req := Requirements{Required: []CapabilityName{CapText}}
	if r.Stream {
		req.Required = append(req.Required, CapStreaming)
	}
	if r.HasTools() {
		req.Required = append(req.Required, CapTools)
		switch r.ToolChoice {
		case "auto", "":
			req.Optional = append(req.Optional, CapToolChoiceAuto)
		case "required":
			req.Required = append(req.Required, CapToolChoiceRequired)
		}
		if r.ParallelTools != nil && *r.ParallelTools {
			req.Optional = append(req.Optional, CapParallelTools)
		}
	}
	if r.HasImages() {
		req.Required = append(req.Required, CapVision)
	}
	if r.HasReasoning() {
		req.Required = append(req.Required, CapReasoning)
		if r.Reasoning.Effort != "" {
			req.Optional = append(req.Optional, CapReasoningEffort)
		}
	}
	if r.HasStructuredOutput() {
		req.Required = append(req.Required, CapStructuredOutput)
		if r.ResponseFormat.Type == "json_schema" {
			req.Required = append(req.Required, CapJSONSchema)
		}
	}
	if r.System != "" {
		req.Optional = append(req.Optional, CapSystemMessage)
	}
	if r.Temperature != nil {
		req.Optional = append(req.Optional, CapTemperature)
	}
	if r.TopP != nil {
		req.Optional = append(req.Optional, CapTopP)
	}
	if len(r.Stop) > 0 {
		req.Optional = append(req.Optional, CapStop)
	}
	if r.MaxTokens > 0 {
		req.Optional = append(req.Optional, CapMaxTokens)
	}
	return req
}

// RequirementsForClaudeCode returns the requirement split for Claude Code
// traffic. Agent mode requires tools; basic chat does not.
func RequirementsForClaudeCode(agentMode, streaming bool) Requirements {
	req := Requirements{Required: []CapabilityName{CapText}}
	if streaming {
		req.Required = append(req.Required, CapStreaming)
	}
	if agentMode {
		req.Required = append(req.Required, CapTools, CapToolChoiceAuto)
		req.Optional = append(req.Optional, CapParallelTools, CapReasoning, CapTemperature)
	} else {
		req.Optional = append(req.Optional, CapTools, CapReasoning, CapTemperature)
	}
	return req
}

// --- Decoders: client protocol -> Canonical IR ---

func floatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func parseStop(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := []string{}
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// FromAnthropicRequest decodes an Anthropic Messages request into canonical form.
func FromAnthropicRequest(in core.AnthropicRequest) (Request, error) {
	out := Request{
		Model:       in.Model,
		Stream:      in.Stream,
		Temperature: floatPtr(in.Temperature),
		TopP:        floatPtr(in.TopP),
		MaxTokens:   in.MaxTokens,
		Stop:        append([]string(nil), in.StopSequences...),
	}
	if len(in.System) > 0 && string(in.System) != "null" {
		var asString string
		if err := json.Unmarshal(in.System, &asString); err == nil {
			out.System = asString
		} else {
			var blocks []map[string]any
			if err := json.Unmarshal(in.System, &blocks); err == nil {
				texts := []string{}
				for _, b := range blocks {
					if t, _ := b["text"].(string); t != "" {
						texts = append(texts, t)
					}
				}
				out.System = strings.Join(texts, "\n")
			}
		}
	}
	for _, t := range in.Tools {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		out.Tools = append(out.Tools, ToolDef{Name: t.Name, Description: t.Description, Parameters: params})
	}
	switch v := in.ToolChoice.(type) {
	case string:
		out.ToolChoice = v
	case map[string]any:
		if typ, _ := v["type"].(string); typ != "" {
			out.ToolChoice = typ
			if name, _ := v["name"].(string); name != "" && (typ == "tool" || typ == "named") {
				out.ToolChoice = "named:" + name
			}
		}
	}
	if len(in.Thinking) > 0 && string(in.Thinking) != "null" {
		var th struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}
		if err := json.Unmarshal(in.Thinking, &th); err == nil && th.Type == "enabled" {
			out.Reasoning = &Reasoning{Enabled: true, Budget: th.BudgetTokens}
		}
	}
	if len(in.Metadata) > 0 && string(in.Metadata) != "null" {
		var meta map[string]any
		if err := json.Unmarshal(in.Metadata, &meta); err == nil {
			out.Metadata = meta
		}
	}
	for _, m := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		msg := Message{Role: role}
		blocks, err := core.ParseAnthContent(m.Content)
		if err != nil {
			return out, err
		}
		if len(blocks) == 0 {
			msg.Parts = append(msg.Parts, ContentPart{Kind: ContentText})
			out.Messages = append(out.Messages, msg)
			continue
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				msg.Parts = append(msg.Parts, ContentPart{Kind: ContentText, Text: b.Text})
			case "image":
				msg.Parts = append(msg.Parts, ContentPart{Kind: ContentImage, ImageURL: anthImageURL(b.Source), MediaType: anthMediaType(b.Source)})
			case "tool_use":
				msg.Parts = append(msg.Parts, ContentPart{Kind: ContentToolCall, ToolCallID: b.ID, ToolName: b.Name, ToolInput: b.Input})
			case "tool_result":
				msg.Parts = append(msg.Parts, ContentPart{Kind: ContentToolResult, ToolUseID: b.ToolUseID, ToolError: b.IsError, ToolContent: toolResultText(b.Content)})
			case "thinking", "redacted_thinking":
				msg.Parts = append(msg.Parts, ContentPart{Kind: ContentReasoning, Text: b.Text})
			default:
				// Unknown block types are preserved as opaque text so the
				// conversation never dead-ends on a future Anthropic addition.
				raw, _ := json.Marshal(map[string]any{"type": b.Type, "text": b.Text})
				msg.Parts = append(msg.Parts, ContentPart{Kind: ContentText, Text: string(raw)})
			}
		}
		out.Messages = append(out.Messages, msg)
	}
	if len(out.Messages) == 0 {
		return out, fmt.Errorf("no messages after canonical decode")
	}
	return out, nil
}

func anthImageURL(src map[string]any) string {
	if src == nil {
		return ""
	}
	typ, _ := src["type"].(string)
	switch typ {
	case "base64":
		mt, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if data == "" {
			return ""
		}
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	default:
		if u, _ := src["url"].(string); u != "" {
			return u
		}
		return ""
	}
}

func anthMediaType(src map[string]any) string {
	if src == nil {
		return ""
	}
	mt, _ := src["media_type"].(string)
	return mt
}

func toolResultText(content json.RawMessage) string {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		return asString
	}
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err == nil {
		texts := []string{}
		for _, b := range blocks {
			if t, _ := b["text"].(string); t != "" {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(content)
}

// FromOpenAIRequest decodes an OpenAI Chat Completions request into canonical form.
func FromOpenAIRequest(in core.OpenAIRequest) (Request, error) {
	out := Request{
		Model:         in.Model,
		Stream:        in.Stream,
		Temperature:   floatPtr(in.Temperature),
		TopP:          floatPtr(in.TopP),
		MaxTokens:     in.MaxTokens,
		Stop:          parseStop(in.Stop),
		ParallelTools: in.ParallelToolCalls,
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = in.MaxCompletionTokens
	}
	for _, t := range in.Tools {
		params := t.Function.Parameters
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		out.Tools = append(out.Tools, ToolDef{Name: t.Function.Name, Description: t.Function.Description, Parameters: params})
	}
	switch v := in.ToolChoice.(type) {
	case string:
		out.ToolChoice = v
	case map[string]any:
		if typ, _ := v["type"].(string); typ != "" {
			if fn, _ := v["function"].(map[string]any); fn != nil {
				if name, _ := fn["name"].(string); name != "" {
					out.ToolChoice = "named:" + name
					break
				}
			}
			out.ToolChoice = typ
		}
	}
	if in.ReasoningEffort != "" {
		out.Reasoning = &Reasoning{Enabled: true, Effort: strings.ToLower(in.ReasoningEffort)}
	} else if len(in.Reasoning) > 0 && string(in.Reasoning) != "null" {
		var r struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(in.Reasoning, &r); err == nil && r.Effort != "" {
			out.Reasoning = &Reasoning{Enabled: true, Effort: strings.ToLower(r.Effort)}
		} else {
			out.Reasoning = &Reasoning{Enabled: true}
		}
	}
	if in.User != "" {
		out.Metadata = map[string]any{"user": in.User}
	}
	for _, m := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" {
			out.System = openAIContentText(m.Content)
			if out.System == "" && m.ReasoningContent != "" {
				out.System = m.ReasoningContent
			}
			continue
		}
		msg := Message{Role: role}
		switch c := m.Content.(type) {
		case nil:
		case string:
			if c != "" {
				msg.Parts = append(msg.Parts, ContentPart{Kind: ContentText, Text: c})
			}
		default:
			for _, part := range openAIContentParts(m.Content) {
				msg.Parts = append(msg.Parts, part)
			}
		}
		for _, tc := range m.ToolCalls {
			args := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			msg.Parts = append(msg.Parts, ContentPart{
				Kind: ContentToolCall, ToolCallID: tc.ID, ToolName: tc.Function.Name,
				ToolInput: args,
			})
		}
		if role == "tool" {
			msg.Parts = append(msg.Parts, ContentPart{
				Kind: ContentToolResult, ToolUseID: m.ToolCallID,
				ToolContent: openAIContentText(m.Content),
			})
		}
		if m.ReasoningContent != "" {
			msg.Parts = append(msg.Parts, ContentPart{Kind: ContentReasoning, Text: m.ReasoningContent})
		}
		if len(msg.Parts) == 0 {
			msg.Parts = append(msg.Parts, ContentPart{Kind: ContentText})
		}
		out.Messages = append(out.Messages, msg)
	}
	if len(out.Messages) == 0 {
		return out, fmt.Errorf("no messages after canonical decode")
	}
	return out, nil
}

func openAIContentText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		texts := []string{}
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["text"].(string); t != "" {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n")
	default:
		return ""
	}
}

func openAIContentParts(content any) []ContentPart {
	parts := []ContentPart{}
	arr, ok := content.([]any)
	if !ok {
		return parts
	}
	for _, p := range arr {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "text", "output_text":
			if t, _ := m["text"].(string); t != "" {
				parts = append(parts, ContentPart{Kind: ContentText, Text: t})
			}
		case "image_url":
			if iu, _ := m["image_url"].(map[string]any); iu != nil {
				if u, _ := iu["url"].(string); u != "" {
					parts = append(parts, ContentPart{Kind: ContentImage, ImageURL: u})
				}
			}
		case "input_text":
			if t, _ := m["text"].(string); t != "" {
				parts = append(parts, ContentPart{Kind: ContentText, Text: t})
			}
		}
	}
	return parts
}

// --- Encoders: Canonical IR -> provider payload ---

// ToOpenAIRequest encodes a canonical request into an OpenAI Chat request.
func (r Request) ToOpenAIRequest(model string) core.OpenAIRequest {
	out := core.OpenAIRequest{
		Model: model, Stream: r.Stream,
		Temperature: r.Temperature, TopP: r.TopP,
		MaxTokens: r.MaxTokens,
	}
	if len(r.Stop) == 1 {
		out.Stop = r.Stop[0]
	} else if len(r.Stop) > 1 {
		out.Stop = r.Stop
	}
	if r.System != "" {
		out.Messages = append(out.Messages, core.OpenAIMessage{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "tool":
			for _, p := range m.Parts {
				if p.Kind != ContentToolResult {
					continue
				}
				out.Messages = append(out.Messages, core.OpenAIMessage{
					Role: "tool", ToolCallID: p.ToolUseID, Content: p.ToolContent,
				})
			}
		case "assistant":
			msg := core.OpenAIMessage{Role: "assistant"}
			texts := []string{}
			for _, p := range m.Parts {
				switch p.Kind {
				case ContentText:
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
				case ContentToolCall:
					arg, _ := json.Marshal(p.ToolInput)
					if len(arg) == 0 {
						arg = []byte("{}")
					}
					msg.ToolCalls = append(msg.ToolCalls, core.OpenAIToolCall{
						ID: p.ToolCallID, Type: "function",
						Function: core.OpenAIFunctionCall{Name: p.ToolName, Arguments: string(arg)},
					})
				case ContentReasoning:
					msg.ReasoningContent = p.Text
				}
			}
			if len(texts) > 0 {
				msg.Content = strings.Join(texts, "\n")
			}
			out.Messages = append(out.Messages, msg)
		default:
			role := m.Role
			if role == "" {
				role = "user"
			}
			parts := []map[string]any{}
			texts := []string{}
			for _, p := range m.Parts {
				switch p.Kind {
				case ContentText:
					if p.Text != "" {
						texts = append(texts, p.Text)
						parts = append(parts, map[string]any{"type": "text", "text": p.Text})
					}
				case ContentImage:
					if p.ImageURL != "" {
						parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.ImageURL}})
					}
				case ContentToolResult:
					out.Messages = append(out.Messages, core.OpenAIMessage{
						Role: "tool", ToolCallID: p.ToolUseID, Content: p.ToolContent,
					})
				}
			}
			if len(parts) == 1 && len(texts) == 1 {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: role, Content: texts[0]})
			} else if len(parts) > 0 {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: role, Content: parts})
			} else if len(texts) > 0 {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: role, Content: strings.Join(texts, "\n")})
			}
		}
	}
	for _, t := range r.Tools {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		out.Tools = append(out.Tools, core.OpenAITool{
			Type:     "function",
			Function: core.OpenAIFunction{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	if r.ToolChoice != "" {
		if strings.HasPrefix(r.ToolChoice, "named:") {
			out.ToolChoice = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": strings.TrimPrefix(r.ToolChoice, "named:")},
			}
		} else {
			out.ToolChoice = r.ToolChoice
		}
	}
	out.ParallelToolCalls = r.ParallelTools
	if r.Reasoning != nil && r.Reasoning.Enabled && r.Reasoning.Effort != "" {
		out.ReasoningEffort = r.Reasoning.Effort
	}
	if len(out.Messages) == 0 {
		out.Messages = append(out.Messages, core.OpenAIMessage{Role: "user", Content: ""})
	}
	return out
}

// FromOpenAIResponse decodes an OpenAI response into canonical form.
func FromOpenAIResponse(in core.OpenAIResponse) Response {
	out := Response{
		InputTokens: in.Usage.PromptTokens, OutputTokens: in.Usage.CompletionTokens,
	}
	if len(in.Choices) == 0 {
		return out
	}
	ch := in.Choices[0]
	if ch.FinishReason != nil {
		out.StopReason = *ch.FinishReason
	}
	out.Text = openAIContentText(ch.Message.Content)
	if ch.Message.ReasoningContent != "" {
		out.Reasoning = ch.Message.ReasoningContent
	}
	for _, tc := range ch.Message.ToolCalls {
		args := map[string]any{}
		raw := strings.TrimSpace(tc.Function.Arguments)
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				args = map[string]any{"_raw": raw}
			}
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args, RawArgs: raw})
	}
	return out
}

// FromAnthropicResponse decodes an Anthropic response into canonical form.
func FromAnthropicResponse(in core.AnthResponse) Response {
	out := Response{
		InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens,
	}
	if in.StopReason != nil {
		out.StopReason = *in.StopReason
	}
	texts := []string{}
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input})
		case "thinking":
			out.Reasoning = b.Thinking
		}
	}
	out.Text = strings.Join(texts, "\n")
	return out
}
