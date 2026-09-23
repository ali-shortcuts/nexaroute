package providers

import (
	"context"
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
