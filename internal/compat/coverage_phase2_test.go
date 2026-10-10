package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoveragePhase2ProbeHelpers(t *testing.T) {
	if got := mapsToCapability(""); got != "" {
		t.Fatal(got)
	}
	if got := mapsToCapability("unknown"); got != "" {
		t.Fatal(got)
	}
	if got := firstText(completionShape{Choices: []struct {
		Message struct {
			Content   any              `json:"content"`
			ToolCalls []map[string]any `json:"tool_calls"`
		} `json:"message"`
	}{{Message: struct {
		Content   any              `json:"content"`
		ToolCalls []map[string]any `json:"tool_calls"`
	}{Content: []any{map[string]any{"text": "hello"}}}}}}); got != "hello" {
		t.Fatalf("firstText=%q", got)
	}
	if got := firstText(completionShape{}); got != "" {
		t.Fatal(got)
	}
	payload := toolProbePayload("m", "required", true)
	var tool map[string]any
	if err := json.Unmarshal(payload, &tool); err != nil {
		t.Fatal(err)
	}
	if tool["tool_choice"] != "required" || len(tool["tools"].([]any)) != 2 {
		t.Fatalf("tool payload: %+v", tool)
	}
}

func TestCoveragePhase2ResponsesRequestConversion(t *testing.T) {
	_, err := chatProbePayloadToResponses([]byte("{"))
	if err == nil {
		t.Fatal("invalid JSON accepted")
	}
	in := `{"model":"m","max_completion_tokens":7,"reasoning_effort":"high","tool_choice":"required","response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"},"strict":true}},"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}},{"type":"custom"}],"messages":[{"role":"system","content":"Be brief"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://x/i.png"}}]},{"role":"assistant","tool_calls":[{"id":"call-1","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-1","content":{"ok":true}}]}`
	out, err := chatProbePayloadToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_output_tokens"] != float64(7) || got["instructions"] != "Be brief" {
		t.Fatalf("converted request: %+v", got)
	}
	input := got["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input count=%d %#v", len(input), input)
	}
	if _, err := chatProbePayloadToResponses([]byte(`{"messages":[{"role":"user","content":""}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestCoveragePhase2ResponsesContentAndResponseConversion(t *testing.T) {
	if got := responsesProbeContent(""); got != nil {
		t.Fatal(got)
	}
	if got := responsesProbeContent(42); got != nil {
		t.Fatal(got)
	}
	parts := responsesProbeContent([]any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "input_image", "image_url": "u"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "v"}}, map[string]any{"type": "other"}})
	if len(parts) != 3 {
		t.Fatalf("parts=%#v", parts)
	}
	body := `{"id":"r1","model":"m","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3}}`
	out, err := responsesProbeBodyToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	choices := got["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || !strings.Contains(choice["message"].(map[string]any)["content"].(string), "hello") {
		t.Fatalf("response conversion: %#v", got)
	}
	if _, err := responsesProbeBodyToChat([]byte("bad")); err == nil {
		t.Fatal("invalid response accepted")
	}
	fallback, err := responsesProbeBodyToChat([]byte(`{"id":"r","status":"incomplete","output":[]}`))
	if err != nil || !strings.Contains(string(fallback), "length") {
		t.Fatalf("fallback=%s err=%v", fallback, err)
	}
}
