package canonical

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func TestCoveragePrimaryCodecs(t *testing.T) {
	temperature, topP := 0.4, 0.8
	parallel := false
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	req := Request{
		Model: "client", System: []Part{{Type: PartText, Text: "system"}},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "look"}, {Type: PartImage, Image: &Image{URL: "data:image/png;base64,AAAA"}}}},
			{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "calling"}, {Type: PartThinking, Thinking: &Thinking{Text: "why", Signature: "sig"}}, {Type: PartToolCall, ToolCall: &ToolCall{ID: "call_1", Name: "WriteFile", Arguments: `{"path":"/tmp/a"}`}}}},
			{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "call_1", Content: "denied", IsError: true}}}},
		},
		Tools: []ToolDef{{Name: "WriteFile", Description: "write", Parameters: schema}}, ToolChoice: &ToolChoice{Mode: "tool", Name: "WriteFile"}, ParallelToolCalls: &parallel,
		Reasoning: &Reasoning{Effort: "high", BudgetTokens: 16384}, ResponseFormat: &ResponseFormat{Kind: FormatJSONSchema, Name: "result", Schema: json.RawMessage(`{"type":"object"}`)},
		Temperature: &temperature, TopP: &topP, MaxOutputTokens: 64, Stop: []string{"END", "STOP"}, Stream: true,
		Metadata: map[string]any{"seed": int64(7), "frequency_penalty": json.Number("0.2"), "presence_penalty": 1},
	}
	if got := req.DetectRequirements(); !got.Tools || !got.NeedsTool || !got.Vision || !got.Streaming || !got.Reasoning || !got.Stop {
		t.Fatalf("requirements=%+v", got)
	}
	if got := (&Response{Blocks: []Block{{Type: PartText, Text: "one"}, {Type: PartText, Text: "two"}}}).Text(); got != "one\ntwo" {
		t.Fatalf("text=%q", got)
	}
	if out, err := EncodeOpenAIChatRequest(req, "openai-upstream", true); err != nil || len(out.Messages) != 4 || len(out.Tools) != 1 || out.StreamOptions == nil || out.Messages[3].Content != "[tool error] denied" {
		t.Fatalf("EncodeOpenAIChatRequest out=%+v err=%v", out, err)
	}
	if out, err := EncodeAnthropicRequest(req, "anthropic-upstream"); err != nil || len(out.Messages) != 3 || len(out.Tools) != 1 || len(out.Thinking) == 0 {
		t.Fatalf("EncodeAnthropicRequest out=%+v err=%v", out, err)
	}
	if payload, err := EncodeResponsesRequest(req, "responses-upstream"); err != nil || !strings.Contains(string(payload), `"WriteFile"`) {
		t.Fatalf("EncodeResponsesRequest payload=%s err=%v", payload, err)
	}

	seed := int64(7)
	decodedOpenAI := DecodeOpenAIChatRequest(core.OpenAIRequest{
		MaxTokens: 16, MaxCompletionTokens: 32, Stream: true, Temperature: &temperature, TopP: &topP, Stop: []string{"END", "STOP"}, Seed: &seed, FrequencyPenalty: &temperature, PresencePenalty: &topP,
		Messages: []core.OpenAIMessage{
			{Role: "system", Content: []any{map[string]any{"type": "text", "text": "sys"}, map[string]any{"type": "input_image", "image_url": "data:image/jpeg;base64,BBBB"}}},
			{Role: "user", Content: []any{map[string]any{"type": "input_text", "text": "hello"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png"}}, map[string]any{"type": "custom", "x": "preserved"}}},
			{Role: "assistant", Content: "answer", ReasoningContent: "think", ToolCalls: []core.OpenAIToolCall{{ID: "call_2", Function: core.OpenAIFunctionCall{Name: "WriteFile", Arguments: `{"path":"x"}`}}}},
			{Role: "tool", ToolCallID: "call_2", Content: []any{map[string]any{"type": "text", "text": "tool output"}}},
		},
		Tools: []core.OpenAITool{{Type: "function", Function: core.OpenAIFunction{Name: "WriteFile", Parameters: map[string]any{"type": "object"}}}}, ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "WriteFile"}}, ParallelToolCalls: &parallel, ReasoningEffort: "low",
	}, "client-openai")
	if len(decodedOpenAI.System) != 2 || len(decodedOpenAI.Messages) != 3 || len(decodedOpenAI.Tools) != 1 {
		t.Fatalf("DecodeOpenAIChatRequest=%+v", decodedOpenAI)
	}

	decodedAnthropic, err := DecodeAnthropicRequest(core.AnthropicRequest{
		MaxTokens: 24, Stream: true, Temperature: &temperature, TopP: &topP, TopK: 10, StopSequences: []string{"END"}, System: json.RawMessage(`[{"type":"text","text":"sys"}]`),
		Messages: []core.AnthMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}],"is_error":true}]`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"WriteFile","input":{"path":"/tmp/a"}},{"type":"thinking","thinking":"reason","signature":"sig"},{"type":"redacted_thinking","redacted_thinking":"hidden"}]`)},
		},
		Tools: []core.AnthTool{{Name: "WriteFile", InputSchema: map[string]any{"type": "object"}}}, ToolChoice: map[string]any{"type": "tool", "name": "WriteFile"}, Thinking: json.RawMessage(`{"type":"adaptive","budget_tokens":0}`), Metadata: json.RawMessage(`{"user_id":"u"}`),
	}, "client-anthropic")
	if err != nil || len(decodedAnthropic.System) != 1 || len(decodedAnthropic.Messages) != 2 || len(decodedAnthropic.Tools) != 1 {
		t.Fatalf("DecodeAnthropicRequest=%+v err=%v", decodedAnthropic, err)
	}
	decodedResponses, err := DecodeResponsesRequest(ResponsesRequest{Model: "client-responses", Stream: true, MaxOutputTokens: 40, Input: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/gif;base64,CCCC"}]},{"type":"function_call","call_id":"call_3","name":"WriteFile","arguments":"{\"path\":\"x\"}"},{"type":"function_call_output","call_id":"call_3","output":"ok"}]`), Instructions: json.RawMessage(`"be brief"`), Tools: []ResponsesTool{{Type: "function", Name: "WriteFile", Parameters: map[string]any{"type": "object"}}}, ToolChoice: "required", Reasoning: json.RawMessage(`{"effort":"low"}`), Text: json.RawMessage(`{"format":{"type":"json_object"}}`)})
	if err != nil || len(decodedResponses.System) != 1 || len(decodedResponses.Messages) != 3 || len(decodedResponses.Tools) != 1 || decodedResponses.ResponseFormat == nil {
		t.Fatalf("DecodeResponsesRequest=%+v err=%v", decodedResponses, err)
	}

	response, err := DecodeResponsesResponse([]byte(`{"id":"resp_1","model":"upstream","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"},{"type":"text","text":"world"}]},{"type":"function_call","call_id":"call_1","name":"WriteFile","arguments":"{\"path\":\"/tmp/a\"}"},{"type":"reasoning","summary":[{"type":"summary_text","text":"why"}]}],"usage":{"input_tokens":3,"output_tokens":4,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}}}`))
	if err != nil || len(response.Blocks) != 3 || response.StopReason != StopToolUse || len(EncodeResponsesResponse(response, "public").Output) != 3 {
		t.Fatalf("Responses response=%+v err=%v", response, err)
	}
	openAIResponse, err := DecodeOpenAIChatResponse([]byte(`{"id":"chat_1","object":"chat.completion","model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"output_text","text":"hello"}],"reasoning_content":"why","tool_calls":[{"id":"call_1","type":"function","function":{"name":"WriteFile","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	if err != nil || len(openAIResponse.Blocks) != 3 || len(EncodeOpenAIChatResponse(openAIResponse, "public").Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("OpenAI response=%+v err=%v", openAIResponse, err)
	}
	if _, err := DecodeAnthropicResponse([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"call_1","name":"WriteFile","input":{"path":"x"}},{"type":"thinking","thinking":"why","signature":"sig"}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`)); err != nil {
		t.Fatalf("DecodeAnthropicResponse: %v", err)
	}
	defs := []ToolDef{{Name: "WriteFile", Parameters: schema}}
	if err := ValidateToolCallArguments(schema, `{"path":"/tmp/a"}`, ValidationMeta{}); err != nil {
		t.Fatalf("ValidateToolCallArguments: %v", err)
	}
	if err := ValidateResponseBlocks([]Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "WriteFile", Arguments: `{"path":{}}`}}}, defs, ValidationMeta{}); err == nil {
		t.Fatal("expected schema error")
	}
	for _, raw := range []string{"", `null`, `[]`, `"{\"path\":\"x\"}"`, `{"path":"x"`} {
		if err := ValidateRawArguments(raw, ValidationMeta{}); err == nil {
			t.Fatalf("expected raw validation error for %q", raw)
		}
	}
}

func TestCoveragePrimaryStreams(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"why"}}`},
		{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"WriteFile"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"more"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"more"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"x\"}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":2}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`},
		{"message_stop", `{"type":"message_stop"}`},
		{"error", `{"type":"error","error":{"type":"api_error","message":"bad"}}`},
	} {
		if _, err := DecodeAnthropicStreamEvent(tc.name, tc.data); err != nil {
			t.Fatalf("DecodeAnthropicStreamEvent(%s): %v", tc.name, err)
		}
	}
	if _, err := DecodeAnthropicStreamEvent("bad", `{`); err == nil {
		t.Fatal("expected invalid Anthropic stream JSON")
	}
	reader := NewSSEReader(strings.NewReader(": keepalive\nid: 1\nevent: notice\ndata: first\ndata: second\n\ndata: trailing"))
	name, data, done, err := reader.Next()
	if err != nil || done || name != "notice" || data != "first\nsecond" {
		t.Fatalf("SSE first name=%q data=%q done=%t err=%v", name, data, done, err)
	}
	_, data, done, err = reader.Next()
	if err != nil || done || data != "trailing" {
		t.Fatalf("SSE trailing data=%q done=%t err=%v", data, done, err)
	}
	for _, protocol := range []string{"openai_chat", "anthropic", "openai_responses"} {
		rr := httptest.NewRecorder()
		emitter := NewStreamEmitter(protocol, rr, "client", "req_1")
		for _, ev := range []StreamEvent{{Type: StreamStart, Usage: &Usage{InputTokens: 1}}, {Type: StreamText, Text: "hello"}, {Type: StreamThinking, Text: "why"}, {Type: StreamToolStart, ToolIndex: 0, ToolID: "call_1", ToolName: "WriteFile"}, {Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `{"path":"x"}`}, {Type: StreamToolEnd, ToolIndex: 0}, {Type: StreamUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 2}}, {Type: StreamEnd, StopReason: StopToolUse}} {
			if err := emitter.Emit(ev); err != nil {
				t.Fatalf("%s emitter %s: %v", protocol, ev.Type, err)
			}
		}
		if err := emitter.Finish(); err != nil || rr.Body.Len() == 0 {
			t.Fatalf("%s finish err=%v", protocol, err)
		}
	}
	for code, want := range map[string]string{"overloaded_error": "overloaded_error", "rate_limit_error": "rate_limit_error", "invalid_request_error": "invalid_request_error", "authentication_error": "authentication_error", "other": "api_error"} {
		if got := mapErrorType(code); got != want {
			t.Fatalf("mapErrorType(%q)=%q", code, got)
		}
	}
	for stop, want := range map[string]string{StopToolUse: "tool_calls", StopMaxTokens: "length", StopRefusal: "content_filter", StopStopSequence: "stop", "other": "stop"} {
		if got := openAIFinish(stop); got != want {
			t.Fatalf("openAIFinish(%q)=%q", stop, got)
		}
	}
}

func TestCoveragePrimaryDataURLs(t *testing.T) {
	if got := parseDataURL("data:,abc"); got == nil || got.mediaType != "application/octet-stream" || got.data != "abc" {
		t.Fatalf("data URL=%+v", got)
	}
	if parseDataURL("https://example.test/a") != nil || parseDataURL("data:image/png") != nil {
		t.Fatal("invalid data URL accepted")
	}
	if decodeAnthImage(map[string]any{"type": "url", "url": "https://example.test/a"}) == nil || decodeAnthImage(nil) != nil || decodeOpenAIImage(map[string]any{"image_url": ""}) != nil {
		t.Fatal("image edge decoding failed")
	}
	for _, input := range []any{"END", []string{"A", ""}, []any{"B", 3}, 4} {
		_ = NormalizeStop(input)
	}
	for _, reason := range []string{"tool_use", "length", "stop", "refusal", "pause_turn", "unknown"} {
		_ = MapStopReason(reason)
	}
	if _, err := EncodeOpenAIChatRequest(Request{}, "upstream", false); err == nil {
		t.Fatal("expected no-message OpenAI encode error")
	}
}

func TestCoverageAdditionalCanonicalBranches(t *testing.T) {
	for _, tc := range []struct {
		expected string
		actual   string
		value    any
		want     bool
	}{
		{"string", "string", "x", true}, {"integer", "integer", float64(2), true}, {"integer", "number", 2.5, false},
		{"number", "integer", float64(2), true}, {"boolean", "boolean", true, true}, {"array", "array", []any{}, true}, {"object", "object", map[string]any{}, true},
	} {
		if got := typeMatches(tc.expected, tc.actual, tc.value); got != tc.want {
			t.Fatalf("typeMatches(%q, %q, %#v)=%t want=%t", tc.expected, tc.actual, tc.value, got, tc.want)
		}
	}
	for _, blocks := range [][]Block{
		{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Bash", Arguments: `{}`}}},
		{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Read", Arguments: `{}`}}},
		{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Other", Arguments: `{"command":[]}`}}},
	} {
		if err := ValidateResponseBlocks(blocks, nil, ValidationMeta{}); err == nil {
			t.Fatalf("expected fallback validation error for %+v", blocks)
		}
	}
	if err := ValidateResponseBlocks([]Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "Other", Arguments: `{"safe":true}`}}}, nil, ValidationMeta{}); err != nil {
		t.Fatalf("unexpected generic validation error: %v", err)
	}
	if got := canonicalPartsToOpenAIContent(nil); len(got) != 1 || got[0]["text"] != "" {
		t.Fatalf("empty parts content=%+v", got)
	}
	if got := canonicalPartsToOpenAIContent([]Part{{Type: PartImage}, {Type: PartImage, Image: &Image{Data: "ZZ"}}}); len(got) != 1 {
		t.Fatalf("image parts content=%+v", got)
	}
	if got := openAIContentToText(map[string]any{"x": "y"}); !strings.Contains(got, `"x":"y"`) {
		t.Fatalf("object content text=%q", got)
	}
	if got := openAIContentToParts([]any{"skip", map[string]any{"type": "unknown", "x": 1}}, true); len(got) != 0 {
		t.Fatalf("system unknown parts=%+v", got)
	}

	for _, mode := range []string{"auto", "none", "required"} {
		out, err := EncodeOpenAIChatRequest(Request{
			Messages:          []Message{{Role: RoleUser}},
			ToolChoice:        &ToolChoice{Mode: mode},
			ResponseFormat:    &ResponseFormat{Kind: FormatJSONObject},
			Reasoning:         &Reasoning{},
			MaxOutputTokens:   5,
			Stop:              []string{"END"},
			ParallelToolCalls: boolPtr(true),
		}, "upstream", false)
		if err != nil || out.ToolChoice != mode || out.ResponseFormatRaw == nil || out.ReasoningEffort != "medium" {
			t.Fatalf("OpenAI edge encode mode=%s out=%+v err=%v", mode, out, err)
		}
	}

	responsesRequest := Request{
		System: []Part{{Type: PartText, Text: "sys"}},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "one"}}},
			{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "call_1", Name: "Tool", Arguments: ""}}}},
			{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "call_1", Content: "done"}}}},
		},
		Tools: []ToolDef{{Name: "Tool"}}, ToolChoice: &ToolChoice{Mode: "required"}, ParallelToolCalls: boolPtr(true), Reasoning: &Reasoning{}, ResponseFormat: &ResponseFormat{Kind: FormatJSONObject}, MaxOutputTokens: 2, Stop: []string{"END"},
	}
	payload, err := EncodeResponsesRequest(responsesRequest, "upstream")
	if err != nil || !strings.Contains(string(payload), `"json_object"`) || !strings.Contains(string(payload), `"arguments":"{}"`) {
		t.Fatalf("Responses edge encode payload=%s err=%v", payload, err)
	}
	for _, request := range []ResponsesRequest{
		{Model: "m", Input: json.RawMessage(`"plain input"`)},
		{Model: "m", Input: json.RawMessage(`["standalone",{"type":"message","content":[{"type":"text","text":"hello"}]}]`), Tools: []ResponsesTool{{Type: "function", Name: "f"}}, Text: json.RawMessage(`{"format":{"type":"json_schema","name":"x","schema":{"type":"object"}}}`)},
	} {
		if _, err := DecodeResponsesRequest(request); err != nil {
			t.Fatalf("DecodeResponsesRequest edge: %v", err)
		}
	}

	temperature, topP, topK := 0.2, 0.7, 4.0
	geminiRequest := Request{
		System: []Part{{Type: PartText, Text: "sys"}},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "user"}, {Type: PartImage, Image: &Image{URL: "data:image/png;base64,AAAA"}}, {Type: PartImage, Image: &Image{URL: "https://remote.invalid/image"}}}},
			{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "call_1", Name: "Tool", Arguments: ""}}}},
			{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "call_1", Content: "not json"}}}},
		},
		Tools:      []ToolDef{{Name: "Tool", Description: "desc", Parameters: json.RawMessage(`{"$schema":"x","type":"object","additionalProperties":false,"properties":{"x":{"type":"string"}}}`)}},
		ToolChoice: &ToolChoice{Mode: "tool", Name: "Tool"}, Temperature: &temperature, TopP: &topP, TopK: &topK, MaxOutputTokens: 9, Stop: []string{"END"}, ResponseFormat: &ResponseFormat{Kind: FormatJSONSchema, Schema: json.RawMessage(`{"$schema":"x","type":"object"}`)},
	}
	if payload, model, err := EncodeGeminiRequest(geminiRequest, "gemini-upstream"); err != nil || model != "gemini-upstream" || !strings.Contains(string(payload), "functionDeclarations") {
		t.Fatalf("EncodeGeminiRequest model=%q payload=%s err=%v", model, payload, err)
	}
	if (&GeminiBlob{MimeType: "a", MimeV1: "b"}).Mime() != "a" || (&GeminiBlob{MimeV1: "b"}).Mime() != "b" {
		t.Fatal("Gemini MIME normalization failed")
	}
	geminiResponse := []byte(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hello"},{"text":"why","thought":true},{"functionCall":{"name":"Tool","args":{"x":"y"}}}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"cachedContentTokenCount":1,"thoughtsTokenCount":1}}`)
	if response, err := DecodeGeminiResponse(geminiResponse); err != nil || len(response.Blocks) != 3 || response.StopReason != StopToolUse {
		t.Fatalf("DecodeGeminiResponse=%+v err=%v", response, err)
	}
	if response, err := DecodeGeminiResponse([]byte(`{"promptFeedback":{"blockReason":"SAFETY"},"candidates":[]}`)); err != nil || response.StopReason != StopRefusal {
		t.Fatalf("DecodeGeminiResponse safety=%+v err=%v", response, err)
	}
	for _, reason := range []string{"MAX_TOKENS", "SAFETY", "STOP", "other"} {
		_ = geminiFinishToStop(reason)
	}

	openAIChunks := []string{
		`{"choices":[{"index":0,"delta":{"content":"hello","reasoning":"why","tool_calls":[{"index":0,"id":"call_1","function":{"name":"Tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		`{"choices":[{"index":0,"delta":{"content":[{"type":"text","text":"array"}]},"finish_reason":null}]}`,
		`{"error":{"type":"server_error","code":"bad","message":"failed"}}`,
	}
	for _, chunk := range openAIChunks {
		if _, _, err := DecodeOpenAIStreamChunk(chunk); err != nil {
			t.Fatalf("DecodeOpenAIStreamChunk(%s): %v", chunk, err)
		}
	}
	if _, _, err := DecodeOpenAIStreamChunk(`{`); err == nil {
		t.Fatal("expected invalid OpenAI stream chunk")
	}
	for _, chunk := range []string{
		`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hello"}]}}]}`,
		`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"Tool","args":{}}}]}}]}`,
	} {
		if _, _, err := DecodeGeminiStreamChunk(chunk); err != nil {
			t.Fatalf("DecodeGeminiStreamChunk: %v", err)
		}
	}
}

func boolPtr(value bool) *bool { return &value }
