package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
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

func TestNativeSSEReportsUpstreamFailuresToClient(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, frame, expected string
	}{
		{"OpenAI error", "openai", "data: {\"error\":{\"message\":\"upstream failed\"}}\n\n", `"error"`},
		{"Anthropic error", "anthropic", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"upstream failed\"}}\n\n", `event: error`},
		{"OpenAI malformed", "openai", "data: {not-json}\n\n", `"error"`},
		{"OpenAI missing terminal", "openai", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", `"error"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(tc.frame))}
			rr := httptest.NewRecorder()
			if err := proxyNativeSSE(rr, resp, tc.protocol); err == nil {
				t.Fatal("invalid upstream stream was accepted")
			}
			out := rr.Body.String()
			if !strings.Contains(out, tc.expected) || !strings.Contains(out, "upstream stream failed") {
				t.Fatalf("upstream error was hidden from SSE client: %q", out)
			}
			if strings.Contains(out, "upstream failed") || strings.Contains(out, "{not-json}") {
				t.Fatalf("untrusted upstream error details leaked to SSE client: %q", out)
			}
		})
	}
}

func TestSessionAffinityIsIsolatedAcrossAuthenticatedClients(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.SessionAffinity = true
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"client-key-alpha", "client-key-bravo"}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "first", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "up-one", Enabled: true, Weight: 1}}},
		{ID: "second", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "up-two", Enabled: true, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	requirement := func(key string) router.Requirement {
		r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
		r.Header.Set("X-Session-ID", "same-session")
		r.Header.Set("Authorization", "Bearer "+key)
		return s.prepareRequirement(router.Requirement{Model: "auto"}, r, "")
	}
	first := requirement("client-key-alpha")
	second := requirement("client-key-bravo")
	if first.SessionKey == second.SessionKey {
		t.Fatal("different client keys shared a session affinity bucket")
	}
	s.rt.ObserveSession(first, "first/m")
	s.rt.ObserveSession(second, "second/m")
	for _, tc := range []struct {
		req  router.Requirement
		want string
	}{{first, "first/m"}, {second, "second/m"}} {
		got := s.rt.Candidates(tc.req)
		if len(got) != 2 || got[0].Deployment.ID != tc.want {
			t.Fatalf("client pin overwritten: want=%s candidates=%+v", tc.want, got)
		}
	}
}
