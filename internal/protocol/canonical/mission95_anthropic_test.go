package canonical

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func mission95AnthReq() core.AnthropicRequest {
	temp := 0.5
	topP := 0.8
	return core.AnthropicRequest{
		Model:       "claude-x",
		MaxTokens:   100,
		Temperature: &temp,
		TopP:        &topP,
		TopK:        5,
		Messages: []core.AnthMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
	}
}

func TestMission95AnthropicDecodeErrorsAndShapes(t *testing.T) {
	// System decode error.
	req := mission95AnthReq()
	req.System = json.RawMessage(`{"not":"a block list"}`)
	// A JSON object is neither a string nor a block list for system.
	if _, err := DecodeAnthropicRequest(req, "m"); err == nil {
		t.Fatalf("bad system should error")
	}
	// Empty-string system -> no parts; null system -> no parts.
	req.System = json.RawMessage(`""`)
	out, err := DecodeAnthropicRequest(req, "m")
	if err != nil || len(out.System) != 0 {
		t.Fatalf("empty system: %+v %v", out.System, err)
	}
	req.System = json.RawMessage(`null`)
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil || len(out.System) != 0 {
		t.Fatalf("null system: %+v %v", out.System, err)
	}
	// System blocks: text with TTL marker, text with type marker,
	// text with bare marker, non-text skipped.
	req.System = json.RawMessage(`[
		{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},
		{"type":"text","text":"b","cache_control":{"type":"ephemeral"}},
		{"type":"text","text":"c","cache_control":{}},
		{"type":"image","source":{"type":"url","url":"https://x/y.png"}}
	]`)
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil || len(out.System) != 3 {
		t.Fatalf("system blocks: %+v %v", out.System, err)
	}
	if out.System[0].CacheControl != "1h" || out.System[1].CacheControl != "ephemeral" || out.System[2].CacheControl != "ephemeral" {
		t.Fatalf("cache values: %+v", out.System)
	}
	if got := anthCacheValue(nil); got != "" {
		t.Fatalf("nil cache: %q", got)
	}
	if got := anthCacheBlock("1h")["ttl"]; got != "1h" {
		t.Fatalf("1h block: %v", anthCacheBlock("1h"))
	}
	if got := anthCacheBlock("ephemeral"); got["type"] != "ephemeral" {
		t.Fatalf("ephemeral block: %v", got)
	}
	// Unsupported role.
	req = mission95AnthReq()
	req.Messages = []core.AnthMessage{{Role: "tool", Content: json.RawMessage(`"x"`)}, {Role: "USER", Content: json.RawMessage(`"y"`)}}
	// First message errors before the second is reached.
	if _, err := DecodeAnthropicRequest(req, "m"); err == nil || !strings.Contains(err.Error(), "unsupported Anthropic message role") {
		t.Fatalf("bad role: %v", err)
	}
	// Role normalization: uppercase USER accepted.
	req.Messages = []core.AnthMessage{{Role: " USER ", Content: json.RawMessage(`"y"`)}, {Role: "Assistant", Content: json.RawMessage(`"z"`)}}
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil || out.Messages[0].Role != "user" || out.Messages[1].Role != "assistant" {
		t.Fatalf("role normalization: %+v %v", out.Messages, err)
	}
	// Content decode error (malformed JSON).
	req = mission95AnthReq()
	req.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`{bad`)}}
	if _, err := DecodeAnthropicRequest(req, "m"); err == nil {
		t.Fatalf("bad content should error")
	}
	// Image block without usable source errors.
	req = mission95AnthReq()
	req.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"","data":""}}]`)}}
	if _, err := DecodeAnthropicRequest(req, "m"); err == nil {
		t.Fatalf("useless image should error")
	}
	// Unsupported block type fails closed.
	req = mission95AnthReq()
	req.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"server_tool_use"}]`)}}
	if _, err := DecodeAnthropicRequest(req, "m"); err == nil {
		t.Fatalf("unsupported block should error")
	}
	// Nameless tool_use dropped; tool_result text shapes.
	req = mission95AnthReq()
	req.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[
		{"type":"tool_use","id":"t0","input":{"a":1}},
		{"type":"tool_use","id":"t1","name":"f","input":{"b":2}},
		{"type":"tool_result","tool_use_id":"t1","content":"plain"},
		{"type":"tool_result","tool_use_id":"t2","content":null},
		{"type":"tool_result","tool_use_id":"t3","content":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"image","source":{}}]},
		{"type":"tool_result","tool_use_id":"t4","content":{"x":1}},
		{"type":"thinking","thinking":"deep","signature":"sig"},
		{"type":"redacted_thinking","content":"redacted-body"}
	]`)}}
	// Note: redacted_thinking in request decode reads Content raw into
	// the redacted field via tb.RedactedThinking? The block JSON uses
	// "redacted_thinking" key; AnthBlock.Content is the raw "content"
	// field, so this case exercises the redacted branch shape.
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages[0].Parts) != 7 {
		t.Fatalf("parts = %d: %+v", len(out.Messages[0].Parts), out.Messages[0].Parts)
	}
	if out.Messages[0].Parts[0].Type != PartToolCall || out.Messages[0].Parts[0].ToolCall.Name != "f" {
		t.Fatalf("nameless drop: %+v", out.Messages[0].Parts[0])
	}
	if out.Messages[0].Parts[1].ToolResult.Content != "plain" {
		t.Fatalf("string result: %+v", out.Messages[0].Parts[1])
	}
	if out.Messages[0].Parts[2].ToolResult.Content != "" {
		t.Fatalf("null result: %+v", out.Messages[0].Parts[2])
	}
	if out.Messages[0].Parts[3].ToolResult.Content != "one\ntwo" {
		t.Fatalf("block result: %q", out.Messages[0].Parts[3].ToolResult.Content)
	}
	// Image URL source.
	req = mission95AnthReq()
	req.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]`)}}
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil || out.Messages[0].Parts[0].Image.URL != "https://x/y.png" {
		t.Fatalf("url image: %+v %v", out.Messages[0].Parts, err)
	}
	// Image with a bare url field (no type) also resolves via fallback.
	req.Messages = []core.AnthMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"url":"https://x/z.png"}}]`)}}
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil || out.Messages[0].Parts[0].Image.URL != "https://x/z.png" {
		t.Fatalf("bare url image: %+v %v", out.Messages[0].Parts, err)
	}
	if img := decodeAnthImage(nil); img != nil {
		t.Fatalf("nil source should be nil")
	}
	if img := decodeAnthImage(map[string]any{"type": "base64"}); img != nil {
		t.Fatalf("incomplete base64 should be nil")
	}
}

func TestMission95AnthropicToolChoiceAndThinking(t *testing.T) {
	for _, tc := range []struct {
		in   any
		mode string
		name string
	}{
		{"auto", "auto", ""}, {"none", "none", ""}, {"any", "required", ""}, {"required", "required", ""},
		{"bogus", "", ""}, {nil, "", ""},
		{map[string]any{"type": "auto"}, "auto", ""}, {map[string]any{"type": "none"}, "none", ""},
		{map[string]any{"type": "any"}, "required", ""}, {map[string]any{"type": "required"}, "required", ""},
		{map[string]any{"type": "tool", "name": "f"}, "tool", "f"},
		{map[string]any{"type": "tool"}, "", ""}, {map[string]any{"type": "bogus"}, "", ""},
	} {
		got := decodeAnthropicToolChoice(tc.in)
		if tc.mode == "" {
			if got != nil {
				t.Fatalf("toolchoice %#v = %+v, want nil", tc.in, got)
			}
			continue
		}
		if got == nil || got.Mode != tc.mode || got.Name != tc.name {
			t.Fatalf("toolchoice %#v = %+v, want mode %q name %q", tc.in, got, tc.mode, tc.name)
		}
	}
	for _, tc := range []struct {
		raw    string
		effort string
		budget int
		dis    bool
		nilOut bool
	}{
		{``, "", 0, false, true}, {`null`, "", 0, false, true}, {`{bad`, "", 0, false, true},
		{`{"type":"enabled","budget_tokens":1024}`, "low", 1024, false, false},
		{`{"type":"enabled","budget_tokens":2048}`, "low", 2048, false, false},
		{`{"type":"enabled","budget_tokens":5000}`, "medium", 5000, false, false},
		{`{"type":"enabled","budget_tokens":20000}`, "high", 20000, false, false},
		{`{"type":"adaptive","budget_tokens":0}`, "high", 0, false, false},
		{`{"type":"adaptive","budget_tokens":3000}`, "medium", 3000, false, false},
		{`{"type":"disabled"}`, "", 0, true, false},
		{`{"type":"weird"}`, "", 0, true, false},
	} {
		got := decodeAnthropicThinking(json.RawMessage(tc.raw))
		if tc.nilOut {
			if got != nil {
				t.Fatalf("thinking %q = %+v, want nil", tc.raw, got)
			}
			continue
		}
		if got == nil || got.Effort != tc.effort || got.BudgetTokens != tc.budget || got.Disabled != tc.dis {
			t.Fatalf("thinking %q = %+v, want effort %q budget %d disabled %v", tc.raw, got, tc.effort, tc.budget, tc.dis)
		}
	}
	// Metadata passthrough: valid map kept, invalid/empty dropped.
	req := mission95AnthReq()
	req.Metadata = json.RawMessage(`{"k":"v"}`)
	out, err := DecodeAnthropicRequest(req, "m")
	if err != nil || out.Metadata["k"] != "v" {
		t.Fatalf("metadata: %+v %v", out.Metadata, err)
	}
	req.Metadata = json.RawMessage(`{}`)
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil || out.Metadata != nil {
		t.Fatalf("empty metadata: %+v %v", out.Metadata, err)
	}
	req.Metadata = json.RawMessage(`"str"`)
	out, err = DecodeAnthropicRequest(req, "m")
	if err != nil || out.Metadata != nil {
		t.Fatalf("non-map metadata: %+v %v", out.Metadata, err)
	}
	// marshalSchema: empty -> nil, non-empty marshaled.
	if got := marshalSchema(nil); got != nil {
		t.Fatalf("empty schema: %s", got)
	}
	if got := marshalSchema(map[string]any{"type": "object"}); string(got) != `{"type":"object"}` {
		t.Fatalf("schema: %s", got)
	}
}

func TestMission95AnthropicEncodeEdges(t *testing.T) {
	// TopK, default max tokens, parallel-tool-calls flag, reasoning budgets.
	low := "low"
	high := "high"
	med := "medium"
	off := false
	req := Request{
		Model: "m", Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}},
		TopK:              func() *float64 { v := 3.9; return &v }(),
		Tools:             []ToolDef{{Name: "f", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)}, {Name: "g", Parameters: json.RawMessage(`{bad`)}},
		ToolChoice:        &ToolChoice{Mode: "auto"},
		ParallelToolCalls: &off,
		Reasoning:         &Reasoning{Effort: low},
	}
	out, err := EncodeAnthropicRequest(req, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	if out.TopK != 3 || out.MaxTokens != 4096 {
		t.Fatalf("topk/max: %+v", out)
	}
	tcMap, _ := out.ToolChoice.(map[string]any)
	if tcMap["type"] != "auto" || tcMap["disable_parallel_tool_use"] != true {
		t.Fatalf("parallel flag: %v", out.ToolChoice)
	}
	// With mode "none" the flag is deliberately not set.
	req.ToolChoice = &ToolChoice{Mode: "none"}
	out, err = EncodeAnthropicRequest(req, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	tcMap, _ = out.ToolChoice.(map[string]any)
	if tcMap["disable_parallel_tool_use"] != nil {
		t.Fatalf("none mode must not set parallel flag: %v", tcMap)
	}
	if len(out.Tools) != 2 || out.Tools[1].InputSchema["type"] != "object" {
		t.Fatalf("tools schema fallback: %+v", out.Tools)
	}
	// Reasoning: low -> 2048 (raises max), high -> 16384, medium -> 8192,
	// explicit budget wins, disabled reasoning omitted, small budget skipped.
	for _, tc := range []struct {
		reason *Reasoning
		budget int
		max    int
		think  bool
	}{
		{&Reasoning{Effort: low}, 2048, 4096, true},
		{&Reasoning{Effort: high}, 16384, 17408, true},
		{&Reasoning{Effort: med}, 8192, 9216, true},
		{&Reasoning{Effort: "bogus"}, 8192, 9216, true},
		{&Reasoning{BudgetTokens: 500}, 500, 4096, false},
		{&Reasoning{BudgetTokens: 1024}, 1024, 4096, true},
		{&Reasoning{Disabled: true, Effort: high}, 0, 4096, false},
	} {
		r := Request{Model: "m", Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}},
			MaxOutputTokens: 4096, Reasoning: tc.reason}
		o, err := EncodeAnthropicRequest(r, "up")
		if err != nil {
			t.Fatal(err)
		}
		if tc.think {
			if len(o.Thinking) == 0 {
				t.Fatalf("reasoning %+v: thinking missing", tc.reason)
			}
			var th map[string]any
			if err := json.Unmarshal(o.Thinking, &th); err != nil || int(th["budget_tokens"].(float64)) != tc.budget {
				t.Fatalf("reasoning %+v: thinking %s", tc.reason, o.Thinking)
			}
		} else if len(o.Thinking) != 0 {
			t.Fatalf("reasoning %+v: thinking should be absent: %s", tc.reason, o.Thinking)
		}
		if o.MaxTokens != tc.max {
			t.Fatalf("reasoning %+v: max %d want %d", tc.reason, o.MaxTokens, tc.max)
		}
	}
	// ToolChoice encodings: auto/none/required/tool, and no choice.
	for _, tc := range []struct {
		mode string
		want any
	}{
		{"auto", "auto"}, {"none", "none"}, {"required", "any"}, {"tool", "tool"},
	} {
		r := Request{Model: "m", Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}},
			ToolChoice: &ToolChoice{Mode: tc.mode, Name: "f"}}
		o, err := EncodeAnthropicRequest(r, "up")
		if err != nil {
			t.Fatal(err)
		}
		tcMap, _ := o.ToolChoice.(map[string]any)
		if tcMap["type"] != tc.want {
			t.Fatalf("choice %s: %v", tc.mode, o.ToolChoice)
		}
	}
	// System: marked blocks form, joined string form, empty parts.
	r := Request{Model: "m", System: []Part{{Type: PartText, Text: "s1", CacheControl: "1h"}, {Type: PartText, Text: "s2"}},
		Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}}}
	o, err := EncodeAnthropicRequest(r, "up")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(o.System), "cache_control") {
		t.Fatalf("marked system should be blocks: %s", o.System)
	}
	r.System = []Part{{Type: PartText, Text: "s1"}, {Type: PartText, Text: "s2"}, {Type: PartImage}}
	o, err = EncodeAnthropicRequest(r, "up")
	if err != nil {
		t.Fatal(err)
	}
	if string(o.System) != `"s1\ns2"` {
		t.Fatalf("joined system: %s", o.System)
	}
	r.System = []Part{{Type: PartImage}}
	o, err = EncodeAnthropicRequest(r, "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(o.System) != 0 {
		t.Fatalf("non-text system should be empty: %s", o.System)
	}
	// Message blocks: empty parts -> placeholder text block; tool role
	// normalizes to user; image URL vs data URL vs base64 vs dropped.
	r = Request{Model: "m", Messages: []Message{
		{Role: "tool", Parts: nil},
		{Role: "user", Parts: []Part{
			{Type: PartImage, Image: &Image{URL: "https://x/i.png"}},
			{Type: PartImage, Image: &Image{URL: "data:image/png;base64,AAA"}},
			{Type: PartImage, Image: &Image{MediaType: "image/gif", Data: "GGG"}},
			{Type: PartImage, Image: &Image{MediaType: "", Data: ""}},
			{Type: PartImage},
			{Type: PartToolCall, ToolCall: &ToolCall{Name: ""}},
			{Type: PartToolCall, ToolCall: &ToolCall{ID: "t", Name: "f", Arguments: "not-json"}},
			{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: ""}},
			{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "t", Content: "res", IsError: true}},
			{Type: PartThinking, Thinking: &Thinking{Text: "unsigned"}},
			{Type: PartThinking, Thinking: &Thinking{Text: "signed", Signature: "sig"}},
			{Type: PartThinking, Thinking: &Thinking{Text: "red", Signature: "redacted"}},
			{Type: PartText, Text: "tail", CacheControl: "5m"},
		}},
	}}
	o, err = EncodeAnthropicRequest(r, "up")
	if err != nil {
		t.Fatal(err)
	}
	if o.Messages[0].Role != "user" {
		t.Fatalf("tool role should normalize: %+v", o.Messages[0])
	}
	var first []map[string]any
	if err := json.Unmarshal(o.Messages[0].Content, &first); err != nil || len(first) != 1 || first[0]["text"] != "" {
		t.Fatalf("empty parts placeholder: %s %v", o.Messages[0].Content, err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(o.Messages[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	// Expected: url image, data-url image, base64 image, tool_use,
	// tool_result, signed thinking, tail text with cache_control = 7.
	if len(blocks) != 7 {
		t.Fatalf("blocks = %d: %s", len(blocks), o.Messages[1].Content)
	}
	if blocks[0]["source"].(map[string]any)["type"] != "url" {
		t.Fatalf("url image: %v", blocks[0])
	}
	if blocks[1]["source"].(map[string]any)["media_type"] != "image/png" {
		t.Fatalf("data-url image: %v", blocks[1])
	}
	if blocks[3]["input"] == nil {
		t.Fatalf("tool_use input missing: %v", blocks[3])
	}
	if blocks[4]["is_error"] != true {
		t.Fatalf("tool_result error flag: %v", blocks[4])
	}
	if blocks[6]["cache_control"] == nil {
		t.Fatalf("tail cache marker missing: %v", blocks[6])
	}
	// Cache marker rides only blocks actually produced: a dropped part
	// (nil image) with a marker must not attach to the previous block.
	r = Request{Model: "m", Messages: []Message{{Role: "user", Parts: []Part{
		{Type: PartText, Text: "keep"},
		{Type: PartImage, CacheControl: "1h"},
	}}}}
	o, err = EncodeAnthropicRequest(r, "up")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(o.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0]["cache_control"] != nil {
		t.Fatalf("dropped marker must not attach: %s", o.Messages[0].Content)
	}
}

func TestMission95AnthropicResponseEdges(t *testing.T) {
	if _, err := DecodeAnthropicResponse([]byte(`{bad`)); err == nil {
		t.Fatalf("invalid response should error")
	}
	// Unsigned and redacted thinking dropped; signed kept; empty
	// content normalized to one empty text block on encode.
	resp, err := DecodeAnthropicResponse([]byte(`{"id":"m1","model":"cl","content":[
		{"type":"thinking","thinking":"u"},
		{"type":"thinking","thinking":"s","signature":"sig"},
		{"type":"redacted_thinking"},
		{"type":"tool_use","id":"t","name":"f","input":{"a":1}},
		{"type":"unknown_thing"}
	],"stop_reason":"max_tokens"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 2 || resp.Blocks[0].Type != PartThinking || resp.Blocks[1].Type != PartToolCall {
		t.Fatalf("response blocks: %+v", resp.Blocks)
	}
	// Encode: empty stop -> end_turn, empty blocks -> empty text,
	// malformed tool args -> _raw, nil tool call skipped, requested
	// model overrides.
	enc := EncodeAnthropicResponse(Response{ID: "r", Model: "orig", Blocks: []Block{
		{Type: PartToolCall},
		{Type: PartToolCall, ToolCall: &ToolCall{ID: "t", Name: "f", Arguments: "not-json"}},
		{Type: PartThinking, Thinking: &Thinking{Text: "u"}},
	}}, "req-model")
	if enc.Model != "req-model" || *enc.StopReason != StopEndTurn {
		t.Fatalf("encode response meta: %+v", enc)
	}
	if len(enc.Content) != 1 || enc.Content[0].Input["_raw"] != "not-json" {
		t.Fatalf("encode response content: %+v", enc.Content)
	}
	enc = EncodeAnthropicResponse(Response{Blocks: nil}, "")
	if len(enc.Content) != 1 || enc.Content[0].Text != "" {
		t.Fatalf("empty content: %+v", enc.Content)
	}
	// mustJSON failure path: marshal of a channel fails -> "null".
	if got := mustJSON(make(chan int)); string(got) != "null" {
		t.Fatalf("mustJSON channel: %s", got)
	}
	// parseDataURL shapes.
	if p := parseDataURL("https://x"); p != nil {
		t.Fatalf("non-data URL: %+v", p)
	}
	if p := parseDataURL("data:,abc"); p == nil || p.mediaType != "application/octet-stream" || p.data != "abc" {
		t.Fatalf("bare data URL: %+v", p)
	}
	if p := parseDataURL("data:image/png;base64,AAA"); p == nil || p.mediaType != "image/png" || p.data != "AAA" {
		t.Fatalf("data URL: %+v", p)
	}
}
