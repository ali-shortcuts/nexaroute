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
