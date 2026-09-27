package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// This file implements the NEXAROUTE TOOL-CALL FIDELITY AUDIT as described in
// the regression task. It covers:
//
// - Schema type preservation
// - Streaming argument assembly with hostile boundaries
// - Long-context stress
// - Multiple tool calls
// - Provider translation matrix
// - Malformed fail-closed
// - Double-encoding audit
// - Size boundaries
// - Exact Bash.command / Read.file_path regression

// ---------- Helpers ----------

func assertJSONStringField(t *testing.T, args string, field string, expected string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("args not valid JSON: %v args=%q", err, args)
	}
	val, ok := m[field]
	if !ok {
		t.Fatalf("field %q missing in args %s", field, args)
	}
	s, ok := val.(string)
	if !ok {
		t.Fatalf("field %q type is %T not string (value=%v) args=%s", field, val, val, args)
	}
	if s != expected {
		t.Fatalf("field %q value %q != expected %q", field, s, expected)
	}
}

func assertArgType(t *testing.T, args string, field string, expectedType string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("args not valid JSON: %v", err)
	}
	val, ok := m[field]
	if !ok {
		t.Fatalf("field %q missing", field)
	}
	switch expectedType {
	case "string":
		if _, ok := val.(string); !ok {
			t.Fatalf("field %q expected string got %T (%v)", field, val, val)
		}
	case "integer":
		// JSON numbers decode as float64
		f, ok := val.(float64)
		if !ok {
			t.Fatalf("field %q expected integer got %T", field, val)
		}
		if f != float64(int(f)) {
			t.Fatalf("field %q expected integer got float %v", field, f)
		}
	case "number":
		if _, ok := val.(float64); !ok {
			t.Fatalf("field %q expected number got %T", field, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			t.Fatalf("field %q expected boolean got %T", field, val)
		}
	case "array":
		if _, ok := val.([]any); !ok {
			t.Fatalf("field %q expected array got %T", field, val)
		}
	case "object":
		if _, ok := val.(map[string]any); !ok {
			t.Fatalf("field %q expected object got %T", field, val)
		}
	}
}

// ---------- 2. Schema Type Preservation ----------

func TestToolFidelity_SchemaTypePreservation(t *testing.T) {
	// Define tool schemas similar to Bash and Read
	bashSchema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)
	readSchema := json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)

	toolDefs := []ToolDef{
		{Name: "Bash", Description: "Run command", Parameters: bashSchema},
		{Name: "Read", Description: "Read file", Parameters: readSchema},
	}

	// Test cases for various types
	tests := []struct {
		name      string
		tool      string
		args      string
		checkField string
		checkType string
		checkValue string
	}{
		{"bash string", "Bash", `{"command":"pwd"}`, "command", "string", "pwd"},
		{"read file_path", "Read", `{"file_path":"/tmp/example.txt"}`, "file_path", "string", "/tmp/example.txt"},
		{"integer", "Bash", `{"command":"echo","count":42}`, "count", "integer", ""},
		{"number", "Bash", `{"command":"echo","ratio":3.14}`, "ratio", "number", ""},
		{"boolean", "Bash", `{"command":"echo","flag":true}`, "flag", "boolean", ""},
		{"array", "Bash", `{"command":"echo","items":["a","b"]}`, "items", "array", ""},
		{"object", "Bash", `{"command":"echo","meta":{"k":"v"}}`, "meta", "object", ""},
		{"enum preserved", "Bash", `{"command":"ls","mode":"fast"}`, "mode", "string", "fast"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Find tool def
			var td *ToolDef
			for i := range toolDefs {
				if toolDefs[i].Name == tc.tool {
					td = &toolDefs[i]
					break
				}
			}
			if td == nil && tc.tool == "Bash" {
				// For generic type tests, use bash def plus extra props
				td = &toolDefs[0]
			}
			// Canonical representation: ToolCall with Arguments as string
			call := ToolCall{Name: tc.tool, Arguments: tc.args, ID: "call_1"}

			// Ensure Arguments remains string type through canonical encode/decode
			// Simulate Anthropic decode/encode
			anthInput := map[string]any{}
			if err := json.Unmarshal([]byte(call.Arguments), &anthInput); err != nil {
				t.Fatalf("unmarshal args: %v", err)
			}
			// Re-encode via canonical Anthropic path
			b, _ := json.Marshal(anthInput)
			// Should still be valid JSON object
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("re-marshaled args invalid: %v", err)
			}

			// Check type preservation
			if tc.checkType != "" {
				assertArgType(t, call.Arguments, tc.checkField, tc.checkType)
			}
			if tc.checkValue != "" {
				assertJSONStringField(t, call.Arguments, tc.checkField, tc.checkValue)
			}

			// Verify through full translation: canonical -> OpenAI -> canonical -> Anthropic
			req := Request{
				Model: "test",
				Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}},
				Tools: []ToolDef{{Name: call.Name, Parameters: td.Parameters}},
			}
			// Encode to OpenAI
			oaiReq, err := EncodeOpenAIChatRequest(Request{
				Model: "test", Messages: req.Messages, Tools: req.Tools,
			}, "upstream", false)
			if err != nil {
				t.Fatalf("encode openai: %v", err)
			}
			// Check tool schema preserved
			if len(oaiReq.Tools) != 1 {
				t.Fatalf("tools lost")
			}
			// Now test response path: OpenAI tool call -> canonical -> Anthropic
			oaiRespBody := fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				tc.tool, tc.args)
			// Need to properly escape args for JSON string
			// Use json.Marshal for arguments string
			argsJSON, _ := json.Marshal(tc.args)
			oaiRespBody2 := fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				tc.tool, string(argsJSON))

			canResp, err := DecodeOpenAIChatResponse([]byte(oaiRespBody2))
			if err != nil {
				t.Fatalf("decode openai resp: %v body=%s err=%v", err, oaiRespBody2, err)
			}
			if len(canResp.Blocks) != 1 {
				t.Fatalf("blocks=%d", len(canResp.Blocks))
			}
			if canResp.Blocks[0].ToolCall == nil {
				t.Fatalf("tool call nil")
			}
			// Arguments must still be string type (in canonical, it's string containing JSON)
			if canResp.Blocks[0].ToolCall.Arguments != tc.args {
				// Allow whitespace differences? Check semantic equality
				var a1, a2 map[string]any
				json.Unmarshal([]byte(canResp.Blocks[0].ToolCall.Arguments), &a1)
				json.Unmarshal([]byte(tc.args), &a2)
				// Compare via JSON marshal
				b1, _ := json.Marshal(a1)
				b2, _ := json.Marshal(a2)
				if string(b1) != string(b2) {
					t.Fatalf("args mismatch: got %q want %q", canResp.Blocks[0].ToolCall.Arguments, tc.args)
				}
			}
			// Check field type preserved
			if tc.checkType != "" {
				assertArgType(t, canResp.Blocks[0].ToolCall.Arguments, tc.checkField, tc.checkType)
			}
			_ = oaiRespBody
		})
	}
}

// ---------- 3. Streaming Argument Assembly ----------

func TestToolFidelity_StreamingFragmentation(t *testing.T) {
	// Simulate hostile chunk boundaries
	cases := []struct {
		name  string
		args  string
		splits []int // split positions
	}{
		{"simple split", `{"command":"git status"}`, []int{2, 5, 10}},
		{"escaped quotes", `{"command":"echo \"hello\""}`, []int{5, 12, 18}},
		{"backslashes", `{"command":"echo \\n test"}`, []int{3, 8, 15}},
		{"unicode", `{"command":"echo café 🚀"}`, []int{4, 10, 15}},
		{"multiline", `{"command":"line1\nline2\nline3"}`, []int{5, 12, 20}},
		{"nested JSON", `{"command":"ls","meta":{"a":1,"b":[2,3]}}`, []int{6, 14, 25}},
		{"large string", fmt.Sprintf(`{"command":%q}`, strings.Repeat("x", 1000)), []int{10, 500, 800}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate streaming assembly
			fragments := []string{}
			prev := 0
			for _, split := range tc.splits {
				if split > len(tc.args) {
					continue
				}
				if split > prev {
					fragments = append(fragments, tc.args[prev:split])
					prev = split
				}
			}
			if prev < len(tc.args) {
				fragments = append(fragments, tc.args[prev:])
			}

			// Reassemble via buffer (must be done BEFORE validation)
			var buf strings.Builder
			for _, f := range fragments {
				buf.WriteString(f)
			}
			assembled := buf.String()
			if assembled != tc.args {
				t.Fatalf("reassembly failed: got %q want %q", assembled, tc.args)
			}

			// Validate complete JSON only after reassembly
			var m map[string]any
			if err := json.Unmarshal([]byte(assembled), &m); err != nil {
				t.Fatalf("assembled invalid JSON: %v", err)
			}

			// Ensure string field remains string
			if cmd, ok := m["command"]; ok {
				if _, ok := cmd.(string); !ok {
					t.Fatalf("command field type corrupted: %T", cmd)
				}
			}

			// Test through canonical streaming decoder
			// Build OpenAI SSE chunks with tool call args split
			var chunks []string
			// First chunk: role + tool start
			chunks = append(chunks, `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
			// Tool start
			chunks = append(chunks, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`)
			// Now deltas for each fragment
			for _, frag := range fragments {
				// Need to JSON-escape fragment for inclusion in SSE
				fragJSON, _ := json.Marshal(frag)
				chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]}}]}`, string(fragJSON))
				chunks = append(chunks, chunk)
			}
			chunks = append(chunks, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
			chunks = append(chunks, `[DONE]`)

			var events []StreamEvent
			for _, c := range chunks {
				evs, _, err := DecodeOpenAIStreamChunk(c)
				if err != nil {
					t.Fatalf("decode chunk %q: %v", c, err)
				}
				events = append(events, evs...)
			}

			// Reassemble from events
			var assembledFromEvents strings.Builder
			for _, ev := range events {
				if ev.Type == StreamToolDelta {
					assembledFromEvents.WriteString(ev.ArgsDelta)
				}
			}
			if assembledFromEvents.String() != tc.args {
				t.Fatalf("event reassembly mismatch: got %q want %q", assembledFromEvents.String(), tc.args)
			}

			// Validate final assembled
			var final map[string]any
			if err := json.Unmarshal([]byte(assembledFromEvents.String()), &final); err != nil {
				t.Fatalf("final assembled invalid: %v", err)
			}
			if _, ok := final["command"].(string); !ok {
				t.Fatalf("command not string after streaming: %T", final["command"])
			}
		})
	}
}

// ---------- 4. Long-Context Stress ----------

func TestToolFidelity_LongContext(t *testing.T) {
	// Simulate large request contexts
	sizes := []int{1024, 16 * 1024, 64 * 1024, 256 * 1024}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			// Build large system prompt
			largeText := strings.Repeat("x", size)

			req := Request{
				Model:  "test",
				System: []Part{{Type: PartText, Text: largeText}},
				Messages: []Message{
					{Role: RoleUser, Parts: []Part{{Type: PartText, Text: largeText}}},
				},
				Tools: []ToolDef{
					{Name: "Bash", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
					{Name: "Read", Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)},
				},
			}

			// Encode to various protocols
			_, err := EncodeOpenAIChatRequest(req, "upstream", false)
			if err != nil {
				t.Fatalf("encode openai failed at size %d: %v", size, err)
			}
			_, err = EncodeAnthropicRequest(req, "upstream")
			if err != nil {
				t.Fatalf("encode anthropic failed at size %d: %v", size, err)
			}

			// Now test tool call preservation after large context
			toolCalls := []struct {
				name string
				args string
			}{
				{"Bash", `{"command":"pwd"}`},
				{"Bash", `{"command":"git status --short"}`},
				{"Read", `{"file_path":"/tmp/example.txt"}`},
			}

			for _, tc := range toolCalls {
				// Simulate response after large context
				argsJSON, _ := json.Marshal(tc.args)
				oaiBody := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
					tc.name, string(argsJSON))
				canResp, err := DecodeOpenAIChatResponse([]byte(oaiBody))
				if err != nil {
					t.Fatalf("decode after large context size %d failed: %v", size, err)
				}
				if len(canResp.Blocks) != 1 || canResp.Blocks[0].ToolCall == nil {
					t.Fatalf("tool call lost after large context")
				}
				// Verify type preservation
				var m map[string]any
				if err := json.Unmarshal([]byte(canResp.Blocks[0].ToolCall.Arguments), &m); err != nil {
					t.Fatalf("args invalid after large context: %v", err)
				}
				// Check that string fields remain strings
				for k, v := range m {
					// For Bash and Read, expected string
					if k == "command" || k == "file_path" {
						if _, ok := v.(string); !ok {
							t.Fatalf("after large context %d, field %q type %T not string (value %v)", size, k, v, v)
						}
					}
				}
			}
		})
	}
}

// ---------- 5. Multiple Tool Call Stress ----------

func TestToolFidelity_MultipleCalls(t *testing.T) {
	// 100 sequential Bash calls
	for i := 0; i < 100; i++ {
		args := fmt.Sprintf(`{"command":"echo %d"}`, i)
		argsJSON, _ := json.Marshal(args)
		body := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c%d","type":"function","function":{"name":"Bash","arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			i, string(argsJSON))
		resp, err := DecodeOpenAIChatResponse([]byte(body))
		if err != nil {
			t.Fatalf("call %d decode failed: %v", i, err)
		}
		if len(resp.Blocks) != 1 {
			t.Fatalf("call %d blocks=%d", i, len(resp.Blocks))
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(resp.Blocks[0].ToolCall.Arguments), &m); err != nil {
			t.Fatalf("call %d args invalid: %v", i, err)
		}
		if _, ok := m["command"].(string); !ok {
			t.Fatalf("call %d command not string: %T", i, m["command"])
		}
	}

	// Mixed sequence: Bash, Read, Bash, Read, Bash
	seq := []struct {
		name string
		args string
	}{
		{"Bash", `{"command":"pwd"}`},
		{"Read", `{"file_path":"/tmp/a.txt"}`},
		{"Bash", `{"command":"ls"}`},
		{"Read", `{"file_path":"/tmp/b.txt"}`},
		{"Bash", `{"command":"whoami"}`},
	}
	for idx, tc := range seq {
		argsJSON, _ := json.Marshal(tc.args)
		body := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c%d","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			idx, tc.name, string(argsJSON))
		resp, err := DecodeOpenAIChatResponse([]byte(body))
		if err != nil {
			t.Fatalf("mixed seq %d failed: %v", idx, err)
		}
		var m map[string]any
		json.Unmarshal([]byte(resp.Blocks[0].ToolCall.Arguments), &m)
		// Verify no contamination from previous call
		if tc.name == "Bash" {
			if _, ok := m["command"]; !ok {
				t.Fatalf("seq %d Bash missing command", idx)
			}
			if _, ok := m["file_path"]; ok {
				t.Fatalf("seq %d Bash contaminated with file_path from previous Read", idx)
			}
		} else {
			if _, ok := m["file_path"]; !ok {
				t.Fatalf("seq %d Read missing file_path", idx)
			}
			if _, ok := m["command"]; ok {
				t.Fatalf("seq %d Read contaminated with command", idx)
			}
		}
	}

	// Parallel tool calls
	parallelBody := `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"pwd\"}"}},{"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	// Need proper escaping
	// Use raw with Marshal
	args1 := `{"command":"pwd"}`
	args2 := `{"file_path":"/tmp/x.txt"}`
	a1, _ := json.Marshal(args1)
	a2, _ := json.Marshal(args2)
	parallelBody2 := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":%s}},{"id":"c2","type":"function","function":{"name":"Read","arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		string(a1), string(a2))
	resp, err := DecodeOpenAIChatResponse([]byte(parallelBody2))
	if err != nil {
		t.Fatalf("parallel decode failed: %v", err)
	}
	if len(resp.Blocks) != 2 {
		t.Fatalf("parallel blocks=%d want 2", len(resp.Blocks))
	}
	for _, b := range resp.Blocks {
		var m map[string]any
		json.Unmarshal([]byte(b.ToolCall.Arguments), &m)
		if b.ToolCall.Name == "Bash" {
			if _, ok := m["command"].(string); !ok {
				t.Fatalf("parallel Bash command not string")
			}
		}
		if b.ToolCall.Name == "Read" {
			if _, ok := m["file_path"].(string); !ok {
				t.Fatalf("parallel Read file_path not string")
			}
		}
	}
	_ = parallelBody
}

// ---------- 6. Provider Translation Matrix ----------

func TestToolFidelity_ProviderTranslationMatrix(t *testing.T) {
	// Test matrix: incoming tool schema -> canonical -> provider -> response -> canonical -> client
	toolDefs := []ToolDef{
		{Name: "Bash", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
		{Name: "Read", Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)},
	}

	testCases := []struct {
		name string
		args string
		tool string
	}{
		{"bash pwd", `{"command":"pwd"}`, "Bash"},
		{"bash git status", `{"command":"git status --short"}`, "Bash"},
		{"read file", `{"file_path":"/tmp/example.txt"}`, "Read"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Canonical -> OpenAI -> Canonical
			req := Request{
				Model: "test",
				Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}},
				Tools: toolDefs,
			}
			oaiReq, err := EncodeOpenAIChatRequest(req, "upstream", false)
			if err != nil {
				t.Fatalf("encode openai: %v", err)
			}
			// Check schema preserved
			if len(oaiReq.Tools) != 2 {
				t.Fatalf("tools lost in openai encode")
			}

			// Canonical -> Anthropic -> Canonical
			anthReq, err := EncodeAnthropicRequest(req, "upstream")
			if err != nil {
				t.Fatalf("encode anthropic: %v", err)
			}
			if len(anthReq.Tools) != 2 {
				t.Fatalf("tools lost in anthropic encode")
			}

			// Now response path: test both OpenAI and Anthropic upstream responses
			// OpenAI upstream -> Anthropic client
			argsJSON, _ := json.Marshal(tc.args)
			oaiBody := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				tc.tool, string(argsJSON))
			canResp, err := DecodeOpenAIChatResponse([]byte(oaiBody))
			if err != nil {
				t.Fatalf("decode openai: %v", err)
			}
			anthResp := EncodeAnthropicResponse(canResp, "client")
			if len(anthResp.Content) == 0 {
				t.Fatalf("anthropic encode empty")
			}
			var toolBlock *struct {
				Name  string
				Input map[string]any
			}
			for _, b := range anthResp.Content {
				if b.Type == "tool_use" {
					toolBlock = &struct {
						Name  string
						Input map[string]any
					}{Name: b.Name, Input: b.Input}
					break
				}
			}
			if toolBlock == nil {
				t.Fatalf("no tool_use in anthropic response")
			}
			// TYPE PRESERVATION: PASS
			if tc.tool == "Bash" {
				if _, ok := toolBlock.Input["command"].(string); !ok {
					t.Fatalf("TYPE PRESERVATION FAIL: command expected string got %T", toolBlock.Input["command"])
				}
			}
			if tc.tool == "Read" {
				if _, ok := toolBlock.Input["file_path"].(string); !ok {
					t.Fatalf("TYPE PRESERVATION FAIL: file_path expected string got %T", toolBlock.Input["file_path"])
				}
			}

			// Anthropic upstream -> OpenAI client
			// Build Anthropic response body
			var anthInput map[string]any
			json.Unmarshal([]byte(tc.args), &anthInput)
			anthInputJSON, _ := json.Marshal(anthInput)
			anthBody := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"c1","name":%q,"input":%s}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
				tc.tool, string(anthInputJSON))
			canResp2, err := DecodeAnthropicResponse([]byte(anthBody))
			if err != nil {
				t.Fatalf("decode anthropic: %v", err)
			}
			oaiResp := EncodeOpenAIChatResponse(canResp2, "client")
			if len(oaiResp.Choices[0].Message.ToolCalls) != 1 {
				t.Fatalf("openai tool calls lost")
			}
			var m map[string]any
			json.Unmarshal([]byte(oaiResp.Choices[0].Message.ToolCalls[0].Function.Arguments), &m)
			if tc.tool == "Bash" {
				if _, ok := m["command"].(string); !ok {
					t.Fatalf("TYPE PRESERVATION FAIL (anth->oai): command %T", m["command"])
				}
			}
			if tc.tool == "Read" {
				if _, ok := m["file_path"].(string); !ok {
					t.Fatalf("TYPE PRESERVATION FAIL (anth->oai): file_path %T", m["file_path"])
				}
			}
		})
	}
}

// ---------- 9. Malformed Fail-Closed ----------

func TestToolFidelity_MalformedFailClosed(t *testing.T) {
	malformed := []struct {
		name string
		args string
		shouldFail bool
	}{
		{"null command", `{"command":null}`, true},
		{"object command", `{"command":{}}`, true},
		{"array command", `{"command":[]}`, true},
		{"number command", `{"command":42}`, true},
		{"bool command", `{"command":true}`, true},
		{"empty object", `{}`, true},
		{"invalid json", `{"command":`, true},
		{"truncated", `{"command":"pwd"`, true},
		{"duplicate keys", `{"command":"pwd","command":"ls"}`, false}, // JSON allows duplicate, last wins, still string
		{"double encoded", `"{\"command\":\"pwd\"}"`, true}, // args encoded twice as JSON string
		{"wrapper object", `{"value":"pwd"}`, true}, // unexpected wrapper
	}

	// Tool schema expects command string
	schema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)

	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			// Attempt to validate against schema
			err := ValidateToolCall(ToolDef{Name: "Bash", Parameters: schema}, ToolCall{Name: "Bash", Arguments: tc.args})
			if tc.shouldFail {
				if err == nil {
					t.Fatalf("expected validation failure for %q but got PASS", tc.args)
				}
				// Ensure error contains diagnostic info
				if !strings.Contains(err.Error(), "command") && !strings.Contains(err.Error(), "Bash") {
					t.Logf("warning: error doesn't contain field/tool diagnostics: %v", err)
				}
			} else {
				// For duplicate keys case, it should still be valid string
				if err != nil {
					t.Fatalf("unexpected failure for %q: %v", tc.args, err)
				}
			}
		})
	}
}

// ---------- 10. Double-Encoding Audit ----------

func TestToolFidelity_DoubleEncoding(t *testing.T) {
	original := `{"command":"pwd"}`
	// Single encoding: string -> JSON string field
	singleJSON, _ := json.Marshal(original) // "\"{\\\"command\\\":\\\"pwd\\\"}\""
	// Double encoding
	doubleJSON, _ := json.Marshal(string(singleJSON))

	// Simulate what would happen if we double-encode
	var decodedOnce string
	if err := json.Unmarshal(singleJSON, &decodedOnce); err != nil {
		t.Fatalf("single decode failed: %v", err)
	}
	if decodedOnce != original {
		t.Fatalf("single decode mismatch")
	}

	// Double-encoded should NOT equal original when decoded once
	var decodedDouble string
	if err := json.Unmarshal(doubleJSON, &decodedDouble); err != nil {
		t.Fatalf("double decode failed: %v", err)
	}
	// decodedDouble is still JSON string of original, not the object
	if decodedDouble == original {
		t.Fatalf("double encoding not detected")
	}

	// Now test canonical path doesn't double-encode
	call := ToolCall{Name: "Bash", Arguments: original}
	// Encode to OpenAI (should be string field, not double-encoded)
	req := Request{
		Model: "test",
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}},
		Tools: []ToolDef{{Name: "Bash", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
	}
	oaiReq, err := EncodeOpenAIChatRequest(req, "m", false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// For response path, ensure Arguments is preserved as single-encoded string
	argsJSON, _ := json.Marshal(original)
	body := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		string(argsJSON))
	resp, err := DecodeOpenAIChatResponse([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Blocks[0].ToolCall.Arguments != original {
		t.Fatalf("double-encoding detected: got %q want %q", resp.Blocks[0].ToolCall.Arguments, original)
	}

	// Ensure no accidental double stringify via Marshal(Marshal(...))
	// This would produce extra quotes
	if strings.HasPrefix(resp.Blocks[0].ToolCall.Arguments, "\"") {
		t.Fatalf("arguments appears double-encoded (starts with quote): %q", resp.Blocks[0].ToolCall.Arguments)
	}

	_ = call
	_ = oaiReq
}

// ---------- 15. Size Boundaries ----------

func TestToolFidelity_SizeBoundaries(t *testing.T) {
	sizes := []int{1024, 16 * 1024, 64 * 1024, 256 * 1024, 1024 * 1024}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%d", size), func(t *testing.T) {
			cmd := strings.Repeat("a", size)
			args := fmt.Sprintf(`{"command":%q}`, cmd)
			// Should parse and preserve
			var m map[string]any
			if err := json.Unmarshal([]byte(args), &m); err != nil {
				t.Fatalf("size %d unmarshal failed: %v", size, err)
			}
			if s, ok := m["command"].(string); !ok || len(s) != size {
				t.Fatalf("size %d preservation failed", size)
			}

			// Through canonical
			argsJSON, _ := json.Marshal(args)
			body := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				string(argsJSON))
			resp, err := DecodeOpenAIChatResponse([]byte(body))
			if err != nil {
				t.Fatalf("size %d decode failed: %v", size, err)
			}
			var m2 map[string]any
			json.Unmarshal([]byte(resp.Blocks[0].ToolCall.Arguments), &m2)
			if s, ok := m2["command"].(string); !ok || len(s) != size {
				t.Fatalf("size %d canonical preservation failed: got len %d", size, len(s))
			}
		})
	}
}

// ---------- 16. Regression Fixture ----------

func TestToolFidelity_Regression_StringMustNotBecomeUnknown(t *testing.T) {
	// Exact regression for observed bug: Bash.command and Read.file_path must remain string
	cases := []struct {
		tool string
		args string
		field string
		value string
	}{
		{"Bash", `{"command":"pwd"}`, "command", "pwd"},
		{"Bash", `{"command":"git status --short"}`, "command", "git status --short"},
		{"Read", `{"file_path":"/tmp/example.txt"}`, "file_path", "/tmp/example.txt"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%s", tc.tool, tc.field), func(t *testing.T) {
			// Test through all supported paths
			// 1. OpenAI Chat -> Canonical -> Anthropic
			argsJSON, _ := json.Marshal(tc.args)
			oaiBody := fmt.Sprintf(`{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				tc.tool, string(argsJSON))
			canResp, err := DecodeOpenAIChatResponse([]byte(oaiBody))
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			// typeof(command) == string
			var m map[string]any
			if err := json.Unmarshal([]byte(canResp.Blocks[0].ToolCall.Arguments), &m); err != nil {
				t.Fatalf("args invalid: %v", err)
			}
			val, ok := m[tc.field]
			if !ok {
				t.Fatalf("field %q missing", tc.field)
			}
			// Must NOT be unknown, null, {}, [], etc.
			if val == nil {
				t.Fatalf("field %q is null, expected string", tc.field)
			}
			if _, ok := val.(string); !ok {
				t.Fatalf("REGRESSION FAIL: %s.%s type is %T not string (value=%v) - this is the 'string -> unknown' bug", tc.tool, tc.field, val, val)
			}
			if val.(string) != tc.value {
				t.Fatalf("value mismatch: got %q want %q", val.(string), tc.value)
			}

			// 2. Anthropic -> Canonical -> OpenAI
			anthBody := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"c1","name":%q,"input":%s}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
				tc.tool, tc.args)
			canResp2, err := DecodeAnthropicResponse([]byte(anthBody))
			if err != nil {
				t.Fatalf("anthropic decode failed: %v", err)
			}
			oaiResp := EncodeOpenAIChatResponse(canResp2, "client")
			var m2 map[string]any
			if err := json.Unmarshal([]byte(oaiResp.Choices[0].Message.ToolCalls[0].Function.Arguments), &m2); err != nil {
				t.Fatalf("openai args invalid: %v", err)
			}
			val2, ok := m2[tc.field]
			if !ok {
				t.Fatalf("field %q missing in openai path", tc.field)
			}
			if _, ok := val2.(string); !ok {
				t.Fatalf("REGRESSION FAIL (anth->oai): %s.%s type %T not string", tc.tool, tc.field, val2)
			}
			if val2.(string) != tc.value {
				t.Fatalf("value mismatch anth->oai: got %q want %q", val2, tc.value)
			}

			// 3. Streaming path
			// Simulate streaming with hostile splits
			splits := []int{2, 5, 10}
			frags := []string{}
			prev := 0
			for _, sp := range splits {
				if sp < len(tc.args) && sp > prev {
					frags = append(frags, tc.args[prev:sp])
					prev = sp
				}
			}
			frags = append(frags, tc.args[prev:])

			var chunks []string
			chunks = append(chunks, `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
			chunks = append(chunks, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":%q,"arguments":""}}]}}]}`, tc.tool))
			for _, frag := range frags {
				fj, _ := json.Marshal(frag)
				chunks = append(chunks, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]}}]}`, string(fj)))
			}
			chunks = append(chunks, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)

			var buf strings.Builder
			for _, ch := range chunks {
				evs, _, err := DecodeOpenAIStreamChunk(ch)
				if err != nil {
					t.Fatalf("stream decode: %v", err)
				}
				for _, ev := range evs {
					if ev.Type == StreamToolDelta {
						buf.WriteString(ev.ArgsDelta)
					}
				}
			}
			if buf.String() != tc.args {
				t.Fatalf("streaming reassembly failed: got %q want %q", buf.String(), tc.args)
			}
			var m3 map[string]any
			json.Unmarshal([]byte(buf.String()), &m3)
			if _, ok := m3[tc.field].(string); !ok {
				t.Fatalf("REGRESSION FAIL (streaming): %s.%s not string", tc.tool, tc.field)
			}
		})
	}
}

// ---------- Additional: Enum, Nullable, Optional ----------

func TestToolFidelity_EnumAndOptional(t *testing.T) {
	// Enum preservation
	args := `{"command":"ls","mode":"fast"}`
	var m map[string]any
	json.Unmarshal([]byte(args), &m)
	if m["mode"] != "fast" {
		t.Fatalf("enum not preserved")
	}

	// Nullable optional field
	args2 := `{"command":"pwd","timeout":null}`
	var m2 map[string]any
	json.Unmarshal([]byte(args2), &m2)
	if m2["timeout"] != nil {
		t.Fatalf("null not preserved")
	}
	// Optional missing field should be ok
	args3 := `{"command":"pwd"}`
	var m3 map[string]any
	json.Unmarshal([]byte(args3), &m3)
	if _, ok := m3["timeout"]; ok {
		t.Fatalf("optional field should be missing")
	}
}
