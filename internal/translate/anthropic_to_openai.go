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
	if sys := core.ParseSystem(in.System); sys != "" {
		out.Messages = append(out.Messages, core.OpenAIMessage{Role: "system", Content: sys})
	}
	for _, m := range in.Messages {
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
				if u := anthImageURL(b.Source); u != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				}
			case "tool_use":
				if m.Role != "assistant" {
					continue
				}
				arg, _ := json.Marshal(b.Input)
				assistantCalls = append(assistantCalls, core.OpenAIToolCall{ID: b.ID, Type: "function", Function: core.OpenAIFunctionCall{Name: b.Name, Arguments: string(arg)}})
			case "tool_result":
				flush()
				var content any = ""
				if len(b.Content) > 0 {
					if json.Unmarshal(b.Content, &content) != nil {
						content = string(b.Content)
					}
				}
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: content})
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
