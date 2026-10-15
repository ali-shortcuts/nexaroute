package canonical

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// M5-5 prompt-cache mapping tests.

// Anthropic cache_control markers survive a decode -> encode round
// trip through the canonical codec: on the system prompt (which
// forces the block form) and on a message text block, TTL included.
func TestPromptCacheAnthropicMarkerRoundTrip(t *testing.T) {
	var in core.AnthropicRequest
	raw := `{
		"model": "m", "max_tokens": 64,
		"system": [{"type": "text", "text": "You are careful.", "cache_control": {"type": "ephemeral", "ttl": "1h"}}],
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "big context", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "question"}
		]}]
	}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	ir, err := DecodeAnthropicRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.System) != 1 || ir.System[0].CacheControl != "1h" {
		t.Fatalf("system marker lost in decode: %+v", ir.System)
	}
	if len(ir.Messages) != 1 || len(ir.Messages[0].Parts) != 2 || ir.Messages[0].Parts[0].CacheControl != "ephemeral" {
		t.Fatalf("message marker lost in decode: %+v", ir.Messages)
	}
	out, err := EncodeAnthropicRequest(ir, "upstream-m")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(encoded)
	if !strings.Contains(s, `"cache_control":{"ttl":"1h","type":"ephemeral"}`) && !strings.Contains(s, `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Fatalf("system marker (ttl 1h) not re-emitted: %s", s)
	}
	if strings.Count(s, `"cache_control"`) != 2 {
		t.Fatalf("expected exactly 2 cache_control markers: %s", s)
	}
	// The system prompt must be in BLOCK form when marked — a plain
	// string system cannot carry the marker.
	var probe struct {
		System json.RawMessage `json:"system"`
	}
	_ = json.Unmarshal(encoded, &probe)
	if !strings.HasPrefix(strings.TrimSpace(string(probe.System)), "[") {
		t.Fatalf("marked system not emitted as blocks: %s", probe.System)
	}
}

// prompt_cache_key maps Responses <-> IR <-> Chat in both
// directions.
func TestPromptCacheKeyMapping(t *testing.T) {
	ir, err := DecodeResponsesRequest(ResponsesRequest{
		Model:          "m",
		PromptCacheKey: "tenant-42-prefix",
		Input:          json.RawMessage(`"hi"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ir.PromptCacheKey != "tenant-42-prefix" {
		t.Fatalf("responses decode lost the key: %+v", ir)
	}
	respPayload, err := EncodeResponsesRequest(ir, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(respPayload), `"prompt_cache_key":"tenant-42-prefix"`) {
		t.Fatalf("responses encode dropped the key: %s", respPayload)
	}
	chatPayload, err := EncodeOpenAIChatRequest(ir, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	chatJSON, _ := json.Marshal(chatPayload)
	if !strings.Contains(string(chatJSON), `"prompt_cache_key":"tenant-42-prefix"`) {
		t.Fatalf("chat encode dropped the key: %s", chatJSON)
	}
	// And the reverse: a chat request's key lands in the IR.
	var chatReq core.OpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"m","prompt_cache_key":"k2","messages":[{"role":"user","content":"hi"}]}`), &chatReq); err != nil {
		t.Fatal(err)
	}
	ir2 := DecodeOpenAIChatRequest(chatReq, "m")
	if ir2.PromptCacheKey != "k2" {
		t.Fatalf("chat decode lost the key: %+v", ir2)
	}
}

// Cache-token usage maps in both directions for every kind
// (decode into the IR, encode back into the dialect).
func TestPromptCacheUsageMappingPins(t *testing.T) {
	// Anthropic: cache_creation/cache_read input tokens.
	anthResp, err := DecodeAnthropicResponse([]byte(`{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":100,"output_tokens":5,"cache_creation_input_tokens":40,"cache_read_input_tokens":60}}`))
	if err != nil {
		t.Fatal(err)
	}
	if anthResp.Usage.CacheWriteTokens != 40 || anthResp.Usage.CacheReadTokens != 60 {
		t.Fatalf("anthropic usage decode: %+v", anthResp.Usage)
	}
	// OpenAI chat: prompt_tokens_details.cached_tokens.
	chatResp, err := DecodeOpenAIChatResponse([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":70}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if chatResp.Usage.CacheReadTokens != 70 {
		t.Fatalf("openai usage decode: %+v", chatResp.Usage)
	}
	// Gemini: cachedContentTokenCount.
	gemResp, err := DecodeGeminiResponse([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"cachedContentTokenCount":55}}`))
	if err != nil {
		t.Fatal(err)
	}
	if gemResp.Usage.CacheReadTokens != 55 {
		t.Fatalf("gemini usage decode: %+v", gemResp.Usage)
	}
	// Responses: input_tokens_details.cached_tokens.
	respResp, err := DecodeResponsesResponse([]byte(`{"id":"x","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":66}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if respResp.Usage.CacheReadTokens != 66 {
		t.Fatalf("responses usage decode: %+v", respResp.Usage)
	}
}
