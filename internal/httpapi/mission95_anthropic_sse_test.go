package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type mission95Backoff struct{ d time.Duration }

func (b mission95Backoff) RetryBackoff() time.Duration { return b.d }

func TestMission95RetryPauseAndRawError(t *testing.T) {
	s := &Server{}
	// Last attempt never pauses.
	start := time.Now()
	s.retryPause(context.Background(), "req-1", mission95Backoff{time.Hour}, 4, 5)
	if time.Since(start) > time.Second {
		t.Error("last-attempt retryPause slept")
	}
	// Zero backoff never pauses.
	s.retryPause(context.Background(), "req-1", mission95Backoff{0}, 0, 5)
	// Cancelled context aborts the pause promptly.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.retryPause(ctx, "req-1", mission95Backoff{time.Hour}, 0, 5)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("retryPause did not abort on cancellation")
	}
	// writeRawUpstreamError preserves status, content type and body.
	rec := httptest.NewRecorder()
	writeRawUpstreamError(rec, http.StatusBadGateway, "application/json", []byte(`{"e":1}`))
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != `{"e":1}` {
		t.Fatalf("raw error = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writeRawUpstreamError(rec, 500, "", []byte("oops"))
	if rec.Header().Get("Content-Type") != "" || rec.Body.String() != "oops" {
		t.Errorf("raw error without type = %q %q", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestMission95CopyUpstreamHeaders(t *testing.T) {
	resp := &http.Response{Header: http.Header{
		"Content-Type":      []string{"text/event-stream"},
		"Content-Length":    []string{"42"},
		"Authorization":     []string{"secret"},
		"Set-Cookie":        []string{"session=abc"},
		"Connection":        []string{"keep-alive, X-Custom-Hop"},
		"X-Custom-Hop":      []string{"hop"},
		"Keep-Alive":        []string{"timeout=5"},
		"X-Upstream":        []string{"yes"},
		"Cache-Control":     []string{"orig"},
		"Transfer-Encoding": []string{"chunked"},
		"X-Multi":           []string{"a", "b"},
	}}
	rec := httptest.NewRecorder()
	copyUpstreamResponseHeaders(rec, resp, true)
	h := rec.Header()
	if h.Get("X-Upstream") != "yes" || len(h.Values("X-Multi")) != 2 {
		t.Errorf("forwarded headers = %v", h)
	}
	for _, blocked := range []string{"Authorization", "Set-Cookie", "Keep-Alive", "Transfer-Encoding", "X-Custom-Hop", "Content-Length"} {
		if h.Get(blocked) != "" {
			t.Errorf("SSE copy leaked %s = %q", blocked, h.Get(blocked))
		}
	}
	if h.Get("Cache-Control") != "no-cache" || h.Get("X-Accel-Buffering") != "no" {
		t.Errorf("SSE headers cache=%q accel=%q", h.Get("Cache-Control"), h.Get("X-Accel-Buffering"))
	}
	// Non-SSE keeps Content-Length and does not force SSE headers.
	rec = httptest.NewRecorder()
	copyUpstreamResponseHeaders(rec, resp, false)
	if rec.Header().Get("Content-Length") != "42" || rec.Header().Get("X-Accel-Buffering") != "" {
		t.Errorf("non-SSE headers = %v", rec.Header())
	}
}

func TestMission95NativeSSETracker(t *testing.T) {
	// OpenAI: [DONE] terminates; finish_reason terminates; usage fires once.
	var got []struct{ p, c int }
	tr := &nativeSSETracker{protocol: "openai", usageHook: func(u nativeSSEUsage) { got = append(got, struct{ p, c int }{u.Input, u.Output}) }}
	if err := tr.consume([]byte("data: {\"choices\":[{\"finish_reason\":null}]}\n\n")); err != nil || tr.terminal {
		t.Fatalf("null finish_reason: terminal=%v err=%v", tr.terminal, err)
	}
	if err := tr.consume([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4}}\n")); err != nil || !tr.terminal {
		t.Fatalf("stop chunk: terminal=%v err=%v", tr.terminal, err)
	}
	if len(got) != 1 || got[0].p != 10 || got[0].c != 4 {
		t.Fatalf("openai usage hook = %v", got)
	}
	// A second usage chunk does not re-fire the hook.
	if err := tr.consume([]byte("data: {\"usage\":{\"prompt_tokens\":99,\"completion_tokens\":99}}\n")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("usage hook re-fired: %v", got)
	}
	if err := tr.finish(); err != nil {
		t.Errorf("terminated tracker finish failed: %v", err)
	}
	// [DONE] alone terminates.
	tr2 := &nativeSSETracker{protocol: "openai"}
	if err := tr2.consume([]byte("data: [DONE]\n")); err != nil || !tr2.terminal {
		t.Fatalf("[DONE] terminal=%v err=%v", tr2.terminal, err)
	}
	// Unterminated stream fails finish with unexpected EOF.
	tr3 := &nativeSSETracker{protocol: "openai"}
	if err := tr3.consume([]byte("data: {\"choices\":[]}\n")); err != nil {
		t.Fatal(err)
	}
	if err := tr3.finish(); err != io.ErrUnexpectedEOF {
		t.Errorf("unterminated finish = %v", err)
	}
	// Non-data lines, comments and empty data are ignored.
	tr4 := &nativeSSETracker{protocol: "openai"}
	if err := tr4.consume([]byte(": comment\nevent: x\ndata:\n\ndata: [DONE]\n")); err != nil || !tr4.terminal {
		t.Fatalf("comment stream: terminal=%v err=%v", tr4.terminal, err)
	}
	// Error envelopes and malformed JSON fail loudly.
	for _, tc := range []struct {
		protocol string
		line     string
	}{
		{"openai", "data: {bad\n"},
		{"openai", "data: {\"error\":{\"message\":\"x\"}}\n"},
		{"openai", "data: {\"choices\":\"nope\"}\n"},
		{"openai", "data: {\"choices\":[{\"finish_reason\":123}]}\n"},
		{"anthropic", "data: {\"type\":\"error\"}\n"},
		{"anthropic", "data: {\"type\":123}\n"},
		{"anthropic", "data: {\"type\":\"message_delta\",\"delta\":\"nope\"}\n"},
		{"unknown-proto", "data: {\"type\":\"message_start\"}\n"},
	} {
		tr := &nativeSSETracker{protocol: tc.protocol}
		if err := tr.consume([]byte(tc.line)); err == nil {
			t.Errorf("protocol %s line %q accepted", tc.protocol, tc.line)
		}
	}
	// Anthropic: message_delta stop_reason terminates; message_stop ends
	// the stream. With the envelope unwrap fix (#712, carried into the
	// nativeSSEUsage hook by #706), standard Anthropic envelopes
	// accumulate real totals and the message_stop hook fires exactly
	// once; the assertions below pin that fixed behavior.
	var aGot []struct{ p, c int }
	atr := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) { aGot = append(aGot, struct{ p, c int }{u.Input, u.Output}) }}
	feed := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n",
	}
	for _, line := range feed {
		if err := atr.consume([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if !atr.terminal {
		t.Error("message_delta with stop_reason did not terminate")
	}
	if err := atr.consume([]byte("data: {\"type\":\"message_stop\"}\n")); err != nil {
		t.Fatal(err)
	}
	if len(aGot) != 1 || aGot[0].p != 12 || aGot[0].c != 7 {
		t.Fatalf("anthropic usage hook = %v, want exactly one call (12, 7)", aGot)
	}
	if err := atr.finish(); err != nil {
		t.Errorf("anthropic finish failed: %v", err)
	}
	// The message_stop fire branch itself: with totals present, the hook
	// fires exactly once and never re-fires.
	direct := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) { aGot = append(aGot, struct{ p, c int }{u.Input, u.Output}) }}
	direct.usageInput, direct.usageOutput = 12, 7
	direct.observeAnthropicUsage("message_stop", map[string]json.RawMessage{})
	if len(aGot) != 2 || aGot[1].p != 12 || aGot[1].c != 7 {
		t.Fatalf("direct message_stop hook = %v", aGot)
	}
	direct.observeAnthropicUsage("message_stop", map[string]json.RawMessage{})
	if len(aGot) != 2 {
		t.Error("message_stop hook re-fired")
	}
	// With zero totals the hook stays silent even on message_stop.
	zero := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) { aGot = append(aGot, struct{ p, c int }{u.Input, u.Output}) }}
	zero.observeAnthropicUsage("message_stop", map[string]json.RawMessage{})
	if len(aGot) != 2 {
		t.Error("zero-total message_stop fired the hook")
	}
	// Finish processes a buffered partial line.
	partial := &nativeSSETracker{protocol: "openai"}
	if err := partial.consume([]byte("data: [DONE]")); err != nil {
		t.Fatal(err)
	}
	if err := partial.finish(); err != nil || !partial.terminal {
		t.Errorf("partial-line finish: terminal=%v err=%v", partial.terminal, err)
	}
	// Oversize lines are rejected, both buffered and complete.
	huge := &nativeSSETracker{protocol: "openai"}
	if err := huge.consume([]byte(strings.Repeat("x", maxNativeSSELineBytes+1))); err == nil {
		t.Error("oversize buffered line accepted")
	}
	huge2 := &nativeSSETracker{protocol: "openai"}
	if err := huge2.consume([]byte(strings.Repeat("x", maxNativeSSELineBytes+1) + "\n")); err == nil {
		t.Error("oversize complete line accepted")
	}
	// Partial lines accumulate across consume calls.
	split := &nativeSSETracker{protocol: "openai"}
	if err := split.consume([]byte("data: [DO")); err != nil {
		t.Fatal(err)
	}
	if err := split.consume([]byte("NE]\n")); err != nil || !split.terminal {
		t.Fatalf("split line: terminal=%v err=%v", split.terminal, err)
	}
}

func TestMission95AnthropicRewritesAndProxy(t *testing.T) {
	// rewriteAnthropicResponseModel: blank model is a no-op; otherwise set.
	body := []byte(`{"id":"m1","model":"old"}`)
	if got, err := rewriteAnthropicResponseModel(body, ""); err != nil || string(got) != string(body) {
		t.Fatalf("blank model rewrite = %s, %v", got, err)
	}
	got, err := rewriteAnthropicResponseModel(body, "new-model")
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(got, &env); err != nil || env["model"] != "new-model" || env["id"] != "m1" {
		t.Fatalf("rewritten = %s, %v", got, err)
	}
	if _, err := rewriteAnthropicResponseModel([]byte(`{bad`), "x"); err == nil {
		t.Error("invalid response rewrite accepted")
	}
	if _, err := rewriteAnthropicResponseModel([]byte(`[1,2]`), "x"); err == nil {
		t.Error("non-object response rewrite accepted")
	}
	// rewriteAnthropicSSEModelLine: only message_start lines, CRLF preserved.
	start := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"old\",\"id\":\"m1\"}}\r\n"
	rewritten := rewriteAnthropicSSEModelLine([]byte(start), "pub")
	if !strings.Contains(string(rewritten), `"model":"pub"`) || !strings.HasSuffix(string(rewritten), "\r\n") || !strings.HasPrefix(string(rewritten), "data: ") {
		t.Fatalf("sse model rewrite = %q", rewritten)
	}
	// Non-message_start, non-data, and malformed lines pass through untouched.
	for _, line := range []string{
		"data: {\"type\":\"content_block_delta\"}\n",
		"event: message_start\n",
		"data: {bad\n",
		"data: {\"type\":\"message_start\",\"message\":null}\n",
		"data: {\"type\":\"message_start\"}\n",
	} {
		if out := rewriteAnthropicSSEModelLine([]byte(line), "pub"); string(out) != line {
			t.Errorf("line %q rewritten to %q", line, out)
		}
	}
	// Leading whitespace before data: is preserved.
	indented := "  data: {\"type\":\"message_start\",\"message\":{\"model\":\"old\"}}\n"
	if out := rewriteAnthropicSSEModelLine([]byte(indented), "pub"); !strings.HasPrefix(string(out), "  data: ") || !strings.Contains(string(out), `"model":"pub"`) {
		t.Fatalf("indented rewrite = %q", out)
	}
	// proxyNativeSSE: non-SSE content type refused; SSE proxied with model
	// rewrite. With the envelope unwrap fix, the Anthropic usage hook
	// fires exactly once for standard envelopes (5 in, 3 out); the
	// OpenAI proxy path below proves the hook wiring end to end.
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("x"))}
	if err := proxyNativeSSE(httptest.NewRecorder(), resp, "anthropic"); err == nil {
		t.Error("non-SSE proxy accepted")
	}
	stream := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"old\",\"usage\":{\"input_tokens\":5}}}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n" +
		"data: {\"type\":\"message_stop\"}\n"
	var hook []struct{ p, c int }
	resp2 := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Upstream": []string{"u"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	rec := httptest.NewRecorder()
	if err := proxyNativeSSEWithModel(rec, resp2, "anthropic", "pub-model", func(u nativeSSEUsage) { hook = append(hook, struct{ p, c int }{u.Input, u.Output}) }); err != nil {
		t.Fatalf("proxy failed: %v", err)
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"model":"pub-model"`) || rec.Header().Get("X-Upstream") != "u" {
		t.Fatalf("proxy output code=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	if len(hook) != 1 || hook[0].p != 5 || hook[0].c != 3 {
		t.Fatalf("anthropic proxy usage hook = %v, want exactly one call (5, 3)", hook)
	}
	// OpenAI proxy: usage chunk fires the hook exactly once.
	oaiStream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n"
	respOAI := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(oaiStream)),
	}
	recOAI := httptest.NewRecorder()
	if err := proxyNativeSSE(recOAI, respOAI, "openai", func(u nativeSSEUsage) { hook = append(hook, struct{ p, c int }{u.Input, u.Output}) }); err != nil {
		t.Fatalf("openai proxy failed: %v", err)
	}
	if len(hook) != 2 || hook[1].p != 8 || hook[1].c != 2 {
		t.Fatalf("openai proxy usage hook = %v", hook)
	}
	// Unterminated proxied stream errors.
	resp3 := &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"content_block_delta\"}\n")),
	}
	if err := proxyNativeSSE(httptest.NewRecorder(), resp3, "anthropic"); err == nil {
		t.Error("unterminated proxied stream accepted")
	}
	// Oversize proxied line errors.
	resp4 := &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxNativeSSELineBytes+1) + "\n")),
	}
	if err := proxyNativeSSE(httptest.NewRecorder(), resp4, "anthropic"); err == nil {
		t.Error("oversize proxied line accepted")
	}
}

func TestMission95AnthropicHandlerEarlyBranches(t *testing.T) {
	s := &Server{}
	// Wrong method: 405 in the Anthropic envelope, no auth consulted.
	rec := httptest.NewRecorder()
	s.anthropicMessages(rec, httptest.NewRequest(http.MethodGet, "/v1/messages", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d body=%s", rec.Code, rec.Body.String())
	}
	// Invalid JSON: 400.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{bad`))
	s.anthropicMessages(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON") {
		t.Fatalf("bad json code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Missing model / empty messages: 400.
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
		`{"model":"m1","messages":[],"max_tokens":10}`,
		`{"model":"  ","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
	} {
		rec = httptest.NewRecorder()
		s.anthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model and messages are required") {
			t.Fatalf("body %s: code=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestMission95AdminProviderPresetsAndCheck(t *testing.T) {
	s := &Server{}
	// Presets: method gate, full list, filtered list, categories present.
	rec := httptest.NewRecorder()
	s.adminProviderPresets(rec, httptest.NewRequest(http.MethodPost, "/admin/api/provider-presets", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("presets POST code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.adminProviderPresets(rec, httptest.NewRequest(http.MethodGet, "/admin/api/provider-presets", nil))
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	presets, _ := env["presets"].([]any)
	total, _ := env["total"].(float64)
	if rec.Code != 200 || len(presets) == 0 || total != float64(len(presets)) {
		t.Fatalf("presets = %v", env)
	}
	if _, ok := env["categories"]; !ok {
		t.Error("presets lack categories")
	}
	// Provider check: method gate, bad JSON, missing base_url.
	rec = httptest.NewRecorder()
	s.adminProviderCheck(rec, httptest.NewRequest(http.MethodGet, "/admin/api/providers/check", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("check GET code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/check", strings.NewReader(`{bad`))
	req.Header.Set("Content-Type", "application/json")
	s.adminProviderCheck(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("check bad json code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/api/providers/check", strings.NewReader(`{"provider":{"id":"p1"}}`))
	req.Header.Set("Content-Type", "application/json")
	s.adminProviderCheck(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "base_url is required") {
		t.Fatalf("check missing base code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Live fake upstream: 200 reachable/auth_ok; 401 auth_ok=false.
	for _, tc := range []struct {
		status   int
		wantOK   bool
		wantAuth bool
	}{
		{200, true, true},
		{401, false, false},
		{500, false, true},
	} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		rec = httptest.NewRecorder()
		body := `{"provider":{"id":"p1","type":"openai_compatible","base_url":"` + up.URL + `","models_path":"/v1/models"}}`
		req = httptest.NewRequest(http.MethodPost, "/admin/api/providers/check", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		s.adminProviderCheck(rec, req)
		up.Close()
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || out["reachable"] != true || out["ok"] != tc.wantOK || out["auth_ok"] != tc.wantAuth || out["status_code"] != float64(tc.status) {
			t.Fatalf("status %d: out=%v", tc.status, out)
		}
	}
	// Unreachable upstream reports reachable=false with an error string.
	rec = httptest.NewRecorder()
	body := `{"provider":{"id":"p1","type":"openai_compatible","base_url":"http://127.0.0.1:1","models_path":"/v1/models"}}`
	req = httptest.NewRequest(http.MethodPost, "/admin/api/providers/check", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.adminProviderCheck(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["reachable"] != false || out["error"] == nil || out["error"] == "" {
		t.Fatalf("unreachable out=%v", out)
	}
}
