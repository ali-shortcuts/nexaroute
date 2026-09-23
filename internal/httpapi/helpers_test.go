package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
