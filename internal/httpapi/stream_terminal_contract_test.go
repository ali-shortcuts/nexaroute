package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #421 item 4 (failure grace): when an upstream stream dies
// mid-reply — partial content already emitted, then EOF with NO
// terminal frame and NO upstream error event — the pump must
// synthesize the protocol-correct terminal error for the
// CLIENT's dialect, never a truncated fake success. One
// upstream shape (Anthropic source, cut mid-text), three client
// contracts.
const truncatedAnthropicStream = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial answer\"}}\n\n"

func pumpTruncated(t *testing.T, clientProtocol string) (string, error) {
	t.Helper()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(truncatedAnthropicStream)),
	}
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, resp, "anthropic", clientProtocol, "client-model", "req-truncated", nil, nil)
	return rr.Body.String(), err
}

func TestPostCommitTruncationTerminalErrorPerDialect(t *testing.T) {
	// Anthropic Messages client: an `event: error` frame, and
	// never a message_stop (which would read as success).
	out, err := pumpTruncated(t, "anthropic")
	if err == nil {
		t.Fatal("anthropic client: truncated stream must return an error")
	}
	if !strings.Contains(out, "event: error") || !strings.Contains(out, "upstream stream ended before completion") {
		t.Fatalf("anthropic client: missing synthesized error event:\n%s", out)
	}
	if strings.Contains(out, "message_stop") {
		t.Fatalf("anthropic client: fake success tail after truncation:\n%s", out)
	}
	if !strings.Contains(out, "partial answer") {
		t.Fatalf("anthropic client: pre-truncation content lost:\n%s", out)
	}

	// OpenAI Chat Completions client: a terminal chunk carrying
	// an error object, and [DONE] is withheld (a [DONE] would
	// read as a completed stream).
	out, err = pumpTruncated(t, "openai_chat")
	if err == nil {
		t.Fatal("openai_chat client: truncated stream must return an error")
	}
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "upstream stream ended before completion") {
		t.Fatalf("openai_chat client: missing terminal error chunk:\n%s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("openai_chat client: [DONE] after truncation reads as success:\n%s", out)
	}
	if strings.Contains(out, `"finish_reason":"stop"`) || strings.Contains(out, `"finish_reason": "stop"`) {
		t.Fatalf("openai_chat client: fake finish_reason after truncation:\n%s", out)
	}

	// OpenAI Responses client: a response.failed event, and
	// never response.completed.
	out, err = pumpTruncated(t, "openai_responses")
	if err == nil {
		t.Fatal("openai_responses client: truncated stream must return an error")
	}
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "upstream stream ended before completion") {
		t.Fatalf("openai_responses client: missing response.failed terminal:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("openai_responses client: fake success tail after truncation:\n%s", out)
	}
}

// The same guarantee when the truncation lands mid tool-call: a
// partial tool stream gets the terminal error too, and no
// success frame ever follows it (the partial stream is not
// completed, replayed or dressed up as finished).
func TestPostCommitTruncationMidToolCall(t *testing.T) {
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\"}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, resp, "anthropic", "anthropic", "client-model", "req-truncated-tool", nil, nil)
	if err == nil {
		t.Fatal("mid-tool truncation must return an error")
	}
	out := rr.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("mid-tool truncation: missing error event:\n%s", out)
	}
	if strings.Contains(out, "message_stop") {
		t.Fatalf("mid-tool truncation: fake success tail:\n%s", out)
	}
}

// The legacy translation pumps carry the same contract:
// OpenAI→Anthropic truncation yields an Anthropic error event
// (never message_stop); Anthropic→OpenAI truncation yields the
// OpenAI error chunk (never [DONE]).
func TestLegacyPumpTruncationTerminalErrors(t *testing.T) {
	openAI := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(openAI)),
	}
	rr := httptest.NewRecorder()
	if err := streamOpenAIToAnthropic(rr, resp, "m", nil); err == nil {
		t.Fatal("openai->anthropic truncation must return an error")
	}
	out := rr.Body.String()
	if !strings.Contains(out, "event: error") || strings.Contains(out, "message_stop") {
		t.Fatalf("openai->anthropic truncation contract violated:\n%s", out)
	}

	anthropic := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	resp2 := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(anthropic)),
	}
	rr2 := httptest.NewRecorder()
	if err := streamAnthropicToOpenAI(rr2, resp2, "m", nil); err == nil {
		t.Fatal("anthropic->openai truncation must return an error")
	}
	out2 := rr2.Body.String()
	if !strings.Contains(out2, `"error"`) || strings.Contains(out2, "[DONE]") {
		t.Fatalf("anthropic->openai truncation contract violated:\n%s", out2)
	}
}
