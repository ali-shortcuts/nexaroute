package canonical

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// Fail-closed decoding contracts (audit 2026-10-23, routing lane D1–D3 +
// S2/S3, each first demonstrated by a failing probe): client content or
// upstream results that have no canonical representation must be rejected
// with an explicit error, never silently dropped, textualized, or reported
// as success. The Anthropic decoder already follows this rule
// ("unsupported Anthropic content block"); these tests pin the same rule
// for the OpenAI-family decoders.

func decodeResponsesRequest(t *testing.T, raw string) (Request, error) {
	t.Helper()
	var in ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	return DecodeResponsesRequest(in)
}

func TestDecodeResponsesRequestRejectsBuiltinTool(t *testing.T) {
	_, err := decodeResponsesRequest(t, `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`)
	if err == nil {
		t.Fatal("built-in web_search tool was silently dropped; want an explicit error")
	}
	if !strings.Contains(err.Error(), "web_search") {
		t.Fatalf("error should name the unsupported tool type, got: %v", err)
	}

	// Control: a function tool still decodes.
	out, err := decodeResponsesRequest(t, `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`)
	if err != nil {
		t.Fatalf("function tool must still decode: %v", err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "f" {
		t.Fatalf("function tool decoded incorrectly: %+v", out.Tools)
	}
}

func TestDecodeResponsesRequestRejectsBuiltinToolChoice(t *testing.T) {
	_, err := decodeResponsesRequest(t, `{"model":"m","input":"hi","tool_choice":{"type":"web_search"}}`)
	if err == nil {
		t.Fatal("tool_choice naming a built-in tool was silently dropped; want an explicit error")
	}
	if !strings.Contains(err.Error(), "web_search") {
		t.Fatalf("error should name the unsupported tool_choice type, got: %v", err)
	}

	// Control: a function tool_choice still decodes.
	out, err := decodeResponsesRequest(t, `{"model":"m","input":"hi","tool_choice":{"type":"function","name":"f"}}`)
	if err != nil {
		t.Fatalf("function tool_choice must still decode: %v", err)
	}
	if out.ToolChoice == nil || out.ToolChoice.Mode != "tool" || out.ToolChoice.Name != "f" {
		t.Fatalf("function tool_choice decoded incorrectly: %+v", out.ToolChoice)
	}
}

func TestDecodeResponsesRequestRejectsPreviousResponseID(t *testing.T) {
	// The gateway does not forward `store`, so a chained response id can
	// never resolve upstream; accepting it would silently sever the
	// conversation. It must be rejected, not discarded.
	_, err := decodeResponsesRequest(t, `{"model":"m","input":"hi","previous_response_id":"resp_123"}`)
	if err == nil {
		t.Fatal("previous_response_id was silently discarded; want an explicit error")
	}
	if !strings.Contains(err.Error(), "previous_response_id") {
		t.Fatalf("error should name the unsupported field, got: %v", err)
	}
}

func TestDecodeResponsesRequestRejectsUnsupportedContentPart(t *testing.T) {
	_, err := decodeResponsesRequest(t, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"listen to this"},{"type":"input_audio","data":"AAAA","format":"wav"}]}]}`)
	if err == nil {
		t.Fatal("input_audio content part was silently dropped; want an explicit error")
	}
	if !strings.Contains(err.Error(), "input_audio") {
		t.Fatalf("error should name the unsupported content part, got: %v", err)
	}

	// Control: text + image parts still decode.
	out, err := decodeResponsesRequest(t, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"https://example.test/x.png"}]}]}`)
	if err != nil {
		t.Fatalf("text+image input must still decode: %v", err)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Parts) != 2 {
		t.Fatalf("text+image input decoded incorrectly: %+v", out.Messages)
	}
}

func TestDecodeResponsesResponseFailedStatusIsError(t *testing.T) {
	// A non-stream Responses body with status "failed" is an upstream
	// failure: it must surface as a decode error (pre-commit failover /
	// 502), mirroring the streaming path's response.failed handling —
	// never as a successful end_turn completion.
	_, err := DecodeResponsesResponse([]byte(`{"id":"r1","model":"m","status":"failed","error":{"message":"model overloaded","code":"rate_limit_error"},"output":[]}`))
	if err == nil {
		t.Fatal("status:failed body decoded as success; want an explicit error")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("error should carry the upstream failure message, got: %v", err)
	}

	// Controls: completed and incomplete keep their existing mappings.
	out, err := DecodeResponsesResponse([]byte(`{"id":"r2","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatalf("completed body must still decode: %v", err)
	}
	if out.StopReason != StopEndTurn {
		t.Fatalf("completed body stop=%q, want end_turn", out.StopReason)
	}
	out, err = DecodeResponsesResponse([]byte(`{"id":"r3","model":"m","status":"incomplete","output":[]}`))
	if err != nil {
		t.Fatalf("incomplete body must still decode: %v", err)
	}
	if out.StopReason != StopMaxTokens {
		t.Fatalf("incomplete body stop=%q, want max_tokens", out.StopReason)
	}
}

func TestDecodeOpenAIChatResponseRejectsErrorEnvelope(t *testing.T) {
	// Some OpenAI-family upstreams answer HTTP 200 with an error
	// envelope. The legacy paths reject those via
	// validateOpenAIResponseJSON; the canonical decoder must not turn
	// one into an empty successful response either.
	_, err := DecodeOpenAIChatResponse([]byte(`{"error":{"message":"upstream exploded","type":"server_error","code":"x"},"choices":[]}`))
	if err == nil {
		t.Fatal("error envelope in a 200 body decoded as success; want an explicit error")
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("error should carry the upstream message, got: %v", err)
	}

	// Control: an ordinary chat completion still decodes.
	out, err := DecodeOpenAIChatResponse([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("ordinary chat completion must still decode: %v", err)
	}
	if len(out.Blocks) != 1 || out.Blocks[0].Text != "hi" {
		t.Fatalf("ordinary chat completion decoded incorrectly: %+v", out.Blocks)
	}
}

func TestDecodeOpenAIChatRequestRejectsUnsupportedContentPart(t *testing.T) {
	// A typed content part with no canonical representation (here
	// input_audio) must be rejected, not forwarded to a translated
	// upstream as a text blob of raw JSON and not silently dropped.
	audioPart := map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "AAAA", "format": "wav"}}
	textPart := map[string]any{"type": "text", "text": "listen to this"}

	_, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
		Model:    "m",
		Messages: []core.OpenAIMessage{{Role: "user", Content: []any{textPart, audioPart}}},
	}, "m")
	if err == nil {
		t.Fatal("input_audio part was silently degraded; want an explicit error")
	}
	if !strings.Contains(err.Error(), "input_audio") {
		t.Fatalf("error should name the unsupported content part, got: %v", err)
	}

	// Control: text + image parts still decode.
	out, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
		Model: "m",
		Messages: []core.OpenAIMessage{{Role: "user", Content: []any{
			textPart,
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/x.png"}},
		}}},
	}, "m")
	if err != nil {
		t.Fatalf("text+image message must still decode: %v", err)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Parts) != 2 {
		t.Fatalf("text+image message decoded incorrectly: %+v", out.Messages)
	}

	// System content stays text-only by contract: unrepresentable
	// system parts are dropped by design (every encoder emits system
	// as text), so they must NOT error.
	out, err = DecodeOpenAIChatRequest(core.OpenAIRequest{
		Model:    "m",
		Messages: []core.OpenAIMessage{{Role: "system", Content: []any{audioPart}}},
	}, "m")
	if err != nil {
		t.Fatalf("system parts must not error (text-only contract): %v", err)
	}
	if len(out.System) != 0 {
		t.Fatalf("system audio part should be dropped, got %+v", out.System)
	}
}
