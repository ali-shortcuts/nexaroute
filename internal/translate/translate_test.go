package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func TestAnthropicToOpenAITool(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{{"type": "text", "text": "hi"}})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 10, Messages: []core.AnthMessage{{Role: "user", Content: content}}, Tools: []core.AnthTool{{Name: "shell", InputSchema: map[string]any{"type": "object"}}}}
	o, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if o.Model != "backend" || len(o.Tools) != 1 {
		t.Fatalf("bad output %#v", o)
	}
}

func TestAnthropicImageToOpenAIDataURL(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}}})
	in := core.AnthropicRequest{Model: "x", MaxTokens: 8, Messages: []core.AnthMessage{{Role: "user", Content: content}}}
	o, err := AnthropicToOpenAI(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := o.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("bad parts %#v", o.Messages[0].Content)
	}
	iu := parts[0]["image_url"].(map[string]any)["url"]
	if iu != "data:image/png;base64,AAAA" {
		t.Fatalf("bad data url %v", iu)
	}
}

func TestOpenAIImageURLToAnthropic(t *testing.T) {
	in := core.OpenAIRequest{Model: "x", MaxTokens: 8, Messages: []core.OpenAIMessage{{Role: "user", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}}}}}}
	a, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(a.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	src := blocks[0]["source"].(map[string]any)
	if src["type"] != "url" {
		t.Fatalf("bad source %#v", src)
	}
}

func TestOpenAIResponseToAnthropicRejectsMalformedToolArguments(t *testing.T) {
	finish := "tool_calls"
	in := core.OpenAIResponse{
		ID: "x",
		Choices: []core.OpenAIChoice{{
			Message: core.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []core.OpenAIToolCall{{
					ID:       "call-1",
					Function: core.OpenAIFunctionCall{Name: "shell", Arguments: "{bad"},
				}},
			},
			FinishReason: &finish,
		}},
	}
	if _, err := OpenAIResponseToAnthropic(in, "m"); err == nil {
		t.Fatal("malformed tool arguments must not be silently converted to an empty object")
	}
}

func TestOpenAIToAnthropicRejectsMalformedToolArguments(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{{
			Role: "assistant",
			ToolCalls: []core.OpenAIToolCall{{
				ID:       "call-1",
				Function: core.OpenAIFunctionCall{Name: "shell", Arguments: "{bad"},
			}},
		}},
	}
	if _, err := OpenAIToAnthropic(in, "backend"); err == nil {
		t.Fatal("malformed historical tool arguments must not be silently converted")
	}
}

func TestOpenAIToAnthropicMapsDeveloperRoleToSystem(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{
			{Role: "developer", Content: "developer policy"},
			{Role: "user", Content: "hello"},
		},
	}
	got, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatal(err)
	}
	var system string
	if err := json.Unmarshal(got.System, &system); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, "developer policy") {
		t.Fatalf("developer message not mapped into Anthropic system context: %q", system)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("unexpected translated messages: %#v", got.Messages)
	}
}

func TestOpenAIToAnthropicRejectsInvalidRolesAndToolIdentity(t *testing.T) {
	cases := []core.OpenAIRequest{
		{Messages: []core.OpenAIMessage{{Role: "nonsense", Content: "x"}}},
		{Messages: []core.OpenAIMessage{{Role: "tool", Content: "x"}}},
		{Messages: []core.OpenAIMessage{{Role: "assistant", ToolCalls: []core.OpenAIToolCall{{Function: core.OpenAIFunctionCall{Name: "f", Arguments: "{}"}}}}}},
	}
	for i, in := range cases {
		if _, err := OpenAIToAnthropic(in, "m"); err == nil {
			t.Fatalf("case %d should have failed", i)
		}
	}
}

func TestAnthropicToOpenAIRejectsInvalidToolStructure(t *testing.T) {
	cases := []core.AnthropicRequest{
		{Messages: []core.AnthMessage{{Role: "invalid", Content: json.RawMessage(`"x"`)}}},
		{Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"tool_use","id":"x","name":"f","input":{}}]`)}}},
		{Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","content":"ok"}]`)}}},
	}
	for i, in := range cases {
		if _, err := AnthropicToOpenAI(in, "m"); err == nil {
			t.Fatalf("case %d should have failed", i)
		}
	}
}

func TestOpenAIToAnthropicRejectsUnsupportedContentParts(t *testing.T) {
	cases := []core.OpenAIRequest{
		{Messages: []core.OpenAIMessage{{Role: "user", Content: []any{map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "x"}}}}}},
		{Messages: []core.OpenAIMessage{{Role: "user", Content: []any{"not-an-object"}}}},
		{Messages: []core.OpenAIMessage{{Role: "user", Content: map[string]any{"type": "text", "text": "x"}}}},
		{Messages: []core.OpenAIMessage{{Role: "system", Content: []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/x.png"}}}}}},
	}
	for i, in := range cases {
		if _, err := OpenAIToAnthropic(in, "m"); err == nil {
			t.Fatalf("case %d should reject unsupported content instead of dropping it", i)
		}
	}
}

func TestAnthropicToOpenAIRejectsUnsupportedContentBlocksAndSystem(t *testing.T) {
	cases := []core.AnthropicRequest{
		{System: json.RawMessage(`{"type":"not-valid-system"}`), Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}},
		{Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"thinking","thinking":"secret"}]`)}}},
		{Messages: []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"","data":""}}]`)}}},
	}
	for i, in := range cases {
		if _, err := AnthropicToOpenAI(in, "m"); err == nil {
			t.Fatalf("case %d should reject unsupported content instead of dropping it", i)
		}
	}
}

func TestOpenAIToAnthropicMapsStopSequences(t *testing.T) {
	cases := []struct {
		stop any
		want []string
	}{
		{"END", []string{"END"}},
		{[]any{"END", "STOP"}, []string{"END", "STOP"}},
		{[]string{"END", "END", "STOP"}, []string{"END", "STOP"}},
		{"", nil},
		{[]any{"  ", 12, "OK"}, []string{"OK"}},
		{nil, nil},
	}
	for i, tc := range cases {
		in := core.OpenAIRequest{Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}}, Stop: tc.stop}
		out, err := OpenAIToAnthropic(in, "m")
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if len(out.StopSequences) != len(tc.want) {
			t.Fatalf("case %d: got %v want %v", i, out.StopSequences, tc.want)
		}
		for j := range tc.want {
			if out.StopSequences[j] != tc.want[j] {
				t.Fatalf("case %d: got %v want %v", i, out.StopSequences, tc.want)
			}
		}
	}
}

func TestOpenAIToAnthropicDropsEmptyMessages(t *testing.T) {
	in := core.OpenAIRequest{
		Messages: []core.OpenAIMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: ""},
			{Role: "assistant", Content: []any{}},
			{Role: "user", Content: "continue"},
		},
	}
	out, err := OpenAIToAnthropic(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("empty messages must be dropped, got %d: %+v", len(out.Messages), out.Messages)
	}
	for _, m := range out.Messages {
		if string(m.Content) == "[]" {
			t.Fatalf("empty content array must never reach the Anthropic API")
		}
	}
	// An assistant message with tool calls must survive even with empty content.
	in2 := core.OpenAIRequest{
		Messages: []core.OpenAIMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []core.OpenAIToolCall{{ID: "t1", Type: "function", Function: core.OpenAIFunctionCall{Name: "f", Arguments: "{}"}}}},
			{Role: "tool", ToolCallID: "t1", Content: "done"},
		},
	}
	out2, err := OpenAIToAnthropic(in2, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(out2.Messages) != 3 {
		t.Fatalf("tool flow messages must be preserved, got %d", len(out2.Messages))
	}
}

func TestAnthropicToOpenAIFlattensToolResultTextBlocks(t *testing.T) {
	in := core.AnthropicRequest{
		Model:     "m",
		MaxTokens: 16,
		Messages: []core.AnthMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"f","input":{}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range out.Messages {
		if m.Role == "tool" {
			found = true
			s, ok := m.Content.(string)
			if !ok {
				t.Fatalf("text-only tool_result must flatten to a string, got %T", m.Content)
			}
			if s != "line one\nline two" {
				t.Fatalf("unexpected flattened content %q", s)
			}
		}
	}
	if !found {
		t.Fatal("tool message missing")
	}
}

func TestAnthropicToOpenAIToolResultImageBecomesImageURLPart(t *testing.T) {
	in := core.AnthropicRequest{
		Model:     "m",
		MaxTokens: 16,
		Messages: []core.AnthMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"screenshot","input":{}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Messages {
		if m.Role == "tool" {
			parts, ok := m.Content.([]any)
			if !ok || len(parts) != 1 {
				t.Fatalf("image tool_result must become one content part, got %T", m.Content)
			}
			part := parts[0].(map[string]any)
			if part["type"] != "image_url" {
				t.Fatalf("expected image_url part, got %v", part)
			}
			return
		}
	}
	t.Fatal("tool message missing")
}

func TestAnthropicResponseToOpenAISetsCreated(t *testing.T) {
	resp := core.AnthResponse{ID: "msg_1", Content: []core.AnthContentBlock{{Type: "text", Text: "ok"}}}
	out := AnthropicResponseToOpenAI(resp, "gpt")
	if out.Created <= 0 {
		t.Fatalf("translated OpenAI response must carry a created timestamp")
	}
}
