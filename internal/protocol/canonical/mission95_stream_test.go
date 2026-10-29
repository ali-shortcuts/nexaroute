package canonical

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestMission95SSEReader(t *testing.T) {
	// Comments, id/retry fields, multi-line data, event names, and a
	// trailing block without a final blank line.
	body := ": keep-alive\nid: 1\nretry: 1000\nevent: msg\ndata: line1\ndata: line2\n\nevent: second\ndata: tail"
	sr := NewSSEReader(strings.NewReader(body))
	name, data, done, err := sr.Next()
	if err != nil || done || name != "msg" || data != "line1\nline2" {
		t.Fatalf("first: %q %q %v %v", name, data, done, err)
	}
	name, data, done, err = sr.Next()
	if err != nil || done || name != "second" || data != "tail" {
		t.Fatalf("trailing: %q %q %v %v", name, data, done, err)
	}
	_, _, done, err = sr.Next()
	if err != nil || !done {
		t.Fatalf("eof: %v %v", done, err)
	}
	// Empty stream ends cleanly; blank-only blocks are skipped.
	sr = NewSSEReader(strings.NewReader("\n\n"))
	_, _, done, err = sr.Next()
	if err != nil || !done {
		t.Fatalf("blank stream: %v %v", done, err)
	}
	// Scanner error surfaces (not EOF).
	sr = NewSSEReader(errReader{})
	_, _, done, err = sr.Next()
	if err == nil || done {
		t.Fatalf("err reader: %v %v", done, err)
	}
	// Oversized event fails with the safe-limit error.
	big := "data: " + strings.Repeat("x", (8<<20)+1) + "\n\n"
	sr = NewSSEReader(strings.NewReader(big))
	_, _, _, err = sr.Next()
	if err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("oversized: %v", err)
	}
	// Event name resets after a blank-only block.
	sr = NewSSEReader(strings.NewReader("event: stale\n\ndata: x\n\n"))
	name, data, _, _ = sr.Next()
	if name != "" || data != "x" {
		t.Fatalf("name reset: %q %q", name, data)
	}
}

func TestMission95OpenAIStreamChunk(t *testing.T) {
	if evs, done, err := DecodeOpenAIStreamChunk("  "); err != nil || done || evs != nil {
		t.Fatalf("blank: %v %v %v", evs, done, err)
	}
	if _, done, err := DecodeOpenAIStreamChunk("[DONE]"); err != nil || !done {
		t.Fatalf("done: %v %v", done, err)
	}
	if _, _, err := DecodeOpenAIStreamChunk(`{bad`); err == nil {
		t.Fatalf("bad json should fail")
	}
	// Error envelope: string code wins, otherwise type; terminal.
	evs, done, err := DecodeOpenAIStreamChunk(`{"error":{"code":"rate_limited","type":"rate_limit_error","message":"slow"}}`)
	if err != nil || !done || len(evs) != 1 || evs[0].ErrorCode != "rate_limited" || evs[0].ErrorMsg != "slow" {
		t.Fatalf("error code: %+v %v %v", evs, done, err)
	}
	evs, _, _ = DecodeOpenAIStreamChunk(`{"error":{"code":42,"type":"api_error","message":"m"}}`)
	if evs[0].ErrorCode != "api_error" {
		t.Fatalf("error type fallback: %+v", evs)
	}
	// Usage only when tokens are positive; details mapped.
	evs, done, err = DecodeOpenAIStreamChunk(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)
	if err != nil || done || len(evs) != 0 {
		t.Fatalf("zero usage: %+v %v %v", evs, done, err)
	}
	evs, _, _ = DecodeOpenAIStreamChunk(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`)
	if len(evs) != 1 || evs[0].Usage.InputTokens != 5 || evs[0].Usage.CacheReadTokens != 2 || evs[0].Usage.ReasoningTokens != 1 {
		t.Fatalf("usage: %+v", evs)
	}
	// Content shapes, reasoning priority, tool calls, finish.
	evs, done, _ = DecodeOpenAIStreamChunk(`{"choices":[
		{"delta":{"content":"hi","reasoning_content":"rc","reasoning":"r2"},"finish_reason":null},
		{"delta":{"content":[{"type":"text","text":"a"},{"type":"output_text","text":"b"},{"type":"other","text":"no"},{"text":"notyped"},42],"reasoning":"only"},"finish_reason":"length"},
		{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}},{"index":1,"function":{"arguments":"x"}}]},"finish_reason":"tool_calls"}
	]}`)
	if done || len(evs) != 10 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	if evs[0].Text != "hi" || evs[1].Text != "rc" {
		t.Fatalf("reasoning priority: %+v", evs[:2])
	}
	if evs[2].Text != "a" || evs[3].Text != "b" || evs[4].Text != "only" {
		t.Fatalf("content array: %+v", evs[2:5])
	}
	if evs[5].Type != StreamEnd || evs[5].StopReason != StopMaxTokens {
		t.Fatalf("finish length: %+v", evs[5])
	}
	if evs[6].Type != StreamToolStart || evs[6].ToolID != "c1" || evs[7].Type != StreamToolDelta || evs[7].ArgsDelta != "{}" || evs[8].Type != StreamToolDelta || evs[8].ArgsDelta != "x" || evs[9].Type != StreamEnd || evs[9].StopReason != StopToolUse {
		t.Fatalf("tool events: %+v", evs[6:])
	}
	// Tool call with args only (no id/name) yields delta but no start.
	evs, _, _ = DecodeOpenAIStreamChunk(`{"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"arguments":"z"}}]}}]}`)
	if len(evs) != 1 || evs[0].Type != StreamToolDelta || evs[0].ToolIndex != 2 {
		t.Fatalf("args-only call: %+v", evs)
	}
	// Empty finish reason is not terminal.
	evs, done, _ = DecodeOpenAIStreamChunk(`{"choices":[{"delta":{"content":"x"},"finish_reason":""}]}`)
	if done || len(evs) != 1 {
		t.Fatalf("empty finish: %+v %v", evs, done)
	}
}

func TestMission95AnthropicStreamEvents(t *testing.T) {
	if evs, err := DecodeAnthropicStreamEvent("e", "  "); err != nil || evs != nil {
		t.Fatalf("blank: %v %v", evs, err)
	}
	if _, err := DecodeAnthropicStreamEvent("e", `{bad`); err == nil {
		t.Fatalf("bad json should fail")
	}
	evs, err := DecodeAnthropicStreamEvent("e", `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	if err != nil || len(evs) != 1 || evs[0].ErrorCode != "overloaded_error" || evs[0].ErrorMsg != "busy" {
		t.Fatalf("error envelope: %+v %v", evs, err)
	}
	evs, err = DecodeAnthropicStreamEvent("e", `{"type":"error","error":null}`)
	if err != nil || len(evs) != 1 || evs[0].ErrorMsg != "anthropic stream error event" {
		t.Fatalf("error type fallback: %+v %v", evs, err)
	}
	// message_start with and without usage.
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"message_start","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}}`)
	if len(evs) != 1 || evs[0].Usage == nil || evs[0].Usage.CacheWriteTokens != 2 {
		t.Fatalf("start usage: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"message_start","message":{"usage":{}}}`)
	if len(evs) != 1 || evs[0].Usage != nil {
		t.Fatalf("start no usage: %+v", evs)
	}
	// content_block_start variants + type-mismatch error.
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`)
	if len(evs) != 1 || evs[0].Text != "hi" {
		t.Fatalf("text start: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	if len(evs) != 0 {
		t.Fatalf("empty text start: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"th"}}`)
	if len(evs) != 1 || evs[0].Type != StreamThinking {
		t.Fatalf("thinking start: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu1","name":"f"}}`)
	if len(evs) != 1 || evs[0].ToolID != "tu1" || evs[0].ToolIndex != 2 {
		t.Fatalf("tool start: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_start","index":0,"content_block":{"type":"other"}}`)
	if len(evs) != 0 {
		t.Fatalf("unknown block: %+v", evs)
	}
	if _, err := DecodeAnthropicStreamEvent("e", `{"type":"content_block_start","index":"x","content_block":{"type":"text"}}`); err == nil {
		t.Fatalf("bad index should fail")
	}
	// Deltas.
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"t"}}`)
	if len(evs) != 1 || evs[0].Text != "t" {
		t.Fatalf("text delta: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"th"}}`)
	if len(evs) != 1 || evs[0].Type != StreamThinking {
		t.Fatalf("thinking delta: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)
	if len(evs) != 1 || evs[0].Type != StreamToolDelta || evs[0].ToolIndex != 3 {
		t.Fatalf("json delta: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_delta","index":0,"delta":{"type":"other","text":"x"}}`)
	if len(evs) != 0 {
		t.Fatalf("unknown delta: %+v", evs)
	}
	if _, err := DecodeAnthropicStreamEvent("e", `{"type":"content_block_delta","index":"x","delta":{"type":"text_delta"}}`); err == nil {
		t.Fatalf("bad delta index should fail")
	}
	// Block stop, message delta, message stop, ping, unknown.
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"content_block_stop","index":4}`)
	if len(evs) != 1 || evs[0].Type != StreamToolEnd || evs[0].ToolIndex != 4 {
		t.Fatalf("block stop: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9,"input_tokens":1}}`)
	if len(evs) != 2 || evs[0].Type != StreamUsage || evs[1].StopReason != StopMaxTokens {
		t.Fatalf("message delta: %+v", evs)
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"message_delta","delta":{},"usage":{}}`)
	if len(evs) != 0 {
		t.Fatalf("empty delta: %+v", evs)
	}
	if _, err := DecodeAnthropicStreamEvent("e", `{"type":"message_delta","delta":{},"usage":{"output_tokens":"x"}}`); err == nil {
		t.Fatalf("bad delta usage should fail")
	}
	evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"message_stop"}`)
	if len(evs) != 1 || evs[0].StopReason != StopEndTurn {
		t.Fatalf("message stop: %+v", evs)
	}
	for _, typ := range []string{"ping", "content_block_start_empty", "mystery"} {
		evs, _ = DecodeAnthropicStreamEvent("e", `{"type":"`+typ+`"}`)
		if len(evs) != 0 {
			t.Fatalf("%s: %+v", typ, evs)
		}
	}
}

func TestMission95GeminiStreamChunk(t *testing.T) {
	if evs, done, err := DecodeGeminiStreamChunk("  "); err != nil || done || evs != nil {
		t.Fatalf("blank: %v %v %v", evs, done, err)
	}
	if _, _, err := DecodeGeminiStreamChunk(`{bad`); err == nil {
		t.Fatalf("bad json should fail")
	}
	// Intermediate chunk: text + usage, not terminal.
	evs, done, err := DecodeGeminiStreamChunk(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3}}`)
	if err != nil || done || len(evs) != 2 || evs[0].Text != "hi" || evs[1].Usage.OutputTokens != 3 {
		t.Fatalf("intermediate: %+v %v %v", evs, done, err)
	}
	// Terminal via finish reason; tool call expands to 3 events.
	evs, done, err = DecodeGeminiStreamChunk(`{"candidates":[{"content":{"parts":[{"text":"t","thought":true},{"functionCall":{"name":"f","args":{"a":1}}}]},"finishReason":"STOP"}]}`)
	if err != nil || !done || len(evs) != 5 {
		t.Fatalf("terminal: %+v %v %v", evs, done, err)
	}
	if evs[0].Type != StreamThinking || evs[1].Type != StreamToolStart || evs[2].Type != StreamToolDelta || evs[3].Type != StreamToolEnd || evs[4].StopReason != StopToolUse {
		t.Fatalf("terminal events: %+v", evs)
	}
	// Terminal via prompt block reason alone.
	evs, done, err = DecodeGeminiStreamChunk(`{"promptFeedback":{"blockReason":"SAFETY"}}`)
	if err != nil || !done || len(evs) != 1 || evs[0].StopReason != StopRefusal {
		t.Fatalf("block terminal: %+v %v %v", evs, done, err)
	}
	// Decode error passthrough: a chunk the raw parse accepts but the
	// response decoder rejects does not occur for plain JSON, so the
	// terminal-but-empty shape is covered above instead.
}

func TestMission95Emitters(t *testing.T) {
	// NewStreamEmitter routing.
	rec := httptest.NewRecorder()
	if _, ok := NewStreamEmitter("anthropic", rec, "m", "r1").(*anthropicEmitter); !ok {
		t.Fatalf("anthropic routing")
	}
	rec = httptest.NewRecorder()
	if _, ok := NewStreamEmitter("openai_responses", rec, "m", "r1").(*responsesEmitter); !ok {
		t.Fatalf("responses routing")
	}
	rec = httptest.NewRecorder()
	if _, ok := NewStreamEmitter("weird", rec, "m", "r1").(*openAIEmitter); !ok {
		t.Fatalf("default routing")
	}
	// Anthropic emitter: text -> thinking -> tool flow, usage, error,
	// Finish idempotence and default stop.
	rec = httptest.NewRecorder()
	ae := NewAnthropicEmitter(rec, "model-x", "req-1")
	_ = ae.Emit(StreamEvent{Type: StreamStart, Usage: &Usage{InputTokens: 4, CacheReadTokens: 1}})
	_ = ae.Emit(StreamEvent{Type: StreamText, Text: "hello "})
	_ = ae.Emit(StreamEvent{Type: StreamText, Text: "world"})
	_ = ae.Emit(StreamEvent{Type: StreamThinking, Text: "th"})
	_ = ae.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 7, ToolName: "f", ArgsDelta: `{"a":`})
	_ = ae.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 7, ArgsDelta: `1}`})
	_ = ae.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 7})
	_ = ae.Emit(StreamEvent{Type: StreamUsage, Usage: &Usage{OutputTokens: 6}})
	_ = ae.Emit(StreamEvent{Type: StreamEnd, StopReason: StopMaxTokens})
	_ = ae.Emit(StreamEvent{Type: StreamError, ErrorCode: "rate_limit_error", ErrorMsg: "slow"})
	if err := ae.Finish(); err != nil {
		t.Fatal(err)
	}
	body1 := rec.Body.String()
	if err := ae.Finish(); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != body1 {
		t.Fatalf("Finish must be idempotent")
	}
	body := body1
	for _, want := range []string{"message_start", "content_block_start", "text_delta", "thinking_delta", "tool_use", "input_json_delta", "content_block_stop", "message_delta", "message_stop", "rate_limit_error", "slow"} {
		if !strings.Contains(body, want) {
			t.Fatalf("anthropic body missing %q", want)
		}
	}
	if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
		t.Fatalf("anthropic stop: %s", body)
	}
	if !strings.Contains(body, `"output_tokens":6`) || !strings.Contains(body, `"input_tokens":4`) {
		t.Fatalf("anthropic usage: %s", body)
	}
	// mapErrorType table + default stop.
	for _, tc := range []struct{ in, want string }{
		{"overloaded_error", "overloaded_error"}, {"invalid_request_error", "invalid_request_error"},
		{"authentication_error", "authentication_error"}, {"other", "api_error"}, {"RATE_LIMIT_ERROR", "rate_limit_error"},
	} {
		if got := mapErrorType(tc.in); got != tc.want {
			t.Fatalf("mapErrorType(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
	rec = httptest.NewRecorder()
	ae = NewAnthropicEmitter(rec, "m", "r")
	_ = ae.Finish()
	if !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Fatalf("default stop: %s", rec.Body.String())
	}
	// OpenAI emitter: limits, chunks, Finish tail.
	rec = httptest.NewRecorder()
	oe := NewOpenAIEmitter(rec, "model-y")
	_ = oe.Emit(StreamEvent{Type: StreamStart})
	_ = oe.Emit(StreamEvent{Type: StreamText, Text: "hi"})
	_ = oe.Emit(StreamEvent{Type: StreamThinking, Text: "th"})
	_ = oe.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: "f"})
	_ = oe.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: "{}"})
	_ = oe.Emit(StreamEvent{Type: StreamToolEnd})
	_ = oe.Emit(StreamEvent{Type: StreamUsage, Usage: &Usage{InputTokens: 2, OutputTokens: 3, CacheReadTokens: 1, ReasoningTokens: 1}})
	_ = oe.Emit(StreamEvent{Type: StreamEnd, StopReason: StopToolUse})
	_ = oe.Emit(StreamEvent{Type: StreamError, ErrorCode: "bad", ErrorMsg: "oops"})
	if err := oe.Finish(); err != nil {
		t.Fatal(err)
	}
	body = rec.Body.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"hi"`, `"reasoning_content":"th"`, `"tool_calls"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":2`, `"cached_tokens":1`, `"reasoning_tokens":1`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("openai body missing %q", want)
		}
	}
	if err := oe.Finish(); err != nil {
		t.Fatal(err)
	}
	// Empty stream still emits the role chunk on Finish.
	rec = httptest.NewRecorder()
	oe = NewOpenAIEmitter(rec, "m")
	_ = oe.Finish()
	if !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Fatalf("empty finish: %s", rec.Body.String())
	}
	// Limits.
	rec = httptest.NewRecorder()
	oe = NewOpenAIEmitter(rec, "m")
	if err := oe.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: -1}); err == nil {
		t.Fatalf("negative index should fail")
	}
	if err := oe.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: strings.Repeat("n", 1025)}); err == nil {
		t.Fatalf("long name should fail")
	}
	if err := oe.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: strings.Repeat("x", (1<<20)+1)}); err == nil {
		t.Fatalf("long delta should fail")
	}
	if got := openAIFinish(StopRefusal); got != "content_filter" {
		t.Fatalf("openAIFinish refusal: %q", got)
	}
	if got := openAIFinish("weird"); got != "stop" {
		t.Fatalf("openAIFinish default: %q", got)
	}
	// Responses emitter: message flow, tool flow, limits, usage merge,
	// incomplete status, error, Finish.
	rec = httptest.NewRecorder()
	re_ := NewResponsesEmitter(rec, "model-z")
	_ = re_.Emit(StreamEvent{Type: StreamStart, Usage: &Usage{InputTokens: 5}})
	_ = re_.Emit(StreamEvent{Type: StreamText, Text: "hi"})
	_ = re_.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: "f"})
	_ = re_.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `{"a":1}`})
	_ = re_.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 0})
	_ = re_.Emit(StreamEvent{Type: StreamUsage, Usage: &Usage{InputTokens: 3, OutputTokens: 4, CacheReadTokens: 2}})
	_ = re_.Emit(StreamEvent{Type: StreamEnd, StopReason: StopMaxTokens})
	if err := re_.Finish(); err != nil {
		t.Fatal(err)
	}
	body = rec.Body.String()
	for _, want := range []string{"response.created", "response.output_text.delta", "response.output_item.added", "function_call", "response.function_call_arguments.done", "response.completed", `"status":"incomplete"`, `"input_tokens":5`, `"output_tokens":4`, `"cached_tokens":2`} {
		if !strings.Contains(body, want) {
			t.Fatalf("responses body missing %q", want)
		}
	}
	// Delta/end before start fail; tool count limit.
	rec = httptest.NewRecorder()
	re_ = NewResponsesEmitter(rec, "m")
	if err := re_.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: "x"}); err == nil {
		t.Fatalf("delta before start should fail")
	}
	if err := re_.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 0}); err == nil {
		t.Fatalf("end before start should fail")
	}
	if err := re_.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: -1}); err == nil {
		t.Fatalf("responses negative index should fail")
	}
	rec = httptest.NewRecorder()
	re_ = NewResponsesEmitter(rec, "m")
	var lastErr error
	for i := 0; i <= maxStreamTools; i++ {
		lastErr = re_.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: i, ToolName: "f"})
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "safe tool count") {
		t.Fatalf("129th tool should fail: %v", lastErr)
	}
	// Error event + thinking delta.
	rec = httptest.NewRecorder()
	re_ = NewResponsesEmitter(rec, "m")
	_ = re_.Emit(StreamEvent{Type: StreamThinking, Text: "th"})
	_ = re_.Emit(StreamEvent{Type: StreamError, ErrorCode: "c", ErrorMsg: "m"})
	_ = re_.Finish()
	body = rec.Body.String()
	if !strings.Contains(body, "response.reasoning_summary_text.delta") || !strings.Contains(body, "response.failed") {
		t.Fatalf("responses error/thinking: %s", body)
	}
	// uniqueMessageID / pad.
	if got := pad("abc", 5); got != "00abc" {
		t.Fatalf("pad: %q", got)
	}
	if got := pad("abcdefgh", 5); got != "defgh" {
		t.Fatalf("pad truncate: %q", got)
	}
	id := uniqueMessageID("zz")
	if !strings.HasPrefix(id, "msg_") || !strings.HasSuffix(id, "_aa") {
		t.Fatalf("unique id zz: %q", id)
	}
	id = uniqueMessageID(strings.Repeat("a", 20))
	if !strings.HasSuffix(id, "_"+strings.Repeat("a", 16)) {
		t.Fatalf("unique id long: %q", id)
	}
	id = uniqueMessageID("")
	if strings.Contains(id, "_") && len(strings.Split(id, "_")) > 2 {
		t.Fatalf("empty base should have no suffix: %q", id)
	}
}
