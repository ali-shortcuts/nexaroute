package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMission95ResponsesDecodeRequest(t *testing.T) {
	// Instructions: whitespace-only dropped, valid kept; previous id
	// fails closed; tool type guard; choice guard; reasoning default.
	if _, err := DecodeResponsesRequest(ResponsesRequest{PreviousResponseID: "resp_1"}); err == nil {
		t.Fatalf("previous id should fail")
	}
	req, err := DecodeResponsesRequest(ResponsesRequest{
		Model: "m", Instructions: json.RawMessage(`"  "`),
		Tools: []ResponsesTool{{Type: "web_search"}},
	})
	if err == nil || !strings.Contains(err.Error(), "web_search") {
		t.Fatalf("builtin tool: %v", err)
	}
	req, err = DecodeResponsesRequest(ResponsesRequest{
		Model: "m", Instructions: json.RawMessage(`"be helpful"`),
		Tools:      []ResponsesTool{{Type: "", Name: "f"}},
		Reasoning:  json.RawMessage(`{"summary":"x"}`),
		Text:       json.RawMessage(`{"format":{"type":"json_object"}}`),
		Metadata:   map[string]any{"k": "v"},
		ToolChoice: map[string]any{"type": "computer_use"},
	})
	if err == nil || !strings.Contains(err.Error(), "computer_use") {
		t.Fatalf("builtin choice: %v", err)
	}
	req, err = DecodeResponsesRequest(ResponsesRequest{
		Model: "m", Instructions: json.RawMessage(`"be helpful"`),
		Tools:     []ResponsesTool{{Type: "", Name: "f"}},
		Reasoning: json.RawMessage(`{"summary":"x"}`),
		Text:      json.RawMessage(`{"format":{"type":"json_object"}}`),
		Metadata:  map[string]any{"k": "v"},
	})
	if err != nil || len(req.System) != 1 || req.System[0].Text != "be helpful" {
		t.Fatalf("instructions: %+v %v", req, err)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "medium" {
		t.Fatalf("reasoning default: %+v", req.Reasoning)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != FormatJSONObject {
		t.Fatalf("format: %+v", req.ResponseFormat)
	}
	if req.Metadata["k"] != "v" || len(req.Tools) != 1 || req.Tools[0].Parameters == nil {
		t.Fatalf("tools/meta: %+v %v", req.Tools, req.Metadata)
	}
	// Reasoning effort passthrough, bad reasoning JSON ignored,
	// schema format, null text ignored.
	req, err = DecodeResponsesRequest(ResponsesRequest{
		Reasoning: json.RawMessage(`{"effort":"high"}`),
		Text:      json.RawMessage(`{"format":{"type":"json_schema","name":"n","schema":{"type":"object"}}}`),
		Input:     json.RawMessage(`"hello"`),
	})
	if err != nil || req.Reasoning.Effort != "high" || req.ResponseFormat.Name != "n" {
		t.Fatalf("effort/schema: %+v %v", req, err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Parts[0].Text != "hello" {
		t.Fatalf("string input: %+v", req.Messages)
	}
	req, err = DecodeResponsesRequest(ResponsesRequest{Reasoning: json.RawMessage(`{bad`), Text: json.RawMessage(`null`), Input: json.RawMessage(`""`)})
	if err != nil || req.Reasoning != nil || req.ResponseFormat != nil || len(req.Messages) != 0 {
		t.Fatalf("bad reasoning/null: %+v %v", req, err)
	}
	// decodeResponsesFormat table.
	if got := decodeResponsesFormat(json.RawMessage(`{bad`)); got != nil {
		t.Fatalf("bad format json: %+v", got)
	}
	if got := decodeResponsesFormat(json.RawMessage(`{"type":"text"}`)); got != nil {
		t.Fatalf("text format: %+v", got)
	}
	if got := decodeResponsesFormat(json.RawMessage(`{"type":"json_schema","name":"n","schema":{"type":"object"}}`)); got == nil || got.Kind != FormatJSONSchema || got.Name != "n" {
		t.Fatalf("schema format: %+v", got)
	}
	// decodeResponsesInput shapes.
	parts, msgs, err := decodeResponsesInput(nil)
	if err != nil || parts != nil || msgs != nil {
		t.Fatalf("nil input: %v %v %v", parts, msgs, err)
	}
	parts, msgs, err = decodeResponsesInput(json.RawMessage(`null`))
	if err != nil || parts != nil || msgs != nil {
		t.Fatalf("null input: %v %v %v", parts, msgs, err)
	}
	if _, _, err := decodeResponsesInput(json.RawMessage(`{"a":1}`)); err == nil {
		t.Fatalf("object input should fail")
	}
	parts, msgs, err = decodeResponsesInput(json.RawMessage(`["a", "", {"type":"message","content":"hi"}, {"type":"message","role":"assistant","content":[{"type":"input_text","text":"x"},{"type":"input_image","image_url":"https://i/p.png"}]}, {"type":"function_call","call_id":"c1","name":"f","arguments":""}, {"type":"function_call","name":""}, {"type":"function_call_output","call_id":"c1","output":"out"}, {"type":"reasoning"}, {"type":"mystery"}, {"call_id":"c2","output":"o2"}, {"name":"g"}, {"role":"user","content":"bare"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Text != "a" {
		t.Fatalf("standalone: %+v", parts)
	}
	// messages: message(user hi), assistant image msg, function_call f,
	// function_call_output, function_call_output (inferred), function_call g,
	// bare message. Nameless call, reasoning, mystery dropped.
	if len(msgs) != 7 {
		t.Fatalf("messages = %d: %+v", len(msgs), msgs)
	}
	if msgs[1].Parts[1].Type != PartImage || msgs[2].Parts[0].ToolCall.Arguments != "{}" {
		t.Fatalf("msg shapes: %+v", msgs)
	}
	if msgs[4].Parts[0].ToolResult.Content != "o2" || msgs[5].Parts[0].ToolCall.Name != "g" || msgs[6].Parts[0].Text != "bare" {
		t.Fatalf("inferred types: %+v", msgs)
	}
	if _, _, err := decodeResponsesInput(json.RawMessage(`[1]`)); err == nil {
		t.Fatalf("number item should fail")
	}
	if _, _, err := decodeResponsesInput(json.RawMessage(`[{"type":"message","content":[{"type":"input_audio"}]}]`)); err == nil {
		t.Fatalf("unsupported content should fail")
	}
	// responsesContentToParts: null/empty/string/array, bad array JSON.
	if got, err := responsesContentToParts(nil); err != nil || got != nil {
		t.Fatalf("nil content: %v %v", got, err)
	}
	if got, err := responsesContentToParts(json.RawMessage(`"s"`)); err != nil || len(got) != 1 || got[0].Text != "s" {
		t.Fatalf("string content: %+v %v", got, err)
	}
	if got, err := responsesContentToParts(json.RawMessage(`{"a":1}`)); err != nil || got != nil {
		t.Fatalf("object content should be empty without error: %+v %v", got, err)
	}
	if got, err := responsesContentToParts(json.RawMessage(`[{"type":"","text":"t"},{"type":"output_text","text":"u"},{"type":"text","text":""}]`)); err != nil || len(got) != 2 {
		t.Fatalf("array content: %+v %v", got, err)
	}
	// rawOrEmptyObject.
	if got := rawOrEmptyObject(nil); string(got) != `{"type":"object"}` {
		t.Fatalf("nil raw: %s", got)
	}
	if got := rawOrEmptyObject(json.RawMessage(`null`)); string(got) != `{"type":"object"}` {
		t.Fatalf("null raw: %s", got)
	}
	if got := rawOrEmptyObject(json.RawMessage(`{"type":"object"}`)); string(got) != `{"type":"object"}` {
		t.Fatalf("kept raw: %s", got)
	}
}

func TestMission95ResponsesEncodeRequest(t *testing.T) {
	req := Request{
		System: []Part{{Type: PartText, Text: "sys"}},
		Messages: []Message{
			{Role: "assistant", Parts: []Part{
				{Type: PartText, Text: "a"}, {Type: PartText, Text: "b"},
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "f", Arguments: "  "}},
				{Type: PartToolCall, ToolCall: &ToolCall{Name: ""}},
			}},
			{Role: "tool", Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "c1", Content: "out"}}, {Type: PartText, Text: "ignored"}}},
			{Role: "user", Parts: nil},
			{Role: "user", Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "c2", Content: "o2"}}}},
			{Role: "user", Parts: []Part{
				{Type: PartText, Text: "see"},
				{Type: PartImage, Image: &Image{Data: "ZZ"}},
				{Type: PartImage},
			}},
		},
		MaxOutputTokens: 10, Temperature: f64p(0.5), TopP: f64p(0.9), Stop: []string{"END"},
		Tools:      []ToolDef{{Name: "f", Description: "d"}},
		ToolChoice: &ToolChoice{Mode: "tool", Name: "f"}, ParallelToolCalls: boolp(true),
		PromptCacheKey: "k", Reasoning: &Reasoning{Effort: ""},
		ResponseFormat: &ResponseFormat{Kind: FormatJSONSchema, Name: "n", Schema: nil},
		Stream:         true,
	}
	b, err := EncodeResponsesRequest(req, "up")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	items, ok := out["input"].([]any)
	if !ok {
		t.Fatalf("input: %#v", out["input"])
	}
	// system, assistant call, assistant message, tool output, user tool
	// output, user multi-part message = 6 items.
	if len(items) != 6 {
		t.Fatalf("items = %d: %v", len(items), items)
	}
	call := items[1].(map[string]any)
	if call["type"] != "function_call" || call["arguments"] != "{}" {
		t.Fatalf("call item: %v", call)
	}
	msg := items[5].(map[string]any)
	content, ok := msg["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("user content: %v", msg)
	}
	img := content[1].(map[string]any)
	if img["image_url"] != "data:image/png;base64,ZZ" {
		t.Fatalf("image item: %v", img)
	}
	if out["max_output_tokens"] != float64(10) || out["temperature"] != 0.5 || out["tool_choice"] == nil || out["parallel_tool_calls"] != true || out["prompt_cache_key"] != "k" {
		t.Fatalf("scalar fields: %v", out)
	}
	if out["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatalf("reasoning: %v", out["reasoning"])
	}
	text := out["text"].(map[string]any)["format"].(map[string]any)
	if text["type"] != "json_schema" || text["name"] != "n" {
		t.Fatalf("text format: %v", text)
	}
	if tools := out["tools"].([]any); tools[0].(map[string]any)["parameters"] == nil {
		t.Fatalf("tool params: %v", tools)
	}
	// JSONObject format + disabled reasoning + tool choice string.
	// Fresh decode target: unmarshaling into the reused map would
	// keep the previous "reasoning" key (absent keys are not cleared).
	req.ResponseFormat = &ResponseFormat{Kind: FormatJSONObject}
	req.Reasoning = &Reasoning{Disabled: true}
	req.ToolChoice = &ToolChoice{Mode: "none"}
	b, _ = EncodeResponsesRequest(req, "up")
	var out2 map[string]any
	if err := json.Unmarshal(b, &out2); err != nil {
		t.Fatal(err)
	}
	if out2["tool_choice"] != "none" {
		t.Fatalf("choice string: %v", out2["tool_choice"])
	}
	if _, ok := out2["reasoning"]; ok {
		t.Fatalf("disabled reasoning should be absent")
	}
	if out2["text"].(map[string]any)["format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("json_object: %v", out2["text"])
	}
}

func TestMission95ResponsesDecodeEncodeResponse(t *testing.T) {
	if _, err := DecodeResponsesResponse([]byte(`{bad`)); err == nil {
		t.Fatalf("bad json should fail")
	}
	if _, err := DecodeResponsesResponse([]byte(`{"status":"failed"}`)); err == nil || !strings.Contains(err.Error(), "upstream Responses request failed") {
		t.Fatalf("failed default: %v", err)
	}
	if _, err := DecodeResponsesResponse([]byte(`{"status":"failed","error":{"message":"boom","code":"c1"}}`)); err == nil || !strings.Contains(err.Error(), "c1") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failed code: %v", err)
	}
	resp, err := DecodeResponsesResponse([]byte(`{"id":"r1","model":"m","status":"completed","output":[
		{"type":"message","content":[{"type":"output_text","text":"a"},{"type":"text","text":"b"},{"type":"other","text":"no"},{"text":"bare"}]},
		{"type":"function_call","name":"","arguments":"{}"},
		{"type":"function_call","name":"f","arguments":""},
		{"type":"reasoning","summary":[{"text":"s1"},{"text":"s2"}]},
		{"type":"reasoning","summary":[]}
	],"usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 3 || resp.Blocks[0].Text != "a\nb\nbare" || resp.Blocks[1].ToolCall.ID != "call_1" || resp.Blocks[1].ToolCall.Arguments != "{}" {
		t.Fatalf("blocks: %+v", resp.Blocks)
	}
	if resp.Blocks[2].Type != PartThinking || resp.Blocks[2].Thinking.Text != "s1\ns2" || resp.Blocks[2].Thinking.Signature != "summary" {
		t.Fatalf("reasoning block: %+v", resp.Blocks[2])
	}
	if resp.StopReason != StopToolUse || resp.Usage.CacheReadTokens != 2 || resp.Usage.ReasoningTokens != 1 {
		t.Fatalf("meta: %+v", resp)
	}
	resp, err = DecodeResponsesResponse([]byte(`{"status":"incomplete","output":[]}`))
	if err != nil || resp.StopReason != StopMaxTokens || len(resp.Blocks) != 0 {
		t.Fatalf("incomplete: %+v %v", resp, err)
	}
	// Encode: status mapping, id synthesis, call id synthesis, args {}.
	enc := EncodeResponsesResponse(Response{StopReason: StopMaxTokens, Blocks: []Block{
		{Type: PartText, Text: "hi"},
		{Type: PartToolCall, ToolCall: &ToolCall{Name: "f"}},
		{Type: PartToolCall, ToolCall: &ToolCall{Name: ""}},
		{Type: PartThinking, Thinking: &Thinking{Text: "th"}},
	}}, "req-model")
	if enc.Status != "incomplete" || enc.Model != "req-model" || enc.ID == "" {
		t.Fatalf("encode status: %+v", enc)
	}
	if len(enc.Output) != 3 || enc.Output[1].CallID != "call_1" || enc.Output[1].Arguments != "{}" || enc.Output[1].ID != "fc_call_1" {
		t.Fatalf("encode output: %+v", enc.Output)
	}
	if enc.Output[2].Type != "reasoning" || enc.Output[2].Summary[0]["text"] != "th" {
		t.Fatalf("encode reasoning: %+v", enc.Output[2])
	}
	enc = EncodeResponsesResponse(Response{StopReason: StopRefusal, Usage: Usage{InputTokens: 1, OutputTokens: 2}}, "")
	if enc.Status != "incomplete" || enc.Usage.TotalTokens != 3 || enc.Usage.OutputTokensDetails.ReasoningTokens != 0 {
		t.Fatalf("refusal encode: %+v", enc)
	}
}

func TestMission95ResponsesStreamEvents(t *testing.T) {
	// Blank data, invalid JSON, unknown type, empty deltas -> no events.
	for _, tc := range []struct{ name, data string }{
		{"blank", "  "},
		{"bad", `{bad`},
		{"unknown", `{"type":"response.created"}`},
		{"empty-delta", `{"type":"response.output_text.delta","delta":""}`},
		{"empty-think", `{"type":"response.reasoning_summary_text.delta","delta":""}`},
		{"non-call-added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`},
		{"empty-arg-delta", `{"type":"response.function_call_arguments.delta","delta":""}`},
		{"empty-arg-done", `{"type":"response.function_call_arguments.done","arguments":""}`},
		{"non-call-done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`},
	} {
		evs, done, err := DecodeResponsesStreamEvent(tc.name, tc.data)
		if tc.name == "bad" {
			if err == nil {
				t.Fatalf("bad should error, got %v %v", evs, done)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(evs) != 0 || done {
			t.Fatalf("%s: %v %v", tc.name, evs, done)
		}
	}
	// Event-name fallback when the payload has no type field.
	evs, _, err := DecodeResponsesStreamEvent("response.output_text.delta", `{"delta":"hi"}`)
	if err != nil || len(evs) != 1 || evs[0].Text != "hi" {
		t.Fatalf("fallback name: %+v %v", evs, err)
	}
	evs, _, err = DecodeResponsesStreamEvent("", `{"type":"response.reasoning_text.delta","delta":"th"}`)
	if err != nil || len(evs) != 1 || evs[0].Type != StreamThinking {
		t.Fatalf("thinking delta: %+v %v", evs, err)
	}
	evs, _, err = DecodeResponsesStreamEvent("", `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"c9","name":"f"}}`)
	if err != nil || len(evs) != 1 || evs[0].Type != StreamToolStart || evs[0].ToolIndex != 2 || evs[0].ToolID != "c9" {
		t.Fatalf("tool start: %+v %v", evs, err)
	}
	evs, _, err = DecodeResponsesStreamEvent("", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"a\":"}`)
	if err != nil || len(evs) != 1 || evs[0].ArgsDelta != `{"a":` {
		t.Fatalf("arg delta: %+v %v", evs, err)
	}
	evs, _, err = DecodeResponsesStreamEvent("", `{"type":"response.function_call_arguments.done","output_index":1,"name":"f","arguments":"{}"}`)
	if err != nil || len(evs) != 1 || !evs[0].ArgsFinal || evs[0].ToolName != "f" {
		t.Fatalf("arg done: %+v %v", evs, err)
	}
	evs, _, err = DecodeResponsesStreamEvent("", `{"type":"response.output_item.done","output_index":3,"item":{"type":"function_call","call_id":"c3","name":"g","arguments":"{}"}}`)
	if err != nil || len(evs) != 2 || evs[0].Type != StreamToolDelta || evs[1].Type != StreamToolEnd {
		t.Fatalf("item done with args: %+v %v", evs, err)
	}
	evs, _, err = DecodeResponsesStreamEvent("", `{"type":"response.output_item.done","output_index":3,"item":{"type":"function_call","call_id":"c3","name":"g"}}`)
	if err != nil || len(evs) != 1 || evs[0].Type != StreamToolEnd {
		t.Fatalf("item done no args: %+v %v", evs, err)
	}
	// Incomplete: usage + max_tokens end, terminal.
	evs, done, err := DecodeResponsesStreamEvent("", `{"type":"response.incomplete","response":{"usage":{"input_tokens":5,"output_tokens":2,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}}}}`)
	if err != nil || !done || len(evs) != 2 || evs[0].Type != StreamUsage || evs[0].Usage.CacheReadTokens != 1 || evs[1].StopReason != StopMaxTokens {
		t.Fatalf("incomplete: %+v %v %v", evs, done, err)
	}
	if _, _, err := DecodeResponsesStreamEvent("", `{"type":"response.incomplete","response":`); err == nil {
		t.Fatalf("bad incomplete should error")
	}
	// Failed: defaults and explicit message/code.
	evs, done, err = DecodeResponsesStreamEvent("", `{"type":"response.failed","response":{"error":{"message":"boom","code":"bad"}}}`)
	if err != nil || !done || len(evs) != 1 || evs[0].ErrorMsg != "boom" || evs[0].ErrorCode != "bad" {
		t.Fatalf("failed: %+v %v %v", evs, done, err)
	}
	evs, _, err = DecodeResponsesStreamEvent("", `{"type":"error"}`)
	if err != nil || len(evs) != 1 || evs[0].ErrorMsg != "responses stream failed" {
		t.Fatalf("error default: %+v %v", evs, err)
	}
	// Completed: tool in output -> tool_use; usage present.
	evs, done, err = DecodeResponsesStreamEvent("", `{"type":"response.completed","response":{"output":[{"type":"function_call"}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	if err != nil || !done || len(evs) != 2 || evs[1].StopReason != StopToolUse {
		t.Fatalf("completed tool: %+v %v %v", evs, done, err)
	}
	evs, done, err = DecodeResponsesStreamEvent("", `{"type":"response.done","response":{"output":[]}}`)
	if err != nil || !done || len(evs) != 1 || evs[0].StopReason != StopEndTurn {
		t.Fatalf("done no usage: %+v %v %v", evs, done, err)
	}
	if _, _, err := DecodeResponsesStreamEvent("", `{"type":"response.completed","response":`); err == nil {
		t.Fatalf("bad completed should error")
	}
}
