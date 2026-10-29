package httpapi

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

// wave2FailWriter is an http.ResponseWriter that fails on the Nth Write
// (1-based). It never implements http.Flusher, so emitter Flush calls are
// no-ops and the write count maps exactly onto emitted frames.
type wave2FailWriter struct {
	header  http.Header
	failOn  int
	writes  int
	failErr error
}

func (w *wave2FailWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *wave2FailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failOn > 0 && w.writes == w.failOn {
		return 0, w.failErr
	}
	return len(p), nil
}

func (w *wave2FailWriter) WriteHeader(int) {}

// wave2SSEBody joins SSE frames (each "event: ...\ndata: {...}") into a
// stream body terminated by a blank line after every frame.
func wave2SSEBody(t *testing.T, frames ...string) io.ReadCloser {
	t.Helper()
	return io.NopCloser(strings.NewReader(strings.Join(frames, "\n\n") + "\n\n"))
}

func wave2PumpResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
}

// wave2ToolStream is a complete Anthropic tool-use stream: tool_use start,
// one JSON-args delta, and the matching content_block_stop.
func wave2ToolStream() string {
	return strings.Join([]string{
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"paris\"}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
	}, "\n\n") + "\n\n"
}

func TestWave2CanonicalPumpCacheTokens(t *testing.T) {
	var got canonical.Usage
	var calls int
	body := wave2SSEBody(t,
		`event: message_start`+"\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3,"cache_read_input_tokens":200,"cache_creation_input_tokens":50}}`,
		`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
	)
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, wave2PumpResponse(body), "anthropic", "anthropic", "client-model", "req-cache", nil,
		func(u canonical.Usage) { calls++; got = u })
	if err != nil {
		t.Fatalf("pump failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("usage hook calls=%d want 1", calls)
	}
	if got.InputTokens != 5 || got.OutputTokens != 3 || got.CacheReadTokens != 200 || got.CacheWriteTokens != 50 {
		t.Fatalf("usage=%+v want input=5 output=3 cacheRead=200 cacheWrite=50", got)
	}
}

func TestWave2CanonicalPumpReaderError(t *testing.T) {
	body := io.NopCloser(&wave2ErrorReader{done: true, err: errors.New("wave2 read failure")})
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, wave2PumpResponse(body), "anthropic", "anthropic", "client-model", "req-readerr", nil, nil)
	if err == nil || err.Error() != "wave2 read failure" {
		t.Fatalf("err=%v want the injected read failure", err)
	}
}

func TestWave2CanonicalPumpToolArgsOverLimit(t *testing.T) {
	frames := []string{
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
	}
	chunk := strings.Repeat("x", 100<<10)
	for i := 0; i < 12; i++ {
		frames = append(frames, `event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+chunk+`"}}`)
	}
	rr := httptest.NewRecorder()
	err := (&Server{}).canonicalStreamPump(rr, wave2PumpResponse(wave2SSEBody(t, frames...)), "anthropic", "anthropic", "client-model", "req-bigargs", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "args exceed 1MB limit") {
		t.Fatalf("err=%v want the 1MB args terminal error", err)
	}
}

func TestWave2CanonicalPumpTextEmitError(t *testing.T) {
	body := wave2SSEBody(t,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
	)
	// The emitter constructor writes message_start + ping (writes 1-2), so
	// failing on write 1 fails the first client-visible emission.
	w := &wave2FailWriter{failOn: 1, failErr: errors.New("wave2 write failure")}
	err := (&Server{}).canonicalStreamPump(w, wave2PumpResponse(body), "anthropic", "anthropic", "client-model", "req-emit", nil, nil)
	if err == nil {
		t.Fatal("expected an emission error, got nil")
	}
}

func TestWave2CanonicalPumpToolStartEmitError(t *testing.T) {
	// Writes: 1-2 emitter constructor, 3 tool-start frame.
	w := &wave2FailWriter{failOn: 3, failErr: errors.New("wave2 write failure")}
	err := (&Server{}).canonicalStreamPump(w, wave2PumpResponse(wave2SSEBody(t, wave2ToolStream())), "anthropic", "anthropic", "client-model", "req-toolstart", nil, nil)
	if err == nil {
		t.Fatal("expected a tool-start emission error, got nil")
	}
}

func TestWave2CanonicalPumpToolArgsEmitError(t *testing.T) {
	// Writes: 1-2 emitter constructor, 3 tool-start, 4 args frame.
	w := &wave2FailWriter{failOn: 4, failErr: errors.New("wave2 write failure")}
	err := (&Server{}).canonicalStreamPump(w, wave2PumpResponse(wave2SSEBody(t, wave2ToolStream())), "anthropic", "anthropic", "client-model", "req-toolargs", nil, nil)
	if err == nil {
		t.Fatal("expected a tool-args emission error, got nil")
	}
}

func TestWave2CanonicalPumpToolEndEmitError(t *testing.T) {
	// Writes: 1-2 emitter constructor, 3 tool-start, 4 args, 5 end frame.
	w := &wave2FailWriter{failOn: 5, failErr: errors.New("wave2 write failure")}
	err := (&Server{}).canonicalStreamPump(w, wave2PumpResponse(wave2SSEBody(t, wave2ToolStream())), "anthropic", "anthropic", "client-model", "req-toolend", nil, nil)
	if err == nil {
		t.Fatal("expected a tool-end emission error, got nil")
	}
}

func TestWave2CanonicalPumpStreamEndToolEmitError(t *testing.T) {
	// A tool left open when the stream ends is validated and emitted by the
	// StreamEnd loop; failing the start frame there must abort the pump.
	frames := []string{
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"paris\"}"}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
	w := &wave2FailWriter{failOn: 3, failErr: errors.New("wave2 write failure")}
	err := (&Server{}).canonicalStreamPump(w, wave2PumpResponse(wave2SSEBody(t, frames...)), "anthropic", "anthropic", "client-model", "req-streamend", nil, nil)
	if err == nil {
		t.Fatal("expected a StreamEnd-loop emission error, got nil")
	}
}

func TestWave2CanonicalPumpFinishError(t *testing.T) {
	// No text or tool frames are emitted, so the only writes are the
	// constructor pair (1-2) and Finish's message_delta frame (3).
	body := wave2SSEBody(t,
		`event: message_start`+"\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
	)
	w := &wave2FailWriter{failOn: 3, failErr: errors.New("wave2 write failure")}
	err := (&Server{}).canonicalStreamPump(w, wave2PumpResponse(body), "anthropic", "anthropic", "client-model", "req-finish", nil, nil)
	if err == nil {
		t.Fatal("expected a Finish error, got nil")
	}
}

func TestWave2CanonicalNonStreamReadError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(&wave2ErrorReader{done: true, err: errors.New("wave2 read failure")}),
	}
	rr := httptest.NewRecorder()
	err := (&Server{}).handleCanonicalResponse(rr, resp, "openai", "openai_chat", "m", "req-nonstream", false, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "upstream response read failed") {
		t.Fatalf("err=%v want the upstream read failure", err)
	}
}

func TestWave2MediaMethodNotAllowed(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/v1/embeddings", nil)
	(&Server{}).mediaEmbeddings(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s want 405", rr.Code, rr.Body.String())
	}
}

func TestWave2MediaAuthFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = []string{"legacy-key"}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s want 401 for a missing client key", rr.Code, rr.Body.String())
	}
}

func TestWave2MediaBodyReadError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings", io.NopCloser(&wave2ErrorReader{done: true, err: errors.New("wave2 read failure")}))
	rr := httptest.NewRecorder()
	(&Server{}).serveMedia(rr, req, mediaEmbeddings)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "request body read failed") {
		t.Fatalf("status=%d body=%s want 400 request body read failed", rr.Code, rr.Body.String())
	}
}

func TestWave2MediaOversizeBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings", strings.NewReader(strings.Repeat("x", (32<<20)+1)))
	rr := httptest.NewRecorder()
	(&Server{}).serveMedia(rr, req, mediaEmbeddings)
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "request body exceeds") {
		t.Fatalf("status=%d body=%s want 413 request body exceeds", rr.Code, rr.Body.String())
	}
}

func wave2Multipart(t *testing.T, fields map[string]string) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

func TestWave2MediaRequestModel(t *testing.T) {
	jsonReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings", nil)
	jsonReq.Header.Set("Content-Type", "application/json")
	if m, err := mediaRequestModel(jsonReq, []byte(`{"model":"m"}`), mediaEmbeddings); err != nil || m != "m" {
		t.Fatalf("JSON model=%q err=%v want m, nil", m, err)
	}
	if _, err := mediaRequestModel(jsonReq, []byte(`{}`), mediaEmbeddings); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("empty model err=%v want model is required", err)
	}
	if _, err := mediaRequestModel(jsonReq, []byte(`not json`), mediaEmbeddings); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("bad JSON err=%v want invalid JSON", err)
	}

	mpReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/audio/transcriptions", nil)
	if _, err := mediaRequestModel(mpReq, []byte(`x`), mediaTranscriptions); err == nil || !strings.Contains(err.Error(), "transcriptions require a multipart/form-data request") {
		t.Fatalf("non-multipart err=%v want the multipart requirement", err)
	}
	raw, ct := wave2Multipart(t, map[string]string{"other": "v"})
	mpReq.Header.Set("Content-Type", ct)
	if _, err := mediaRequestModel(mpReq, raw, mediaTranscriptions); err == nil || !strings.Contains(err.Error(), "model form field is required") {
		t.Fatalf("missing model err=%v want model form field is required", err)
	}
	raw, ct = wave2Multipart(t, map[string]string{"model": "  "})
	mpReq.Header.Set("Content-Type", ct)
	if _, err := mediaRequestModel(mpReq, raw, mediaTranscriptions); err == nil || !strings.Contains(err.Error(), "model form field is empty") {
		t.Fatalf("empty model err=%v want model form field is empty", err)
	}
	raw, ct = wave2Multipart(t, map[string]string{"model": "whisper-1"})
	mpReq.Header.Set("Content-Type", ct)
	if m, err := mediaRequestModel(mpReq, raw, mediaTranscriptions); err != nil || m != "whisper-1" {
		t.Fatalf("multipart model=%q err=%v want whisper-1, nil", m, err)
	}
	// A malformed part before any model part must fail the parse, not be
	// silently skipped.
	bad := "--B\r\nBADLINE\r\n\r\nx\r\n--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nwhisper-1\r\n--B--\r\n"
	mpReq.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	if _, err := mediaRequestModel(mpReq, []byte(bad), mediaTranscriptions); err == nil || !strings.Contains(err.Error(), "invalid multipart body") {
		t.Fatalf("malformed multipart err=%v want invalid multipart body", err)
	}
}

func TestWave2MediaRewriteMultipart(t *testing.T) {
	raw, ct := wave2Multipart(t, map[string]string{"model": "old-model", "note": "keep"})
	out, _, err := rewriteMultipartModel(raw, ct, "new-model")
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	if !strings.Contains(string(out), "new-model") || strings.Contains(string(out), "old-model") {
		t.Fatalf("rewritten body=%s want new-model without old-model", out)
	}
	if _, _, err := rewriteMultipartModel([]byte("--B\r\nBADLINE\r\n\r\nx\r\n--B--\r\n"), "multipart/form-data; boundary=B", "new-model"); err == nil {
		t.Fatal("expected a rewrite error for a malformed part, got nil")
	}
	if _, _, err := rewriteMultipartModel(raw, ct, ""); err != nil {
		// Empty replacement is still a well-formed rewrite; the model field
		// is replaced with the empty value rather than erroring.
		t.Fatalf("empty-model rewrite err=%v want nil", err)
	}
}

func TestWave2MediaServeModelError(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "model is required") {
		t.Fatalf("status=%d body=%s want 400 model is required", rr.Code, rr.Body.String())
	}
}

func TestWave2MediaServeDisabledVirtualEndpoint(t *testing.T) {
	s := wave2AdminServer(t)
	if rr := wave2Call(s, "POST", "/admin/api/virtual-endpoints", `{"id":"ve-off2","public_model":"ve-model2","route_profile":"profile1","enabled":false}`); rr.Code != 201 {
		t.Fatalf("disabled VE create: %d %s", rr.Code, rr.Body.String())
	}
	if rr := wave2Call(s, "POST", "/v1/embeddings", `{"model":"ve-model2","input":"hi"}`); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatalf("disabled VE embeddings: %d %s want 404 disabled", rr.Code, rr.Body.String())
	}
}

func TestWave2MediaServeMultipartRewriteError(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	// mediaRequestModel stops at the model part, but the rewrite pass
	// walks every part and must fail on the malformed trailing part.
	raw := "--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--B\r\nBADLINE\r\n\r\nx\r\n--B--\r\n"
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/audio/transcriptions", strings.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid multipart body") {
		t.Fatalf("status=%d body=%s want 400 invalid multipart body", rr.Code, rr.Body.String())
	}
}

func TestWave2MediaUpstreamPathUnknownKind(t *testing.T) {
	if got := mediaUpstreamPath(config.ProviderConfig{}, mediaKind("bogus")); got != "" {
		t.Fatalf("unknown kind path=%q want empty", got)
	}
}

func TestWave2MediaUpstreamTransportError(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s want 502 for an unreachable upstream", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "connection refused") {
		t.Fatalf("body=%s want the dial failure surfaced", rr.Body.String())
	}
}

func TestWave2AdminEvalMethodGuards(t *testing.T) {
	s := &Server{}
	if rr := httptest.NewRecorder(); func() *httptest.ResponseRecorder {
		s.adminEvaluationSuites(rr, httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluations/suites", nil))
		return rr
	}().Code != http.StatusMethodNotAllowed {
		t.Fatal("suites POST must be 405")
	}
	if rr := httptest.NewRecorder(); func() *httptest.ResponseRecorder {
		s.adminEvaluationRuns(rr, httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluations/runs", nil))
		return rr
	}().Code != http.StatusMethodNotAllowed {
		t.Fatal("runs POST must be 405")
	}
	if rr := httptest.NewRecorder(); func() *httptest.ResponseRecorder {
		s.adminEvaluationRun(rr, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/evaluations/run", nil))
		return rr
	}().Code != http.StatusMethodNotAllowed {
		t.Fatal("run GET must be 405")
	}
}

func TestWave2AdminEvalRunBodyReadError(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Evaluation.Enabled = true
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluations/run", io.NopCloser(&wave2ErrorReader{done: true, err: errors.New("wave2 read failure")}))
	rr := httptest.NewRecorder()
	s.adminEvaluationRun(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "cannot read request body") {
		t.Fatalf("status=%d body=%s want 400 cannot read request body", rr.Code, rr.Body.String())
	}
}

func TestWave2AdminEvalNilGuards(t *testing.T) {
	s := &Server{}
	if _, ok := s.deploymentForEvaluation("nope"); ok {
		t.Fatal("deployment lookup without a router must report absent")
	}
	resp := map[string]any{}
	s.emitEvaluationEvent(eval.Result{}, resp)
	if len(resp) != 0 {
		t.Fatalf("nil-bus event emission must not write to the response, got %v", resp)
	}
	if got := boundedEvalLabel(strings.Repeat("x", 40)); len(got) != 32 || got != strings.Repeat("x", 32) {
		t.Fatalf("bounded label=%q want 32 x's", got)
	}
	if got := boundedEvalLabel("short"); got != "short" {
		t.Fatalf("short label=%q want unchanged", got)
	}
}

func TestWave2ResponsesTooLargeBody(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(strings.Repeat("x", (16<<20)+1)))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s want 413 for a 16MB+ responses body", rr.Code, rr.Body.String())
	}
}

func TestWave2ResponsesToolChoice(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-q","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"tools":[{"type":"function","name":"get_weather","description":"d","parameters":{}}],"tool_choice":"auto","max_output_tokens":64}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200 for a tool_choice request", rr.Code, rr.Body.String())
	}
}

func TestWave2ResponsesTooComplex(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	var sb strings.Builder
	sb.WriteString(`{"model":"m","input":[`)
	for i := 0; i < 100001; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}`)
	}
	sb.WriteString(`]}`)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(sb.String()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "too complex") {
		t.Fatalf("status=%d body=%.200s want 400 too complex", rr.Code, rr.Body.String())
	}
}

func TestWave2ResponsesDisabledVirtualEndpoint(t *testing.T) {
	s := wave2AdminServer(t)
	if rr := wave2Call(s, "POST", "/admin/api/virtual-endpoints", `{"id":"ve-off","public_model":"ve-model","route_profile":"profile1","enabled":false}`); rr.Code != 201 {
		t.Fatalf("disabled VE create: %d %s", rr.Code, rr.Body.String())
	}
	body := `{"model":"ve-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"max_output_tokens":64}`
	if rr := wave2Call(s, "POST", "/v1/responses", body); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatalf("disabled VE responses: %d %s want 404 disabled", rr.Code, rr.Body.String())
	}
}
