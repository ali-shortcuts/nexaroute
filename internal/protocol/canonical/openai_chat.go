package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func timeNow() int64 { return time.Now().Unix() }

// ---------- OpenAI Chat Completions <-> Canonical ----------

// DecodeOpenAIChatRequest converts an OpenAI Chat Completions request into the IR.
func DecodeOpenAIChatRequest(in core.OpenAIRequest, clientModel string) Request {
	out := Request{
		Model:          clientModel,
		Stream:         in.Stream,
		ClientProtocol: "openai_chat",
		Stop:           NormalizeStop(in.Stop),
	}
	if in.MaxTokens > 0 {
		out.MaxOutputTokens = in.MaxTokens
	}
	if in.MaxCompletionTokens > 0 && in.MaxCompletionTokens > out.MaxOutputTokens {
		out.MaxOutputTokens = in.MaxCompletionTokens
	}
	out.Temperature, out.TopP = in.Temperature, in.TopP
	if in.Seed != nil {
		if out.Metadata == nil {
			out.Metadata = map[string]any{}
		}
		out.Metadata["seed"] = *in.Seed
	}
	if in.FrequencyPenalty != nil || in.PresencePenalty != nil {
		if out.Metadata == nil {
			out.Metadata = map[string]any{}
		}
		if in.FrequencyPenalty != nil {
			out.Metadata["frequency_penalty"] = *in.FrequencyPenalty
		}
		if in.PresencePenalty != nil {
			out.Metadata["presence_penalty"] = *in.PresencePenalty
		}
	}
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			out.System = append(out.System, openAIContentToParts(m.Content, true)...)
		case "assistant":
			msg := Message{Role: RoleAssistant}
			msg.Parts = append(msg.Parts, openAIContentToParts(m.Content, false)...)
			if strings.TrimSpace(m.ReasoningContent) != "" {
				msg.Parts = append(msg.Parts, Part{Type: PartThinking, Thinking: &Thinking{Text: m.ReasoningContent}})
			}
			for _, tc := range m.ToolCalls {
				msg.Parts = append(msg.Parts, Part{Type: PartToolCall, ToolCall: &ToolCall{
					ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
				}})
			}
			out.Messages = append(out.Messages, msg)
		case "tool":
			out.Messages = append(out.Messages, Message{Role: RoleTool, Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{ToolUseID: m.ToolCallID, Content: openAIContentToText(m.Content)},
			}}})
		default:
			out.Messages = append(out.Messages, Message{Role: RoleUser, Parts: openAIContentToParts(m.Content, false)})
		}
	}
	for _, t := range in.Tools {
		out.Tools = append(out.Tools, ToolDef{
			Name: t.Function.Name, Description: t.Function.Description,
			Parameters: marshalSchema(t.Function.Parameters),
		})
	}
	out.ToolChoice = decodeOpenAIToolChoice(in.ToolChoice)
	out.ParallelToolCalls = in.ParallelToolCalls
	if strings.TrimSpace(in.ReasoningEffort) != "" {
		out.Reasoning = &Reasoning{Effort: in.ReasoningEffort}
	} else if len(in.Reasoning) > 0 && string(in.Reasoning) != "null" {
		out.Reasoning = &Reasoning{Effort: "medium"}
	}
	return out
}

func openAIContentToText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		parts := openAIContentToParts(v, false)
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == PartText && p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
}

func openAIContentToParts(v any, system bool) []Part {
	switch c := v.(type) {
	case nil:
		return nil
	case string:
		if c == "" {
			return nil
		}
		return []Part{{Type: PartText, Text: c}}
	case []any:
		parts := make([]Part, 0, len(c))
		for _, raw := range c {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text":
				txt, _ := m["text"].(string)
				if txt != "" {
					parts = append(parts, Part{Type: PartText, Text: txt})
				}
			case "image_url", "input_image":
				img := decodeOpenAIImage(m)
				if img != nil {
					parts = append(parts, Part{Type: PartImage, Image: img})
				}
			default:
				if b, err := json.Marshal(m); err == nil && !system {
					parts = append(parts, Part{Type: PartText, Text: string(b)})
				}
			}
		}
		return parts
	default:
		if b, err := json.Marshal(v); err == nil {
			return []Part{{Type: PartText, Text: string(b)}}
		}
		return nil
	}
}

func decodeOpenAIImage(m map[string]any) *Image {
	var raw any
	if u, ok := m["image_url"]; ok {
		if s, ok := u.(string); ok {
			raw = s
		} else if mm, ok := u.(map[string]any); ok {
			raw = mm["url"]
		}
	} else if u, ok := m["url"]; ok {
		raw = u
	}
	s, _ := raw.(string)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "data:") {
		if parsed := parseDataURL(s); parsed != nil {
			return &Image{MediaType: parsed.mediaType, Data: parsed.data}
		}
	}
	return &Image{URL: s}
}

func decodeOpenAIToolChoice(v any) *ToolChoice {
	switch c := v.(type) {
	case string:
		switch c {
		case "auto", "none", "required":
			return &ToolChoice{Mode: c}
		}
	case map[string]any:
		typ, _ := c["type"].(string)
		if typ == "function" {
			if fn, ok := c["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					return &ToolChoice{Mode: "tool", Name: name}
				}
			}
		}
		if typ == "tool" {
			if name, _ := c["name"].(string); name != "" {
				return &ToolChoice{Mode: "tool", Name: name}
			}
		}
	}
	return nil
}

// EncodeOpenAIChatRequest builds an OpenAI Chat Completions payload from the IR.
func EncodeOpenAIChatRequest(in Request, upstreamModel string, includeUsage bool) (core.OpenAIRequest, error) {
	out := core.OpenAIRequest{
		Model:       upstreamModel,
		Stream:      in.Stream,
		Tools:       []core.OpenAITool{},
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	if in.MaxOutputTokens > 0 {
		out.MaxTokens = in.MaxOutputTokens
	}
	if len(in.Stop) == 1 {
		out.Stop = in.Stop[0]
	} else if len(in.Stop) > 1 {
		out.Stop = in.Stop
	}
	if in.Stream && includeUsage {
		out.StreamOptions = json.RawMessage(`{"include_usage":true}`)
	}
	if seed, ok := intFromMetadata(in.Metadata, "seed"); ok {
		out.Seed = &seed
	}
	if fp, ok := floatFromMetadata(in.Metadata, "frequency_penalty"); ok {
		out.FrequencyPenalty = &fp
	}
	if pp, ok := floatFromMetadata(in.Metadata, "presence_penalty"); ok {
		out.PresencePenalty = &pp
	}
	appendSystem := func(parts []Part) {
		if len(parts) == 0 {
			return
		}
		txt := partsToText(parts)
		if strings.TrimSpace(txt) != "" {
			out.Messages = append(out.Messages, core.OpenAIMessage{Role: "system", Content: txt})
		}
	}
	appendSystem(in.System)
	for _, m := range in.Messages {
		switch m.Role {
		case RoleAssistant:
			msg := core.OpenAIMessage{Role: "assistant"}
			var textParts []map[string]any
			var calls []core.OpenAIToolCall
			var plain strings.Builder
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					if p.Text != "" {
						textParts = append(textParts, map[string]any{"type": "text", "text": p.Text})
						plain.WriteString(p.Text)
					}
				case PartToolCall:
					if p.ToolCall != nil && p.ToolCall.Name != "" {
						calls = append(calls, core.OpenAIToolCall{
							ID: p.ToolCall.ID, Type: "function",
							Function: core.OpenAIFunctionCall{Name: p.ToolCall.Name, Arguments: p.ToolCall.Arguments},
						})
					}
				case PartThinking:
					if p.Thinking != nil && strings.TrimSpace(p.Thinking.Text) != "" {
						msg.ReasoningContent = p.Thinking.Text
					}
				}
			}
			if len(calls) > 0 {
				if len(textParts) > 0 {
					msg.Content = textParts
				}
				msg.ToolCalls = calls
			} else if len(textParts) == 1 {
				msg.Content = plain.String()
			} else if len(textParts) > 1 {
				msg.Content = textParts
			} else {
				msg.Content = ""
			}
			out.Messages = append(out.Messages, msg)
		case RoleTool:
			if m.Parts == nil || m.Parts[0].ToolResult == nil {
				continue
			}
			tr := m.Parts[0].ToolResult
			out.Messages = append(out.Messages, core.OpenAIMessage{
				Role: "tool", ToolCallID: tr.ToolUseID,
				Content: toolResultToOpenAIContent(tr),
			})
		default:
			parts := m.Parts
			if len(parts) == 0 {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: "user", Content: ""})
				continue
			}
			if len(parts) == 1 && parts[0].Type == PartText {
				out.Messages = append(out.Messages, core.OpenAIMessage{Role: "user", Content: parts[0].Text})
				continue
			}
			out.Messages = append(out.Messages, core.OpenAIMessage{Role: "user", Content: canonicalPartsToOpenAIContent(parts)})
		}
	}
	for _, t := range in.Tools {
		params := map[string]any{"type": "object"}
		if len(t.Parameters) > 0 {
			var parsed map[string]any
			if json.Unmarshal(t.Parameters, &parsed) == nil && parsed != nil {
				params = parsed
			}
		}
		out.Tools = append(out.Tools, core.OpenAITool{
			Type:     "function",
			Function: core.OpenAIFunction{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	if in.ToolChoice != nil {
		switch in.ToolChoice.Mode {
		case "auto", "none", "required":
			out.ToolChoice = in.ToolChoice.Mode
		case "tool":
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": in.ToolChoice.Name}}
		}
	}
	if in.ParallelToolCalls != nil {
		p := *in.ParallelToolCalls
		out.ParallelToolCalls = &p
	}
	if in.Reasoning != nil && !in.Reasoning.Disabled {
		effort := in.Reasoning.Effort
		if effort == "" {
			effort = "medium"
		}
		out.ReasoningEffort = effort
	}
	if in.ResponseFormat != nil {
		switch in.ResponseFormat.Kind {
		case FormatJSONObject:
			out.ResponseFormatRaw = map[string]any{"type": "json_object"}
		case FormatJSONSchema:
			rf := map[string]any{"type": "json_schema"}
			schemaPart := map[string]any{}
			if len(in.ResponseFormat.Schema) > 0 {
				var schema map[string]any
				if json.Unmarshal(in.ResponseFormat.Schema, &schema) == nil {
					schemaPart["schema"] = schema
				}
			}
			if in.ResponseFormat.Name != "" {
				schemaPart["name"] = in.ResponseFormat.Name
			}
			rf["json_schema"] = schemaPart
			out.ResponseFormatRaw = rf
		}
	}
	if len(out.Messages) == 0 {
		return out, fmt.Errorf("no messages after translation")
	}
	return out, nil
}

func toolResultToOpenAIContent(tr *ToolResult) any {
	content := tr.Content
	if tr.IsError {
		content = "[tool error] " + content
	}
	return content
}

func canonicalPartsToOpenAIContent(parts []Part) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case PartText:
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case PartImage:
			if p.Image == nil {
				continue
			}
			u := p.Image.URL
			if u == "" && p.Image.Data != "" {
				mt := p.Image.MediaType
				if mt == "" {
					mt = "image/png"
				}
				u = "data:" + mt + ";base64," + p.Image.Data
			}
			if u != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "text", "text": ""})
	}
	return out
}

func partsToText(parts []Part) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == PartText && p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func intFromMetadata(meta map[string]any, key string) (int64, bool) {
	if meta == nil {
		return 0, false
	}
	switch v := meta[key].(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
}

func floatFromMetadata(meta map[string]any, key string) (float64, bool) {
	if meta == nil {
		return 0, false
	}
	switch v := meta[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

// DecodeOpenAIChatResponse converts an OpenAI Chat Completions response body
// into the IR.
func DecodeOpenAIChatResponse(b []byte) (Response, error) {
	if err := ValidateOpenAIChatResponseJSON(b); err != nil {
		return Response{}, err
	}
	var in core.OpenAIResponse
	if err := json.Unmarshal(b, &in); err != nil {
		return Response{}, fmt.Errorf("invalid OpenAI response: %w", err)
	}
	out := Response{ID: in.ID, Model: in.Model, StopReason: StopEndTurn, Raw: append(json.RawMessage(nil), b...)}
	if len(in.Choices) > 0 {
		c := in.Choices[0]
		switch v := c.Message.Content.(type) {
		case string:
			if v != "" {
				out.Blocks = append(out.Blocks, Block{Type: PartText, Text: v})
			}
		case []any:
			var sb strings.Builder
			for _, raw := range v {
				m, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := m["type"].(string)
				if typ == "text" || typ == "output_text" {
					if txt, _ := m["text"].(string); txt != "" {
						if sb.Len() > 0 {
							sb.WriteByte('\n')
						}
						sb.WriteString(txt)
					}
				}
			}
			if sb.Len() > 0 {
				out.Blocks = append(out.Blocks, Block{Type: PartText, Text: sb.String()})
			}
		}
		if strings.TrimSpace(c.Message.ReasoningContent) != "" {
			out.Blocks = append(out.Blocks, Block{Type: PartThinking, Thinking: &Thinking{Text: c.Message.ReasoningContent}})
		}
		for _, tc := range c.Message.ToolCalls {
			out.Blocks = append(out.Blocks, Block{Type: PartToolCall, ToolCall: &ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			}})
		}
		if c.FinishReason != nil {
			out.StopReason = MapStopReason(*c.FinishReason)
		}
	}
	out.Usage = Usage{InputTokens: in.Usage.PromptTokens, OutputTokens: in.Usage.CompletionTokens}
	if in.Usage.PromptTokensDetails != nil {
		out.Usage.CacheReadTokens = in.Usage.PromptTokensDetails.CachedTokens
	}
	if in.Usage.CompletionTokensDetails != nil {
		out.Usage.ReasoningTokens = in.Usage.CompletionTokensDetails.ReasoningTokens
	}
	if out.Blocks == nil {
		out.Blocks = []Block{}
	}
	return out, nil
}

// EncodeOpenAIChatResponse builds an OpenAI Chat Completions response from the IR.
func EncodeOpenAIChatResponse(in Response, requestedModel string) core.OpenAIResponse {
	finish := "stop"
	switch in.StopReason {
	case StopToolUse:
		finish = "tool_calls"
	case StopMaxTokens:
		finish = "length"
	case StopRefusal:
		finish = "content_filter"
	case StopStopSequence:
		finish = "stop"
	}
	msg := core.OpenAIMessage{Role: "assistant"}
	var text strings.Builder
	for _, b := range in.Blocks {
		switch b.Type {
		case PartText:
			if b.Text != "" {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(b.Text)
			}
		case PartThinking:
			if b.Thinking != nil && b.Thinking.Text != "" {
				msg.ReasoningContent = b.Thinking.Text
			}
		case PartToolCall:
			if b.ToolCall != nil && b.ToolCall.Name != "" {
				id := b.ToolCall.ID
				if id == "" {
					id = fmt.Sprintf("call_%d", len(msg.ToolCalls))
				}
				msg.ToolCalls = append(msg.ToolCalls, core.OpenAIToolCall{
					ID: id, Type: "function",
					Function: core.OpenAIFunctionCall{Name: b.ToolCall.Name, Arguments: b.ToolCall.Arguments},
				})
			}
		}
	}
	msg.Content = text.String()
	model := in.Model
	if requestedModel != "" {
		model = requestedModel
	}
	return core.OpenAIResponse{
		ID: in.ID, Object: "chat.completion", Created: timeNow(), Model: model,
		Choices: []core.OpenAIChoice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage: core.OpenAIUsage{
			PromptTokens: in.Usage.InputTokens, CompletionTokens: in.Usage.OutputTokens, TotalTokens: in.Usage.InputTokens + in.Usage.OutputTokens,
			PromptTokensDetails:     &core.OpenAIUsageDetail{CachedTokens: in.Usage.CacheReadTokens},
			CompletionTokensDetails: &core.OpenAIUsageDetail{ReasoningTokens: in.Usage.ReasoningTokens},
		},
	}
}

var messageClockNanos = time.Now().UnixNano()

// messageClock yields compact monotonic ids for stream/message identifiers.
func messageClock() int64 {
	n := time.Now().UnixNano()
	if n <= messageClockNanos {
		n = messageClockNanos + 1
	}
	messageClockNanos = n
	return n / 1000000
}
