package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// Canonical IR -> client response encoders. Gemini (and future non-OpenAI,
// non-Anthropic) upstreams decode into canonical.Response and are rendered
// into the client's protocol here, mirroring the translate package's
// conventions: empty content becomes one empty text block (never fabricated
// text), malformed tool arguments are preserved via {"_raw": ...}, and
// reasoning is surfaced only where the protocol tolerates it.

// ToOpenAIResponse encodes a canonical response into an OpenAI Chat
// Completions response for the requested client-facing model name.
func (r Response) ToOpenAIResponse(model string) core.OpenAIResponse {
	msg := core.OpenAIMessage{Role: "assistant"}
	if r.Text != "" {
		msg.Content = r.Text
	}
	if r.Reasoning != "" {
		msg.ReasoningContent = r.Reasoning
	}
	for i, tc := range r.ToolCalls {
		arg, _ := json.Marshal(tc.Arguments)
		if len(arg) == 0 {
			arg = []byte("{}")
		}
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			id = fmt.Sprintf("call_canon_%d", i)
		}
		msg.ToolCalls = append(msg.ToolCalls, core.OpenAIToolCall{
			ID: id, Type: "function",
			Function: core.OpenAIFunctionCall{Name: tc.Name, Arguments: string(arg)},
		})
	}
	finish := canonicalStopToOpenAI(r.StopReason)
	if len(r.ToolCalls) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	return core.OpenAIResponse{
		ID:      fmt.Sprintf("chatcmpl-canon-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []core.OpenAIChoice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage: core.OpenAIUsage{
			PromptTokens: r.InputTokens, CompletionTokens: r.OutputTokens,
			TotalTokens: r.InputTokens + r.OutputTokens,
		},
	}
}

// ToAnthropicResponse encodes a canonical response into an Anthropic
// Messages response for the requested client-facing model name.
func (r Response) ToAnthropicResponse(model string) core.AnthResponse {
	content := []core.AnthContentBlock{}
	if r.Text != "" {
		content = append(content, core.AnthContentBlock{Type: "text", Text: r.Text})
	}
	for i, tc := range r.ToolCalls {
		args := tc.Arguments
		if len(args) == 0 && strings.TrimSpace(tc.RawArgs) != "" {
			args = map[string]any{}
			if err := json.Unmarshal([]byte(tc.RawArgs), &args); err != nil {
				args = map[string]any{"_raw": tc.RawArgs}
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			id = fmt.Sprintf("toolu_canon_%d", i)
		}
		content = append(content, core.AnthContentBlock{
			Type: "tool_use", ID: id, Name: tc.Name, Input: args,
		})
	}
	if len(content) == 0 {
		content = append(content, core.AnthContentBlock{Type: "text", Text: ""})
	}
	stop := canonicalStopToAnthropic(r.StopReason)
	if len(r.ToolCalls) > 0 && stop == "end_turn" {
		stop = "tool_use"
	}
	return core.AnthResponse{
		ID: fmt.Sprintf("msg_canon_%d", time.Now().UnixNano()), Type: "message", Role: "assistant",
		Content: content, Model: model, StopReason: &stop,
		Usage: core.AnthUsage{InputTokens: r.InputTokens, OutputTokens: r.OutputTokens},
	}
}

// canonicalStopToOpenAI normalizes canonical stop reasons onto the OpenAI
// finish_reason vocabulary.
func canonicalStopToOpenAI(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens":
		return "length"
	case "tool_calls", "tool_use", "function_call":
		return "tool_calls"
	case "content_filter", "refusal":
		return "content_filter"
	case "stop_sequence":
		return "stop"
	case "stop", "end_turn", "":
		return "stop"
	default:
		return "stop"
	}
}

// canonicalStopToAnthropic normalizes canonical stop reasons onto the
// Anthropic stop_reason vocabulary.
func canonicalStopToAnthropic(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool_calls", "tool_use", "function_call":
		return "tool_use"
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter", "refusal":
		return "refusal"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}
