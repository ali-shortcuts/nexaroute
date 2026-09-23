package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestConfiguredCredentialOverridesStaleCustomAuthHeader(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{}}`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "real-key", AuthMode: "bearer", Headers: map[string]string{"Authorization": "Bearer stale-key"}, Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer real-key" {
		t.Fatalf("configured credential lost precedence: %q", gotAuth)
	}
}

func TestCustomAuthorizationAllowedWhenAuthModeNone(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: srv.URL + "/v1", AuthMode: "none", Headers: map[string]string{"Authorization": "Custom abc"}, Enabled: true}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Custom abc" {
		t.Fatalf("custom auth unexpectedly replaced: %q", gotAuth)
	}
}

func TestEndpointAllowsAbsoluteOverride(t *testing.T) {
	got := endpoint("https://api.example.com/v1", "https://catalog.example.com/models")
	if got != "https://catalog.example.com/models" {
		t.Fatalf("absolute endpoint=%q", got)
	}
}

func TestCredentialP2CPrefersLessActiveKey(t *testing.T) {
	a := &httpAdapter{creds: []credentialState{{Key: "a"}, {Key: "b"}}}
	first, _, ok := a.reserveCredential(nil)
	if !ok {
		t.Fatal("first credential was not selected")
	}
	second, _, ok := a.reserveCredential(nil)
	if !ok {
		t.Fatal("second credential was not selected")
	}
	if first == second {
		t.Fatalf("power-of-two key selection reused busy key %d", first)
	}
	a.releaseCredential(first)
	a.releaseCredential(second)
}

func TestCredentialP2CSkipsCoolingKey(t *testing.T) {
	a := &httpAdapter{creds: []credentialState{
		{Key: "cooling", CooldownUntil: time.Now().Add(time.Hour)},
		{Key: "ready"},
	}}
	idx, key, ok := a.reserveCredential(nil)
	if !ok || idx != 1 || key != "ready" {
		t.Fatalf("selected idx=%d key=%q ok=%v", idx, key, ok)
	}
	a.releaseCredential(idx)
}

type terminalErrorBody struct{}

func (terminalErrorBody) Read([]byte) (int, error) { return 0, errors.New("terminal read failure") }
func (terminalErrorBody) Close() error             { return nil }

func TestReleaseOnDoneBodyReleasesOnTerminalReadError(t *testing.T) {
	released := 0
	body := &releaseOnDoneBody{
		ReadCloser: terminalErrorBody{},
		release: func() {
			released++
		},
	}
	if _, err := body.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected terminal read error")
	}
	if released != 1 {
		t.Fatalf("release count=%d want 1", released)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("release ran more than once: %d", released)
	}
}

func TestProbeRequiresProtocolValidSuccessEnvelope(t *testing.T) {
	tests := []struct {
		name         string
		providerType string
		body         string
		wantErr      bool
	}{
		{"malformed json", "openai_compatible", "{bad", true},
		{"wrong openai envelope", "openai_compatible", `{"ok":true}`, true},
		{"valid openai", "openai_compatible", `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{}}`, false},
		{"null anthropic content", "anthropic_compatible", `{"id":"x","type":"message","role":"assistant","content":null,"model":"m"}`, true},
		{"valid anthropic", "anthropic_compatible", `{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := config.ProviderConfig{
				ID: "p", Name: "P", Type: tc.providerType, BaseURL: srv.URL,
				ChatPath: "/", MessagesPath: "/", MaxConcurrency: 1, Enabled: true,
			}
			a, err := newHTTPAdapter(p, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = a.Probe(context.Background(), "m", 1)
			if tc.wantErr && err == nil {
				t.Fatal("expected invalid probe response to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid probe failed: %v", err)
			}
		})
	}
}

func TestProbeRejectsTruncatedSuccessBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[`)
	}))
	defer srv.Close()
	p := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL,
		ChatPath: "/", MaxConcurrency: 1, Enabled: true,
	}
	a, err := newHTTPAdapter(p, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Probe(context.Background(), "m", 1); err == nil {
		t.Fatal("truncated 2xx probe body must not mark a deployment healthy")
	}
}
