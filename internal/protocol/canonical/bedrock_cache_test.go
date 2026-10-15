package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

// Bedrock cachePoint mapping tests.
//
// Contract (AWS Bedrock Converse API reference + prompt-caching user
// guide, live-Bedrock behavior UNVERIFIED — no live AWS calls here):
//   - ContentBlock is a UNION; cachePoint (CachePointBlock) is a
//     standalone member placed immediately AFTER the content it
//     caches: {"cachePoint": {"type": "default"}}.
//   - SystemContentBlock is likewise text | cachePoint.
//   - The Tool union also accepts cachePoint, but the IR carries no
//     per-tool-definition marker, so none is emitted there.
//   - CachePointBlock: {"type": "default"} plus optional
//     "ttl": "5m" | "1h" (omitted = 5-minute default; sending "1h"
//     to a 5-minute-only model is a ValidationException).
//   - At most 4 checkpoints per request; minimum prefix tokens
//     (512-4096 by model) are enforced server-side — a
//     below-threshold checkpoint still returns 200, just uncached.

// Text markers become cachePoint blocks at the correct positions:
// after the marked system prompt and after the marked message block.
// TTL "1h" is carried; the default marker omits "ttl".
func TestBedrockCachePointTextMapping(t *testing.T) {
	in := Request{
		System: []Part{{Type: PartText, Text: "You are careful.", CacheControl: "1h"}},
		Messages: []Message{{
			Role: "user",
			Parts: []Part{
				{Type: PartText, Text: "big context", CacheControl: "ephemeral"},
				{Type: PartText, Text: "question"},
			},
		}},
		MaxOutputTokens: 64,
	}
	payload, _, err := EncodeBedrockRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	var got BedrockRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	// System: [text, cachePoint(ttl 1h)].
	if len(got.System) != 2 {
		t.Fatalf("system blocks=%+v", got.System)
	}
	if got.System[0].Text != "You are careful." || got.System[0].CachePoint != nil {
		t.Fatalf("marked system text corrupted: %+v", got.System[0])
	}
	cp := got.System[1].CachePoint
	if cp == nil || cp.Type != "default" || cp.TTL != "1h" {
		t.Fatalf("system cachePoint wrong: %+v", got.System[1])
	}
	if got.System[1].Text != "" {
		t.Fatalf("cachePoint block must not carry text: %+v", got.System[1])
	}
	// Message: [text, cachePoint(default), text] — the unmarked
	// trailing block gets no checkpoint.
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 3 {
		t.Fatalf("message blocks=%+v", got.Messages)
	}
	c := got.Messages[0].Content
	if c[0].Text != "big context" || c[0].CachePoint != nil {
		t.Fatalf("marked text corrupted: %+v", c[0])
	}
	if c[1].CachePoint == nil || c[1].CachePoint.Type != "default" || c[1].CachePoint.TTL != "" {
		t.Fatalf("default cachePoint must omit ttl: %+v", c[1])
	}
	if c[2].Text != "question" || c[2].CachePoint != nil {
		t.Fatalf("unmarked text must not gain a checkpoint: %+v", c[2])
	}
}

// Markers on tool_use / tool_result message blocks are honored too:
// Converse allows cachePoint anywhere in messages.content, and the
// API table lists messages as a checkpoint-accepting field.
func TestBedrockCachePointToolBlocks(t *testing.T) {
	in := Request{
		Messages: []Message{
			{Role: "assistant", Parts: []Part{
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "tu1", Name: "lookup", Arguments: `{"q":"x"}`}, CacheControl: "ephemeral"},
			}},
			{Role: "user", Parts: []Part{
				{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "tu1", Content: "answer"}, CacheControl: "5m"},
			}},
		},
	}
	payload, _, err := EncodeBedrockRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	var got BedrockRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages=%+v", got.Messages)
	}
	assistant := got.Messages[0].Content
	if len(assistant) != 2 || assistant[0].ToolUse == nil || assistant[0].ToolUse.ToolUseID != "tu1" {
		t.Fatalf("tool_use block corrupted: %+v", assistant)
	}
	if assistant[1].CachePoint == nil || assistant[1].CachePoint.Type != "default" {
		t.Fatalf("missing cachePoint after tool_use: %+v", assistant)
	}
	result := got.Messages[1].Content
	if len(result) != 2 || result[0].ToolResult == nil || result[0].ToolResult.Content[0].Text != "answer" {
		t.Fatalf("tool_result block corrupted: %+v", result)
	}
	if result[1].CachePoint == nil || result[1].CachePoint.Type != "default" {
		t.Fatalf("missing cachePoint after tool_result: %+v", result)
	}
}

// Beyond the 4-checkpoint cap only the MARKER is dropped: every user
// content block still encodes, and exactly 4 cachePoints appear.
func TestBedrockCachePointCheckpointLimitDropsMarkerOnly(t *testing.T) {
	parts := make([]Part, 0, 6)
	for i := 0; i < 6; i++ {
		parts = append(parts, Part{Type: PartText, Text: string(rune('a' + i)), CacheControl: "ephemeral"})
	}
	in := Request{Messages: []Message{{Role: "user", Parts: parts}}}
	payload, _, err := EncodeBedrockRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	var got BedrockRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages=%+v", got.Messages)
	}
	content := got.Messages[0].Content
	texts, points := 0, 0
	for _, b := range content {
		switch {
		case b.CachePoint != nil:
			points++
			if b.Text != "" || b.ToolUse != nil || b.ToolResult != nil || b.Image != nil {
				t.Fatalf("cachePoint block must be standalone: %+v", b)
			}
		case b.Text != "":
			texts++
		default:
			t.Fatalf("unexpected block (content lost?): %+v", b)
		}
	}
	if texts != 6 {
		t.Fatalf("all 6 user texts must survive, got %d: %s", texts, payload)
	}
	if points != BedrockMaxCacheCheckpoints {
		t.Fatalf("checkpoints=%d want cap %d: %s", points, BedrockMaxCacheCheckpoints, payload)
	}
}

// A request without markers must encode byte-identically to the
// pre-cachePoint shape: no "cachePoint" key anywhere, golden bytes
// pinned.
func TestBedrockNoCacheMarkerByteIdentical(t *testing.T) {
	in := Request{
		System: []Part{{Type: PartText, Text: "be terse"}},
		Messages: []Message{
			{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}},
			{Role: "assistant", Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "tu1", Name: "lookup", Arguments: `{"q":"x"}`}}}},
		},
		MaxOutputTokens: 128,
	}
	payload, _, err := EncodeBedrockRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	const golden = `{"messages":[{"role":"user","content":[{"text":"hi"}]},{"role":"assistant","content":[{"toolUse":{"toolUseId":"tu1","name":"lookup","input":{"q":"x"}}}]}],"system":[{"text":"be terse"}],"inferenceConfig":{"maxTokens":128}}`
	if string(payload) != golden {
		t.Fatalf("unmarked Bedrock payload changed:\n got: %s\nwant: %s", payload, golden)
	}
	if strings.Contains(string(payload), "cachePoint") {
		t.Fatalf("unmarked payload must not mention cachePoint: %s", payload)
	}
}

// Streaming (ConverseStream) and non-streaming (Converse) share the
// encoder; only the URL path differs. The payload must be identical.
func TestBedrockCachePointStreamParity(t *testing.T) {
	in := Request{
		System: []Part{{Type: PartText, Text: "sys", CacheControl: "ephemeral"}},
		Messages: []Message{{
			Role: "user",
			Parts: []Part{
				{Type: PartText, Text: "ctx", CacheControl: "1h"},
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "t", Name: "n", Arguments: `{}`}, CacheControl: "ephemeral"},
			},
		}},
		Stream: true,
	}
	plain := in
	plain.Stream = false
	streamPayload, _, err := EncodeBedrockRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	plainPayload, _, err := EncodeBedrockRequest(plain, "m")
	if err != nil {
		t.Fatal(err)
	}
	if string(streamPayload) != string(plainPayload) {
		t.Fatalf("stream/non-stream payloads differ:\nstream: %s\nplain:  %s", streamPayload, plainPayload)
	}
	if got := BedrockModelPath("m", true); got != "/model/m/converse-stream" {
		t.Fatalf("stream path=%q", got)
	}
	if got := BedrockModelPath("m", false); got != "/model/m/converse" {
		t.Fatalf("path=%q", got)
	}
}

// Converse cache-token usage (usage.cacheReadInputTokens /
// usage.cacheWriteInputTokens, and the same keys in the stream
// metadata event) maps onto the canonical cache counters.
func TestBedrockCacheUsageMapping(t *testing.T) {
	resp, err := DecodeBedrockResponse([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":3,"cacheReadInputTokens":60,"cacheWriteInputTokens":40}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 3 ||
		resp.Usage.CacheReadTokens != 60 || resp.Usage.CacheWriteTokens != 40 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	events, terminal, err := DecodeBedrockStreamChunk(`{"eventType":"metadata","payload":{"usage":{"inputTokens":10,"outputTokens":3,"cacheReadInputTokens":60,"cacheWriteInputTokens":40}}}`)
	if err != nil || terminal || len(events) != 1 || events[0].Usage == nil {
		t.Fatalf("stream metadata: events=%+v terminal=%v err=%v", events, terminal, err)
	}
	if events[0].Usage.CacheReadTokens != 60 || events[0].Usage.CacheWriteTokens != 40 {
		t.Fatalf("stream usage: %+v", events[0].Usage)
	}
	// Absent cache keys stay zero (no checkpoint applied).
	resp, err = DecodeBedrockResponse([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.CacheReadTokens != 0 || resp.Usage.CacheWriteTokens != 0 {
		t.Fatalf("uncached usage must stay zero: %+v", resp.Usage)
	}
}

// Other providers must not leak Bedrock checkpoints: a marked IR
// encodes to OpenAI Chat and Gemini with the markers dropped and no
// cachePoint key, exactly as before this change.
func TestBedrockMarkersDoNotLeakToOtherProviders(t *testing.T) {
	in := Request{
		System: []Part{{Type: PartText, Text: "sys", CacheControl: "1h"}},
		Messages: []Message{{Role: "user", Parts: []Part{
			{Type: PartText, Text: "ctx", CacheControl: "ephemeral"},
		}}},
		MaxOutputTokens: 16,
	}
	chat, err := EncodeOpenAIChatRequest(in, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	chatJSON, _ := json.Marshal(chat)
	if strings.Contains(string(chatJSON), "cachePoint") || strings.Contains(string(chatJSON), "cache_control") {
		t.Fatalf("marker leaked into OpenAI Chat: %s", chatJSON)
	}
	gemPayload, _, err := EncodeGeminiRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gemPayload), "cachePoint") || strings.Contains(string(gemPayload), "cache_control") {
		t.Fatalf("marker leaked into Gemini: %s", gemPayload)
	}
	// Native Anthropic encode still emits cache_control (not
	// cachePoint) — passthrough semantics untouched.
	anth, err := EncodeAnthropicRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	anthJSON, _ := json.Marshal(anth)
	if !strings.Contains(string(anthJSON), "cache_control") {
		t.Fatalf("anthropic markers lost: %s", anthJSON)
	}
	if strings.Contains(string(anthJSON), "cachePoint") {
		t.Fatalf("cachePoint leaked into Anthropic: %s", anthJSON)
	}
}
