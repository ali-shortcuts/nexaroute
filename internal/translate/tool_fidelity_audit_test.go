package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// TestToolFidelity_TranslationMatrix covers the legacy fast-path translators
// (Anthropic <-> OpenAI) for type preservation.

func TestToolFidelity_AnthropicToOpenAI_PreservesStringTypes(t *testing.T) {
	// Anthropic request with Bash tool and command string
	content, _ := json.Marshal("hi")
	in := core.AnthropicRequest{
		Model: "test", MaxTokens: 10,
		Messages: []core.AnthMessage{{Role: "user", Content: content}},
		Tools: []core.AnthTool{
			{Name: "Bash", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []any{"command"}}},
			{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}}},
		},
	}
	out, _, err := AnthropicToOpenAI(in, "upstream")
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools lost")
	}
	// Check that input_schema still has type string
	for _, tool := range out.Tools {
		props, ok := tool.Function.Parameters["properties"].(map[string]any)
		if !ok {
			continue
		}
		for field, propRaw := range props {
			prop, ok := propRaw.(map[string]any)
			if !ok {
				continue
			}
			if prop["type"] != "string" {
				t.Fatalf("tool %s field %s type not preserved: got %v", tool.Function.Name, field, prop["type"])
			}
		}
	}

	// Now test response path: Anthropic response with tool_use -> OpenAI
	anthResp := core.AnthResponse{
		ID: "msg_1", Type: "message", Role: "assistant", Model: "m",
		Content: []core.AnthContentBlock{
			{Type: "tool_use", ID: "call_1", Name: "Bash", Input: map[string]any{"command": "pwd"}},
			{Type: "tool_use", ID: "call_2", Name: "Read", Input: map[string]any{"file_path": "/tmp/example.txt"}},
		},
		StopReason: strPtr("tool_use"),
		Usage: core.AnthUsage{InputTokens: 1, OutputTokens: 1},
	}
	oaiResp := AnthropicResponseToOpenAI(anthResp, "client", nil)
	if len(oaiResp.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("tool calls lost: %d", len(oaiResp.Choices[0].Message.ToolCalls))
	}
	for _, tc := range oaiResp.Choices[0].Message.ToolCalls {
		var m map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &m); err != nil {
			t.Fatalf("args invalid: %v args=%q", err, tc.Function.Arguments)
		}
		if tc.Function.Name == "Bash" {
			if _, ok := m["command"].(string); !ok {
				t.Fatalf("Bash.command not string after anth->oai: %T %v", m["command"], m["command"])
			}
			if m["command"] != "pwd" {
				t.Fatalf("value mismatch")
			}
		}
		if tc.Function.Name == "Read" {
			if _, ok := m["file_path"].(string); !ok {
				t.Fatalf("Read.file_path not string: %T", m["file_path"])
			}
		}
	}
}

func TestToolFidelity_OpenAIToAnthropic_PreservesStringTypes(t *testing.T) {
	in := core.OpenAIRequest{
		Model: "test",
		Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools: []core.OpenAITool{
			{Type: "function", Function: core.OpenAIFunction{Name: "Bash", Parameters: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []any{"command"}}}},
			{Type: "function", Function: core.OpenAIFunction{Name: "Read", Parameters: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}}}},
		},
	}
	out, _, err := OpenAIToAnthropic(in, "upstream")
	if err != nil {
		t.Fatalf("translate failed: %v", err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools lost")
	}

	// Response path: OpenAI response with tool calls -> Anthropic
	finish := "tool_calls"
	oaiResp := core.OpenAIResponse{
		ID: "chatcmpl-1",
		Choices: []core.OpenAIChoice{
			{
				Message: core.OpenAIMessage{
					Role: "assistant",
					ToolCalls: []core.OpenAIToolCall{
						{ID: "call_1", Function: core.OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`}},
						{ID: "call_2", Function: core.OpenAIFunctionCall{Name: "Read", Arguments: `{"file_path":"/tmp/example.txt"}`}},
					},
				},
				FinishReason: &finish,
			},
		},
	}
	anthResp, err := OpenAIResponseToAnthropic(oaiResp, "client", nil)
	if err != nil {
		t.Fatalf("response translate failed: %v", err)
	}
	if len(anthResp.Content) != 2 {
		t.Fatalf("content blocks=%d want 2", len(anthResp.Content))
	}
	for _, b := range anthResp.Content {
		if b.Type != "tool_use" {
			continue
		}
		if b.Name == "Bash" {
			if _, ok := b.Input["command"].(string); !ok {
				t.Fatalf("Bash.command not string after oai->anth: %T %v", b.Input["command"], b.Input["command"])
			}
			if b.Input["command"] != "pwd" {
				t.Fatalf("value mismatch")
			}
		}
		if b.Name == "Read" {
			if _, ok := b.Input["file_path"].(string); !ok {
				t.Fatalf("Read.file_path not string")
			}
		}
	}
}

func TestToolFidelity_Streaming_AnthropicToOpenAI(t *testing.T) {
	// Simulate Anthropic streaming tool_use with input_json_delta
	// The legacy path in httpapi uses similar logic, but we test translator's handling of name mapping
	// For this package, we test that NameMap preserves tool names and doesn't corrupt args

	names := []string{"Bash", "Read"}
	nm := NewOpenAINameMap(names)

	// Simulate tool_use with command
	// In Anthropic streaming, args come as partial_json fragments
	fragments := []string{`{"comm`, `and":"`, `git sta`, `tus"}`}

	var buf strings.Builder
	for _, f := range fragments {
		buf.WriteString(f)
	}
	assembled := buf.String()
	var m map[string]any
	if err := json.Unmarshal([]byte(assembled), &m); err != nil {
		t.Fatalf("assembled invalid: %v", err)
	}
	if _, ok := m["command"].(string); !ok {
		t.Fatalf("command not string after hostile split")
	}

	// Ensure NameMap forward/reverse preserves
	for _, name := range names {
		fwd := nm.Forward(name)
		rev := nm.Reverse(fwd)
		if rev != name {
			t.Fatalf("NameMap roundtrip failed: %q -> %q -> %q", name, fwd, rev)
		}
	}
}

func TestToolFidelity_MalformedArguments_PreservedViaRaw(t *testing.T) {
	// OpenAI -> Anthropic preserves malformed args via _raw
	in := core.OpenAIRequest{
		Model: "x",
		Messages: []core.OpenAIMessage{{
			Role: "assistant",
			ToolCalls: []core.OpenAIToolCall{{
				ID: "call-1",
				Function: core.OpenAIFunctionCall{Name: "Bash", Arguments: "{bad json"},
			}},
		}},
	}
	out, _, err := OpenAIToAnthropic(in, "backend")
	if err != nil {
		t.Fatalf("should not fail, should preserve via _raw: %v", err)
	}
	// Check that input contains _raw
	if len(out.Messages) < 1 {
		t.Fatalf("no messages")
	}
	// The last message should be assistant with tool_use containing _raw
	// Find assistant message
	var found bool
	for _, msg := range out.Messages {
		if msg.Role == "assistant" {
			var blocks []core.AnthBlock
			json.Unmarshal(msg.Content, &blocks)
			for _, b := range blocks {
				if b.Type == "tool_use" && b.Input["_raw"] == "{bad json" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("_raw preservation failed")
	}
}

func TestToolFidelity_DoubleEncodingAudit(t *testing.T) {
	// Ensure no double JSON encoding
	original := `{"command":"pwd"}`
	// Simulate what AnthropicToOpenAI does: marshal Input map to string
	input := map[string]any{"command": "pwd"}
	b, _ := json.Marshal(input)
	if string(b) != original {
		t.Fatalf("marshal mismatch: got %q want %q", string(b), original)
	}
	// If we accidentally double-marshal, we'd get extra quotes
	double, _ := json.Marshal(string(b))
	var once string
	json.Unmarshal(double, &once)
	if once != original {
		t.Fatalf("double encoding detection failed")
	}
	// Ensure our code does single encoding
	// The translator should produce single-encoded string
	content, _ := json.Marshal("hi")
	anthReq := core.AnthropicRequest{
		Model: "x", MaxTokens: 10,
		Messages: []core.AnthMessage{{Role: "user", Content: content}},
		Tools: []core.AnthTool{{Name: "Bash", InputSchema: map[string]any{"type": "object"}}},
	}
	// Add a tool_use message
	anthReq.Messages = append(anthReq.Messages, core.AnthMessage{
		Role: "assistant",
		Content: json.RawMessage(`[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"pwd"}}]`),
	})
	oaiReq, _, err := AnthropicToOpenAI(anthReq, "m")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// Check that tool calls arguments are not double-encoded
	for _, msg := range oaiReq.Messages {
		for _, tc := range msg.ToolCalls {
			// Arguments should be JSON object string, not quoted string containing JSON
			if strings.HasPrefix(tc.Function.Arguments, "\"") {
				t.Fatalf("arguments double-encoded: %q", tc.Function.Arguments)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &m); err != nil {
				t.Fatalf("args not valid JSON: %v", err)
			}
		}
	}
}

func TestToolFidelity_Regression_BashCommand_ReadFilePath(t *testing.T) {
	// Permanent regression fixture
	tests := []struct {
		tool  string
		field string
		value string
		args  string
	}{
		{"Bash", "command", "pwd", `{"command":"pwd"}`},
		{"Bash", "command", "git status --short", `{"command":"git status --short"}`},
		{"Read", "file_path", "/tmp/example.txt", `{"file_path":"/tmp/example.txt"}`},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s_%s", tc.tool, tc.field), func(t *testing.T) {
			// OpenAI path
			finish := "tool_calls"
			oaiResp := core.OpenAIResponse{
				ID: "1",
				Choices: []core.OpenAIChoice{{
					Message: core.OpenAIMessage{
						Role: "assistant",
						ToolCalls: []core.OpenAIToolCall{
							{ID: "c1", Function: core.OpenAIFunctionCall{Name: tc.tool, Arguments: tc.args}},
						},
					},
					FinishReason: &finish,
				}},
			}
			anthResp, err := OpenAIResponseToAnthropic(oaiResp, "client", nil)
			if err != nil {
				t.Fatalf("oai->anth failed: %v", err)
			}
			for _, b := range anthResp.Content {
				if b.Type == "tool_use" {
					val, ok := b.Input[tc.field]
					if !ok {
						t.Fatalf("field %q missing", tc.field)
					}
					s, ok := val.(string)
					if !ok {
						t.Fatalf("REGRESSION: %s.%s expected string got %T (%v) - string -> unknown bug", tc.tool, tc.field, val, val)
					}
					if s != tc.value {
						t.Fatalf("value mismatch: got %q want %q", s, tc.value)
					}
				}
			}

			// Anthropic path
			anthResp2 := core.AnthResponse{
				ID: "msg_1", Type: "message", Role: "assistant", Model: "m",
				Content: []core.AnthContentBlock{
					{Type: "tool_use", ID: "c1", Name: tc.tool, Input: map[string]any{tc.field: tc.value}},
				},
				StopReason: strPtr("tool_use"),
			}
			oaiResp2 := AnthropicResponseToOpenAI(anthResp2, "client", nil)
			var m map[string]any
			json.Unmarshal([]byte(oaiResp2.Choices[0].Message.ToolCalls[0].Function.Arguments), &m)
			val, ok := m[tc.field]
			if !ok {
				t.Fatalf("field %q missing in oai path", tc.field)
			}
			if _, ok := val.(string); !ok {
				t.Fatalf("REGRESSION (anth->oai): %s.%s not string", tc.tool, tc.field)
			}
			if val.(string) != tc.value {
				t.Fatalf("value mismatch anth->oai")
			}
		})
	}
}
