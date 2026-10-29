package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

func mission95BedrockReq() Request {
	temp := 0.7
	topP := 0.9
	return Request{
		Model: "m1",
		System: []Part{
			{Type: PartText, Text: "sys", CacheControl: "1h"},
			{Type: PartText, Text: ""},
		},
		Messages: []Message{
			{Role: "assistant", Parts: []Part{
				{Type: PartText, Text: "hi", CacheControl: "ephemeral"},
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "t1", Name: "f", Arguments: ""}},
				{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "t1", Content: `{"ok":true}`}},
				{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "t2", Content: "plain", IsError: true}},
				{Type: PartThinking, Thinking: &Thinking{Text: "should be dropped"}},
				{Type: PartImage, Image: &Image{MediaType: "image/png", Data: "aGVsbG8="}},
				{Type: PartImage, Image: &Image{MediaType: "image/png", Data: ""}},
				{Type: PartToolCall},
				{Type: PartToolResult},
			}},
			{Role: "system", Parts: []Part{{Type: PartText, Text: "role becomes user"}}},
			{Role: "user", Parts: nil},
		},
		Temperature:     &temp,
		TopP:            &topP,
		MaxOutputTokens: 100,
		Stop:            []string{"END"},
		Tools: []ToolDef{
			{Name: "f", Description: "does f"},
			{Name: "g"},
		},
		ToolChoice: &ToolChoice{Mode: "auto"},
	}
}

func TestMission95BedrockEncode(t *testing.T) {
	payload, model, err := EncodeBedrockRequest(mission95BedrockReq(), "upstream-model")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if model != "upstream-model" {
		t.Fatalf("model = %q", model)
	}
	var out BedrockRequest
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	// System text + its 1h cachePoint; empty system text dropped.
	if len(out.System) != 2 {
		t.Fatalf("system blocks = %d: %+v", len(out.System), out.System)
	}
	if out.System[0].Text != "sys" || out.System[1].CachePoint == nil || out.System[1].CachePoint.TTL != "1h" {
		t.Fatalf("system cachePoint: %+v", out.System)
	}
	// Assistant message: text+cachePoint, toolUse ({} args), JSON tool
	// result, error text result, image; thinking/empty/nil parts dropped.
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d", len(out.Messages))
	}
	asm := out.Messages[0]
	if asm.Role != "assistant" {
		t.Fatalf("assistant role = %q", asm.Role)
	}
	if len(asm.Content) != 6 {
		t.Fatalf("assistant content = %d: %+v", len(asm.Content), asm.Content)
	}
	if asm.Content[1].CachePoint == nil || asm.Content[1].CachePoint.TTL != "" {
		t.Fatalf("ephemeral cachePoint should omit ttl: %+v", asm.Content[1])
	}
	if string(asm.Content[2].ToolUse.Input) != "{}" {
		t.Fatalf("empty tool args should become {}: %s", asm.Content[2].ToolUse.Input)
	}
	if len(asm.Content[3].ToolResult.Content) != 1 || string(asm.Content[3].ToolResult.Content[0].JSON) != `{"ok":true}` {
		t.Fatalf("JSON tool result: %+v", asm.Content[3])
	}
	if asm.Content[4].ToolResult.Status != "error" || asm.Content[4].ToolResult.Content[0].Text != "plain" {
		t.Fatalf("error text result: %+v", asm.Content[4])
	}
	if asm.Content[5].Image == nil || asm.Content[5].Image.Format != "png" || asm.Content[5].Image.Source.Bytes != "aGVsbG8=" {
		t.Fatalf("image block: %+v", asm.Content[5])
	}
	// Non-assistant role normalizes to user; empty message dropped.
	if out.Messages[1].Role != "user" || len(out.Messages[1].Content) != 1 {
		t.Fatalf("second message: %+v", out.Messages[1])
	}
	if out.InferenceConfig == nil || out.InferenceConfig.MaxTokens != 100 || *out.InferenceConfig.Temperature != 0.7 {
		t.Fatalf("inference config: %+v", out.InferenceConfig)
	}
	if out.ToolConfig == nil || len(out.ToolConfig.Tools) != 2 || out.ToolConfig.ToolChoice == nil || out.ToolConfig.ToolChoice.Auto == nil {
		t.Fatalf("tool config: %+v", out.ToolConfig)
	}
	if string(out.ToolConfig.Tools[1].ToolSpec.InputSchema.JSON) != `{"type":"object"}` {
		t.Fatalf("default schema: %s", out.ToolConfig.Tools[1].ToolSpec.InputSchema.JSON)
	}
}

func TestMission95BedrockEncodeChoicesAndLimits(t *testing.T) {
	base := mission95BedrockReq()
	// ToolChoice none drops toolConfig entirely.
	base.ToolChoice = &ToolChoice{Mode: "none"}
	payload, _, err := EncodeBedrockRequest(base, "m")
	if err != nil {
		t.Fatal(err)
	}
	var out BedrockRequest
	out = BedrockRequest{}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.ToolConfig != nil {
		t.Fatalf("none tool choice should drop toolConfig")
	}
	// required -> Any, tool -> named tool, unknown mode -> auto.
	for _, tc := range []struct {
		mode, want string
	}{
		{"required", "any"}, {"tool", "tool"}, {"bogus", "auto"},
	} {
		base.ToolChoice = &ToolChoice{Mode: tc.mode, Name: "f"}
		payload, _, err := EncodeBedrockRequest(base, "m")
		if err != nil {
			t.Fatal(err)
		}
		out = BedrockRequest{}
		if err := json.Unmarshal(payload, &out); err != nil {
			t.Fatal(err)
		}
		switch tc.want {
		case "any":
			if out.ToolConfig.ToolChoice.Any == nil {
				t.Fatalf("%s: want Any", tc.mode)
			}
		case "tool":
			if out.ToolConfig.ToolChoice.Tool == nil || out.ToolConfig.ToolChoice.Tool.Name != "f" {
				t.Fatalf("tool choice: %+v", out.ToolConfig.ToolChoice)
			}
		case "auto":
			if out.ToolConfig.ToolChoice.Auto == nil {
				t.Fatalf("bogus mode should default to auto")
			}
		}
	}
	// No inference config when every knob is zero.
	empty := Request{Messages: []Message{{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}}}}
	payload, _, err = EncodeBedrockRequest(empty, "m")
	if err != nil {
		t.Fatal(err)
	}
	out = BedrockRequest{}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.InferenceConfig != nil {
		t.Fatalf("empty inference config should be omitted: %+v", out.InferenceConfig)
	}
	// No tools: no toolConfig even with a choice.
	empty.ToolChoice = &ToolChoice{Mode: "auto"}
	payload, _, err = EncodeBedrockRequest(empty, "m")
	if err != nil {
		t.Fatal(err)
	}
	out = BedrockRequest{}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.ToolConfig != nil {
		t.Fatalf("no tools should omit toolConfig")
	}
	// Nil ToolChoice with tools: tools still emitted, no choice.
	empty.Tools = []ToolDef{{Name: "f"}}
	empty.ToolChoice = nil
	payload, _, err = EncodeBedrockRequest(empty, "m")
	if err != nil {
		t.Fatal(err)
	}
	out = BedrockRequest{}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.ToolConfig == nil || out.ToolConfig.ToolChoice != nil {
		t.Fatalf("nil choice: %+v", out.ToolConfig)
	}
	// CachePoint cap: 5 markers -> exactly 4 checkpoints, content kept.
	many := Request{Messages: []Message{{Role: "user", Parts: []Part{
		{Type: PartText, Text: "a", CacheControl: "ephemeral"},
		{Type: PartText, Text: "b", CacheControl: "ephemeral"},
		{Type: PartText, Text: "c", CacheControl: "ephemeral"},
		{Type: PartText, Text: "d", CacheControl: "ephemeral"},
		{Type: PartText, Text: "e", CacheControl: "ephemeral"},
	}}}}
	payload, _, err = EncodeBedrockRequest(many, "m")
	if err != nil {
		t.Fatal(err)
	}
	out = BedrockRequest{}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	cps := 0
	for _, b := range out.Messages[0].Content {
		if b.CachePoint != nil {
			cps++
		}
	}
	if cps != BedrockMaxCacheCheckpoints {
		t.Fatalf("cachePoints = %d, want %d", cps, BedrockMaxCacheCheckpoints)
	}
	if len(out.Messages[0].Content) != 9 {
		t.Fatalf("content blocks = %d, want 9 (5 text + 4 points)", len(out.Messages[0].Content))
	}
	// Image format mapping + unsupported error.
	for _, tc := range []struct {
		mt, want string
	}{
		{"image/png", "png"}, {"image/jpeg", "jpeg"}, {"image/jpg", "jpeg"},
		{"image/gif", "gif"}, {"image/webp", "webp"}, {"IMAGE/PNG", "png"}, {" image/jpeg ", "jpeg"},
	} {
		if got := bedrockImageFormat(tc.mt); got != tc.want {
			t.Errorf("bedrockImageFormat(%q) = %q, want %q", tc.mt, got, tc.want)
		}
	}
	if got := bedrockImageFormat("image/svg+xml"); got != "" {
		t.Fatalf("svg should be unsupported: %q", got)
	}
	bad := Request{Messages: []Message{{Role: "user", Parts: []Part{{Type: PartImage, Image: &Image{MediaType: "image/bmp", Data: "xx"}}}}}}
	if _, _, err := EncodeBedrockRequest(bad, "m"); err == nil || !strings.Contains(err.Error(), "unsupported image media type") {
		t.Fatalf("unsupported image should error: %v", err)
	}
	// CachePoint mapping: only "1h" sets TTL.
	if cp := bedrockCachePoint("1h"); cp.TTL != "1h" || cp.Type != "default" {
		t.Fatalf("1h cachePoint: %+v", cp)
	}
	if cp := bedrockCachePoint("5m"); cp.TTL != "" {
		t.Fatalf("5m cachePoint should omit ttl: %+v", cp)
	}
	if cp := bedrockCachePoint(" 1h "); cp.TTL != "1h" {
		t.Fatalf("trimmed 1h: %+v", cp)
	}
}

func TestMission95BedrockDecode(t *testing.T) {
	if _, err := DecodeBedrockResponse([]byte(`{bad`)); err == nil {
		t.Fatalf("invalid JSON should error")
	}
	resp, err := DecodeBedrockResponse([]byte(`{"output":{"message":{"role":"assistant","content":[
		{"text":"hello"},
		{"toolUse":{"toolUseId":"t1","name":"f"}},
		{"image":{"format":"png","source":{"bytes":"xx"}}}
	]}},"stopReason":"tool_use","usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":3,"cacheWriteInputTokens":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 2 || resp.Blocks[1].ToolCall.Arguments != "{}" {
		t.Fatalf("blocks: %+v", resp.Blocks)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop = %q", resp.StopReason)
	}
	if resp.Usage.CacheReadTokens != 3 || resp.Usage.CacheWriteTokens != 2 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if !resp.HasToolCalls() {
		t.Fatalf("HasToolCalls should be true")
	}
	// Tool use upgrades end_turn to tool_use.
	resp, err = DecodeBedrockResponse([]byte(`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"t","name":"f","input":"{}"}}]}},"stopReason":"end_turn"}`))
	if err != nil || resp.StopReason != StopToolUse {
		t.Fatalf("upgrade: %+v %v", resp, err)
	}
	// Stop reason mapping table.
	for _, tc := range []struct {
		in, want string
	}{
		{"end_turn", StopEndTurn}, {"stop_sequence", StopStopSequence}, {"tool_use", StopToolUse},
		{"max_tokens", StopMaxTokens}, {"content_filtered", StopRefusal}, {"guardrail_intervened", StopRefusal},
		{"unknown_reason", StopEndTurn}, {"", StopEndTurn},
	} {
		resp, err := DecodeBedrockResponse([]byte(`{"output":{"message":{"content":[]}},"stopReason":"` + tc.in + `"}`))
		if err != nil || resp.StopReason != tc.want {
			t.Fatalf("stop %q -> %q (want %q) err %v", tc.in, resp.StopReason, tc.want, err)
		}
	}
	// Model path: stream vs not, escaping of ':' and '/'.
	if got := BedrockModelPath("anthropic.claude-3", false); got != "/model/anthropic.claude-3/converse" {
		t.Fatalf("path: %q", got)
	}
	if got := BedrockModelPath("us.anthropic.claude:profile", true); got != "/model/us.anthropic.claude%3Aprofile/converse-stream" {
		t.Fatalf("stream path: %q", got)
	}
	if got := BedrockModelPath("arn:aws:bedrock/us/model/x", false); got != "/model/arn%3Aaws%3Abedrock%2Fus%2Fmodel%2Fx/converse" {
		t.Fatalf("arn path: %q", got)
	}
	if got := bedrockEscapeSegment("a-b.c_d~e"); got != "a-b.c_d~e" {
		t.Fatalf("unreserved passthrough: %q", got)
	}
}

func TestMission95BedrockStreamChunk(t *testing.T) {
	// Empty / whitespace.
	if evs, term, err := DecodeBedrockStreamChunk("  "); evs != nil || term || err != nil {
		t.Fatalf("empty: %v %v %v", evs, term, err)
	}
	if _, _, err := DecodeBedrockStreamChunk(`{bad`); err == nil {
		t.Fatalf("invalid envelope should error")
	}
	// messageStart.
	evs, term, err := DecodeBedrockStreamChunk(`{"eventType":"messageStart","payload":{}}`)
	if err != nil || term || len(evs) != 1 || evs[0].Type != StreamStart {
		t.Fatalf("messageStart: %v %v %v", evs, term, err)
	}
	// contentBlockStart: tool, non-tool, invalid payload.
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"contentBlockStart","payload":{"contentBlockIndex":2,"start":{"toolUse":{"toolUseId":"t9","name":"f"}}}}`)
	if err != nil || term || len(evs) != 1 || evs[0].Type != StreamToolStart || evs[0].ToolIndex != 2 || evs[0].ToolID != "t9" {
		t.Fatalf("blockStart tool: %v %v %v", evs, term, err)
	}
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"contentBlockStart","payload":{"contentBlockIndex":0,"start":{}}}`)
	if err != nil || term || len(evs) != 0 {
		t.Fatalf("blockStart text: %v %v %v", evs, term, err)
	}
	if _, _, err := DecodeBedrockStreamChunk(`{"eventType":"contentBlockStart","payload":"str"}`); err == nil {
		t.Fatalf("blockStart bad payload should error")
	}
	// contentBlockDelta: tool, text, empty, invalid.
	evs, _, err = DecodeBedrockStreamChunk(`{"eventType":"contentBlockDelta","payload":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"a\":"}}}}`)
	if err != nil || len(evs) != 1 || evs[0].Type != StreamToolDelta {
		t.Fatalf("delta tool: %v %v", evs, err)
	}
	evs, _, err = DecodeBedrockStreamChunk(`{"eventType":"contentBlockDelta","payload":{"delta":{"text":"hi"}}}`)
	if err != nil || len(evs) != 1 || evs[0].Type != StreamText || evs[0].Text != "hi" {
		t.Fatalf("delta text: %v %v", evs, err)
	}
	evs, _, err = DecodeBedrockStreamChunk(`{"eventType":"contentBlockDelta","payload":{"delta":{}}}`)
	if err != nil || len(evs) != 0 {
		t.Fatalf("delta empty: %v %v", evs, err)
	}
	if _, _, err := DecodeBedrockStreamChunk(`{"eventType":"contentBlockDelta","payload":"str"}`); err == nil {
		t.Fatalf("delta bad payload should error")
	}
	// contentBlockStop: deliberately no event.
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"contentBlockStop","payload":{}}`)
	if err != nil || term || len(evs) != 0 {
		t.Fatalf("blockStop: %v %v %v", evs, term, err)
	}
	// messageStop: terminal, mapped reason, invalid payload.
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"messageStop","payload":{"stopReason":"max_tokens"}}`)
	if err != nil || !term || len(evs) != 1 || evs[0].Type != StreamEnd || evs[0].StopReason != StopMaxTokens {
		t.Fatalf("messageStop: %v %v %v", evs, term, err)
	}
	if _, _, err := DecodeBedrockStreamChunk(`{"eventType":"messageStop","payload":"str"}`); err == nil {
		t.Fatalf("messageStop bad payload should error")
	}
	// metadata: usage event, invalid payload.
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"metadata","payload":{"usage":{"inputTokens":4,"outputTokens":2,"cacheReadInputTokens":1}}}`)
	if err != nil || term || len(evs) != 1 || evs[0].Type != StreamUsage || evs[0].Usage.InputTokens != 4 || evs[0].Usage.CacheReadTokens != 1 {
		t.Fatalf("metadata: %v %v %v", evs, term, err)
	}
	if _, _, err := DecodeBedrockStreamChunk(`{"eventType":"metadata","payload":"str"}`); err == nil {
		t.Fatalf("metadata bad payload should error")
	}
	// Exceptions (both casings) are terminal errors; unknown events ignored.
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"throttlingException","payload":{"message":"slow"}}`)
	if err != nil || !term || len(evs) != 1 || evs[0].Type != StreamError || evs[0].ErrorCode != "throttlingException" {
		t.Fatalf("exception: %v %v %v", evs, term, err)
	}
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"modelexception","payload":{}}`)
	if err != nil || !term || len(evs) != 1 || evs[0].Type != StreamError {
		t.Fatalf("lower exception: %v %v %v", evs, term, err)
	}
	evs, term, err = DecodeBedrockStreamChunk(`{"eventType":"futureEvent","payload":{}}`)
	if err != nil || term || len(evs) != 0 {
		t.Fatalf("unknown event: %v %v %v", evs, term, err)
	}
}
