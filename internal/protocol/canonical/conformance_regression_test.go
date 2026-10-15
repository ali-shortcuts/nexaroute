package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

// M5-2 conformance regressions: tool-result conversations must
// survive every decoder -> encoder pairing. The IR has two shapes
// for tool results (dedicated RoleTool messages from OpenAI-family
// decoders; PartToolResult parts inside user messages from the
// Anthropic decoder) and every encoder must handle both.

func toolResultIRFromResponses(t *testing.T) Request {
	t.Helper()
	req, err := DecodeResponsesRequest(ResponsesRequest{
		Model: "m",
		Input: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"t1","name":"lookup","arguments":"{\"q\":\"x\"}"},{"type":"function_call_output","call_id":"t1","output":"tool-output"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestConformanceToolResultToAnthropicRoles(t *testing.T) {
	// Responses-shaped IR (RoleTool messages) encoded for Anthropic
	// must not contain role "tool" — the real API rejects it.
	in := toolResultIRFromResponses(t)
	encoded, err := EncodeAnthropicRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"role":"tool"`) {
		t.Fatalf("anthropic payload contains role tool: %s", body)
	}
	if !strings.Contains(string(body), `"type":"tool_result"`) || !strings.Contains(string(body), "tool-output") {
		t.Fatalf("tool result lost toward anthropic: %s", body)
	}
}

func TestConformanceToolResultToResponsesAndOpenAI(t *testing.T) {
	// Anthropic-shaped IR (tool result inside a user message)
	// encoded for the OpenAI families must keep the result.
	in := Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "t1", Name: "lookup", Arguments: `{"q":"x"}`}}}},
		{Role: RoleUser, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "t1", Content: "tool-output"}}}},
	}}
	respPayload, err := EncodeResponsesRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(respPayload), `"function_call_output"`) || !strings.Contains(string(respPayload), "tool-output") {
		t.Fatalf("tool result lost toward responses: %s", respPayload)
	}
	chatPayload, err := EncodeOpenAIChatRequest(in, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	chatJSON, err := json.Marshal(chatPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chatJSON), `"role":"tool"`) || !strings.Contains(string(chatJSON), "tool-output") {
		t.Fatalf("tool result lost toward openai chat: %s", chatJSON)
	}
}
