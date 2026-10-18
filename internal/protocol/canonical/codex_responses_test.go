package canonical

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Codex CLI reconstructs ResponseItems exclusively from
// response.output_item.added / response.output_item.done; it marks
// response.function_call_arguments.delta/.done, response.content_part.*
// and response.output_text.done as unhandled
// (openai/codex codex-rs/codex-api/src/sse/responses.rs). These tests
// pin the wire shapes Codex depends on so a cross-protocol upstream
// (Chat/Anthropic/Gemini) does not silently break the Codex tool loop.

// responsesFrames collects emitter output by SSE event name.
func responsesFrames(t *testing.T, body string) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	var current string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			current = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var frame map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				t.Fatalf("non-JSON Responses frame: %v (%s)", err, line)
			}
			out[current] = append(out[current], frame)
		}
	}
	return out
}

func TestResponsesEmitterCompletedUsageIncludesTotalTokens(t *testing.T) {
	rr := httptest.NewRecorder()
	e := NewResponsesEmitter(rr, "model")
	if err := e.Emit(StreamEvent{Type: StreamUsage, Usage: &Usage{InputTokens: 8, OutputTokens: 4}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(StreamEvent{Type: StreamEnd, StopReason: StopEndTurn}); err != nil {
		t.Fatal(err)
	}
	if err := e.Finish(); err != nil {
		t.Fatal(err)
	}
	frames := responsesFrames(t, rr.Body.String())
	completed := frames["response.completed"]
	if len(completed) != 1 {
		t.Fatalf("want exactly one response.completed, got %d: %s", len(completed), rr.Body.String())
	}
	resp, _ := completed[0]["response"].(map[string]any)
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("response.completed lost usage: %s", rr.Body.String())
	}
	if got, ok := usage["total_tokens"].(float64); !ok || int(got) != 12 {
		t.Fatalf("response.completed usage total_tokens=%v want 12 (Codex requires it)", usage["total_tokens"])
	}
}

func TestResponsesEmitterFunctionCallItemIsCompleteInOutputItemDone(t *testing.T) {
	rr := httptest.NewRecorder()
	e := NewResponsesEmitter(rr, "model")
	events := []StreamEvent{
		{Type: StreamToolStart, ToolIndex: 0, ToolID: "call_1", ToolName: "lookup"},
		{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `{"q":`},
		{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `"x"}`},
		{Type: StreamToolEnd, ToolIndex: 0},
		{Type: StreamEnd, StopReason: StopToolUse},
	}
	for _, ev := range events {
		if err := e.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Finish(); err != nil {
		t.Fatal(err)
	}
	frames := responsesFrames(t, rr.Body.String())
	dones := frames["response.output_item.done"]
	if len(dones) == 0 {
		t.Fatalf("no response.output_item.done emitted: %s", rr.Body.String())
	}
	var item map[string]any
	for _, f := range dones {
		if it, _ := f["item"].(map[string]any); it != nil && it["type"] == "function_call" {
			item = it
		}
	}
	if item == nil {
		t.Fatalf("no completed function_call item: %s", rr.Body.String())
	}
	if item["name"] != "lookup" {
		t.Fatalf("completed function_call item lost name: %v", item["name"])
	}
	if item["arguments"] != `{"q":"x"}` {
		t.Fatalf("completed function_call item arguments=%q want %q (Codex parses arguments here)", item["arguments"], `{"q":"x"}`)
	}
	if item["call_id"] != "call_1" {
		t.Fatalf("completed function_call item lost call_id: %v", item["call_id"])
	}
}

func TestResponsesEmitterFunctionCallArgumentsDoneCarriesArguments(t *testing.T) {
	rr := httptest.NewRecorder()
	e := NewResponsesEmitter(rr, "model")
	for _, ev := range []StreamEvent{
		{Type: StreamToolStart, ToolIndex: 0, ToolID: "call_1", ToolName: "lookup"},
		{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `{"q":"x"}`},
		{Type: StreamToolEnd, ToolIndex: 0},
		{Type: StreamEnd, StopReason: StopToolUse},
	} {
		if err := e.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Finish(); err != nil {
		t.Fatal(err)
	}
	frames := responsesFrames(t, rr.Body.String())
	done := frames["response.function_call_arguments.done"]
	if len(done) != 1 {
		t.Fatalf("want one function_call_arguments.done, got %d: %s", len(done), rr.Body.String())
	}
	if done[0]["arguments"] != `{"q":"x"}` {
		t.Fatalf("function_call_arguments.done arguments=%q want full args", done[0]["arguments"])
	}
	if done[0]["name"] != "lookup" {
		t.Fatalf("function_call_arguments.done lost name: %v", done[0]["name"])
	}
}

func TestResponsesEmitterEmitsMessageItemLifecycle(t *testing.T) {
	rr := httptest.NewRecorder()
	e := NewResponsesEmitter(rr, "model")
	for _, ev := range []StreamEvent{
		{Type: StreamText, Text: "Hello"},
		{Type: StreamText, Text: " world"},
		{Type: StreamEnd, StopReason: StopEndTurn},
	} {
		if err := e.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Finish(); err != nil {
		t.Fatal(err)
	}
	frames := responsesFrames(t, rr.Body.String())

	addedMsg := false
	for _, f := range frames["response.output_item.added"] {
		if it, _ := f["item"].(map[string]any); it != nil && it["type"] == "message" {
			addedMsg = true
		}
	}
	if !addedMsg {
		t.Fatalf("no message response.output_item.added: %s", rr.Body.String())
	}
	var doneItem map[string]any
	for _, f := range frames["response.output_item.done"] {
		if it, _ := f["item"].(map[string]any); it != nil && it["type"] == "message" {
			doneItem = it
		}
	}
	if doneItem == nil {
		t.Fatalf("no completed message item (Codex history needs it): %s", rr.Body.String())
	}
	content, _ := doneItem["content"].([]any)
	var text string
	for _, c := range content {
		if m, _ := c.(map[string]any); m != nil {
			if s, _ := m["text"].(string); s != "" {
				text += s
			}
		}
	}
	if text != "Hello world" {
		t.Fatalf("completed message content=%q want %q", text, "Hello world")
	}
}

// TestResponsesStreamDecoderPreservesArgumentsOnlyInDoneEvent is the
// regression for the LiteLLM-style defect: models such as
// gpt-5.3-codex-spark emit function arguments only in
// response.function_call_arguments.done (and the completed
// output_item.done), never as deltas. The decoder must not lose them.
func TestResponsesStreamDecoderPreservesArgumentsOnlyInDoneEvent(t *testing.T) {
	events := []struct{ name, data string }{
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","item_id":"fc_call_1","output_index":0,"name":"lookup","arguments":"{\"q\":\"x\"}"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}}`},
	}
	var got []StreamEvent
	for _, e := range events {
		evs, _, err := DecodeResponsesStreamEvent(e.name, e.data)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, evs...)
	}
	var incremental string
	var final string
	sawFinal := false
	for _, ev := range got {
		if ev.Type != StreamToolDelta {
			continue
		}
		if ev.ArgsFinal {
			sawFinal = true
			if final == "" {
				final = ev.ArgsDelta
			}
			continue
		}
		incremental += ev.ArgsDelta
	}
	if incremental != "" {
		t.Fatalf("no deltas were streamed but decoder produced incremental args %q", incremental)
	}
	if !sawFinal || final != `{"q":"x"}` {
		t.Fatalf("arguments delivered only in .done were lost: decoded=%+v final=%q", got, final)
	}
}

func TestResponsesStreamDecoderPrefersDeltasOverFinalArguments(t *testing.T) {
	// A delta-emitting model also sends the full arguments in
	// .done and output_item.done; the decoded deltas must not be
	// doubled by the final-arguments fallback.
	events := []struct{ name, data string }{
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"f"}}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"x\"}"}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":0,"name":"f","arguments":"{\"q\":\"x\"}"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c","name":"f","arguments":"{\"q\":\"x\"}"}}`},
	}
	var got []StreamEvent
	for _, e := range events {
		evs, _, err := DecodeResponsesStreamEvent(e.name, e.data)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, evs...)
	}
	var args string
	for _, ev := range got {
		if ev.Type == StreamToolDelta && !ev.ArgsFinal {
			args += ev.ArgsDelta
		}
	}
	if args != `{"q":"x"}` {
		t.Fatalf("delta arguments were altered by final-arg events: %q decoded=%+v", args, got)
	}
}
