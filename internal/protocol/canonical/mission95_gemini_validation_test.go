package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMission95GeminiEncodeEdges(t *testing.T) {
	// Image branches: nil image, remote URL without data (dropped),
	// data-URL parsed, empty data dropped, missing media type defaults.
	req := Request{Messages: []Message{{Role: "user", Parts: []Part{
		{Type: PartImage},
		{Type: PartImage, Image: &Image{URL: "https://x/i.png"}},
		{Type: PartImage, Image: &Image{URL: "data:image/gif;base64,R0lG"}},
		{Type: PartImage, Image: &Image{Data: "AAA"}},
		{Type: PartImage, Image: &Image{MediaType: "image/webp", Data: ""}},
		{Type: PartToolResult},
	}}}}
	payload, model, err := EncodeGeminiRequest(req, "gemini-model")
	if err != nil || model != "gemini-model" {
		t.Fatalf("encode: %v %q", err, model)
	}
	var out GeminiRequest
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 1 || len(out.Contents[0].Parts) != 2 {
		t.Fatalf("parts = %+v", out.Contents)
	}
	if out.Contents[0].Parts[0].InlineData.MimeType != "image/gif" || out.Contents[0].Parts[0].InlineData.Data != "R0lG" {
		t.Fatalf("data-url part: %+v", out.Contents[0].Parts[0])
	}
	if out.Contents[0].Parts[1].InlineData.MimeType != "image/png" {
		t.Fatalf("default media type: %+v", out.Contents[0].Parts[1])
	}
	// Empty message parts -> placeholder empty text part; assistant -> model.
	req = Request{Messages: []Message{
		{Role: "assistant", Parts: nil},
		{Role: "system", Parts: []Part{{Type: PartText, Text: "x"}}},
	}}
	payload, _, err = EncodeGeminiRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Contents[0].Role != "model" || len(out.Contents[0].Parts) != 1 || out.Contents[0].Parts[0].Text != "" {
		t.Fatalf("assistant placeholder: %+v", out.Contents[0])
	}
	if out.Contents[1].Role != "user" {
		t.Fatalf("non-assistant role: %+v", out.Contents[1])
	}
	// Tool result name resolution: found via assistant tool call,
	// empty id -> unknown, unknown id -> id itself, non-assistant ignored.
	req = Request{Messages: []Message{
		{Role: "user", Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "u1", Name: "usertool"}}}},
		{Role: "assistant", Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "a1", Name: "astool"}}}},
		{Role: "user", Parts: []Part{
			{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "a1", Content: `{"ok":1}`}},
			{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "", Content: ""}},
			{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "missing", Content: "plain"}},
			{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "u1", Content: "x"}},
		}},
	}}
	payload, _, err = EncodeGeminiRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	resps := out.Contents[2].Parts
	if len(resps) != 4 {
		t.Fatalf("responses = %d", len(resps))
	}
	if resps[0].FunctionResponse.Name != "astool" || string(resps[0].FunctionResponse.Response) != `{"ok":1}` {
		t.Fatalf("found name: %+v", resps[0])
	}
	if resps[1].FunctionResponse.Name != "unknown" || string(resps[1].FunctionResponse.Response) != `{"result":""}` {
		t.Fatalf("empty content: %+v", resps[1])
	}
	// Plain text is not JSON, so parseOrQuote wraps it in {"result": ...}.
	if resps[2].FunctionResponse.Name != "missing" || string(resps[2].FunctionResponse.Response) != `{"result":"plain"}` {
		t.Fatalf("plain wrapped: %+v", resps[2])
	}
	if resps[3].FunctionResponse.Name != "u1" {
		t.Fatalf("non-assistant call must not resolve: %+v", resps[3])
	}
	if got := toolNameForID(Request{}, ""); got != "unknown" {
		t.Fatalf("empty id: %q", got)
	}
	if got := parseOrQuote("  "); string(got) != `{"result": ""}` {
		t.Fatalf("blank parseOrQuote: %s", got)
	}
	if got := parseOrQuote(`[1,2]`); string(got) != `[1,2]` {
		t.Fatalf("json parseOrQuote: %s", got)
	}
	// ToolChoice modes and the no-tools guard.
	for _, tc := range []struct {
		mode, want string
		allowed    []string
	}{
		{"none", "MODE_NONE", nil}, {"required", "MODE_ANY", nil}, {"any", "MODE_ANY", nil},
		{"tool", "MODE_ANY", []string{"f"}}, {"bogus", "MODE_AUTO", nil},
	} {
		r := Request{Tools: []ToolDef{{Name: "f"}}, ToolChoice: &ToolChoice{Mode: tc.mode, Name: "f"},
			Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}}}
		payload, _, err := EncodeGeminiRequest(r, "m")
		if err != nil {
			t.Fatal(err)
		}
		var o GeminiRequest
		if err := json.Unmarshal(payload, &o); err != nil {
			t.Fatal(err)
		}
		if o.ToolConfig == nil || o.ToolConfig.FunctionCallingConfig.Mode != tc.want {
			t.Fatalf("mode %s: %+v", tc.mode, o.ToolConfig)
		}
		if len(o.ToolConfig.FunctionCallingConfig.AllowedFunctionNames) != len(tc.allowed) {
			t.Fatalf("mode %s allowed: %v", tc.mode, o.ToolConfig.FunctionCallingConfig)
		}
	}
	r := Request{ToolChoice: &ToolChoice{Mode: "auto"}, Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}}}
	payload, _, err = EncodeGeminiRequest(r, "m")
	if err != nil {
		t.Fatal(err)
	}
	var o GeminiRequest
	if err := json.Unmarshal(payload, &o); err != nil {
		t.Fatal(err)
	}
	if o.ToolConfig != nil {
		t.Fatalf("choice without tools should not configure: %+v", o.ToolConfig)
	}
	// Response formats and schema stripping (nested + arrays + scalars).
	temp := 0.3
	r = Request{
		Temperature: &temp,
		ResponseFormat: &ResponseFormat{Kind: FormatJSONSchema, Schema: json.RawMessage(`{
			"type":"object","$schema":"http://x","additionalProperties":false,
			"properties":{"a":{"type":"string","$id":"x"},"b":{"type":"array","items":[{"type":"string","additionalProperties":true},42,"s"]}}
		}`)},
		Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}},
	}
	payload, _, err = EncodeGeminiRequest(r, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &o); err != nil {
		t.Fatal(err)
	}
	if o.GenerationConfig.ResponseMimeType != "application/json" {
		t.Fatalf("schema mime: %+v", o.GenerationConfig)
	}
	schemaStr := string(o.GenerationConfig.ResponseSchema)
	if strings.Contains(schemaStr, "$schema") || strings.Contains(schemaStr, "additionalProperties") || strings.Contains(schemaStr, "$id") {
		t.Fatalf("schema keywords survived: %s", schemaStr)
	}
	if !strings.Contains(schemaStr, `"type":"string"`) {
		t.Fatalf("schema stripped too much: %s", schemaStr)
	}
	// JSONObject format: mime only, no schema. Fresh decode targets:
	// unmarshaling into a reused struct would keep the previous
	// responseSchema (absent fields are not cleared).
	r.ResponseFormat = &ResponseFormat{Kind: FormatJSONObject}
	payload, _, err = EncodeGeminiRequest(r, "m")
	if err != nil {
		t.Fatal(err)
	}
	var o2 GeminiRequest
	if err := json.Unmarshal(payload, &o2); err != nil {
		t.Fatal(err)
	}
	if o2.GenerationConfig.ResponseMimeType != "application/json" || len(o2.GenerationConfig.ResponseSchema) != 0 {
		t.Fatalf("json_object: %+v", o2.GenerationConfig)
	}
	// JSONSchema with empty schema: mime only.
	r.ResponseFormat = &ResponseFormat{Kind: FormatJSONSchema}
	payload, _, err = EncodeGeminiRequest(r, "m")
	if err != nil {
		t.Fatal(err)
	}
	var o3 GeminiRequest
	if err := json.Unmarshal(payload, &o3); err != nil {
		t.Fatal(err)
	}
	if len(o3.GenerationConfig.ResponseSchema) != 0 {
		t.Fatalf("empty schema should stay empty: %+v", o3.GenerationConfig)
	}
	// Tool declarations: bad parameters JSON skipped, description omitted when empty.
	r = Request{Tools: []ToolDef{{Name: "ok", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}, {Name: "bad", Parameters: json.RawMessage(`{bad`)}},
		Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}}}
	payload, _, err = EncodeGeminiRequest(r, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &o); err != nil {
		t.Fatal(err)
	}
	if len(o.Tools) != 1 || len(o.Tools[0].FunctionDeclarations) != 2 {
		t.Fatalf("decls: %+v", o.Tools)
	}
	if o.Tools[0].FunctionDeclarations[1]["parameters"] != nil {
		t.Fatalf("bad params should be skipped: %v", o.Tools[0].FunctionDeclarations[1])
	}
	// System instruction only when non-blank.
	r = Request{System: []Part{{Type: PartText, Text: "  "}}, Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}}}
	payload, _, err = EncodeGeminiRequest(r, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &o); err != nil {
		t.Fatal(err)
	}
	if o.SystemInstruction != nil {
		t.Fatalf("blank system should not instruct: %+v", o.SystemInstruction)
	}
}

func TestMission95GeminiDecodeEdges(t *testing.T) {
	if _, err := DecodeGeminiResponse([]byte(`{bad`)); err == nil {
		t.Fatalf("invalid JSON should error")
	}
	// Block reason without candidates -> refusal; with candidates the
	// candidate finish reason wins.
	resp, err := DecodeGeminiResponse([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	if err != nil || resp.StopReason != StopRefusal || len(resp.Blocks) != 0 {
		t.Fatalf("block only: %+v %v", resp, err)
	}
	resp, err = DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"MAX_TOKENS"}],"promptFeedback":{"blockReason":"SAFETY"}}`))
	if err != nil || resp.StopReason != StopMaxTokens {
		t.Fatalf("block+candidate: %+v %v", resp, err)
	}
	// Thought part, function calls (both spellings), empty args.
	resp, err = DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"parts":[
		{"text":"thinking aloud","thought":true},
		{"text":"plain"},
		{"functionCall":{"name":"f","args":{"a":1}}},
		{"functionCall":{"name":"g"}},
		{"function_call":{"name":"h","args":""}},
		{"inlineData":{"mimeType":"image/png","data":"xx"}}
	]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"cachedContentTokenCount":2,"thoughtsTokenCount":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 5 {
		t.Fatalf("blocks = %d: %+v", len(resp.Blocks), resp.Blocks)
	}
	if resp.Blocks[0].Type != PartThinking || resp.Blocks[0].Thinking.Signature != "thought" {
		t.Fatalf("thought block: %+v", resp.Blocks[0])
	}
	// Absent args become {}; an explicit JSON empty string is preserved
	// verbatim (RawMessage), it is not treated as absent.
	if resp.Blocks[2].ToolCall.Arguments != `{"a":1}` || resp.Blocks[3].ToolCall.Arguments != "{}" || resp.Blocks[4].ToolCall.Name != "h" || resp.Blocks[4].ToolCall.Arguments != `""` {
		t.Fatalf("call args: %+v", resp.Blocks)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("tool upgrade: %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 4 || resp.Usage.CacheReadTokens != 2 || resp.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	// Finish reason mapping.
	for _, tc := range []struct {
		in, want string
	}{
		{"MAX_TOKENS", StopMaxTokens}, {"max_tokens", StopMaxTokens},
		{"SAFETY", StopRefusal}, {"PROHIBITED_CONTENT", StopRefusal}, {"BLOCKLIST", StopRefusal}, {"SPII", StopRefusal},
		{"STOP", StopEndTurn}, {"", StopEndTurn}, {"OTHER", StopEndTurn}, {"  stop  ", StopEndTurn},
	} {
		resp, err := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"` + tc.in + `"}]}`))
		if err != nil || resp.StopReason != tc.want {
			t.Fatalf("finish %q -> %q want %q err %v", tc.in, resp.StopReason, tc.want, err)
		}
	}
	// No candidates, no usage -> empty blocks slice, end_turn.
	resp, err = DecodeGeminiResponse([]byte(`{}`))
	if err != nil || resp.StopReason != StopEndTurn || resp.Blocks == nil || len(resp.Blocks) != 0 {
		t.Fatalf("empty response: %+v %v", resp, err)
	}
	// Candidate with nil content -> no blocks, finish still applied.
	resp, err = DecodeGeminiResponse([]byte(`{"candidates":[{"finishReason":"SAFETY"}]}`))
	if err != nil || resp.StopReason != StopRefusal {
		t.Fatalf("nil content: %+v %v", resp, err)
	}
}

func TestMission95ValidationEdges(t *testing.T) {
	meta := ValidationMeta{Stage: "test", Protocol: "anthropic", Provider: "p"}
	def := ToolDef{Name: "f", Parameters: json.RawMessage(`{"type":"object","properties":{
		"name":{"type":"string"},"count":{"type":"integer"},"ratio":{"type":"number"},
		"flag":{"type":"boolean"},"tags":{"type":"array"},"cfg":{"type":"object"},"maybe":{"type":"string"}
	},"required":["name"]}`)}
	// Empty, invalid JSON, non-object top level.
	for _, args := range []string{"", "  ", "{bad", "[1,2]", `"str"`, "42", "null"} {
		if err := ValidateToolCallWithMeta(def, ToolCall{Arguments: args}, meta); err == nil {
			t.Fatalf("args %q should fail", args)
		}
	}
	// No schema: object passes. Unparsable schema: validation skipped.
	if err := ValidateToolCall(ToolDef{Name: "g"}, ToolCall{Arguments: `{"a":1}`}); err != nil {
		t.Fatalf("no schema: %v", err)
	}
	if err := ValidateToolCall(ToolDef{Name: "g", Parameters: json.RawMessage(`{bad`)}, ToolCall{Arguments: `{"a":1}`}); err != nil {
		t.Fatalf("bad schema should skip: %v", err)
	}
	// Required missing; optional absent is fine.
	if err := ValidateToolCallWithMeta(def, ToolCall{Arguments: `{}`}, meta); err == nil {
		t.Fatalf("missing required should fail")
	}
	if err := ValidateToolCallWithMeta(def, ToolCall{Arguments: `{"name":"x"}`}, meta); err != nil {
		t.Fatalf("optional absent: %v", err)
	}
	// Type mismatches per kind, plus the string-wrapper bug class.
	badCases := []string{
		`{"name":42}`, `{"name":"x","count":1.5}`, `{"name":"x","count":"3"}`,
		`{"name":"x","ratio":"a"}`, `{"name":"x","flag":1}`, `{"name":"x","tags":{}}`,
		`{"name":"x","cfg":[]}`, `{"name":"x","maybe":{"value":"v"}}`,
	}
	for _, args := range badCases {
		if err := ValidateToolCallWithMeta(def, ToolCall{Arguments: args}, meta); err == nil {
			t.Fatalf("args %s should fail", args)
		}
	}
	goodCases := []string{
		`{"name":"x","count":3}`, `{"name":"x","ratio":1.5}`, `{"name":"x","flag":true}`,
		`{"name":"x","tags":[]}`, `{"name":"x","cfg":{}}`, `{"name":"x","maybe":null}`,
	}
	// Note: "maybe":null with expected "string": typeMatches("string",
	// "null") is false, so null for a string field fails; drop it from
	// the good set and assert separately below.
	goodCases = goodCases[:len(goodCases)-1]
	for _, args := range goodCases {
		if err := ValidateToolCallWithMeta(def, ToolCall{Arguments: args}, meta); err != nil {
			t.Fatalf("args %s should pass: %v", args, err)
		}
	}
	if err := ValidateToolCallWithMeta(def, ToolCall{Arguments: `{"name":"x","maybe":null}`}, meta); err == nil {
		t.Fatalf("null for string field should fail (no null in type)")
	}
	// Nullable type string containing "null": null value skipped.
	nullable := ToolDef{Name: "n", Parameters: json.RawMessage(`{"type":"object","properties":{"v":{"type":"string|null"}}}`)}
	// The schema type is a single string "string|null", not an array;
	// typeMatches default branch returns true for any value, and the
	// nullable early-continue only applies when val == nil.
	if err := ValidateToolCallWithMeta(nullable, ToolCall{Arguments: `{"v":null}`}, meta); err != nil {
		t.Fatalf("nullable null: %v", err)
	}
	// Property without a type, or with a non-string type, is skipped.
	loose := ToolDef{Name: "l", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{},"b":{"type":3}}}`)}
	if err := ValidateToolCallWithMeta(loose, ToolCall{Arguments: `{"a":1,"b":2}`}, meta); err != nil {
		t.Fatalf("typeless props: %v", err)
	}
	// Unknown expected type: typeMatches default returns true.
	weird := ToolDef{Name: "w", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"enum"}}}`)}
	if err := ValidateToolCallWithMeta(weird, ToolCall{Arguments: `{"a":123}`}, meta); err != nil {
		t.Fatalf("unknown type should pass: %v", err)
	}
	// jsonTypeOf / typeMatches direct: integer vs number, %T fallback.
	if got := jsonTypeOf(3.0); got != "integer" {
		t.Fatalf("3.0 -> %q", got)
	}
	if got := jsonTypeOf(3.5); got != "number" {
		t.Fatalf("3.5 -> %q", got)
	}
	if got := jsonTypeOf(int(3)); got != "int" {
		t.Fatalf("int fallback -> %q", got)
	}
	if !typeMatches("integer", "integer", float64(3)) || typeMatches("integer", "number", float64(3.5)) {
		t.Fatalf("integer matching")
	}
	// Error string format carries meta.
	err := ValidateToolCallWithMeta(def, ToolCall{Arguments: ``}, meta)
	if err == nil || !strings.Contains(err.Error(), "stage=test") || !strings.Contains(err.Error(), "arguments empty") {
		t.Fatalf("error format: %v", err)
	}
	// ValidateToolCallArguments convenience + ValidateRawArguments.
	if err := ValidateToolCallArguments(def.Parameters, `{"name":"x"}`, meta); err != nil {
		t.Fatalf("convenience: %v", err)
	}
	for _, args := range []string{"", `"{\"a\":1}"`, "{bad", "[1]", "null"} {
		if err := ValidateRawArguments(args, meta); err == nil {
			t.Fatalf("raw %q should fail", args)
		}
	}
	if err := ValidateRawArguments(`{"a":1}`, meta); err != nil {
		t.Fatalf("raw good: %v", err)
	}
	// ValidateResponseBlocks: non-tool blocks skipped; raw failure
	// enriched with tool name; schema failure enriched from "unknown";
	// no-def critical-field and Bash/Read required checks.
	blocks := []Block{{Type: PartText, Text: "hi"}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err != nil {
		t.Fatalf("text blocks: %v", err)
	}
	blocks = []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "f", Arguments: `{bad`}}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err == nil || !strings.Contains(err.Error(), "tool=f") {
		t.Fatalf("raw enrich: %v", err)
	}
	blocks = []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "f", Arguments: `{}`}}}
	if err := ValidateResponseBlocks(blocks, []ToolDef{def}, meta); err == nil || !strings.Contains(err.Error(), "tool=f") {
		t.Fatalf("schema enrich: %v", err)
	}
	blocks = []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Bash", Arguments: `{"command":42}`}}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err == nil {
		t.Fatalf("Bash non-string command should fail")
	}
	blocks = []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Bash", Arguments: `{}`}}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err == nil {
		t.Fatalf("Bash without command should fail")
	}
	blocks = []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Read", Arguments: `{}`}}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err == nil {
		t.Fatalf("Read without file_path should fail")
	}
	blocks = []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Read", Arguments: `{"file_path":"/x"}`}}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err != nil {
		t.Fatalf("Read good: %v", err)
	}
	blocks = []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Other", Arguments: `{"a":1}`}}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err != nil {
		t.Fatalf("unknown tool good: %v", err)
	}
	blocks = []Block{{Type: PartToolCall}}
	if err := ValidateResponseBlocks(blocks, nil, meta); err != nil {
		t.Fatalf("nil tool call skipped: %v", err)
	}
}

func TestMission95CanonicalRequirementsAndStop(t *testing.T) {
	// DetectRequirements: every branch.
	req := Request{}
	if got := req.DetectRequirements(); got.Tools || got.Vision || got.NeedsTool || got.Streaming || got.Reasoning || got.StructuredOutput || got.ParallelTools || got.Stop {
		t.Fatalf("empty requirements: %+v", got)
	}
	pt := true
	req = Request{
		System:            []Part{{Type: PartImage}},
		Messages:          []Message{{Role: "user", Parts: []Part{{Type: PartToolCall}}}},
		ToolChoice:        &ToolChoice{Mode: "auto"},
		Reasoning:         &Reasoning{Effort: "low"},
		ResponseFormat:    &ResponseFormat{Kind: FormatJSONSchema},
		ParallelToolCalls: &pt,
		Stop:              []string{"END"},
		Stream:            true,
	}
	got := req.DetectRequirements()
	if !got.Vision || !got.Tools || got.NeedsTool || !got.Streaming || !got.Reasoning || !got.StructuredOutput || !got.ParallelTools || !got.Stop {
		t.Fatalf("full requirements: %+v", got)
	}
	req.ToolChoice = &ToolChoice{Mode: "required"}
	if got := req.DetectRequirements(); !got.NeedsTool {
		t.Fatalf("required needs tool: %+v", got)
	}
	req.ToolChoice = &ToolChoice{Mode: "bogus"}
	if got := req.DetectRequirements(); got.NeedsTool {
		t.Fatalf("bogus mode must not need tool: %+v", got)
	}
	req.Reasoning = &Reasoning{Disabled: true}
	if got := req.DetectRequirements(); got.Reasoning {
		t.Fatalf("disabled reasoning: %+v", got)
	}
	req.ResponseFormat = &ResponseFormat{Kind: FormatText}
	if got := req.DetectRequirements(); got.StructuredOutput {
		t.Fatalf("text format is not structured: %+v", got)
	}
	req.ResponseFormat = &ResponseFormat{Kind: ""}
	if got := req.DetectRequirements(); got.StructuredOutput {
		t.Fatalf("empty kind is not structured: %+v", got)
	}
	// NormalizeStop shapes.
	if got := NormalizeStop(""); got != nil {
		t.Fatalf("empty string: %v", got)
	}
	if got := NormalizeStop("END"); len(got) != 1 || got[0] != "END" {
		t.Fatalf("string: %v", got)
	}
	if got := NormalizeStop([]string{"a", "", "b"}); len(got) != 2 {
		t.Fatalf("string slice: %v", got)
	}
	if got := NormalizeStop([]string{"", ""}); got != nil {
		t.Fatalf("all-empty slice: %v", got)
	}
	if got := NormalizeStop([]any{"a", 42, "", "b"}); len(got) != 2 {
		t.Fatalf("any slice: %v", got)
	}
	if got := NormalizeStop([]any{1, 2}); got != nil {
		t.Fatalf("non-string any: %v", got)
	}
	if got := NormalizeStop(42); got != nil {
		t.Fatalf("other type: %v", got)
	}
	// Response.Text joins non-empty text blocks.
	resp := Response{Blocks: []Block{{Type: PartText, Text: "a"}, {Type: PartText, Text: ""}, {Type: PartToolCall}, {Type: PartText, Text: "b"}}}
	if got := resp.Text(); got != "a\nb" {
		t.Fatalf("Text = %q", got)
	}
	if resp.HasToolCalls() != true {
		t.Fatalf("HasToolCalls")
	}
	noTools := Response{Blocks: []Block{{Type: PartText, Text: "x"}}}
	if noTools.HasToolCalls() {
		t.Fatalf("no tools")
	}
	// MapStopReason vocabulary.
	for _, tc := range []struct {
		in, want string
	}{
		{"tool_use", StopToolUse}, {"tool_calls", StopToolUse}, {"function_call", StopToolUse},
		{"max_tokens", StopMaxTokens}, {"length", StopMaxTokens},
		{"stop_sequence", StopStopSequence}, {"end_turn", StopEndTurn}, {"", StopEndTurn}, {"weird", StopEndTurn},
	} {
		if got := MapStopReason(tc.in); got != tc.want {
			t.Fatalf("MapStopReason(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}
