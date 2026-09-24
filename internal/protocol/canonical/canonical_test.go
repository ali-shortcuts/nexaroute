package canonical

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func mustJSONObj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	return m
}

// TestAnthropicToOpenAIRequestGolden verifies the Claude Code style request
// decodes into canonical IR and re-encodes to a correct OpenAI payload.
func TestAnthropicToOpenAIRequestGolden(t *testing.T) {
	anth := core.AnthropicRequest{
		Model: "client-model", MaxTokens: 512, Stream: true,
		System: json.RawMessage(`"You are helpful."`),
		Messages: []core.AnthMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Weather in Paris?"}]`)},
		},
		Tools: []core.AnthTool{{
			Name:        "get_weather",
			Description: "Get weather",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
		ToolChoice:  json.RawMessage(`{"type":"any"}`),
		Temperature: func() *float64 { f := 0.7; return &f }(),
	}
	anthBytes, _ := json.Marshal(anth)
	var wire core.AnthropicRequest
	if err := json.Unmarshal(anthBytes, &wire); err != nil {
		t.Fatal(err)
	}
	canReq, err := DecodeAnthropicRequest(wire, "client-model")
	if err != nil {
		t.Fatal(err)
	}
	if canReq.MaxOutputTokens != 512 || !canReq.Stream || len(canReq.System) != 1 || len(canReq.Tools) != 1 {
		t.Fatalf("canonical request decoded wrong: %+v", canReq)
	}
	if canReq.ToolChoice == nil || canReq.ToolChoice.Mode != "required" {
		t.Fatalf("tool_choice=%+v", canReq.ToolChoice)
	}
	oai, err := EncodeOpenAIChatRequest(canReq, "upstream-model", true)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(oai)
	got := mustJSONObj(t, payload)
	if got["model"] != "upstream-model" {
		t.Fatalf("model=%v", got["model"])
	}
	if got["max_tokens"].(float64) != 512 {
		t.Fatalf("max_tokens=%v", got["max_tokens"])
	}
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice=%v", got["tool_choice"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want system+user", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message role=%v", msgs[0].(map[string]any)["role"])
	}
	tools := got["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("tools=%v", tools)
	}
}

// TestOpenAIToAnthropicResponseGolden verifies tool calls survive the round
// trip into the Anthropic response shape.
func TestOpenAIToAnthropicResponseGolden(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Let me check.","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7}
	}`)
	canResp, err := DecodeOpenAIChatResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !canResp.HasToolCalls() || canResp.StopReason != StopToolUse {
		t.Fatalf("canonical response wrong: %+v", canResp)
	}
	anth := EncodeAnthropicResponse(canResp, "client-model")
	if anth.Type != "message" || anth.Role != "assistant" {
		t.Fatalf("envelope wrong: %+v", anth)
	}
	if anth.StopReason == nil || *anth.StopReason != "tool_use" {
		t.Fatalf("stop_reason=%v", anth.StopReason)
	}
	var toolBlock, textBlock *core.AnthContentBlock
	for i := range anth.Content {
		switch anth.Content[i].Type {
		case "tool_use":
			toolBlock = &anth.Content[i]
		case "text":
			textBlock = &anth.Content[i]
		}
	}
	if toolBlock == nil || toolBlock.Name != "get_weather" || toolBlock.ID != "call_1" {
		t.Fatalf("tool block wrong: %+v", toolBlock)
	}
	if toolBlock.Input["city"] != "Paris" {
		t.Fatalf("tool input=%v", toolBlock.Input)
	}
	if textBlock == nil || textBlock.Text != "Let me check." {
		t.Fatalf("text block wrong: %+v", textBlock)
	}
	if anth.Usage.InputTokens != 11 || anth.Usage.OutputTokens != 7 {
		t.Fatalf("usage=%+v", anth.Usage)
	}
}

// TestAnthropicResponseDecodeGolden verifies native Anthropic responses decode
// (thinking blocks included) and re-encode to OpenAI.
func TestAnthropicResponseDecodeGolden(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
		"content":[
			{"type":"text","text":"Answer"},
			{"type":"tool_use","id":"tu_1","name":"run_cmd","input":{"cmd":"ls"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}
	}`)
	canResp, err := DecodeAnthropicResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if canResp.StopReason != StopToolUse || !canResp.HasToolCalls() {
		t.Fatalf("wrong decode: %+v", canResp)
	}
	if canResp.Usage.CacheReadTokens != 2 || canResp.Usage.CacheWriteTokens != 3 {
		t.Fatalf("usage=%+v", canResp.Usage)
	}
	oai := EncodeOpenAIChatResponse(canResp, "client-model")
	if *oai.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish=%v", *oai.Choices[0].FinishReason)
	}
	if len(oai.Choices[0].Message.ToolCalls) != 1 || oai.Choices[0].Message.ToolCalls[0].Function.Name != "run_cmd" {
		t.Fatalf("tool calls=%+v", oai.Choices[0].Message.ToolCalls)
	}
}

// TestResponsesRequestDecodeGolden covers the /v1/responses input shapes.
func TestResponsesRequestDecodeGolden(t *testing.T) {
	raw := []byte(`{
		"model":"resp-model",
		"instructions":"Be terse.",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_9","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_9","output":"done"}
		],
		"stream":true,
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],
		"reasoning":{"effort":"high"}
	}`)
	var in ResponsesRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	canReq, err := DecodeResponsesRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(canReq.System) != 1 || canReq.Reasoning == nil || canReq.Reasoning.Effort != "high" {
		t.Fatalf("canonical=%+v", canReq)
	}
	roles := []string{}
	for _, m := range canReq.Messages {
		roles = append(roles, m.Role)
	}
	if len(roles) != 3 || roles[0] != RoleUser || roles[1] != RoleAssistant || roles[2] != RoleTool {
		t.Fatalf("roles=%v", roles)
	}
	if !canReq.Stream {
		t.Fatal("stream lost")
	}
	// Re-encode for a responses-native upstream and check structure.
	payload, err := EncodeResponsesRequest(canReq, "up-model")
	if err != nil {
		t.Fatal(err)
	}
	got := mustJSONObj(t, payload)
	if got["model"] != "up-model" || got["stream"] != true {
		t.Fatalf("payload=%s", payload)
	}
	items := got["input"].([]any)
	if len(items) != 4 { // system + 3
		t.Fatalf("items=%d", len(items))
	}
	last := items[3].(map[string]any)
	if last["type"] != "function_call_output" || last["call_id"] != "call_9" {
		t.Fatalf("last item=%v", last)
	}
}

// TestGeminiRequestEncodeGolden verifies canonical encodes to Gemini with the
// function-call/function-response mapping.
func TestGeminiRequestEncodeGolden(t *testing.T) {
	canReq := Request{
		Model:  "client",
		System: []Part{{Type: PartText, Text: "Be terse."}},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Weather in Paris?"}}},
			{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "get_weather", Arguments: `{"city":"Paris"}`}}}},
			{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "c1", Content: "15C sunny"}}}},
		},
		Tools: []ToolDef{{Name: "get_weather", Description: "Get weather", Parameters: json.RawMessage(`{"type":"object","$schema":"http://x","properties":{"city":{"type":"string"}},"additionalProperties":false}`)}},
	}
	payload, model, err := EncodeGeminiRequest(canReq, "gemini-2")
	if err != nil {
		t.Fatal(err)
	}
	if model != "gemini-2" {
		t.Fatalf("model=%q", model)
	}
	got := mustJSONObj(t, payload)
	if got["systemInstruction"] == nil {
		t.Fatal("systemInstruction missing")
	}
	contents := got["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents=%d", len(contents))
	}
	if contents[0].(map[string]any)["role"] != "user" || contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("roles wrong: %v", contents)
	}
	toolResp := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if toolResp["name"] != "get_weather" {
		t.Fatalf("functionResponse name=%v (tool name must be resolved from the call)", toolResp["name"])
	}
	tools := got["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	schema := tools[0].(map[string]any)["parameters"].(map[string]any)
	if _, has := schema["$schema"]; has {
		t.Fatal("$schema must be stripped for Gemini")
	}
	if _, has := schema["additionalProperties"]; has {
		t.Fatal("additionalProperties must be stripped for Gemini")
	}
}

// TestGeminiResponseDecodeGolden verifies a functionCall response decodes and
// re-encodes toward Anthropic clients.
func TestGeminiResponseDecodeGolden(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4}
	}`)
	canResp, err := DecodeGeminiResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !canResp.HasToolCalls() || canResp.Usage.InputTokens != 9 || canResp.Usage.OutputTokens != 4 {
		t.Fatalf("decode=%+v", canResp)
	}
	anth := EncodeAnthropicResponse(canResp, "client-model")
	found := false
	for _, b := range anth.Content {
		if b.Type == "tool_use" && b.Name == "get_weather" {
			found = true
			if b.Input["city"] != "Paris" {
				t.Fatalf("input=%v", b.Input)
			}
		}
	}
	if !found {
		t.Fatal("tool_use block missing after gemini->anthropic encode")
	}
}

// TestOpenAIStreamToAnthropicFrames covers event-by-event SSE normalization:
// OpenAI chat chunks (with streamed tool arguments) become Anthropic frames.
func TestOpenAIStreamToAnthropicFrames(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
		`[DONE]`,
	}
	var events []StreamEvent
	terminal := false
	for _, c := range chunks {
		evs, term, err := DecodeOpenAIStreamChunk(c)
		if err != nil {
			t.Fatalf("chunk %s: %v", c, err)
		}
		events = append(events, evs...)
		terminal = terminal || term
	}
	if !terminal {
		t.Fatal("stream never terminated")
	}
	texts, toolStarts, toolDeltas, ends := 0, 0, 0, 0
	for _, ev := range events {
		switch ev.Type {
		case StreamText:
			texts++
		case StreamToolStart:
			toolStarts++
			if ev.ToolID != "call_1" || ev.ToolName != "f" {
				t.Fatalf("tool start=%+v", ev)
			}
		case StreamToolDelta:
			toolDeltas++
		case StreamEnd:
			ends++
			if ev.StopReason != StopToolUse {
				t.Fatalf("stop=%q", ev.StopReason)
			}
		}
	}
	if texts != 2 || toolStarts != 1 || toolDeltas != 2 || ends != 1 {
		t.Fatalf("texts=%d starts=%d deltas=%d ends=%d", texts, toolStarts, toolDeltas, ends)
	}
	// Replay through the Anthropic emitter and validate frames.
	frames := captureAnthropicFrames(t, events)
	joined := strings.Join(frames, "\n")
	for _, want := range []string{"message_start", "content_block_start", "input_json_delta", `"stop_reason":"tool_use"`, "message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in frames:\n%s", want, joined)
		}
	}
}

// captureAnthropicFrames runs the events through the Anthropic emitter into a
// buffer and returns the raw SSE frames (event name + data separated).
func captureAnthropicFrames(t *testing.T, events []StreamEvent) []string {
	t.Helper()
	var out []string
	var currentEvent string
	// The emitter writes to an http.ResponseWriter; use a recorder-like shim.
	w := &sliceWriter{cb: func(s string) {
		for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
			if strings.HasPrefix(line, "event: ") {
				currentEvent = strings.TrimPrefix(line, "event: ")
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				out = append(out, currentEvent+" "+strings.TrimPrefix(line, "data: "))
			}
		}
	}}
	emitter := NewAnthropicEmitter(w, "model", "req")
	for _, ev := range events {
		if err := emitter.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := emitter.Finish(); err != nil {
		t.Fatal(err)
	}
	return out
}

type sliceWriter struct {
	cb   func(string)
	hdrs http.Header
}

func (s *sliceWriter) Header() http.Header {
	if s.hdrs == nil {
		s.hdrs = http.Header{}
	}
	return s.hdrs
}
func (s *sliceWriter) Write(p []byte) (int, error) {
	s.cb(string(p))
	return len(p), nil
}
func (s *sliceWriter) WriteHeader(int) {}

func TestGeminiStreamChunkOnlyTerminatesOnFinalFinishReason(t *testing.T) {
	nonFinal := `{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}`
	evs, terminal, err := DecodeGeminiStreamChunk(nonFinal)
	if err != nil {
		t.Fatal(err)
	}
	if terminal {
		t.Fatal("Gemini chunk without finishReason was marked terminal")
	}
	for _, ev := range evs {
		if ev.Type == StreamEnd {
			t.Fatalf("non-final Gemini chunk emitted StreamEnd: %+v", evs)
		}
	}

	final := `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}]}`
	evs, terminal, err = DecodeGeminiStreamChunk(final)
	if err != nil {
		t.Fatal(err)
	}
	if !terminal {
		t.Fatal("Gemini chunk with finishReason was not marked terminal")
	}
	foundEnd := false
	for _, ev := range evs {
		if ev.Type == StreamEnd {
			foundEnd = true
			if ev.StopReason == "" {
				t.Fatalf("terminal Gemini event missing stop reason: %+v", ev)
			}
		}
	}
	if !foundEnd {
		t.Fatalf("final Gemini chunk did not emit StreamEnd: %+v", evs)
	}
}

func TestAnthropicEmitterKeepsTextAndThinkingBlockIndexesSeparate(t *testing.T) {
	rr := httptest.NewRecorder()
	emitter := NewAnthropicEmitter(rr, "model", "req-blocks")
	events := []StreamEvent{
		{Type: StreamText, Text: "text-a"},
		{Type: StreamThinking, Text: "think-b"},
		{Type: StreamText, Text: "text-c"},
		{Type: StreamEnd, StopReason: StopEndTurn},
	}
	for _, ev := range events {
		if err := emitter.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := emitter.Finish(); err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if frame["type"] != "content_block_delta" {
			continue
		}
		delta, _ := frame["delta"].(map[string]any)
		typ, _ := delta["type"].(string)
		var key string
		switch typ {
		case "text_delta":
			key, _ = delta["text"].(string)
		case "thinking_delta":
			key, _ = delta["thinking"].(string)
		default:
			continue
		}
		idx, _ := frame["index"].(float64)
		got[key] = int(idx)
	}
	if got["text-a"] != 0 || got["think-b"] != 1 || got["text-c"] != 2 {
		t.Fatalf("interleaved text/thinking deltas used wrong block indexes: %#v\n%s", got, rr.Body.String())
	}
}

func TestResponsesEmitterUsesDistinctOutputIndexesForParallelTools(t *testing.T) {
	rr := httptest.NewRecorder()
	emitter := NewResponsesEmitter(rr, "model")
	events := []StreamEvent{
		{Type: StreamToolStart, ToolIndex: 0, ToolID: "c0", ToolName: "first"},
		{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: "{\"a\":1}"},
		{Type: StreamToolEnd, ToolIndex: 0},
		{Type: StreamToolStart, ToolIndex: 1, ToolID: "c1", ToolName: "second"},
		{Type: StreamToolDelta, ToolIndex: 1, ArgsDelta: "{\"b\":2}"},
		{Type: StreamToolEnd, ToolIndex: 1},
		{Type: StreamEnd, StopReason: StopToolUse},
	}
	for _, ev := range events {
		if err := emitter.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := emitter.Finish(); err != nil {
		t.Fatal(err)
	}

	indexByCall := map[string]int{}
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		item, _ := frame["item"].(map[string]any)
		callID, _ := item["call_id"].(string)
		if callID == "" {
			if itemID, _ := frame["item_id"].(string); strings.HasPrefix(itemID, "fc_") {
				callID = strings.TrimPrefix(itemID, "fc_")
			}
		}
		if callID == "" {
			continue
		}
		idx, ok := frame["output_index"].(float64)
		if !ok {
			continue
		}
		if prev, exists := indexByCall[callID]; exists && prev != int(idx) {
			t.Fatalf("tool %s changed output_index from %d to %d\n%s", callID, prev, int(idx), rr.Body.String())
		}
		indexByCall[callID] = int(idx)
	}
	if indexByCall["c0"] != 1 || indexByCall["c1"] != 2 {
		t.Fatalf("parallel tools reused output indexes: %#v\n%s", indexByCall, rr.Body.String())
	}
}

func TestResponsesStreamDecoderPreservesParallelToolIndexes(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		wantType  string
		wantIndex int
	}{
		{"start0", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c0","name":"first"}}`, StreamToolStart, 1},
		{"delta0", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"a\":1}"}`, StreamToolDelta, 1},
		{"end0", `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"c0"}}`, StreamToolEnd, 1},
		{"start1", `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"c1","name":"second"}}`, StreamToolStart, 2},
		{"delta1", `{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"b\":2}"}`, StreamToolDelta, 2},
		{"end1", `{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"c1"}}`, StreamToolEnd, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs, terminal, err := DecodeResponsesStreamEvent("", tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				t.Fatalf("tool event unexpectedly terminal: %+v", evs)
			}
			if len(evs) != 1 || evs[0].Type != tc.wantType || evs[0].ToolIndex != tc.wantIndex {
				t.Fatalf("decoded=%+v want type=%s index=%d", evs, tc.wantType, tc.wantIndex)
			}
		})
	}
}

func TestResponsesStreamIncompleteIsTerminalMaxTokensNotError(t *testing.T) {
	data := `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":11,"output_tokens":5}}}`
	evs, terminal, err := DecodeResponsesStreamEvent("", data)
	if err != nil {
		t.Fatal(err)
	}
	if !terminal {
		t.Fatal("response.incomplete must terminate the stream")
	}
	var sawUsage, sawEnd bool
	for _, ev := range evs {
		if ev.Type == StreamError {
			t.Fatalf("response.incomplete was misclassified as hard stream error: %+v", evs)
		}
		if ev.Type == StreamUsage && ev.Usage != nil && ev.Usage.InputTokens == 11 && ev.Usage.OutputTokens == 5 {
			sawUsage = true
		}
		if ev.Type == StreamEnd && ev.StopReason == StopMaxTokens {
			sawEnd = true
		}
	}
	if !sawUsage || !sawEnd {
		t.Fatalf("incomplete terminal semantics lost: %+v", evs)
	}
}

func TestResponsesCompletedWithFunctionCallSignalsToolUse(t *testing.T) {
	data := `{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"c1","name":"tool","arguments":"{}"}],"usage":{"input_tokens":3,"output_tokens":4}}}`
	evs, terminal, err := DecodeResponsesStreamEvent("", data)
	if err != nil {
		t.Fatal(err)
	}
	if !terminal {
		t.Fatal("response.completed must terminate the stream")
	}
	found := false
	for _, ev := range evs {
		if ev.Type == StreamEnd {
			found = true
			if ev.StopReason != StopToolUse {
				t.Fatalf("function-call completion stop=%q want %q", ev.StopReason, StopToolUse)
			}
		}
	}
	if !found {
		t.Fatalf("completed response emitted no StreamEnd: %+v", evs)
	}
}
