package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

func TestCapabilityDetectionIgnoresWordsInsideUserText(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"Explain the image format and the reasoning behind it"}]}`)
	if hasVisionOpenAI(raw) || hasVisionAnth(raw) || hasReasoningOpenAI(raw) || hasReasoningAnth(raw) {
		t.Fatal("plain user text must not imply vision or reasoning capability")
	}
}

func TestCapabilityDetectionFindsNestedContent(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}],"reasoning_effort":"high"}`)
	if !hasVisionOpenAI(raw) || !hasReasoningOpenAI(raw) {
		t.Fatal("nested image and reasoning fields should be detected")
	}
}

func TestReadJSONRejectsOversizedBody(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(strings.Repeat("x", maxJSONBodyBytes+1))))
	var dst map[string]any
	_, err := readJSON(req, &dst)
	if _, ok := err.(*requestTooLargeError); !ok {
		t.Fatalf("expected requestTooLargeError, got %T: %v", err, err)
	}
}

func TestSanitizeMetricLabelRemovesLineBreaksAndEscapes(t *testing.T) {
	got := sanitizeMetricLabel("provider\nname\rwith\\quote\"")
	if strings.ContainsAny(got, "\n\r\\\"") {
		t.Fatalf("metric label still contains unsafe characters: %q", got)
	}
}

func TestStatusWriterKeepsFirstCommittedStatus(t *testing.T) {
	rr := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rr, status: http.StatusOK}
	sw.WriteHeader(http.StatusCreated)
	sw.WriteHeader(http.StatusInternalServerError)
	if sw.status != http.StatusCreated || rr.Code != http.StatusCreated {
		t.Fatalf("status writer drifted after second WriteHeader: status=%d recorder=%d", sw.status, rr.Code)
	}
}

func TestStatusWriterFlushCommitsOK(t *testing.T) {
	rr := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rr, status: http.StatusOK}
	sw.Flush()
	sw.WriteHeader(http.StatusInternalServerError)
	if sw.status != http.StatusOK || rr.Code != http.StatusOK {
		t.Fatalf("flush did not lock implicit 200: status=%d recorder=%d", sw.status, rr.Code)
	}
}

func TestErrorTypeForStatus(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:       "provider_auth_failed",
		http.StatusPaymentRequired:    "provider_billing",
		http.StatusTooManyRequests:    "provider_rate_limited",
		http.StatusServiceUnavailable: "provider_overloaded",
		http.StatusGatewayTimeout:     "provider_timeout",
		http.StatusBadRequest:         "caller_invalid_request",
	}
	for code, want := range cases {
		if got := errorTypeForStatus(code); got != want {
			t.Fatalf("status %d type=%q want=%q", code, got, want)
		}
	}
}

func TestJitteredRetryBackoffCapsHugeDurationsWithoutOverflow(t *testing.T) {
	got := jitteredRetryBackoff(time.Duration(1<<62), 6, "req")
	if got < 0 || got > 5*time.Second {
		t.Fatalf("backoff=%s outside safe cap", got)
	}
}

func TestSessionKeyPrefersClaudeCodeHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Claude-Code-Session-Id", "claude-session")
	raw := []byte(`{"metadata":{"session_id":"body-session"}}`)
	if got := sessionKeyFromRequest(req, raw); got != "claude-session" {
		t.Fatalf("session=%q want claude-session", got)
	}
}

func TestSessionKeyReadsNestedMetadata(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	raw := []byte(`{"metadata":{"user_id":{"session_id":"nested-session"}}}`)
	if got := sessionKeyFromRequest(req, raw); got != "nested-session" {
		t.Fatalf("session=%q want nested-session", got)
	}
}

func TestRequestInspectionFindsCapabilitiesAndSessionInOnePass(t *testing.T) {
	raw := []byte(`{"metadata":{"user_id":{"session_id":"s-1"}},"messages":[{"content":[{"type":"image_url","image_url":{"url":"x"}}]}],"reasoning_effort":"high"}`)
	got := inspectRequestJSON(raw, "image_url", []string{"reasoning_effort", "reasoning"})
	if !got.Vision || !got.Reasoning || got.BodySessionKey != "s-1" || got.TooComplex {
		t.Fatalf("unexpected inspection result: %+v", got)
	}
}

func TestRequestInspectionRejectsPathologicalNodeCount(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"messages":[`)
	for i := 0; i < maxRequestInspectionNodes+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteString(`]}`)
	got := inspectRequestJSON([]byte(b.String()), "image_url", nil)
	if !got.TooComplex {
		t.Fatal("pathological JSON structure should hit inspection node bound")
	}
}

func TestNormalizeRequestIDBoundsAndSanitizes(t *testing.T) {
	got := normalizeRequestID(strings.Repeat("abc/", 100))
	if len(got) > 128 {
		t.Fatalf("request id len=%d want <=128", len(got))
	}
	if strings.ContainsAny(got, "/\r\n\t ") {
		t.Fatalf("request id contains unsafe characters: %q", got)
	}
}

type failingStreamWriter struct {
	header http.Header
	err    error
}

func (w *failingStreamWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *failingStreamWriter) WriteHeader(int) {}
func (w *failingStreamWriter) Write([]byte) (int, error) {
	if w.err == nil {
		w.err = errors.New("client write failed")
	}
	return 0, w.err
}

func TestTranslatedStreamsStopOnClientWriteFailure(t *testing.T) {
	t.Run("openai to anthropic", func(t *testing.T) {
		w := &failingStreamWriter{}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}
		if err := streamOpenAIToAnthropic(w, resp, "m", nil); err == nil || !strings.Contains(err.Error(), "client write failed") {
			t.Fatalf("unexpected stream error: %v", err)
		}
	})
	t.Run("anthropic to openai", func(t *testing.T) {
		w := &failingStreamWriter{}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"message_stop\"}\n\n")),
		}
		if err := streamAnthropicToOpenAI(w, resp, "m", nil); err == nil || !strings.Contains(err.Error(), "client write failed") {
			t.Fatalf("unexpected stream error: %v", err)
		}
	})
}

func TestDataPlaneAdmissionIsBoundedAndRecoverable(t *testing.T) {
	s := &Server{cfg: config.Config{Routing: config.RoutingConfig{MaxInflightRequests: 2}}}
	if !s.tryAcquireDataPlane() || !s.tryAcquireDataPlane() {
		t.Fatal("first two admissions should succeed")
	}
	if s.tryAcquireDataPlane() {
		t.Fatal("third admission should be rejected")
	}
	s.releaseDataPlane()
	if !s.tryAcquireDataPlane() {
		t.Fatal("capacity should recover after release")
	}
	s.releaseDataPlane()
	s.releaseDataPlane()
	if got := s.inflight.Load(); got != 0 {
		t.Fatalf("inflight=%d want 0", got)
	}
}

func TestDataPlaneAdmissionOnlyCoversExpensivePostEndpoints(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/v1/messages", true},
		{http.MethodPost, "/v1/messages/count_tokens", true},
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodPost, "/v1/responses", true},
		{http.MethodGet, "/v1/models", false},
		{http.MethodGet, "/healthz", false},
		{http.MethodPost, "/admin/api/probe", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if got := isDataPlaneRequest(req); got != tc.want {
			t.Fatalf("%s %s admission=%v want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestReadyMeshSettingsControlsAreWiredInEmbeddedUI(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	js := string(app)
	controls := []string{
		"rtSessionAffinity", "rtSessionTTL", "rtP2CWindow", "rtCapacityWeight",
		"rtAttempts", "rtMaxInflight", "rtFailureThreshold", "rtProviderFailureThreshold",
		"rtProviderFailureWindow", "rtProviderCooldown", "rtCapabilityThreshold",
		"rtCapabilityCooldown", "rtCooldown", "rtTimeout", "rtBackoff", "rtRetryAfter",
		"prEnabled", "prOnStart", "prInterval", "prReadyLease", "prTimeout",
		"prTokens", "prConcurrency", "prRecoveryAttempts", "prRecoveryRetry",
	}
	for _, id := range controls {
		if !strings.Contains(html, "id=\""+id+"\"") {
			t.Fatalf("control %s missing from embedded HTML", id)
		}
		if !strings.Contains(js, "#"+id) {
			t.Fatalf("control %s is present in HTML but not wired in app.js", id)
		}
	}
}

type readerFromResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
	used   bool
}

func (w *readerFromResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *readerFromResponseWriter) WriteHeader(code int) { w.status = code }
func (w *readerFromResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}
func (w *readerFromResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	w.used = true
	return io.Copy(&w.body, r)
}

func TestStatusWriterPreservesReaderFromFastPath(t *testing.T) {
	base := &readerFromResponseWriter{}
	sw := &statusWriter{ResponseWriter: base, status: http.StatusOK}
	n, err := sw.ReadFrom(strings.NewReader("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 || base.body.String() != "abcdef" {
		t.Fatalf("copied=%d body=%q", n, base.body.String())
	}
	if !base.used {
		t.Fatal("underlying ReaderFrom fast path was not used")
	}
	if sw.status != http.StatusOK || base.status != http.StatusOK {
		t.Fatalf("status not committed as 200: wrapper=%d base=%d", sw.status, base.status)
	}
}

func TestCapabilityDetectionIgnoresToolSchemaLookalikes(t *testing.T) {
	raw := []byte(`{
                "model":"m",
                "messages":[{"role":"user","content":"hello"}],
                "tools":[{"type":"function","function":{"name":"x","parameters":{
                        "type":"object",
                        "properties":{
                                "reasoning":{"type":"string"},
                                "example":{"type":"image_url"},
                                "anthropic_example":{"type":"image"}
                        }
                }}}]
        }`)
	if got := inspectRequestJSON(raw, "image_url", []string{"reasoning_effort", "reasoning"}); got.Vision || got.Reasoning {
		t.Fatalf("OpenAI tool schema lookalikes must not imply capabilities: %+v", got)
	}
	if got := inspectRequestJSON(raw, "image", []string{"thinking", "reasoning"}); got.Vision || got.Reasoning {
		t.Fatalf("Anthropic tool schema lookalikes must not imply capabilities: %+v", got)
	}
}

func TestCapabilityDetectionUsesTopLevelReasoningAndMessageVision(t *testing.T) {
	raw := []byte(`{
                "model":"m",
                "reasoning_effort":"high",
                "messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]}]
        }`)
	got := inspectRequestJSON(raw, "image_url", []string{"reasoning_effort", "reasoning"})
	if !got.Vision || !got.Reasoning || got.TooComplex {
		t.Fatalf("real protocol controls should be detected: %+v", got)
	}
}

func TestTranslatedStreamsRejectMalformedSSEJSON(t *testing.T) {
	t.Run("openai to anthropic", func(t *testing.T) {
		rr := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {bad}\n\n")),
		}
		err := streamOpenAIToAnthropic(rr, resp, "m", nil)
		if err == nil || !strings.Contains(err.Error(), "invalid OpenAI SSE JSON") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("anthropic to openai", func(t *testing.T) {
		rr := httptest.NewRecorder()
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {bad}\n\n")),
		}
		err := streamAnthropicToOpenAI(rr, resp, "m", nil)
		if err == nil || !strings.Contains(err.Error(), "invalid Anthropic SSE JSON") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestCloneConfigPreservesExplicitEmptyForwardHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: "https://example.com",
		ForwardHeaders: []string{}, Enabled: true,
	}}
	got := cloneConfig(cfg)
	if got.Providers[0].ForwardHeaders == nil {
		t.Fatal("cloneConfig collapsed explicit empty forward_headers to nil")
	}
	if len(got.Providers[0].ForwardHeaders) != 0 {
		t.Fatalf("cloneConfig changed explicit empty forward_headers: %#v", got.Providers[0].ForwardHeaders)
	}
}

func TestDiscoverModelsBoundsErrorBody(t *testing.T) {
	secret := "super-secret-key"
	huge := strings.Repeat("X", 10000) + secret
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, huge)
	}))
	defer srv.Close()

	p := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL,
		APIKey: secret, AuthMode: "bearer", ModelsPath: "/", MaxConcurrency: 1, Enabled: true,
	}
	_, _, err := discoverModels(context.Background(), p)
	if err == nil {
		t.Fatal("expected discovery failure")
	}
	msg := err.Error()
	if len(msg) > 2300 {
		t.Fatalf("discovery error was not bounded, len=%d", len(msg))
	}
	if strings.Contains(msg, secret) {
		t.Fatal("discovery error leaked provider credential")
	}
}

func TestSuccessfulResponseEnvelopeValidatorsRejectEmptyShapes(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"choices":[]}`),
		[]byte(`{"choices":[{"message":null}]}`),
		[]byte(`{"choices":[{"message":{}}]}`),
	} {
		if err := validateOpenAIResponseJSON(raw); err == nil {
			t.Fatalf("OpenAI validator accepted %s", raw)
		}
	}
	for _, raw := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"type":"message","role":"assistant","content":null}`),
		[]byte(`{"type":"message","role":"user","content":[]}`),
	} {
		if err := validateAnthropicResponseJSON(raw); err == nil {
			t.Fatalf("Anthropic validator accepted %s", raw)
		}
	}
	if err := validateOpenAIResponseJSON([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)); err != nil {
		t.Fatalf("valid OpenAI envelope rejected: %v", err)
	}
	if err := validateAnthropicResponseJSON([]byte(`{"type":"message","role":"assistant","content":[]}`)); err != nil {
		t.Fatalf("valid Anthropic envelope rejected: %v", err)
	}
}

func TestRetryAfterDurationIsBoundedAndOverflowSafe(t *testing.T) {
	max := 60 * time.Second
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"5", 5 * time.Second},
		{"3600", max},
		{"999999999999999999999999999", 30 * time.Second},
		{"", 30 * time.Second},
	}
	for _, tc := range cases {
		h := http.Header{"Retry-After": []string{tc.value}}
		if got := retryAfterDuration(h, max); got != tc.want {
			t.Fatalf("Retry-After %q => %s want %s", tc.value, got, tc.want)
		}
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)
	if got := retryAfterDuration(http.Header{"Retry-After": []string{future}}, max); got != max {
		t.Fatalf("future HTTP-date => %s want %s", got, max)
	}
}

func TestAccessLoggingIsSampledAndDoesNotTreatLongStreamsAsSlowRequests(t *testing.T) {
	cfg := config.Default()
	s := &Server{cfg: cfg}
	if s.shouldLogRequest(http.StatusOK, 100*time.Millisecond, false, 1) {
		t.Fatal("ordinary success should not be logged before its sample slot")
	}
	if !s.shouldLogRequest(http.StatusOK, 100*time.Millisecond, false, uint64(cfg.Logging.SuccessSampleEvery)) {
		t.Fatal("sample slot should be logged")
	}
	if !s.shouldLogRequest(http.StatusBadGateway, 10*time.Millisecond, false, 1) {
		t.Fatal("errors must always be access-logged")
	}
	if !s.shouldLogRequest(http.StatusOK, time.Duration(cfg.Logging.SlowRequestMS+1)*time.Millisecond, false, 1) {
		t.Fatal("slow non-streaming request should be logged")
	}
	if s.shouldLogRequest(http.StatusOK, time.Hour, true, 1) {
		t.Fatal("normal long-lived stream should not be treated as a slow request")
	}
	cfg.Logging.AccessMode = "off"
	s.cfg = cfg
	if s.shouldLogRequest(http.StatusInternalServerError, time.Second, false, 1) {
		t.Fatal("access_mode=off should disable request access lines")
	}
}

func TestAdminAPIResponsesAreNoStore(t *testing.T) {
	cfg := config.Default()
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q want no-store", got)
	}
	if got := rr.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma=%q want no-cache", got)
	}
}

func TestAdminReadJSONRejectsTextPlainBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/providers", strings.NewReader(`{"provider":{}}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "text/plain")
	var dst map[string]any
	if _, err := readJSON(req, &dst); err == nil || !strings.Contains(err.Error(), "application/json") {
		t.Fatalf("text/plain admin JSON should be rejected, got %v", err)
	}
}

func TestAdminReadJSONAcceptsJSONCharset(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/providers", strings.NewReader(`{"ok":true}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	var dst map[string]any
	if _, err := readJSON(req, &dst); err != nil {
		t.Fatalf("valid admin JSON content type rejected: %v", err)
	}
}

func TestAnthropicStopToOpenAIFinishMatrix(t *testing.T) {
	cases := map[string]string{
		"tool_use":      "tool_calls",
		"max_tokens":    "length",
		"refusal":       "content_filter",
		"pause_turn":    "stop",
		"end_turn":      "stop",
		"stop_sequence": "stop",
	}
	for k, want := range cases {
		if got := anthropicStopToOpenAIFinish(k); got != want {
			t.Fatalf("stop_reason %q: want %q got %q", k, want, got)
		}
	}
}

func TestStripStreamOptions(t *testing.T) {
	payload := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	stripped, ok := stripStreamOptions(payload)
	if !ok {
		t.Fatal("expected stream_options to be stripped")
	}
	if strings.Contains(string(stripped), "stream_options") {
		t.Fatalf("stream_options still present: %s", stripped)
	}
	if !strings.Contains(string(stripped), `"model":"m"`) {
		t.Fatalf("other fields lost: %s", stripped)
	}
	if _, ok := stripStreamOptions([]byte(`{"model":"m"}`)); ok {
		t.Fatal("no stream_options should report false")
	}
}

func parseSSEEvents(t *testing.T, body string) []struct {
	Name string
	Data string
} {
	t.Helper()
	var events []struct {
		Name string
		Data string
	}
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data += strings.TrimPrefix(line, "data: ")
			}
		}
		events = append(events, struct {
			Name string
			Data string
		}{name, data})
	}
	return events
}

func TestStreamOpenAIToAnthropicUsageAndContentVariants(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		``,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hel"},{"type":"text","text":"lo"}]}}]}`,
		``,
		`event: ping`,
		`data: {"keep":true}`,
		``,
		`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"gi","arguments":"{\"a\""}}]}}]}`,
		``,
		`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		``,
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`,
		``,
		`data: {"id":"1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstream))}
	err := streamOpenAIToAnthropic(rr, resp, "m", nil, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	events := parseSSEEvents(t, rr.Body.String())
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.Name)
	}
	for _, want := range []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %q event in %v", want, names)
		}
	}
	var md string
	for _, e := range events {
		if e.Name == "message_delta" {
			md = e.Data
		}
	}
	if !strings.Contains(md, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason wrong: %s", md)
	}
	if !strings.Contains(md, `"input_tokens":11`) || !strings.Contains(md, `"output_tokens":7`) || !strings.Contains(md, `"cache_read_input_tokens":4`) {
		t.Fatalf("usage not propagated into message_delta: %s", md)
	}
	// Content-part array deltas must surface as text deltas.
	joined := rr.Body.String()
	if !strings.Contains(joined, `hel`) || !strings.Contains(joined, `lo`) {
		t.Fatalf("array content dropped: %s", joined)
	}
	if !strings.Contains(joined, `"partial_json":"{\"a\"`) || !strings.Contains(joined, `"partial_json":":1}"`) {
		t.Fatalf("tool argument fragments not streamed: %s", joined)
	}
}

func TestStreamAnthropicToOpenAIRoleReasoningAndUsage(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":9,"cache_read_input_tokens":3,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"srv.tool","input":{}}}`,
		``,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"done"}}`,
		``,
		`data: {"type":"content_block_stop","index":2}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstream))}
	nm := translate.NewAnthropicNameMap([]string{"srv.tool"})
	err := streamAnthropicToOpenAI(rr, resp, "m", nm, "req-2")
	if err != nil {
		t.Fatal(err)
	}
	out := rr.Body.String()
	lines := strings.Split(strings.TrimSpace(out), "\n\n")
	// First chunk must carry the assistant role.
	first := lines[0]
	if !strings.HasPrefix(first, "data: ") || !strings.Contains(first, `"role":"assistant"`) {
		t.Fatalf("first chunk must carry assistant role: %q", first)
	}
	if !strings.Contains(out, `"reasoning_content":"pondering"`) {
		t.Fatalf("thinking delta not surfaced as reasoning_content: %s", out)
	}
	if !strings.Contains(out, `"name":"srv.tool"`) {
		t.Fatalf("tool name not reverse-mapped: %s", out)
	}
	// The tool block never streamed arguments; it must be padded to "{}".
	if !strings.Contains(out, `"arguments":"{}"`) {
		t.Fatalf("empty tool arguments not padded: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason wrong: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":9`) || !strings.Contains(out, `"completion_tokens":6`) {
		t.Fatalf("usage chunk missing: %s", out)
	}
	if !strings.Contains(out, `"cached_tokens":3`) {
		t.Fatalf("cache tokens not surfaced: %s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream must end with [DONE]: %q", out[len(out)-40:])
	}
}

func TestStreamAnthropicToOpenAIRefusalStopReason(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"no"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":1}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstream))}
	if err := streamAnthropicToOpenAI(rr, resp, "m", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), `"finish_reason":"content_filter"`) {
		t.Fatalf("refusal must map to content_filter: %s", rr.Body.String())
	}
}

func TestSSEReaderMultiLineDataAndComments(t *testing.T) {
	payload := "data: {\"a\":\n" +
		"data: 1}\n" +
		"\n" +
		": keep-alive comment\n" +
		"event: custom\n" +
		"data: second\n" +
		"\n" +
		"data: tail-no-blank"
	r := newSSEReader(strings.NewReader(payload))
	ev, done, err := r.Next()
	if err != nil || done {
		t.Fatalf("first event: %v %v", done, err)
	}
	if ev.data != "{\"a\":\n1}" {
		t.Fatalf("multi-line data not joined: %q", ev.data)
	}
	ev2, done2, err2 := r.Next()
	if err2 != nil || done2 {
		t.Fatalf("second event: %v %v", done2, err2)
	}
	if ev2.name != "custom" || ev2.data != "second" {
		t.Fatalf("event field lost: %+v", ev2)
	}
	ev3, done3, err3 := r.Next()
	if err3 != nil || done3 {
		t.Fatalf("trailing frame: %v %v", done3, err3)
	}
	if ev3.data != "tail-no-blank" {
		t.Fatalf("frame without trailing blank lost: %q", ev3.data)
	}
	if _, done4, _ := r.Next(); !done4 {
		t.Fatal("expected clean EOF")
	}
}

func TestAdminKeylessModeRejectsRebindHost(t *testing.T) {
	cfg := config.Default()
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.Host = "evil.example.com"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("rebound host status=%d want 401", rr.Code)
	}
	req.Host = "localhost:8080"
	req.RemoteAddr = "127.0.0.2:12346"
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("localhost host status=%d want 200", rr.Code)
	}
}

func TestAdminRateLimitBlocksBurstAfterAuthFailures(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "secret-key"
	srv := testGateway(t, cfg)
	var last int
	for i := 0; i < 120; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
		req.Host = "127.0.0.1"
		req.RemoteAddr = "10.1.1.1:5555"
		req.Header.Set("x-admin-key", "wrong")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		last = rr.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 after sustained auth failures", last)
	}
	// A different IP is unaffected.
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.2:5556"
	req.Header.Set("x-admin-key", "secret-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fresh IP status=%d want 200", rr.Code)
	}
}

func TestAdminProbeRequiresJSONContentType(t *testing.T) {
	cfg := config.Default()
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/probe", strings.NewReader("x"))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("text/plain probe status=%d want 400", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/probe", strings.NewReader(`{}`))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12347"
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("json probe status=%d want 202", rr.Code)
	}
}

func TestStatusWriterWriteMarksCommitted(t *testing.T) {
	rr := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rr, status: http.StatusOK}
	if responseCommitted(sw) {
		t.Fatal("fresh writer reported committed")
	}
	sw.Write([]byte("stream bytes"))
	if !responseCommitted(sw) {
		t.Fatal("implicit Write did not mark the response committed")
	}
	if sw.status != http.StatusOK {
		t.Fatalf("implicit commit status=%d want 200", sw.status)
	}
}

func TestStreamAnthropicToOpenAIDefersRoleChunkUntilUpstreamAlive(t *testing.T) {
	// Upstream answers 200 and dies before sending a single event. The
	// client response must remain uncommitted so the caller can fail over
	// to the next candidate.
	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: pr}
	go func() { pw.CloseWithError(io.EOF) }()
	rr := httptest.NewRecorder()
	err := streamAnthropicToOpenAI(rr, resp, "m", nil)
	if err == nil {
		t.Fatal("dead upstream stream must return an error")
	}
	if responseCommitted(rr) {
		t.Fatalf("response committed on dead upstream: status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestStreamAnthropicToOpenAIEmitsTerminalErrorChunkMidStream(t *testing.T) {
	// A mid-stream failure after content was delivered commits the response
	// and must surface an OpenAI-style error chunk instead of a silent cut.
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: broken\ndata: {not-json\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	rr := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rr, status: http.StatusOK}
	err := streamAnthropicToOpenAI(sw, resp, "m", nil)
	if err == nil {
		t.Fatal("malformed upstream event must return an error")
	}
	out := rr.Body.String()
	if !responseCommitted(sw) {
		t.Fatal("mid-stream failure should have committed via role/content chunks")
	}
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "gateway_stream_error") {
		t.Fatalf("terminal error chunk missing: %q", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatal("[DONE] must not follow a stream error")
	}
	// The role chunk must still precede content chunks.
	roleIdx := strings.Index(out, `"role":"assistant"`)
	if roleIdx < 0 || roleIdx > strings.Index(out, `"content":"hi"`) {
		t.Fatalf("role chunk ordering broken: %q", out)
	}
}

func TestStreamAnthropicToOpenAISuccessStillEmitsRoleFirst(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	rr := httptest.NewRecorder()
	if err := streamAnthropicToOpenAI(rr, resp, "m", nil); err != nil {
		t.Fatalf("healthy stream failed: %v", err)
	}
	out := rr.Body.String()
	if !strings.HasPrefix(out, "data: {") {
		t.Fatalf("first chunk must be a role chunk, got %q", out[:60])
	}
	if roleIdx := strings.Index(out, `"role":"assistant"`); roleIdx < 0 || roleIdx > strings.Index(out, `"content":"hello"`) {
		t.Fatalf("role chunk must precede content chunks: %q", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, `"content":"hello"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("happy path degraded: %q", out)
	}
}

func TestFailurePolicySeparatesModelAndProviderFailures(t *testing.T) {
	modelMissing := policyForStatus(http.StatusNotFound)
	if !modelMissing.Failover || !modelMissing.QuarantineDeployment || modelMissing.SignalProvider {
		t.Fatalf("404 policy should isolate deployment only: %+v", modelMissing)
	}
	overloaded := policyForStatus(http.StatusServiceUnavailable)
	if !overloaded.Failover || !overloaded.QuarantineDeployment || !overloaded.SignalProvider {
		t.Fatalf("503 policy should signal provider incident: %+v", overloaded)
	}
	conflict := policyForStatus(http.StatusConflict)
	if !conflict.Failover || conflict.QuarantineDeployment || conflict.SignalProvider {
		t.Fatalf("409 should fail over without poisoning health: %+v", conflict)
	}
}

func TestObserveFirstByteRecordsOnlyFirstRead(t *testing.T) {
	var calls int
	var observed time.Duration
	start := time.Now().Add(-50 * time.Millisecond)
	body := observeFirstByte(io.NopCloser(strings.NewReader("hello")), start, func(d time.Duration) {
		calls++
		observed = d
	})
	buf := make([]byte, 2)
	_, _ = body.Read(buf)
	_, _ = body.Read(buf)
	_ = body.Close()
	if calls != 1 {
		t.Fatalf("first-byte observer calls=%d want 1", calls)
	}
	if observed < 40*time.Millisecond {
		t.Fatalf("observed ttft=%s unexpectedly small", observed)
	}
}
func TestQuotaRemainingPressureStartsBelowQuarterBudget(t *testing.T) {
	cases := []struct {
		remaining, limit int64
		wantMin, wantMax float64
	}{
		{100, 100, 0, 0},
		{25, 100, 0, 0},
		{20, 100, 0.79, 0.81},
		{10, 100, 2.39, 2.41},
		{0, 100, 4, 4},
		{-1, 100, 0, 0},
		{10, 0, 0, 0},
	}
	for _, tc := range cases {
		got := quotaRemainingPressure(tc.remaining, tc.limit)
		if got < tc.wantMin || got > tc.wantMax {
			t.Fatalf("pressure(%d/%d)=%f want [%f,%f]", tc.remaining, tc.limit, got, tc.wantMin, tc.wantMax)
		}
	}
}

func TestEmbeddedUIExposesCostAwareStrategy(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `value="cost_aware"`) {
		t.Fatal("cost-aware strategy missing from embedded dashboard")
	}
}

func TestProviderLoadUsesEffectiveReservedQuotaHeadroom(t *testing.T) {
	now := time.Now().Unix()
	st := providers.ProviderStats{
		MaxConcurrency:             32,
		ActiveRequests:             2,
		WaitingRequests:            1,
		RequestLimit:               100,
		RemainingRequests:          20,
		ReservedRequests:           10,
		EffectiveRemainingRequests: 10,
		RequestResetUnix:           now + 60,
		TokenLimit:                 1000,
		RemainingTokens:            800,
		EffectiveRemainingTokens:   800,
		TokenResetUnix:             now + 60,
	}
	load := providerLoadFromStats(st, now)
	// Raw request headroom 20/100 would yield 0.8 pressure. Effective
	// headroom 10/100 must yield 2.4 and therefore dominate capacity pressure.
	if load.QuotaPressure < 2.39 || load.QuotaPressure > 2.41 {
		t.Fatalf("quota pressure ignored local reservations: %+v", load)
	}
	if load.QuotaExhausted {
		t.Fatalf("10%% effective headroom should be pressured, not exhausted: %+v", load)
	}

	st.EffectiveRemainingRequests = 0
	load = providerLoadFromStats(st, now)
	if !load.QuotaExhausted || load.QuotaPressure != 4 {
		t.Fatalf("effective zero headroom must surface as exhausted pressure: %+v", load)
	}
}

func TestQuotaReservationMetricsAndUIWiring(t *testing.T) {
	srv := testGateway(t, config.Default())
	req := httptest.NewRequest(http.MethodGet, "http://gateway/metrics", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, metric := range []string{
		"nexaroute_provider_reserved_requests",
		"nexaroute_provider_effective_remaining_requests",
		"nexaroute_provider_reserved_tokens",
		"nexaroute_provider_effective_remaining_tokens",
	} {
		if !strings.Contains(rr.Body.String(), metric) {
			t.Fatalf("metrics missing %s", metric)
		}
	}

	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, field := range []string{
		"effective_remaining_requests",
		"reserved_requests",
		"effective_remaining_tokens",
		"reserved_tokens",
	} {
		if !strings.Contains(js, field) {
			t.Fatalf("dashboard is not wired to quota field %s", field)
		}
	}
}

func TestResponsesInspectionUsesInputAndInstructionsOnly(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-x",
		"instructions":"system guidance",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"describe this"},
			{"type":"input_image","image_url":"https://example.invalid/image.png"}
		]}],
		"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"fake":{"type":"input_image"}}}}]
	}`)
	got := inspectResponsesRequestJSON(raw)
	if !got.Vision {
		t.Fatal("Responses input_image was not detected")
	}
	if got.EstimatedPromptTokens <= 16 {
		t.Fatalf("Responses input/instructions were not included in token estimate: %+v", got)
	}

	noImageInput := []byte(`{
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"plain"}]}],
		"tools":[{"parameters":{"example":{"type":"input_image"}}}]
	}`)
	got = inspectResponsesRequestJSON(noImageInput)
	if got.Vision {
		t.Fatal("tool schema input_image falsely triggered Responses vision")
	}
}

func TestResponsesOverloadUsesResponsesErrorEnvelope(t *testing.T) {
	s := &Server{
		cfg: config.Config{Routing: config.RoutingConfig{MaxInflightRequests: 1}},
		bus: events.New(16),
	}
	if !s.tryAcquireDataPlane() {
		t.Fatal("failed to occupy the only admission slot")
	}
	defer s.releaseDataPlane()

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(`{"model":"m","input":"x"}`))
	rr := httptest.NewRecorder()
	s.rejectOverloaded(rr, req, "req-overload")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q want 1", rr.Header().Get("Retry-After"))
	}
	if !strings.Contains(rr.Body.String(), `"type":"server_error"`) || !strings.Contains(rr.Body.String(), `"code":"server_error"`) {
		t.Fatalf("Responses overload envelope is not protocol-appropriate: %s", rr.Body.String())
	}
}
