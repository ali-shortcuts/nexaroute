package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestValidOpencodeSessionValue(t *testing.T) {
	valid := []string{"sess-1", "vk-1:abc.def_2", "ses_0123456789abcdef"}
	for _, v := range valid {
		if !ValidOpencodeSessionValue(v) {
			t.Fatalf("valid value rejected: %q", v)
		}
	}
	invalid := []string{"", "a\rb", "a\nb", "a\tb", "a\x7fb", strings.Repeat("x", 513)}
	for _, v := range invalid {
		if ValidOpencodeSessionValue(v) {
			t.Fatalf("invalid value accepted: %q", v)
		}
	}
}

func TestOpencodeSessionAdapterGating(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{}}`)
	}))
	t.Cleanup(srv.Close)

	do := func(p config.ProviderConfig, ctx context.Context, method, path string, forward http.Header) http.Header {
		t.Helper()
		seen = nil
		p.BaseURL = srv.URL + "/v1"
		a, err := newHTTPAdapter(p, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := a.DoPath(ctx, method, path, []byte(`{"model":"x"}`), false, forward)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return seen
	}

	flagged := config.ProviderConfig{ID: "oc", Name: "oc", Type: "openai_compatible", AuthMode: "none", Enabled: true, OpencodeSession: true}
	unflagged := flagged
	unflagged.OpencodeSession = false
	ctx := WithOpencodeSession(context.Background(), "vk-1:sess-1")

	// Inference POST to a flagged provider carries the synthesized value.
	if got := do(flagged, ctx, http.MethodPost, "/v1/chat/completions", nil).Get("x-opencode-session"); got != "vk-1:sess-1" {
		t.Fatalf("flagged inference: got %q, want vk-1:sess-1", got)
	}
	// Same value, unflagged provider: never sent.
	if got := do(unflagged, ctx, http.MethodPost, "/v1/chat/completions", nil).Get("x-opencode-session"); got != "" {
		t.Fatalf("unflagged provider: got %q, want empty", got)
	}
	// Flagged provider, non-inference calls: never sent.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/v1/embeddings"},
		{http.MethodPost, "/v1/messages/count_tokens"},
		{http.MethodPost, "/v1/images/generations"},
	} {
		if got := do(flagged, ctx, tc.method, tc.path, nil).Get("x-opencode-session"); got != "" {
			t.Fatalf("flagged %s %s: got %q, want empty", tc.method, tc.path, got)
		}
	}
	// Invalid context value is dropped, not sent.
	badCtx := WithOpencodeSession(context.Background(), "a\rb")
	if got := do(flagged, badCtx, http.MethodPost, "/v1/chat/completions", nil).Get("x-opencode-session"); got != "" {
		t.Fatalf("invalid value: got %q, want empty", got)
	}
	// No context value: nothing synthesized.
	if got := do(flagged, context.Background(), http.MethodPost, "/v1/chat/completions", nil).Get("x-opencode-session"); got != "" {
		t.Fatalf("no context value: got %q, want empty", got)
	}
	// Synthesized value wins over an allowlisted client header of the
	// same name, so the virtual-key prefix cannot be stripped by
	// sending the raw session header alongside it.
	flaggedForward := flagged
	flaggedForward.ForwardHeaders = []string{"x-opencode-session"}
	forward := http.Header{}
	forward.Set("x-opencode-session", "sess-1")
	if got := do(flaggedForward, ctx, http.MethodPost, "/v1/chat/completions", forward).Get("x-opencode-session"); got != "vk-1:sess-1" {
		t.Fatalf("synthesized should win: got %q, want vk-1:sess-1", got)
	}
}
