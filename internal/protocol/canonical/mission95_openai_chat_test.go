package canonical

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

func f64p(v float64) *float64 { return &v }
func i64p(v int64) *int64     { return &v }
func boolp(v bool) *bool      { return &v }
func strp(v string) *string   { return &v }

func TestMission95OpenAIChatDecode(t *testing.T) {
	// Seed + penalties populate metadata; max_completion wins only
	// when larger than max_tokens.
	req, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
		MaxTokens: 10, MaxCompletionTokens: 20, Seed: i64p(7),
		FrequencyPenalty: f64p(0.5), PresencePenalty: f64p(-0.2),
		Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "client-model")
	if err != nil || req.Model != "client-model" || req.MaxOutputTokens != 20 || req.Metadata["seed"] != int64(7) {
		t.Fatalf("decode: %+v %v", req, err)
	}
	if req.Metadata["frequency_penalty"] != 0.5 || req.Metadata["presence_penalty"] != -0.2 {
		t.Fatalf("penalties: %v", req.Metadata)
	}
	req, err = DecodeOpenAIChatRequest(core.OpenAIRequest{
		MaxTokens: 30, MaxCompletionTokens: 5,
		Messages: []core.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "m")
	if err != nil || req.MaxOutputTokens != 30 {
		t.Fatalf("max wins: %+v %v", req, err)
	}
	// Unsupported user content part fails closed; the same part in
	// system content is dropped by design.
	if _, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
		Messages: []core.OpenAIMessage{{Role: "user", Content: []any{map[string]any{"type": "audio", "x": 1}}}},
	}, "m"); err == nil {
		t.Fatalf("unsupported user part should error")
	}
	req, err = DecodeOpenAIChatRequest(core.OpenAIRequest{
		Messages: []core.OpenAIMessage{
			{Role: "system", Content: []any{map[string]any{"type": "audio"}}},
			{Role: "system", Content: "   "},
			{Role: "user", Content: "hi"},
		},
	}, "m")
	if err != nil || len(req.System) != 0 {
		t.Fatalf("system drops: %+v %v", req.System, err)
	}
	// Assistant: nameless call dropped, reasoning appended, tool kept.
	req, err = DecodeOpenAIChatRequest(core.OpenAIRequest{
		Messages: []core.OpenAIMessage{{Role: "assistant", Content: "ans", ReasoningContent: "think",
			ToolCalls: []core.OpenAIToolCall{
				{ID: "c1", Function: core.OpenAIFunctionCall{Name: "", Arguments: "{}"}},
				{ID: "c2", Function: core.OpenAIFunctionCall{Name: "f", Arguments: `{"a":1}`}},
			}}},
	}, "m")
	if err != nil || len(req.Messages) != 1 || len(req.Messages[0].Parts) != 3 {
		t.Fatalf("assistant parts: %+v %v", req.Messages, err)
	}
	if req.Messages[0].Parts[1].Type != PartThinking || req.Messages[0].Parts[2].ToolCall.Name != "f" {
		t.Fatalf("assistant part order: %+v", req.Messages[0].Parts)
	}
	// Tool role + developer role + tool-choice shapes.
	req, err = DecodeOpenAIChatRequest(core.OpenAIRequest{
		Messages: []core.OpenAIMessage{
			{Role: "developer", Content: "sys"},
			{Role: "tool", ToolCallID: "c2", Content: []any{map[string]any{"type": "text", "text": "res"}, map[string]any{"type": "image_url"}, map[string]any{"type": "input_text", "text": "res2"}, "junk"}},
		},
		ToolChoice: map[string]any{"type": "function", "name": "f"},
	}, "m")
	if err != nil || len(req.System) != 1 || req.Messages[0].Parts[0].ToolResult.Content != "res\nres2" {
		t.Fatalf("tool role: %+v %v", req.Messages, err)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != "tool" || req.ToolChoice.Name != "f" {
		t.Fatalf("responses-shape choice: %+v", req.ToolChoice)
	}
	// Tool choice table.
	for _, tc := range []struct {
		in   any
		want *ToolChoice
	}{
		{"auto", &ToolChoice{Mode: "auto"}}, {"none", &ToolChoice{Mode: "none"}}, {"required", &ToolChoice{Mode: "required"}},
		{"bogus", nil}, {42, nil},
		{map[string]any{"type": "function", "function": map[string]any{"name": "g"}}, &ToolChoice{Mode: "tool", Name: "g"}},
		{map[string]any{"type": "function"}, nil},
		{map[string]any{"type": "tool", "name": "h"}, &ToolChoice{Mode: "tool", Name: "h"}},
		{map[string]any{"type": "other", "name": "h"}, nil},
	} {
		if got := decodeOpenAIToolChoice(tc.in); (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("choice %#v -> %+v want %+v", tc.in, got, tc.want)
		}
	}
	// Reasoning: explicit effort wins; raw reasoning object -> medium;
	// raw "null" -> none.
	req, _ = DecodeOpenAIChatRequest(core.OpenAIRequest{ReasoningEffort: "high", Messages: []core.OpenAIMessage{{Role: "user", Content: "x"}}}, "m")
	if req.Reasoning == nil || req.Reasoning.Effort != "high" {
		t.Fatalf("effort: %+v", req.Reasoning)
	}
	req, _ = DecodeOpenAIChatRequest(core.OpenAIRequest{Reasoning: json.RawMessage(`{"effort":"low"}`), Messages: []core.OpenAIMessage{{Role: "user", Content: "x"}}}, "m")
	if req.Reasoning == nil || req.Reasoning.Effort != "medium" {
		t.Fatalf("raw reasoning: %+v", req.Reasoning)
	}
	req, _ = DecodeOpenAIChatRequest(core.OpenAIRequest{Reasoning: json.RawMessage(`null`), Messages: []core.OpenAIMessage{{Role: "user", Content: "x"}}}, "m")
	if req.Reasoning != nil {
		t.Fatalf("null reasoning: %+v", req.Reasoning)
	}
	// openAIContentToParts / ToText shapes.
	if got := openAIContentToText(nil); got != "" {
		t.Fatalf("nil text: %q", got)
	}
	if got := openAIContentToText(float64(42)); got != "42" {
		t.Fatalf("scalar text: %q", got)
	}
	if got := openAIContentToText(map[string]any{"a": 1}); got != `{"a":1}` {
		t.Fatalf("map text: %q", got)
	}
	parts, err := openAIContentToParts(nil, false)
	if err != nil || parts != nil {
		t.Fatalf("nil parts: %v %v", parts, err)
	}
	parts, err = openAIContentToParts("", false)
	if err != nil || parts != nil {
		t.Fatalf("empty parts: %v %v", parts, err)
	}
	parts, err = openAIContentToParts(float64(1.5), false)
	if err != nil || len(parts) != 1 || parts[0].Text != "1.5" {
		t.Fatalf("scalar parts: %+v %v", parts, err)
	}
	// Images: map image_url, plain url field, data URL, empty data URL.
	parts, err = openAIContentToParts([]any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/i.png"}},
		map[string]any{"type": "input_image", "url": "data:image/png;base64,AAA"},
		map[string]any{"type": "image_url", "image_url": "data:image/png;base64,"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": ""}},
		map[string]any{"type": "text", "text": ""},
		map[string]any{"type": "text", "text": "t"},
		"not-a-map",
	}, false)
	if err != nil || len(parts) != 3 || parts[0].Image.URL != "https://x/i.png" || parts[1].Image.Data != "AAA" || parts[2].Text != "t" {
		t.Fatalf("image parts: %+v %v", parts, err)
	}
	// System image dropped even when valid.
	parts, err = openAIContentToParts([]any{map[string]any{"type": "image_url", "image_url": "https://x/i.png"}}, true)
	if err != nil || len(parts) != 0 {
		t.Fatalf("system image: %+v %v", parts, err)
	}
	// Metadata converters.
	if v, ok := intFromMetadata(map[string]any{"seed": float64(3)}, "seed"); !ok || v != 3 {
		t.Fatalf("float seed: %v %v", v, ok)
	}
	if v, ok := intFromMetadata(map[string]any{"seed": 4}, "seed"); !ok || v != 4 {
		t.Fatalf("int seed: %v %v", v, ok)
	}
	if v, ok := intFromMetadata(map[string]any{"seed": json.Number("5")}, "seed"); !ok || v != 5 {
		t.Fatalf("number seed: %v %v", v, ok)
	}
	if _, ok := intFromMetadata(map[string]any{"seed": json.Number("x")}, "seed"); ok {
		t.Fatalf("bad number seed should fail")
	}
	if _, ok := intFromMetadata(map[string]any{"seed": "s"}, "seed"); ok {
		t.Fatalf("string seed should fail")
	}
	if _, ok := intFromMetadata(nil, "seed"); ok {
		t.Fatalf("nil meta should fail")
	}
	if v, ok := floatFromMetadata(map[string]any{"fp": 2}, "fp"); !ok || v != 2 {
		t.Fatalf("int float: %v %v", v, ok)
	}
	if v, ok := floatFromMetadata(map[string]any{"fp": json.Number("1.5")}, "fp"); !ok || v != 1.5 {
		t.Fatalf("number float: %v %v", v, ok)
	}
	if _, ok := floatFromMetadata(map[string]any{"fp": json.Number("x")}, "fp"); ok {
		t.Fatalf("bad number float should fail")
	}
	if _, ok := floatFromMetadata(nil, "fp"); ok {
		t.Fatalf("nil float meta should fail")
	}
}

func TestMission95OpenAIChatEncode(t *testing.T) {
	// Stop shapes, stream options, metadata round-trip, tool choice.
	req := Request{
		Stop: []string{"END"}, Stream: true,
		Metadata:   map[string]any{"seed": float64(9), "frequency_penalty": json.Number("0.25"), "presence_penalty": 1},
		ToolChoice: &ToolChoice{Mode: "tool", Name: "f"},
		Messages:   []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}},
	}
	out, err := EncodeOpenAIChatRequest(req, "up", true)
	if err != nil || out.Stop != "END" || string(out.StreamOptions) != `{"include_usage":true}` {
		t.Fatalf("encode: %+v %v", out, err)
	}
	if out.Seed == nil || *out.Seed != 9 || out.FrequencyPenalty == nil || *out.FrequencyPenalty != 0.25 || out.PresencePenalty == nil || *out.PresencePenalty != 1 {
		t.Fatalf("meta out: %+v", out)
	}
	choice, ok := out.ToolChoice.(map[string]any)
	if !ok || choice["type"] != "function" {
		t.Fatalf("tool choice out: %#v", out.ToolChoice)
	}
	req.Stop = []string{"a", "b"}
	req.Stream = false
	out, err = EncodeOpenAIChatRequest(req, "up", true)
	if err != nil {
		t.Fatal(err)
	}
	if ss, ok := out.Stop.([]string); !ok || len(ss) != 2 {
		t.Fatalf("multi stop: %#v", out.Stop)
	}
	if len(out.StreamOptions) != 0 {
		t.Fatalf("no stream options when not streaming")
	}
	// Assistant shapes.
	req = Request{Messages: []Message{{Role: "assistant", Parts: []Part{
		{Type: PartText, Text: "t1"}, {Type: PartText, Text: "t2"},
		{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "f", Arguments: "{}"}},
		{Type: PartToolCall, ToolCall: &ToolCall{Name: ""}},
		{Type: PartThinking, Thinking: &Thinking{Text: "th"}},
	}}}}
	out, err = EncodeOpenAIChatRequest(req, "up", false)
	if err != nil {
		t.Fatal(err)
	}
	m := out.Messages[0]
	if len(m.ToolCalls) != 1 || m.ReasoningContent != "th" {
		t.Fatalf("assistant with calls: %+v", m)
	}
	if _, ok := m.Content.([]map[string]any); !ok {
		t.Fatalf("assistant content with calls must be parts: %#v", m.Content)
	}
	req.Messages[0].Parts = []Part{{Type: PartText, Text: "only"}}
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if out.Messages[0].Content != "only" {
		t.Fatalf("single text: %#v", out.Messages[0].Content)
	}
	req.Messages[0].Parts = nil
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if out.Messages[0].Content != "" {
		t.Fatalf("no parts: %#v", out.Messages[0].Content)
	}
	// Tool role: nil parts skipped, nil result skipped, error prefix.
	req = Request{Messages: []Message{
		{Role: "tool", Parts: nil},
		{Role: "tool", Parts: []Part{{Type: PartText, Text: "x"}}},
		{Role: "tool", Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "c1", Content: "boom", IsError: true}}}},
	}}
	out, err = EncodeOpenAIChatRequest(req, "up", false)
	if err != nil || len(out.Messages) != 1 || out.Messages[0].Content != "[tool error] boom" {
		t.Fatalf("tool role encode: %+v %v", out.Messages, err)
	}
	// Default role: empty -> user "", tool-result parts split out,
	// only-result message skipped, image content conversion.
	req = Request{Messages: []Message{
		{Role: "user", Parts: nil},
		{Role: "user", Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "c9", Content: "r"}}}},
		{Role: "user", Parts: []Part{
			{Type: PartText, Text: "see"},
			{Type: PartImage, Image: &Image{URL: "https://x/i.png"}},
			{Type: PartImage, Image: &Image{Data: "ZZZ"}},
			{Type: PartImage},
			{Type: PartImage, Image: &Image{MediaType: "image/webp", Data: ""}},
		}},
	}}
	out, err = EncodeOpenAIChatRequest(req, "up", false)
	if err != nil || len(out.Messages) != 3 {
		t.Fatalf("default role: %+v %v", out.Messages, err)
	}
	if out.Messages[0].Content != "" || out.Messages[1].Role != "tool" || out.Messages[1].ToolCallID != "c9" {
		t.Fatalf("split messages: %+v", out.Messages)
	}
	parts, ok := out.Messages[2].Content.([]map[string]any)
	if !ok || len(parts) != 3 || parts[1]["image_url"] == nil || parts[2]["image_url"] == nil {
		t.Fatalf("image content: %#v", out.Messages[2].Content)
	}
	if got := parts[2]["image_url"].(map[string]any)["url"]; got != "data:image/png;base64,ZZZ" {
		t.Fatalf("default png: %v", got)
	}
	// canonicalPartsToOpenAIContent with nothing convertible -> empty text.
	if got := canonicalPartsToOpenAIContent([]Part{{Type: PartImage}}); len(got) != 1 || got[0]["text"] != "" {
		t.Fatalf("fallback content: %v", got)
	}
	// Tools: bad JSON -> default object; response formats; reasoning.
	req = Request{
		Tools:          []ToolDef{{Name: "bad", Parameters: json.RawMessage(`{bad`)}, {Name: "ok", Description: "d", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
		Reasoning:      &Reasoning{Effort: ""},
		ResponseFormat: &ResponseFormat{Kind: FormatJSONSchema, Name: "n", Schema: json.RawMessage(`{"type":"object"}`)},
		Messages:       []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}},
	}
	out, err = EncodeOpenAIChatRequest(req, "up", false)
	if err != nil || len(out.Tools) != 2 {
		t.Fatalf("tools: %+v %v", out.Tools, err)
	}
	if out.Tools[0].Function.Parameters["type"] != "object" {
		t.Fatalf("bad params default: %v", out.Tools[0].Function.Parameters)
	}
	if out.ReasoningEffort != "medium" {
		t.Fatalf("empty effort -> medium: %q", out.ReasoningEffort)
	}
	rf, ok := out.ResponseFormatRaw.(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("schema format: %#v", out.ResponseFormatRaw)
	}
	req.ResponseFormat = &ResponseFormat{Kind: FormatJSONObject}
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if rf, _ := out.ResponseFormatRaw.(map[string]any); rf["type"] != "json_object" {
		t.Fatalf("json_object: %#v", out.ResponseFormatRaw)
	}
	req.ResponseFormat = &ResponseFormat{Kind: "other"}
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if out.ResponseFormatRaw != nil {
		t.Fatalf("unknown format: %#v", out.ResponseFormatRaw)
	}
	req.Reasoning = &Reasoning{Disabled: true}
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if out.ReasoningEffort != "" {
		t.Fatalf("disabled reasoning: %q", out.ReasoningEffort)
	}
	req.ToolChoice = &ToolChoice{Mode: "bogus"}
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if out.ToolChoice != nil {
		t.Fatalf("bogus choice: %#v", out.ToolChoice)
	}
	// Blank system dropped; no messages -> error.
	req = Request{System: []Part{{Type: PartText, Text: "  "}}, Messages: nil}
	if _, err := EncodeOpenAIChatRequest(req, "up", false); err == nil {
		t.Fatalf("no messages should error")
	}
	req = Request{System: []Part{{Type: PartText, Text: "sys"}}, Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}}}
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if len(out.Messages) != 2 || out.Messages[0].Role != "system" || out.Messages[0].Content != "sys" {
		t.Fatalf("system message: %+v", out.Messages)
	}
	// MaxOutputTokens and parallel flag.
	req = Request{MaxOutputTokens: 55, ParallelToolCalls: boolp(false), Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "x"}}}}}
	out, _ = EncodeOpenAIChatRequest(req, "up", false)
	if out.MaxTokens != 55 || out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Fatalf("max/parallel: %+v", out)
	}
}

func TestMission95OpenAIChatResponse(t *testing.T) {
	if _, err := DecodeOpenAIChatResponse([]byte(`{bad`)); err == nil || !strings.Contains(err.Error(), "invalid OpenAI response") {
		t.Fatalf("bad json: %v", err)
	}
	if _, err := DecodeOpenAIChatResponse([]byte(`{"error":{"message":"boom","code":"bad_request"}}`)); err == nil || !strings.Contains(err.Error(), "bad_request") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("envelope code: %v", err)
	}
	if _, err := DecodeOpenAIChatResponse([]byte(`{"error":{}}`)); err == nil || !strings.Contains(err.Error(), "upstream OpenAI request failed") {
		t.Fatalf("envelope default: %v", err)
	}
	if _, err := DecodeOpenAIChatResponse([]byte(`{"error":{"message":"only"}}`)); err == nil || !strings.Contains(err.Error(), "only") {
		t.Fatalf("envelope msg only: %v", err)
	}
	// Content shapes, finish mapping, usage details.
	resp, err := DecodeOpenAIChatResponse([]byte(`{"id":"r1","model":"m","choices":[{"message":{
		"role":"assistant",
		"content":[{"type":"text","text":"a"},{"type":"output_text","text":"b"},{"type":"image","text":"no"},{"text":"notyped"}],
		"reasoning_content":"th",
		"tool_calls":[{"id":"c1","function":{"name":"","arguments":"{}"}},{"id":"c2","function":{"name":"f","arguments":"{}"}}]
	},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 3 || resp.Blocks[0].Text != "a\nb" || resp.Blocks[1].Type != PartThinking || resp.Blocks[2].ToolCall.Name != "f" {
		t.Fatalf("response blocks: %+v", resp.Blocks)
	}
	if resp.StopReason != StopMaxTokens || resp.Usage.CacheReadTokens != 2 || resp.Usage.ReasoningTokens != 1 || resp.ID != "r1" {
		t.Fatalf("response meta: %+v", resp)
	}
	resp, err = DecodeOpenAIChatResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"plain"},"finish_reason":"content_filter"}]}`))
	if err != nil || resp.StopReason != StopRefusal || len(resp.Blocks) != 1 || resp.Blocks[0].Text != "plain" {
		t.Fatalf("string content: %+v %v", resp, err)
	}
	resp, err = DecodeOpenAIChatResponse([]byte(`{}`))
	if err != nil || len(resp.Blocks) != 0 || resp.StopReason != StopEndTurn {
		t.Fatalf("empty response: %+v %v", resp, err)
	}
	// Encode response: finish mapping, nameless drop, id synthesis,
	// model override, usage totals.
	for _, tc := range []struct {
		in, want string
	}{
		{StopToolUse, "tool_calls"}, {StopMaxTokens, "length"}, {StopRefusal, "content_filter"},
		{StopStopSequence, "stop"}, {StopEndTurn, "stop"}, {"weird", "stop"},
	} {
		enc := EncodeOpenAIChatResponse(Response{StopReason: tc.in, Blocks: []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "f", Arguments: "{}"}}}}, "")
		if *enc.Choices[0].FinishReason != tc.want {
			t.Fatalf("finish %q -> %q want %q", tc.in, *enc.Choices[0].FinishReason, tc.want)
		}
		if enc.Choices[0].Message.ToolCalls[0].ID != "call_0" {
			t.Fatalf("synthesized id: %v", enc.Choices[0].Message.ToolCalls)
		}
	}
	enc := EncodeOpenAIChatResponse(Response{Model: "orig", ID: "id1", Usage: Usage{InputTokens: 4, OutputTokens: 2, CacheReadTokens: 1, ReasoningTokens: 3},
		Blocks: []Block{{Type: PartText, Text: "a"}, {Type: PartText, Text: "b"}, {Type: PartThinking, Thinking: &Thinking{Text: "th"}}, {Type: PartToolCall, ToolCall: &ToolCall{Name: ""}}}}, "requested")
	if enc.Model != "requested" || enc.Choices[0].Message.Content != "a\nb" || enc.Choices[0].Message.ReasoningContent != "th" || len(enc.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("encode response: %+v", enc.Choices[0])
	}
	if enc.Usage.TotalTokens != 6 || enc.Usage.PromptTokensDetails.CachedTokens != 1 || enc.Usage.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Fatalf("encode usage: %+v", enc.Usage)
	}
	if enc.Object != "chat.completion" || enc.ID != "id1" || enc.Created == 0 {
		t.Fatalf("encode envelope: %+v", enc)
	}
}
