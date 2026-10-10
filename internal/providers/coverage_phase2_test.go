package providers

import (
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestCoveragePhase2AdapterHelpers(t *testing.T) {
	p := config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: "https://api.example/v1", ChatPath: "/v1/chat/completions", APIKey: "secret", AuthMode: "bearer", Enabled: true}
	a, err := newHTTPAdapter(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != "p" || a.Kind() != "openai_compatible" {
		t.Fatal("identity helpers failed")
	}
	if !a.CredentialsMatch([]string{"secret"}) || a.CredentialsMatch([]string{"other"}) {
		t.Fatal("credential match failed")
	}
	if sameHTTPOrigin(nil, nil) || sameHTTPOrigin(mustURL("https://a"), mustURL("https://b")) || !sameHTTPOrigin(mustURL("HTTPS://a"), mustURL("https://a")) {
		t.Fatal("origin comparison failed")
	}
	for _, tc := range []struct{ base, suffix, want string }{
		{"https://a/v1", "chat", "https://a/v1/chat"},
		{"https://a/v1/chat/completions", "/v1/models", "https://a/v1/models"},
		{"https://a", "https://b/models", "https://b/models"},
	} {
		if got := endpoint(tc.base, tc.suffix); got != tc.want {
			t.Fatalf("endpoint(%q,%q)=%q", tc.base, tc.suffix, got)
		}
	}
	if got := (&httpAdapter{p: config.ProviderConfig{Type: "openai_responses", ResponsesPath: "/responses"}}).defaultPath(); got != "/responses" {
		t.Fatal(got)
	}
	if got := (&httpAdapter{p: config.ProviderConfig{Type: "anthropic_compatible", MessagesPath: "/messages"}}).defaultPath(); got != "/messages" {
		t.Fatal(got)
	}
	if got := a.geminiModelPath("models/gemini 1.5", true); !strings.Contains(got, "streamGenerateContent") {
		t.Fatal(got)
	}
	redacted := string(a.RedactBody([]byte(`secret value`)))
	if strings.Contains(redacted, "secret") {
		t.Fatal(redacted)
	}
}

func TestCoveragePhase2RetryAndStreamingBodies(t *testing.T) {
	e := &RetryAfterError{Cause: errors.New("busy"), After: time.Second}
	if e.Error() != "busy" || !errors.Is(e, e.Cause) {
		t.Fatal("retry error wrapping failed")
	}
	if d, ok := RetryAfter(e); !ok || d != time.Second {
		t.Fatalf("retry=%s %v", d, ok)
	}
	if _, ok := RetryAfter(errors.New("other")); ok {
		t.Fatal("plain error classified as retry")
	}
	cancelled := false
	body := newIdleBody(io.NopCloser(strings.NewReader("ok")), time.Second, func() { cancelled = true })
	b, err := io.ReadAll(body)
	if err != nil || string(b) != "ok" {
		t.Fatalf("read=%q err=%v", b, err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("close did not cancel")
	}
	cancelOnClose := &cancelOnCloseBody{ReadCloser: io.NopCloser(strings.NewReader("x")), cancel: func() {}}
	if err := cancelOnClose.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
