package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestNativeAnthropicStreamRecordsUsageEndToEnd(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","usage":{"input_tokens":9,"output_tokens":0}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(
		`{"model":"m","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)))
	if rr.Code != http.StatusOK || rr.Body.String() != stream {
		t.Fatalf("native SSE changed: status=%d body=%q", rr.Code, rr.Body.String())
	}
	usage := s.usage.Snapshot(nil)
	if usage.TotalPromptTokens != 9 || usage.TotalCompletionTokens != 4 || usage.TotalRequests != 1 {
		t.Fatalf("native Anthropic stream lost token usage: %+v", usage)
	}
}

func TestNativeSSEAcceptsMultilineData(t *testing.T) {
	for _, tc := range []struct{ name, protocol, body string }{
		{"OpenAI", "openai", "data: {\"choices\":[{\"finish_reason\":\n" + "data: \"stop\"}]}\n\n"},
		{"Anthropic", "anthropic", "event: message_stop\n" + "data: {\"type\":\n" + "data: \"message_stop\"}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(tc.body))}
			rr := httptest.NewRecorder()
			if err := proxyNativeSSE(rr, resp, tc.protocol); err != nil {
				t.Fatalf("valid multi-line SSE event rejected: %v", err)
			}
			if rr.Body.String() != tc.body {
				t.Fatalf("native SSE was not passed through unchanged: got=%q want=%q", rr.Body.String(), tc.body)
			}
		})
	}
}

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:1])
}

func TestNativeSSEMultilineFrameSurvivesFragmentedReads(t *testing.T) {
	body := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":\n" +
		"data: {\"input_tokens\":7}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(oneByteReader{strings.NewReader(body)})}
	rr := httptest.NewRecorder()
	calls, in, out := 0, 0, 0
	if err := proxyNativeSSE(rr, resp, "anthropic", func(p, c int) {
		calls++
		in, out = p, c
	}); err != nil {
		t.Fatal(err)
	}
	if rr.Body.String() != body || calls != 1 || in != 7 || out != 3 {
		t.Fatalf("fragmented stream lost data or usage: calls=%d tokens=%d/%d body=%q", calls, in, out, rr.Body.String())
	}
}
