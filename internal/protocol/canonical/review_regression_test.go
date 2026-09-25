package canonical

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewResponsesStringContentAndOrder(t *testing.T) {
	in := ResponsesRequest{Input: json.RawMessage(`[{"role":"assistant","content":"first"},"second",{"role":"user","content":"third"}]`)}
	r, err := DecodeResponsesRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 3 || r.Messages[0].Parts[0].Text != "first" || r.Messages[1].Parts[0].Text != "second" || r.Messages[2].Parts[0].Text != "third" {
		t.Fatalf("messages reordered: %#v", r.Messages)
	}
}

func TestReviewResponsesForcedTool(t *testing.T) {
	r, err := DecodeResponsesRequest(ResponsesRequest{Input: json.RawMessage(`"hi"`), ToolChoice: map[string]any{"type": "function", "name": "lookup"}})
	if err != nil || r.ToolChoice == nil || r.ToolChoice.Mode != "tool" || r.ToolChoice.Name != "lookup" {
		t.Fatalf("forced tool lost: %+v %v", r.ToolChoice, err)
	}
}

func TestReviewResponsesToolStopAndUnsignedSummary(t *testing.T) {
	r, err := DecodeResponsesResponse([]byte(`{"id":"r","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.StopReason != StopToolUse {
		t.Errorf("tool call stop=%q", r.StopReason)
	}
	for _, b := range EncodeAnthropicResponse(r, "m").Content {
		if b.Type == "thinking" {
			t.Error("fabricated summary signature")
		}
	}
}

func TestReviewResponsesDoesNotStoreUpstream(t *testing.T) {
	b, err := EncodeResponsesRequest(Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	json.Unmarshal(b, &obj)
	if obj["store"] != false {
		t.Fatalf("stateless gateway did not disable upstream storage: %s", b)
	}
}

func TestReviewResponsesRejectsDiscardedInput(t *testing.T) {
	for _, raw := range []string{`[{"type":"item_reference","id":"old"}]`, `[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]`} {
		if _, err := DecodeResponsesRequest(ResponsesRequest{Input: json.RawMessage(raw)}); err == nil {
			t.Fatalf("silently ignored %s", raw)
		}
	}
}

func TestReviewResponsesContentFilterReason(t *testing.T) {
	raw := `{"status":"incomplete","output":[],"incomplete_details":{"reason":"content_filter"}}`
	r, err := DecodeResponsesResponse([]byte(raw))
	if err != nil || r.StopReason != StopRefusal {
		t.Errorf("response stop=%q %v", r.StopReason, err)
	}
	evs, _, err := DecodeResponsesStreamEvent("", `{"type":"response.incomplete","response":`+raw+`}`)
	if err != nil || len(evs) != 1 || evs[0].StopReason != StopRefusal {
		t.Fatalf("stream=%v %v", evs, err)
	}
}

func TestReviewSSEMultilineFrameBound(t *testing.T) {
	// Each line is small, but their aggregate must still be bounded.
	input := strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", 17000) + "\n"
	_, _, _, err := NewSSEReader(strings.NewReader(input)).Next()
	if err == nil {
		t.Fatal("SSE multiline frame has no total byte bound")
	}
}

func TestReviewResponsesExactItemBound(t *testing.T) {
	w := httptest.NewRecorder()
	e := NewResponsesEmitter(w, "m")
	for i := 0; i < 4096; i++ {
		if err := e.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: i, ToolName: "f"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 4096, ToolName: "f"}); err == nil {
		t.Fatal("4097th output item accepted")
	}
}

func TestReviewResponsesMissingToolStartAtByteBound(t *testing.T) {
	e := NewResponsesEmitter(httptest.NewRecorder(), "m")
	// Nested implicit tool creation must propagate failure, never index absent items.
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panic on bound: %v", p)
		}
	}()
	if err := e.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 1, ToolName: strings.Repeat("n", 4<<20), ArgsDelta: strings.Repeat("a", 5<<20)}); err == nil {
		t.Fatal("expected bounded failure")
	}
}

func TestReviewNormalStopIsNotTokenExhaustion(t *testing.T) {
	for _, reason := range []string{"stop", "stop_sequence"} {
		stop := MapStopReason(reason)
		resp := EncodeAnthropicResponse(Response{StopReason: stop}, "m")
		if *resp.StopReason == StopMaxTokens {
			t.Errorf("%s incorrectly became token exhaustion", reason)
		}
		rr := httptest.NewRecorder()
		e := NewAnthropicEmitter(rr, "m", "r")
		e.Emit(StreamEvent{Type: StreamEnd, StopReason: stop})
		e.Finish()
		if strings.Contains(rr.Body.String(), `"stop_reason":"max_tokens"`) {
			t.Errorf("stream %s incorrectly became token exhaustion", reason)
		}
	}
}

func TestReviewResponsesIncompleteRoundTrip(t *testing.T) {
	out := EncodeResponsesResponse(Response{StopReason: StopRefusal, Usage: Usage{InputTokens: 3, OutputTokens: 4}}, "m")
	b, _ := json.Marshal(out)
	r, err := DecodeResponsesResponse(b)
	if err != nil || r.StopReason != StopRefusal {
		t.Fatalf("lost incomplete reason: %s %v", b, err)
	}
	var obj map[string]any
	json.Unmarshal(b, &obj)
	if obj["usage"].(map[string]any)["total_tokens"] != float64(7) {
		t.Fatalf("missing total usage: %s", b)
	}
}

func TestReviewResponsesEmptyInputRejected(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `""`, `[null]`} {
		if _, err := DecodeResponsesRequest(ResponsesRequest{Input: json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted empty input: %s", raw)
		}
	}
}
