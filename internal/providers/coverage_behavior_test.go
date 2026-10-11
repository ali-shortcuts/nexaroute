package providers

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
)

func TestCountTokensUsesConfiguredEndpointAndForwardHeaders(t *testing.T) {
	var gotPath, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeader = r.URL.Path, r.Header.Get("X-Trace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":7,"estimated":false}`)
	}))
	defer srv.Close()
	a, err := newHTTPAdapter(config.ProviderConfig{ID: "count", Type: "openai_compatible", BaseURL: srv.URL, CountTokensPath: "/v1/messages/count_tokens", AuthMode: "none", ForwardHeaders: []string{"X-Trace"}, Enabled: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.CountTokens(context.Background(), []byte(`{"messages":[]}`), http.Header{"X-Trace": []string{"coverage"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || gotPath != "/v1/messages/count_tokens" || gotHeader != "coverage" {
		t.Fatalf("status=%d path=%q header=%q", resp.StatusCode, gotPath, gotHeader)
	}
}

func TestProviderErrorsAndEndpointValidation(t *testing.T) {
	cause := errors.New("upstream unavailable")
	wrapped := &RetryAfterError{Cause: cause, After: 2 * time.Second}
	if wrapped.Error() != cause.Error() || !errors.Is(wrapped, cause) {
		t.Fatal("RetryAfterError does not preserve cause")
	}
	if delay, ok := RetryAfter(wrapped); !ok || delay != 2*time.Second {
		t.Fatalf("RetryAfter=%v,%v", delay, ok)
	}
	if _, ok := RetryAfter(errors.New("ordinary")); ok {
		t.Fatal("ordinary error reported retry delay")
	}
	if _, err := newHTTPAdapter(config.ProviderConfig{ID: "proxy", Type: "openai_compatible", BaseURL: "https://example.test", ProxyURL: "://bad", Enabled: true}, time.Second); err == nil {
		t.Fatal("invalid proxy URL accepted")
	}
	a := &httpAdapter{p: config.ProviderConfig{BaseURL: "not a url"}, c: &http.Client{}}
	if _, err := a.DoPath(context.Background(), http.MethodPost, "/v1/chat/completions", nil, false, nil); err == nil || !strings.Contains(err.Error(), "invalid endpoint") {
		t.Fatalf("invalid endpoint error=%v", err)
	}
}

func TestStreamingCloseWrappersReleaseAndCancelExactlyOnce(t *testing.T) {
	releases := 0
	body := &releaseOnDoneBody{ReadCloser: io.NopCloser(bytes.NewReader([]byte("chunk"))), release: func() { releases++ }}
	buf := make([]byte, 5)
	if n, err := body.Read(buf); err != nil || n != 5 {
		t.Fatalf("read n=%d err=%v", n, err)
	}
	if err := body.Close(); err != nil || releases != 1 {
		t.Fatalf("close err=%v releases=%d", err, releases)
	}
	if err := body.Close(); err != nil || releases != 1 {
		t.Fatalf("second close err=%v releases=%d", err, releases)
	}

	cancelled := 0
	cancelBody := &cancelOnCloseBody{ReadCloser: io.NopCloser(strings.NewReader("x")), cancel: func() { cancelled++ }}
	if err := cancelBody.Close(); err != nil || cancelled != 1 {
		t.Fatalf("cancel body err=%v cancelled=%d", err, cancelled)
	}

	idleCancelled := 0
	idle := newIdleBody(io.NopCloser(strings.NewReader("payload")), time.Hour, func() { idleCancelled++ })
	if _, err := idle.Read(make([]byte, 7)); err != nil {
		t.Fatal(err)
	}
	if err := idle.Close(); err != nil || idleCancelled != 1 {
		t.Fatalf("idle close err=%v cancellations=%d", err, idleCancelled)
	}
}

func TestQuotaEstimateAndAdapterIdentityContracts(t *testing.T) {
	if got := WithQuotaEstimate(nil, 5, 7).Value(quotaEstimateContextKey{}); got != int64(12) {
		t.Fatalf("quota estimate=%v", got)
	}
	if _, ok := quotaEstimateFromContext(context.Background()); ok {
		t.Fatal("plain context unexpectedly had quota estimate")
	}
	a, err := newHTTPAdapter(config.ProviderConfig{ID: "identity", Type: "gemini", BaseURL: "https://example.test", APIKey: "key", Enabled: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != "identity" || a.Kind() != "gemini" || !a.CredentialsMatch([]string{"key"}) {
		t.Fatalf("identity contract failed id=%q kind=%q creds=%v", a.ID(), a.Kind(), a.CredentialsMatch([]string{"key"}))
	}
	if got := a.geminiModelPath("models/gemini-pro", true); !strings.Contains(got, "streamGenerateContent") {
		t.Fatalf("stream Gemini path=%q", got)
	}
	a.CloseIdleConnections()
}
