package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMission95SSEReader(t *testing.T) {
	readAll := func(input string) ([]sseEvent, bool, error) {
		r := newSSEReader(strings.NewReader(input))
		var out []sseEvent
		for {
			ev, done, err := r.Next()
			if err != nil {
				return out, false, err
			}
			if done {
				return out, true, nil
			}
			out = append(out, ev)
		}
	}

	// Multi-line data joins with \n; comments and unknown fields ignored;
	// CRLF tolerated; blank-line framing preserved.
	events, done, err := readAll(": comment\r\nevent: message\r\nid: 42\r\ndata: line1\r\ndata: line2\r\nretry: 3000\r\n\r\ndata: second\r\n")
	if err != nil || !done || len(events) != 2 {
		t.Fatalf("events = %v, done=%v, err=%v", events, done, err)
	}
	if events[0].name != "message" || events[0].id != "42" || events[0].data != "line1\nline2" {
		t.Fatalf("first event = %+v", events[0])
	}
	if events[1].data != "second" || events[1].name != "" {
		t.Fatalf("second event = %+v", events[1])
	}
	// Leading blank lines are skipped, not emitted as empty events.
	events, done, err = readAll("\n\n\ndata: only\n\n")
	if err != nil || !done || len(events) != 1 || events[0].data != "only" {
		t.Fatalf("blank-prefix events = %v, done=%v, err=%v", events, done, err)
	}
	// A final frame without a trailing blank line is still delivered.
	events, done, err = readAll("data: tail")
	if err != nil || !done || len(events) != 1 || events[0].data != "tail" {
		t.Fatalf("unterminated frame = %v, done=%v, err=%v", events, done, err)
	}
	// A field without a colon is a field with an empty value.
	events, _, err = readAll("data\n\n")
	if err != nil || len(events) != 1 || events[0].data != "" {
		t.Fatalf("colon-less field = %v, %v", events, err)
	}
	// Clean EOF with no pending frame reports done with no event.
	events, done, err = readAll("")
	if err != nil || !done || len(events) != 0 {
		t.Fatalf("empty stream = %v, done=%v, err=%v", events, done, err)
	}
	// Scanner errors surface instead of being swallowed as EOF.
	events, _, err = readAll("")
	_ = events
	broken := newSSEReader(&errReader{err: errors.New("stream broken")})
	if _, _, err := broken.Next(); err == nil {
		t.Error("scanner error swallowed")
	}
	// Oversize event field is rejected rather than buffered unboundedly.
	huge := "event: " + strings.Repeat("x", maxSSEEventBytes+1) + "\n\n"
	if _, _, err := newSSEReader(strings.NewReader(huge)).Next(); err == nil {
		t.Error("oversize event field accepted")
	}
	hugeData := "data: " + strings.Repeat("y", maxSSEEventBytes+1) + "\n\n"
	if _, _, err := newSSEReader(strings.NewReader(hugeData)).Next(); err == nil {
		t.Error("oversize data field accepted")
	}
}

type errReader struct{ err error }

func (e *errReader) Read(p []byte) (int, error) { return 0, e.err }

func TestMission95ReadJSON(t *testing.T) {
	// Admin paths enforce application/json.
	var dst map[string]any
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "text/plain")
	if _, err := readJSON(req, &dst); err == nil || !strings.Contains(err.Error(), "Content-Type") {
		t.Errorf("non-JSON admin body accepted: %v", err)
	}
	// Content-Type with parameters is accepted.
	req = httptest.NewRequest(http.MethodPost, "/admin/api/providers", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if _, err := readJSON(req, &dst); err != nil || dst["a"] != float64(1) {
		t.Fatalf("parameterized JSON rejected: %v, %v", dst, err)
	}
	// Non-admin paths do not enforce content type.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"a":2}`))
	if _, err := readJSON(req, &dst); err != nil || dst["a"] != float64(2) {
		t.Fatalf("data-plane JSON rejected: %v, %v", dst, err)
	}
	// Invalid JSON is an error and returns the raw bytes.
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{bad`))
	raw, err := readJSON(req, &dst)
	if err == nil || string(raw) != "{bad" {
		t.Errorf("invalid JSON: raw=%q err=%v", raw, err)
	}
	// A body read error propagates.
	req = httptest.NewRequest(http.MethodPost, "/", &errReader{err: errors.New("read fail")})
	if _, err := readJSON(req, &dst); err == nil {
		t.Error("body read error swallowed")
	}
	// Oversize admin body is rejected with the typed error.
	huge := strings.Repeat(" ", maxJSONBodyBytes+1)
	req = httptest.NewRequest(http.MethodPost, "/admin/api/x", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	_, err = readJSON(req, &dst)
	var tooLarge *requestTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.limit != maxJSONBodyBytes {
		t.Fatalf("oversize error = %v", err)
	}
	if tooLarge.Error() == "" {
		t.Error("oversize error has empty message")
	}
}

func TestMission95JSONHelpers(t *testing.T) {
	// writeJSON sets JSON content type and status; errorJSON envelope shape.
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]any{"ok": true})
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("writeJSON code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	errorJSON(rec, http.StatusBadRequest, "boom")
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["type"] != "gateway_error" || errObj["message"] != "boom" {
		t.Fatalf("error envelope = %v", env)
	}
	// readJSONLimited passes small bodies through unchanged.
	if got, err := readJSONLimited(strings.NewReader(`{"x":1}`)); err != nil || string(got) != `{"x":1}` {
		t.Fatalf("readJSONLimited = %q, %v", got, err)
	}
	if _, err := readJSONLimited(&errReader{err: errors.New("boom")}); err == nil {
		t.Error("readJSONLimited error swallowed")
	}
	// decodeValidatedJSONLimited: validator failure, bad JSON, success.
	var out map[string]any
	if err := decodeValidatedJSONLimited(strings.NewReader(`{"x":1}`), &out, func(b []byte) error { return errors.New("nope") }); err == nil {
		t.Error("validator rejection swallowed")
	}
	if err := decodeValidatedJSONLimited(strings.NewReader(`{`), &out, nil); err == nil {
		t.Error("invalid JSON accepted by decoder")
	}
	if err := decodeValidatedJSONLimited(strings.NewReader(`{"x":3}`), &out, nil); err != nil || out["x"] != float64(3) {
		t.Fatalf("decoder = %v, %v", out, err)
	}
	// Vision/reasoning detectors.
	if !hasVisionAnth([]byte(`{"messages":[{"content":[{"type":"image"}]}]}`)) {
		t.Error("anthropic vision missed")
	}
	if hasVisionAnth([]byte(`{"messages":[{"content":[{"type":"text"}]}]}`)) {
		t.Error("anthropic vision false positive")
	}
	if !hasVisionOpenAI([]byte(`{"messages":[{"content":[{"type":"image_url"}]}]}`)) {
		t.Error("openai vision missed")
	}
	if !hasReasoningAnth([]byte(`{"thinking":{"type":"enabled"}}`)) {
		t.Error("anthropic reasoning missed")
	}
	if !hasReasoningOpenAI([]byte(`{"reasoning_effort":"high"}`)) {
		t.Error("openai reasoning missed")
	}
	if hasReasoningOpenAI([]byte(`{"model":"x"}`)) {
		t.Error("openai reasoning false positive")
	}
}

func TestMission95ResponseValidators(t *testing.T) {
	openAIGood := `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	if err := validateOpenAIResponseJSON([]byte(openAIGood)); err != nil {
		t.Errorf("valid openai response rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid json", `{`},
		{"error envelope", `{"error":{"message":"x"},"choices":[{"message":{}}]}`},
		{"null error is not an error envelope but choices missing", `{"error":null}`},
		{"choices missing", `{}`},
		{"choices not array", `{"choices":{}}`},
		{"empty choices", `{"choices":[]}`},
		{"message missing", `{"choices":[{}]}`},
		{"message not object", `{"choices":[{"message":"hi"}]}`},
		{"empty message", `{"choices":[{"message":{}}]}`},
	} {
		if err := validateOpenAIResponseJSON([]byte(tc.body)); err == nil {
			t.Errorf("openai %s: accepted", tc.name)
		}
	}
	anthGood := `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`
	if err := validateAnthropicResponseJSON([]byte(anthGood)); err != nil {
		t.Errorf("valid anthropic response rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid json", `{`},
		{"error envelope", `{"error":{"type":"x"},"type":"message","role":"assistant","content":[]}`},
		{"wrong type", `{"type":"error","role":"assistant","content":[]}`},
		{"wrong role", `{"type":"message","role":"user","content":[]}`},
		{"content missing", `{"type":"message","role":"assistant"}`},
		{"content not array", `{"type":"message","role":"assistant","content":{}}`},
	} {
		if err := validateAnthropicResponseJSON([]byte(tc.body)); err == nil {
			t.Errorf("anthropic %s: accepted", tc.name)
		}
	}
	// Null error envelope is tolerated when the rest of the envelope is valid.
	if err := validateAnthropicResponseJSON([]byte(`{"error":null,"type":"message","role":"assistant","content":[]}`)); err != nil {
		t.Errorf("null error envelope rejected: %v", err)
	}
}

func TestMission95NormalizeRequestIDAndStreamID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  abc-123_X.Y:Z  ", "abc-123_X.Y:Z"},
		{"a b/c", "a_b_c"},
		{"___abc___", "abc"},
		{strings.Repeat("a", 200), strings.Repeat("a", 128)},
	}
	for _, tc := range cases {
		if got := normalizeRequestID(tc.in); got != tc.want {
			t.Errorf("normalizeRequestID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := uniqueStreamID("req", "abc-123"); got != "req_abc-123" {
		t.Errorf("stream id with request id = %q", got)
	}
	if got := uniqueStreamID("req", "a b!"); got != "req_ab" {
		t.Errorf("stream id sanitizes = %q", got)
	}
	if got := uniqueStreamID("req"); !strings.HasPrefix(got, "req_") || len(got) <= 4 {
		t.Errorf("stream id without request id = %q", got)
	}
	if got := uniqueStreamID("req", "!!!"); !strings.HasPrefix(got, "req_") || got == "req_" {
		t.Errorf("stream id with unusable request id = %q", got)
	}
	if uniqueStreamID("req", "same") != uniqueStreamID("req", "same") {
		t.Error("stream id not deterministic for a given request id")
	}
	// anthropicErrorJSON maps statuses to error types.
	typeCases := map[int]string{
		http.StatusBadRequest:            "invalid_request_error",
		http.StatusMethodNotAllowed:      "invalid_request_error",
		http.StatusUnprocessableEntity:   "invalid_request_error",
		http.StatusUnauthorized:          "authentication_error",
		http.StatusForbidden:             "permission_error",
		http.StatusNotFound:              "not_found_error",
		http.StatusRequestEntityTooLarge: "request_too_large",
		http.StatusTooManyRequests:       "rate_limit_error",
		http.StatusInternalServerError:   "api_error",
	}
	for code, want := range typeCases {
		rec := httptest.NewRecorder()
		anthropicErrorJSON(rec, code, "msg")
		var env map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		errObj, _ := env["error"].(map[string]any)
		if rec.Code != code || errObj["type"] != want || errObj["message"] != "msg" {
			t.Errorf("code %d: body=%s", code, rec.Body.String())
		}
	}
	// bytes import guard: Write path used by stream tests elsewhere.
	_ = bytes.MinRead
	_ = io.Discard
}
