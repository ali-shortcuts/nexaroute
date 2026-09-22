package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ali-shortcuts/universal-llm-gateway/internal/core"
)

func OpenAIToAnthropic(in core.OpenAIRequest, model string) (core.AnthropicRequest, error) {
	out := core.AnthropicRequest{Model: model, MaxTokens: in.MaxTokens, Stream: in.Stream, Temperature: in.Temperature, TopP: in.TopP}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 1024
	}
	var system strings.Builder
	for _, m := range in.Messages {
		if m.Role == "system" {
			for _, b := range openAIContentToAnthBlocks(m.Content) {
				if b["type"] == "text" {
					if system.Len() > 0 {
						system.WriteByte('\n')
					}
					system.WriteString(fmt.Sprint(b["text"]))
				}
			}
			continue
		}
		blocks := openAIContentToAnthBlocks(m.Content)
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				obj := map[string]any{}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &obj)
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": obj})
			}
		}
		role := m.Role
		if m.Role == "tool" {
			role = "user"
			blocks = []map[string]any{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": normalizeToolResultContent(m.Content)}}
		}
		raw, _ := json.Marshal(blocks)
		out.Messages = append(out.Messages, core.AnthMessage{Role: role, Content: raw})
	}
	if system.Len() > 0 {
		out.System, _ = json.Marshal(system.String())
	}
	for _, t := range in.Tools {
		out.Tools = append(out.Tools, core.AnthTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters})
	}
	switch v := in.ToolChoice.(type) {
	case string:
		if v == "required" {
			out.ToolChoice = map[string]any{"type": "any"}
		} else {
			out.ToolChoice = map[string]any{"type": v}
		}
	case map[string]any:
		if f, ok := v["function"].(map[string]any); ok {
			if name, _ := f["name"].(string); name != "" {
				out.ToolChoice = map[string]any{"type": "tool", "name": name}
			}
		}
	}
	if len(out.Messages) == 0 {
		return out, fmt.Errorf("no messages after translation")
	}
	return out, nil
}

func openAIContentToAnthBlocks(content any) []map[string]any {
	out := []map[string]any{}
	switch v := content.(type) {
	case string:
		if v != "" {
			out = append(out, map[string]any{"type": "text", "text": v})
		}
	case []any:
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text":
				txt, _ := m["text"].(string)
				if txt != "" {
					out = append(out, map[string]any{"type": "text", "text": txt})
				}
			case "image_url":
				if src := openAIImageToAnthSource(m["image_url"]); src != nil {
					out = append(out, map[string]any{"type": "image", "source": src})
				}
			}
		}
	case []map[string]any:
		for _, m := range v {
			for _, b := range openAIContentToAnthBlocks([]any{m}) {
				out = append(out, b)
			}
		}
	case nil:
	default:
		if b, err := json.Marshal(v); err == nil {
			out = append(out, map[string]any{"type": "text", "text": string(b)})
		}
	}
	return out
}

func openAIImageToAnthSource(v any) map[string]any {
	var u string
	switch x := v.(type) {
	case string:
		u = x
	case map[string]any:
		u, _ = x["url"].(string)
	}
	if u == "" {
		return nil
	}
	if strings.HasPrefix(u, "data:") {
		rest := strings.TrimPrefix(u, "data:")
		parts := strings.SplitN(rest, ",", 2)
		if len(parts) == 2 && strings.HasSuffix(parts[0], ";base64") {
			return map[string]any{"type": "base64", "media_type": strings.TrimSuffix(parts[0], ";base64"), "data": parts[1]}
		}
	}
	return map[string]any{"type": "url", "url": u}
}

func normalizeToolResultContent(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func AnthropicResponseToOpenAI(in core.AnthResponse, requestedModel string) core.OpenAIResponse {
	msg := core.OpenAIMessage{Role: "assistant"}
	text := ""
	calls := []core.OpenAIToolCall{}
	for _, b := range in.Content {
		if b.Type == "text" {
			text += b.Text
		}
		if b.Type == "tool_use" {
			arg, _ := json.Marshal(b.Input)
			calls = append(calls, core.OpenAIToolCall{ID: b.ID, Type: "function", Function: core.OpenAIFunctionCall{Name: b.Name, Arguments: string(arg)}})
		}
	}
	msg.Content = text
	msg.ToolCalls = calls
	finish := "stop"
	if in.StopReason != nil {
		switch *in.StopReason {
		case "tool_use":
			finish = "tool_calls"
		case "max_tokens":
			finish = "length"
		}
	}
	return core.OpenAIResponse{ID: in.ID, Object: "chat.completion", Model: requestedModel, Choices: []core.OpenAIChoice{{Index: 0, Message: msg, FinishReason: &finish}}, Usage: core.OpenAIUsage{PromptTokens: in.Usage.InputTokens, CompletionTokens: in.Usage.OutputTokens, TotalTokens: in.Usage.InputTokens + in.Usage.OutputTokens}}
}
