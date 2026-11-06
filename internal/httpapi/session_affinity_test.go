package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionKeyFromRequestPartsIgnoresPerRequestTraceID(t *testing.T) {
	t.Run("trace ID alone is not a session key", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("x-litellm-trace-id", "trace-per-request")
		if got := sessionKeyFromRequestParts(r, ""); got != "" {
			t.Fatalf("session key = %q, want empty when only a trace ID is present", got)
		}
	})

	tests := []struct {
		name   string
		header string
		value  string
		body   string
		want   string
	}{
		{name: "Claude Code session header", header: "x-claude-code-session-id", value: "claude-session", want: "claude-session"},
		{name: "LiteLLM session header", header: "x-litellm-session-id", value: "litellm-session", want: "litellm-session"},
		{name: "generic session header", header: "x-session-id", value: "generic-session", want: "generic-session"},
		{name: "body session fallback", body: "body-session", want: "body-session"},
		{name: "session header takes precedence over trace", header: "x-litellm-session-id", value: "stable-session", body: "body-session", want: "stable-session"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			r.Header.Set("x-litellm-trace-id", "trace-per-request")
			if got := sessionKeyFromRequestParts(r, tc.body); got != tc.want {
				t.Fatalf("session key = %q, want %q", got, tc.want)
			}
		})
	}
}
