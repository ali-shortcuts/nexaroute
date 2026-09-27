package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPAPISSEReaderRejectsOversizedMultilineEvent(t *testing.T) {
	var b strings.Builder
	chunk := strings.Repeat("x", 64<<10)
	for i := 0; i < 129; i++ {
		b.WriteString("data: ")
		b.WriteString(chunk)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, _, err := newSSEReader(strings.NewReader(b.String())).Next()
	if err == nil || !strings.Contains(err.Error(), "safe limit") {
		t.Fatalf("expected bounded SSE error, got %v", err)
	}
}

func TestOpenAIToAnthropicRejectsArgsBeforeToolName(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"x\\\":1}\"}}]}}]}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	err := streamOpenAIToAnthropic(httptest.NewRecorder(), resp, "model", nil)
	if err == nil || !strings.Contains(err.Error(), "before tool name") {
		t.Fatalf("expected bounded ordering error, got %v", err)
	}
}
