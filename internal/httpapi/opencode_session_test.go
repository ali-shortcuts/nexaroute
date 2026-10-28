package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func opencodeTestRequest(t *testing.T, headers map[string]string, id clientIdentity, withIdentity bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://localhost/v1/chat/completions", nil)
	for k, v := range headers {
		r.Header[k] = []string{v}
	}
	if withIdentity {
		r = r.WithContext(context.WithValue(r.Context(), clientIdentityContextKey{}, id))
	}
	return r
}

func TestResolveOpencodeSessionValue(t *testing.T) {
	vk := clientIdentity{ID: "vk-42", Virtual: true}
	// Client header wins over every session source, and is prefixed.
	r := opencodeTestRequest(t, map[string]string{
		"X-Opencode-Session": "client-sess",
		"X-Session-Id":       "other-sess",
	}, vk, true)
	if got := resolveOpencodeSessionValue(r, "body-sess"); got != "vk-42:client-sess" {
		t.Fatalf("client header: got %q, want vk-42:client-sess", got)
	}
	// Invalid client header (CR/LF) is ignored, falling through to the
	// existing session header, not rejected and not sent.
	r = opencodeTestRequest(t, map[string]string{
		"X-Session-Id": "sess-9",
	}, vk, true)
	r.Header["X-Opencode-Session"] = []string{"bad\r\ninjected"}
	if got := resolveOpencodeSessionValue(r, ""); got != "vk-42:sess-9" {
		t.Fatalf("invalid client header fallback: got %q, want vk-42:sess-9", got)
	}
	// Body session_id is the next fallback after headers.
	r = opencodeTestRequest(t, nil, vk, true)
	if got := resolveOpencodeSessionValue(r, "body-sess"); got != "vk-42:body-sess" {
		t.Fatalf("body fallback: got %q, want vk-42:body-sess", got)
	}
	// No session anywhere: per-request UUID, still prefixed.
	r = opencodeTestRequest(t, nil, vk, true)
	got := resolveOpencodeSessionValue(r, "")
	if !regexp.MustCompile(`^vk-42:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(got) {
		t.Fatalf("UUID fallback: got %q, want vk-42:<uuid-v4>", got)
	}
	if got2 := resolveOpencodeSessionValue(r, ""); got2 == got {
		t.Fatalf("UUID fallback must differ per call: %q twice", got)
	}
	// Legacy (non-virtual) identity gets no prefix, so all legacy
	// callers are not collapsed into one namespace.
	r = opencodeTestRequest(t, map[string]string{"X-Session-Id": "sess-1"},
		clientIdentity{ID: "legacy", Virtual: false}, true)
	if got := resolveOpencodeSessionValue(r, ""); got != "sess-1" {
		t.Fatalf("legacy identity: got %q, want sess-1", got)
	}
	// No identity at all: no prefix, no secret material.
	r = opencodeTestRequest(t, map[string]string{"X-Session-Id": "sess-1"}, clientIdentity{}, false)
	if got := resolveOpencodeSessionValue(r, ""); got != "sess-1" {
		t.Fatalf("anonymous: got %q, want sess-1", got)
	}
	// An over-long client header is invalid and falls through.
	r = opencodeTestRequest(t, map[string]string{"X-Session-Id": "sess-1"}, vk, true)
	r.Header["X-Opencode-Session"] = []string{strings.Repeat("x", 300)}
	if got := resolveOpencodeSessionValue(r, ""); got != "vk-42:sess-1" {
		t.Fatalf("over-long client header fallback: got %q, want vk-42:sess-1", got)
	}
}

func TestValidVirtualKeyID(t *testing.T) {
	if !validVirtualKeyID("vk-42") || !validVirtualKeyID("a.b_c-1") {
		t.Fatal("ordinary virtual-key IDs rejected")
	}
	for _, bad := range []string{"", "vk:1", "vk 1", "vk/1", strings.Repeat("x", 129)} {
		if validVirtualKeyID(bad) {
			t.Fatalf("bad virtual-key ID accepted: %q", bad)
		}
	}
}
