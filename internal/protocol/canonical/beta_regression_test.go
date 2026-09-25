package canonical

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiChunkRequiresExplicitFinish(t *testing.T) {
	evs, _, err := DecodeGeminiStreamChunk(`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Type == StreamEnd {
			t.Fatal("partial chunk marked complete")
		}
	}
}

func TestResponsesParallelToolIndices(t *testing.T) {
	for _, data := range []string{
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"c2","name":"two"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"c2","name":"two"}}`,
	} {
		evs, _, err := DecodeResponsesStreamEvent("", data)
		if err != nil || len(evs) != 1 || evs[0].ToolIndex != 2 {
			t.Fatalf("index lost: %v %v", evs, err)
		}
	}
}

func TestResponsesEmitterKeepsFinalOutput(t *testing.T) {
	rr := httptest.NewRecorder()
	e := NewResponsesEmitter(rr, "m")
	e.Emit(StreamEvent{Type: StreamText, Text: "hello"})
	e.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 4, ToolID: "c4", ToolName: "lookup"})
	e.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 4, ArgsDelta: `{"q":1}`})
	e.Emit(StreamEvent{Type: StreamEnd, StopReason: StopToolUse})
	e.Finish()
	out := rr.Body.String()
	if !strings.Contains(out, "response.output_text.done") || !strings.Contains(out, `"arguments":"{\"q\":1}"`) {
		t.Fatalf("missing final content: %s", out)
	}
}

func TestResponsesRejectsUnsupportedStateAndTools(t *testing.T) {
	for _, in := range []ResponsesRequest{
		{PreviousResponseID: "resp_old"},
		{Tools: []ResponsesTool{{Type: "web_search"}}},
	} {
		if _, err := DecodeResponsesRequest(in); err == nil {
			t.Fatal("silently discarded semantics")
		}
	}
}

func TestGeminiThinkingDoesNotForgeAnthropicSignature(t *testing.T) {
	r, err := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"secret thought","thought":true},{"text":"answer"}]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := EncodeAnthropicResponse(r, "m")
	for _, b := range out.Content {
		if b.Type == "thinking" {
			t.Fatal("Gemini thought became signed Anthropic thinking")
		}
	}
}

func TestResponsesIncompleteIsNotTransportFailure(t *testing.T) {
	evs, done, err := DecodeResponsesStreamEvent("", `{"type":"response.incomplete","response":{"usage":{"input_tokens":3,"output_tokens":4},"incomplete_details":{"reason":"max_output_tokens"}}}`)
	if err != nil || !done || len(evs) != 2 || evs[1].StopReason != StopMaxTokens {
		t.Fatalf("%v %v %v", evs, done, err)
	}
	rr := httptest.NewRecorder()
	e := NewResponsesEmitter(rr, "m")
	for _, ev := range evs {
		e.Emit(ev)
	}
	e.Finish()
	if !strings.Contains(rr.Body.String(), "event: response.incomplete") || strings.Contains(rr.Body.String(), "event: response.completed") {
		t.Fatal(rr.Body.String())
	}
}

func TestResponsesFinalOutputParallelTools(t *testing.T) {
	rr := httptest.NewRecorder()
	e := NewResponsesEmitter(rr, "m")
	for _, idx := range []int{2, 7} {
		e.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: idx, ToolName: "lookup"})
	}
	e.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 7, ArgsDelta: `{"b":2}`})
	e.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 2, ArgsDelta: `{"a":1}`})
	e.Finish()
	var final map[string]any
	reader := NewSSEReader(strings.NewReader(rr.Body.String()))
	for {
		name, data, done, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if name == "response.completed" {
			if err := json.Unmarshal([]byte(data), &final); err != nil {
				t.Fatal(err)
			}
		}
	}
	out := final["response"].(map[string]any)["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["arguments"] != `{"a":1}` || out[1].(map[string]any)["arguments"] != `{"b":2}` {
		t.Fatalf("%v", out)
	}
}

func TestResponsesOutputBufferIsBounded(t *testing.T) {
	e := NewResponsesEmitter(httptest.NewRecorder(), "m")
	if err := e.Emit(StreamEvent{Type: StreamText, Text: strings.Repeat("x", (8<<20)+1)}); err == nil {
		t.Fatal("unbounded output buffer")
	}
}
