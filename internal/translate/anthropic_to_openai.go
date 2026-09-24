package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func AnthropicToOpenAI(in core.AnthropicRequest, model string) (core.OpenAIRequest, error) {
	out := core.OpenAIRequest{Model: model, MaxTokens: in.MaxTokens, Stream: in.Stream, Temperature: in.Temperature, TopP: in.TopP}
	if len(in.StopSequences) == 1 {
		out.Stop = in.StopSequences[0]
	} else if len(in.StopSequences) > 1 {
		out.Stop = in.StopSequences
	}
	sys, err := parseAnthropicSystemStrict(in.System)
	if err != nil {
		return out, err
	}
	if sys != "" {
		out.Messages = append(out.Messages, core.OpenAIMessage{Role: "system", Content: sys})
	}
	for _, m := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "user" && role != "assistant" {
			return out, fmt.Errorf("unsupported Anthropic message role %q for OpenAI translation", m.Role)
		}
		blocks, err := core.ParseAnthContent(m.Content)
		if err != nil {
			return out, err
		}
		if len(blocks) == 0 {
			out.Messages = append(out.Messages, core.OpenAIMessage{Role: m.Role, Content: ""})
			continue
		}
		parts := []map[string]any{}
		var assistantCalls []core.OpenAIToolCall
		flush := func() {
			if len(parts) > 0 {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: m.Role, Content: parts})
				parts = nil
			}
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			case "image":
				u := anthImageURL(b.Source)
				if u == "" {
					return out, fmt.Errorf("Anthropic image block has no usable source")
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			case "tool_use":
				if role != "assistant" || strings.TrimSpace(b.ID) == "" || strings.TrimSpace(b.Name) == "" {
					return out, fmt.Errorf("Anthropic tool_use requires assistant role, id, and name")
				}
				arg, err := json.Marshal(b.Input)
				if err != nil {
					return out, fmt.Errorf("encode Anthropic tool input: %w", err)
				}
				assistantCalls = append(assistantCalls, core.OpenAIToolCall{ID: b.ID, Type: "function", Function: core.OpenAIFunctionCall{Name: b.Name, Arguments: string(arg)}})
			case "tool_result":
				if role != "user" || strings.TrimSpace(b.ToolUseID) == "" {
					return out, fmt.Errorf("Anthropic tool_result requires user role and tool_use_id")
				}
				flush()
				var content any = ""
				if len(b.Content) > 0 {
					if err := json.Unmarshal(b.Content, &content); err != nil {
						return out, fmt.Errorf("Anthropic tool_result content is invalid JSON: %w", err)
					}
				}
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: normalizeOpenAIToolResultContent(content)})
			default:
				return out, fmt.Errorf("unsupported Anthropic content block type %q for OpenAI translation", b.Type)
			}
		}
		if len(assistantCalls) > 0 {
			var content any = nil
			if len(parts) > 0 {
				content = parts
			}
			out.Messages = append(out.Messages, core.OpenAIMessage{Role: "assistant", Content: content, ToolCalls: assistantCalls})
			parts = nil
		}
		flush()
	}
	for _, t := range in.Tools {
		out.Tools = append(out.Tools, core.OpenAITool{Type: "function", Function: core.OpenAIFunction{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}})
	}
	switch v := in.ToolChoice.(type) {
	case string:
		out.ToolChoice = v
	case map[string]any:
		typ, _ := v["type"].(string)
		if typ == "auto" || typ == "none" {
			out.ToolChoice = typ
		} else if typ == "any" {
			out.ToolChoice = "required"
		} else if typ == "tool" {
			if name, _ := v["name"].(string); name != "" {
				out.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
	if len(out.Messages) == 0 {
		return out, fmt.Errorf("no messages after translation")
	}
	return out, nil
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

func normalizeOpenAIToolResultContent(v any) any {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		// Anthropic tool_result.content is commonly an array of content
		// blocks. Many OpenAI-compatible upstreams only accept string tool
		// content, so text-only arrays are flattened into one string.
		// Non-text blocks are converted to OpenAI content parts instead.
		texts := make([]string, 0, len(t))
		parts := make([]any, 0, len(t))
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				return v
			}
			typ, _ := m["type"].(string)
			if typ == "text" {
				s, _ := m["text"].(string)
				texts = append(texts, s)
				continue
			}
			if typ == "image" {
				if src, ok := m["source"].(map[string]any); ok {
					if u := anthImageURL(src); u != "" {
						parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
						continue
					}
				}
			}
			return v
		}
		if len(parts) > 0 {
			if len(texts) > 0 {
				parts = append(parts, map[string]any{"type": "text", "text": strings.Join(texts, "\n")})
			}
			return parts
		}
		return strings.Join(texts, "\n")
	default:
		return v
	}
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

func OpenAIResponseToAnthropic(in core.OpenAIResponse, requestedModel string) (core.AnthResponse, error) {
	stop := "end_turn"
	content := []core.AnthContentBlock{}
	if len(in.Choices) > 0 {
		c := in.Choices[0]
		if c.Message.Content != nil {
			switch v := c.Message.Content.(type) {
			case string:
				if v != "" {
					content = append(content, core.AnthContentBlock{Type: "text", Text: v})
				}
			}
		}
		for _, tc := range c.Message.ToolCalls {
			obj := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &obj); err != nil {
					return core.AnthResponse{}, fmt.Errorf("tool call %q arguments are invalid JSON: %w", tc.ID, err)
				}
			}
			content = append(content, core.AnthContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: obj})
		}
		if c.FinishReason != nil {
			switch *c.FinishReason {
			case "tool_calls":
				stop = "tool_use"
			case "length":
				stop = "max_tokens"
			case "stop":
				stop = "end_turn"
			}
		}
	}
	if len(content) == 0 {
		content = append(content, core.AnthContentBlock{Type: "text", Text: ""})
	}
	return core.AnthResponse{ID: in.ID, Type: "message", Role: "assistant", Content: content, Model: requestedModel, StopReason: &stop, Usage: core.AnthUsage{InputTokens: in.Usage.PromptTokens, OutputTokens: in.Usage.CompletionTokens}}, nil
}
