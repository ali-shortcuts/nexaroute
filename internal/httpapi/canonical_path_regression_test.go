package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
