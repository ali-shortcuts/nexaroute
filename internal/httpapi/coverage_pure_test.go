package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCoverageRoutingHelpersAndPolicies(t *testing.T) {
	ctxA, cancelA := routeContext(context.Background(), false, 0)
	ctxB, cancelB := routeContext(context.Background(), true, time.Second)
	defer cancelA()
	defer cancelB()
	if ctxA == nil || ctxB == nil {
		t.Fatal("route context was not created")
	}
	if gatewayDeadlineError(context.Background(), context.Background(), errors.New("timeout")) {
		t.Fatal("unbounded context reported gateway deadline")
	}
	if jitteredRetryBackoff(0, 1, "id") != 0 || jitteredRetryBackoff(time.Second, -1, "id") > time.Second {
		t.Fatal("retry backoff bounds violated")
	}
	for _, tc := range []struct {
		status int
		kind   string
		retry  bool
	}{
		{http.StatusBadRequest, "caller_invalid_request", false},
		{http.StatusUnauthorized, "provider_auth_failed", true},
		{http.StatusPaymentRequired, "provider_billing", true},
		{http.StatusNotFound, "provider_request_rejected", true},
		{http.StatusTooManyRequests, "provider_rate_limited", true},
		{http.StatusServiceUnavailable, "provider_overloaded", true},
		{http.StatusInternalServerError, "provider_server_error", true},
		{http.StatusOK, "_OTHER", false},
	} {
		if got := errorTypeForStatus(tc.status); got != tc.kind || retryable(tc.status) != tc.retry {
			t.Fatalf("status=%d kind=%q retry=%v", tc.status, got, retryable(tc.status))
		}
	}
	for _, raw := range []string{"", "5", "999999", "not-a-delay"} {
		h := http.Header{}
		if raw != "" {
			h.Set("Retry-After", raw)
		}
		if got := retryAfterDuration(h, 10*time.Second); got < 0 || got > 10*time.Second {
			t.Fatalf("Retry-After %q out of bounds: %v", raw, got)
		}
	}
	if got := retryAfterResponseValue(http.Header{"Retry-After": []string{"999"}}, 3*time.Second); got != "3" {
		t.Fatalf("bounded Retry-After=%q", got)
	}
}

func TestCoverageRequestInspectionAndHeaderSafety(t *testing.T) {
	patched, err := patchJSONModel([]byte(`{"model":"old","messages":[]}`), "new")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if json.Unmarshal(patched, &obj) != nil || obj["model"] != "new" {
		t.Fatalf("patched request=%s", patched)
	}
	if _, err := patchJSONModel([]byte("{"), "new"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if got := bodySessionKey(map[string]any{"metadata": map[string]any{"user_id": map[string]any{"session_id": "nested"}}}); got != "nested" {
		t.Fatalf("nested session key=%q", got)
	}
	if got := boundedSessionValue(strings.Repeat("x", 300)); len(got) != 256 {
		t.Fatalf("session value length=%d", len(got))
	}
	inspection := inspectRequestJSON([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}],"reasoning_effort":"high"}`), "image_url", []string{"reasoning_effort"})
	if !inspection.Vision || !inspection.Reasoning || inspection.EstimatedPromptTokens == 0 {
		t.Fatalf("inspection=%+v", inspection)
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "secret")
	r.Header.Set("X-Trace", "trace")
	copied := copySelectedRequestHeaders(r)
	if copied.Get("Authorization") != "" || copied.Get("X-Trace") != "trace" {
		t.Fatalf("copied headers=%v", copied)
	}
}

func TestCoverageStreamWriterAndCanonicalErrors(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := &streamCommitWriter{ResponseWriter: recorder}
	if _, err := w.Write([]byte("ok")); err != nil || !w.committed {
		t.Fatalf("stream writer committed=%v err=%v", w.committed, err)
	}
	for _, protocol := range []string{"anthropic", "openai_responses", "openai"} {
		r := httptest.NewRecorder()
		canonicalErrorJSON(r, protocol, http.StatusBadRequest, "invalid_request", "bad")
		if r.Code != http.StatusBadRequest || !bytes.Contains(r.Body.Bytes(), []byte("bad")) {
			t.Fatalf("protocol=%s response=%q", protocol, r.Body.Bytes())
		}
	}
}
