package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestCanonicalStreamPumpClosesBodyOnProtocolError(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader("data: {not-json}\n\n")}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	rr := httptest.NewRecorder()
	s := &Server{}
	err := s.canonicalStreamPump(rr, resp, "openai_chat", "openai_chat", "client-model", "req-close", nil)
	if err == nil {
		t.Fatal("expected canonical stream protocol error")
	}
	if !body.closed.Load() {
		t.Fatal("canonical stream pump leaked upstream response body on early failure")
	}
}

func TestResponsesIngressCarriesQuotaReservationEstimate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-q","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	body := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello quota reservation"}]}],"max_output_tokens":64}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(rr, req)
		close(done)
	}()

	<-started
	st, ok := s.reg.Stat("p")
	if !ok {
		t.Fatal("provider stats missing")
	}
	if st.ReservedRequests != 1 {
		t.Fatalf("Responses ingress did not reserve upstream request quota: %+v", st)
	}
	if st.ReservedTokens <= 64 {
		t.Fatalf("Responses ingress token reservation omitted prompt estimate or output bound: %+v", st)
	}

	close(release)
	<-done
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	st, _ = s.reg.Stat("p")
	if st.ReservedRequests != 0 || st.ReservedTokens != 0 {
		t.Fatalf("Responses ingress leaked quota reservation after response completion: %+v", st)
	}
}

func TestCanonicalStreamUsageRecordedOnceAfterSuccessfulTerminal(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":0}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	rr := httptest.NewRecorder()
	calls := 0
	gotInput, gotOutput := 0, 0
	err := (&Server{}).canonicalStreamPump(rr, resp, "anthropic", "openai_chat", "client-model", "req-usage",
		func(input, output int) {
			calls++
			gotInput, gotOutput = input, output
		})
	if err != nil {
		t.Fatalf("canonical stream failed: %v", err)
	}
	if calls != 1 || gotInput != 7 || gotOutput != 3 {
		t.Fatalf("usage hook calls=%d input=%d output=%d want 1,7,3", calls, gotInput, gotOutput)
	}
}

func TestAnthropicTextBlockStopDoesNotBecomeResponsesToolEnd(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":4,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, resp, "anthropic", "openai_responses", "client-model", "req-text-stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := rr.Body.String()
	if !strings.Contains(out, "response.output_text.delta") || !strings.Contains(out, "hello") {
		t.Fatalf("text delta missing from Responses stream: %s", out)
	}
	if strings.Contains(out, "function_call_arguments.done") || strings.Contains(out, `"type":"function_call"`) {
		t.Fatalf("Anthropic text block stop was misclassified as tool completion: %s", out)
	}
}

func TestCanonicalStreamErrorDoesNotAppendSuccessTail(t *testing.T) {
	stream := "data: {\"error\":{\"message\":\"upstream exploded\",\"type\":\"server_error\"}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, resp, "openai_chat", "openai_responses", "client-model", "req-stream-error", nil)
	if err == nil {
		t.Fatal("expected upstream stream error")
	}
	out := rr.Body.String()
	if !strings.Contains(out, "response.failed") {
		t.Fatalf("Responses client did not receive failure terminal: %s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("canonical stream emitted success after failure: %s", out)
	}
}

func TestAnthropicMessageStopDoesNotOverwriteToolUseReason(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":4,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"lookup","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, resp, "anthropic", "openai_chat", "client-model", "req-tool-stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := rr.Body.String()
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("Anthropic tool_use stop reason was overwritten: %s", out)
	}
}

func TestCanonicalStreamStopsReadingAfterUpstreamError(t *testing.T) {
	for _, tc := range []struct{ name, kind, frame string }{
		{"OpenAI", "openai_chat", "data: {\"error\":{\"message\":\"bad key\"}}\n\n"},
		{"Anthropic", "anthropic", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"bad key\"}}\n\n"},
		{"Responses", "openai_responses", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"bad key\"}}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			resp := &http.Response{Body: reader, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
			rr := httptest.NewRecorder()
			finished := make(chan error, 1)
			go func() {
				finished <- (&Server{}).canonicalStreamPump(rr, resp, tc.kind, "openai_responses", "m", "error-event", nil)
			}()
			if _, err := io.WriteString(writer, tc.frame); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("upstream failure was reported as stream success")
				}
				if out := rr.Body.String(); !strings.Contains(out, "response.failed") || strings.Contains(out, "response.completed") {
					t.Fatalf("wrong terminal frame after upstream error: %s", out)
				}
			case <-time.After(time.Second):
				_ = writer.Close() // Unblock the faulty implementation before failing.
				<-finished
				t.Fatal("gateway kept waiting for upstream data after a terminal error event")
			}
		})
	}
}

func TestCanonicalResponseRejectsTruncatedOversizeBody(t *testing.T) {
	const limit = 8 << 20
	valid := `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`
	padded := valid + strings.Repeat(" ", limit-len(valid))
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"exactly at limit", padded, true},
		{"invalid trailing data beyond limit", padded + "MALFORMED", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(tc.body))}
			rr := httptest.NewRecorder()
			err := (&Server{}).handleCanonicalResponse(rr, resp, "gemini", "openai_chat", "m", "size-limit", false, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v body=%s", tc.valid, err, rr.Body.String())
			}
			if tc.valid && !strings.Contains(rr.Body.String(), "OK") || !tc.valid && rr.Body.Len() != 0 {
				t.Fatalf("wrong output for valid=%v: %q", tc.valid, rr.Body.String())
			}
		})
	}
}
