package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

// Regression (audit 2026-10-15): the stream pump OVERWROTE its
// terminal flag with each chunk's own flag for openai_chat and
// openai_responses, while the Bedrock arm latched it. With
// stream_options.include_usage, OpenAI sends a usage-only chunk
// AFTER the finish_reason chunk and before [DONE]; that chunk
// decodes with terminal=false, so a connection that then ended
// without [DONE] made the pump report a completed stream as
// "upstream stream ended before completion", skip Finish() (no
// final chunk / [DONE] to the client) and skip the usage hook.
// Terminal must latch for every kind.
func TestStreamPumpTerminalLatchesAcrossUsageOnlyChunk(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	rr := httptest.NewRecorder()
	var hooked *canonical.Usage
	err := (&Server{}).canonicalStreamPump(rr, resp, "openai_chat", "openai_chat", "client-model", "req-latch", nil,
		func(u canonical.Usage) { c := u; hooked = &c })
	if err != nil {
		t.Fatalf("completed stream must finish cleanly, got err=%v (body=%q)", err, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "ended before completion") {
		t.Fatalf("client saw a spurious truncation error: %q", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("Finish() framing ([DONE]) missing from client stream: %q", rr.Body.String())
	}
	if hooked == nil || hooked.InputTokens != 5 || hooked.OutputTokens != 2 {
		t.Fatalf("usage hook = %+v, want input=5 output=2 (usage from the trailing chunk must be recorded)", hooked)
	}
}
