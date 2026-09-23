package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// AnthropicToOpenAI converts an Anthropic Messages request into an
// OpenAI Chat Completions request. It never fails on exotic-but-valid content:
// reasoning/thinking blocks are dropped (they cannot be replayed against
// OpenAI upstreams), unknown Anthropic block types are dropped rather than
// hard-failing the conversation, and tool names are sanitized through a
// reversible NameMap so MCP-style names survive the round trip.
//
// The returned *NameMap is nil when no tool name needed sanitizing; it must be
// passed back to OpenAIResponseToAnthropic and the streaming translator so
// upstream tool names restore to their original client-facing forms.
func AnthropicToOpenAI(in core.AnthropicRequest, model string) (core.OpenAIRequest, *NameMap, error) {
	out := core.OpenAIRequest{
		Model:       model,
		MaxTokens:   in.MaxTokens,
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	if len(in.StopSequences) == 1 {
		out.Stop = in.StopSequences[0]
	} else if len(in.StopSequences) > 1 {
		out.Stop = in.StopSequences
	}
	sys, err := parseAnthropicSystemStrict(in.System)
	if err != nil {
		return out, nil, err
	}
	if sys != "" {
		out.Messages = append(out.Messages, core.OpenAIMessage{Role: "system", Content: sys})
	}

	names := make([]string, 0, len(in.Tools))
	for _, t := range in.Tools {
		names = append(names, t.Name)
	}
	nm := NewOpenAINameMap(names)

	for _, m := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "user" && role != "assistant" {
			return out, nm, fmt.Errorf("unsupported Anthropic message role %q for OpenAI translation", m.Role)
		}
		blocks, err := core.ParseAnthContent(m.Content)
		if err != nil {
			return out, nm, err
		}
		if len(blocks) == 0 {
			out.Messages = append(out.Messages, core.OpenAIMessage{Role: m.Role, Content: ""})
			continue
		}
		// OpenAI requires tool messages to immediately follow the assistant
		// tool_calls they answer, so tool results inside a user message are
		// emitted before any trailing user text regardless of block order.
		parts := []map[string]any{}
		var toolMsgs []core.OpenAIMessage
		var assistantCalls []core.OpenAIToolCall
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			case "image":
				u := anthImageURL(b.Source)
				if u == "" {
					return out, nm, fmt.Errorf("Anthropic image block has no usable source")
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			case "tool_use":
				if role != "assistant" || strings.TrimSpace(b.ID) == "" || strings.TrimSpace(b.Name) == "" {
					return out, nm, fmt.Errorf("Anthropic tool_use requires assistant role, id, and name")
				}
				arg, err := json.Marshal(b.Input)
				if err != nil {
					return out, nm, fmt.Errorf("encode Anthropic tool input: %w", err)
				}
				assistantCalls = append(assistantCalls, core.OpenAIToolCall{
					ID:       b.ID,
					Type:     "function",
					Function: core.OpenAIFunctionCall{Name: nm.Forward(b.Name), Arguments: string(arg)},
				})
			case "tool_result":
				if role != "user" || strings.TrimSpace(b.ToolUseID) == "" {
					return out, nm, fmt.Errorf("Anthropic tool_result requires user role and tool_use_id")
				}
				toolMsgs = append(toolMsgs, core.OpenAIMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    toolResultToOpenAIContent(b),
				})
			case "thinking", "redacted_thinking":
				// Internal Anthropic reasoning state. It cannot be replayed to
				// an OpenAI-compatible upstream, so it is dropped rather than
				// failing the whole conversation (extended-thinking sessions
				// legitimately contain these blocks in assistant history).
			default:
				// Unknown block types (server_tool_use, web_search_tool_result,
				// document, code execution, future additions) are dropped so a
				// conversation never dead-ends on a feature this gateway does
				// not model. Structurally invalid tool blocks above still fail
				// closed because dropping them would corrupt tool-call pairing.
			}
		}
		if len(assistantCalls) > 0 {
			var content any
			if len(parts) > 0 {
				content = parts
			}
			out.Messages = append(out.Messages, core.OpenAIMessage{Role: "assistant", Content: content, ToolCalls: assistantCalls})
		} else if len(toolMsgs) == 0 && len(parts) > 0 {
			// Plain text-only message. A single text part collapses to a plain
			// string, maximizing upstream compatibility with providers that
			// reject array content in some roles.
			if len(parts) == 1 {
				if txt, ok := parts[0]["text"].(string); ok {
					out.Messages = append(out.Messages, core.OpenAIMessage{Role: m.Role, Content: txt})
					parts = nil
				}
			}
			if len(parts) > 0 {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: m.Role, Content: parts})
			}
		}
		// Tool messages precede trailing user text so they directly follow the
		// assistant tool_calls they answer.
		out.Messages = append(out.Messages, toolMsgs...)
		if len(assistantCalls) == 0 && len(toolMsgs) > 0 && len(parts) > 0 {
			// User text arriving after tool results becomes its own user turn.
			if len(parts) == 1 {
				if txt, ok := parts[0]["text"].(string); ok {
					out.Messages = append(out.Messages, core.OpenAIMessage{Role: m.Role, Content: txt})
					parts = nil
				}
			}
			if len(parts) > 0 {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: m.Role, Content: parts})
			}
		}
	}
	for _, t := range in.Tools {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		out.Tools = append(out.Tools, core.OpenAITool{
			Type:     "function",
			Function: core.OpenAIFunction{Name: nm.Forward(t.Name), Description: t.Description, Parameters: params},
		})
	}
	applyAnthropicToolChoiceToOpenAI(&out, in.ToolChoice)
	applyAnthropicThinkingToOpenAI(&out, in.Thinking)
	applyAnthropicMetadataToOpenAI(&out, in.Metadata)
	if len(out.Messages) == 0 {
		return out, nm, fmt.Errorf("no messages after translation")
	}
	return out, nm, nil
}

// toolResultToOpenAIContent normalizes an Anthropic tool_result block into
// OpenAI tool-message content. Strings pass through, block arrays become text
// (and data-URL images where possible), and is_error is preserved as a visible
// prefix so the upstream model keeps the failure signal.
func toolResultToOpenAIContent(b core.AnthBlock) any {
	const errPrefix = "[tool error] "
	if len(b.Content) == 0 || string(b.Content) == "null" {
		if b.IsError {
			return errPrefix + "(empty result)"
		}
		return ""
	}
	var asString string
	if err := json.Unmarshal(b.Content, &asString); err == nil {
		if b.IsError {
			return errPrefix + asString
		}
		return asString
	}
	var rawBlocks []map[string]any
	if err := json.Unmarshal(b.Content, &rawBlocks); err == nil && rawBlocks != nil {
		// Preserve the author-specified block order; consecutive text blocks
		// merge so providers that dislike many tiny parts stay happy.
		parts := []map[string]any{}
		appendText := func(txt string) {
			if n := len(parts); n > 0 {
				if prev, ok := parts[n-1]["text"].(string); ok && parts[n-1]["type"] == "text" {
					parts[n-1]["text"] = prev + "\n" + txt
					return
				}
			}
			parts = append(parts, map[string]any{"type": "text", "text": txt})
		}
		for _, rb := range rawBlocks {
			typ, _ := rb["type"].(string)
			switch typ {
			case "text":
				txt, _ := rb["text"].(string)
				appendText(txt)
			case "image":
				if u := anthImageURL(asSourceMap(rb["source"])); u != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				}
			default:
				encoded, err := json.Marshal(rb)
				if err == nil {
					appendText(string(encoded))
				}
			}
		}
		if len(parts) > 0 {
			return parts
		}
	}
	// Not a string and not a block array: preserve the JSON verbatim.
	var generic any
	if err := json.Unmarshal(b.Content, &generic); err == nil {
		if b.IsError {
			return errPrefix + string(b.Content)
		}
		return generic
	}
	if b.IsError {
		return errPrefix + string(b.Content)
	}
	return string(b.Content)
}

func asSourceMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func applyAnthropicToolChoiceToOpenAI(out *core.OpenAIRequest, choice any) {
	switch v := choice.(type) {
	case string:
		switch v {
		case "any":
			out.ToolChoice = "required"
		case "auto", "none", "required":
			out.ToolChoice = v
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch typ {
		case "auto":
			out.ToolChoice = "auto"
			if disable, ok := v["disable_parallel_tool_use"].(bool); ok {
				p := !disable
				out.ParallelToolCalls = &p
			}
		case "none":
			out.ToolChoice = "none"
		case "any":
			out.ToolChoice = "required"
			if disable, ok := v["disable_parallel_tool_use"].(bool); ok {
				p := !disable
				out.ParallelToolCalls = &p
			}
		case "tool":
			if name, _ := v["name"].(string); name != "" {
				out.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
}

// applyAnthropicThinkingToOpenAI approximates an Anthropic thinking budget with
// an OpenAI reasoning_effort level. Budgets are mapped conservatively: small
// budgets stay cheap, large budgets request maximum effort.
func applyAnthropicThinkingToOpenAI(out *core.OpenAIRequest, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var cfg struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return
	}
	if cfg.Type == "disabled" {
		return
	}
	if cfg.Type != "enabled" && cfg.Type != "adaptive" {
		return
	}
	budget := cfg.BudgetTokens
	if cfg.Type == "adaptive" && budget <= 0 {
		out.ReasoningEffort = "high"
		return
	}
	switch {
	case budget <= 0:
		out.ReasoningEffort = "medium"
	case budget <= 2048:
		out.ReasoningEffort = "low"
	case budget <= 12288:
		out.ReasoningEffort = "medium"
	default:
		out.ReasoningEffort = "high"
	}
}

func applyAnthropicMetadataToOpenAI(out *core.OpenAIRequest, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var meta struct {
		UserID any `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return
	}
	switch v := meta.UserID.(type) {
	case string:
		out.User = strings.TrimSpace(v)
	case map[string]any:
		// Some clients nest account/session identifiers; a plain string field
		// keeps OpenAI's user param valid.
		if s, ok := v["id"].(string); ok {
			out.User = strings.TrimSpace(s)
		}
	}
}

func parseAnthropicSystemStrict(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []core.AnthBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("invalid Anthropic system content: %w", err)
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("unsupported Anthropic system block type %q", block.Type)
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(block.Text)
	}
	return strings.TrimSpace(b.String()), nil
}

func anthImageURL(src map[string]any) string {
	if src == nil {
		return ""
	}
	typ, _ := src["type"].(string)
	if typ == "base64" {
		mt, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if mt != "" && data != "" {
			return "data:" + mt + ";base64," + data
		}
	}
	if typ == "url" {
		u, _ := src["url"].(string)
		return u
	}
	if u, _ := src["url"].(string); u != "" {
		return u
	}
	return ""
}

// OpenAIResponseToAnthropic converts an OpenAI Chat Completions response into
// an Anthropic Messages response. Malformed tool arguments are preserved in a
// {"_raw": ...} object instead of failing the whole turn: the data reaches the
// client unchanged and the conversation stays recoverable.
func OpenAIResponseToAnthropic(in core.OpenAIResponse, requestedModel string, nm *NameMap) (core.AnthResponse, error) {
	stop := "end_turn"
	content := []core.AnthContentBlock{}
	if len(in.Choices) > 0 {
		c := in.Choices[0]
		content = append(content, openAIResponseContentToAnthBlocks(c.Message.Content)...)
		for _, tc := range c.Message.ToolCalls {
			obj := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &obj); err != nil {
					obj = map[string]any{"_raw": tc.Function.Arguments}
				}
			}
			content = append(content, core.AnthContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  nm.Reverse(tc.Function.Name),
				Input: obj,
			})
		}
		if c.FinishReason != nil {
			stop = finishReasonToAnthropicStop(*c.FinishReason)
		}
	}
	if len(content) == 0 {
		content = append(content, core.AnthContentBlock{Type: "text", Text: ""})
	}
	usage := core.AnthUsage{
		InputTokens:  in.Usage.PromptTokens,
		OutputTokens: in.Usage.CompletionTokens,
	}
	if in.Usage.PromptTokensDetails != nil {
		usage.CacheReadInputTokens = in.Usage.PromptTokensDetails.CachedTokens
	}
	return core.AnthResponse{
		ID:         in.ID,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      requestedModel,
		StopReason: &stop,
		Usage:      usage,
	}, nil
}

func finishReasonToAnthropicStop(reason string) string {
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	case "stop_sequence":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// openAIResponseContentToAnthBlocks handles both plain-string and content-part
// array responses. reasoning_content is intentionally not surfaced as an
// unsigned Anthropic thinking block: fabricated thinking blocks without a
// signature would poison the client's history when replayed against native
// Anthropic upstreams.
func openAIResponseContentToAnthBlocks(v any) []core.AnthContentBlock {
	out := []core.AnthContentBlock{}
	switch c := v.(type) {
	case string:
		if c != "" {
			out = append(out, core.AnthContentBlock{Type: "text", Text: c})
		}
	case []any:
		var text strings.Builder
		for _, raw := range c {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text":
				txt, _ := m["text"].(string)
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(txt)
			}
		}
		if text.Len() > 0 {
			out = append(out, core.AnthContentBlock{Type: "text", Text: text.String()})
		}
	}
	return out
}
