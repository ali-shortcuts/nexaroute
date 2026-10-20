package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Issue #394: discovery support is a property of the implemented protocol,
// not of a defaulted ModelsPath. Azure deployments, Vertex projects and
// Bedrock Converse do not expose a plain /models resource and must report
// unsupported without making a network call.
func TestSupportsDiscoveryIsProtocolAware(t *testing.T) {
	cases := []struct {
		providerType string
		want         bool
	}{
		{"openai_compatible", true},
		{"anthropic_compatible", true},
		{"openai_responses", true},
		{"gemini", true},
		{"azure_openai", false},
		{"vertex", false},
		{"bedrock", false},
	}
	for _, tc := range cases {
		p := config.ProviderConfig{ID: "p", Type: tc.providerType, BaseURL: "https://example.invalid", ModelsPath: "/v1/models"}
		p.ApplyDefaults()
		if got := SupportsDiscovery(p); got != tc.want {
			t.Errorf("SupportsDiscovery(%s)=%v want %v", tc.providerType, got, tc.want)
		}
	}
}

func TestDiscoverModelsUnsupportedDoesNotCallUpstream(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"should-not-be-fetched"}]}`))
	}))
	defer up.Close()

	for _, providerType := range []string{"azure_openai", "vertex", "bedrock"} {
		p := config.ProviderConfig{ID: "p", Type: providerType, BaseURL: up.URL, ModelsPath: "/v1/models", Enabled: true}
		models, status, err := DiscoverModels(t.Context(), p)
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: err=%v want not-supported", providerType, err)
		}
		if len(models) != 0 || status != 0 {
			t.Errorf("%s: models=%v status=%d", providerType, models, status)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("unsupported discovery made %d upstream call(s)", got)
	}
}

func TestDiscoverModelsIsBoundedAndRedactsCredential(t *testing.T) {
	const secret = "discovery-secret-canary-2f9a"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"denied ` + secret + `"}`))
	}))
	defer up.Close()

	p := config.ProviderConfig{
		ID: "p", Type: "openai_compatible", BaseURL: up.URL,
		APIKey: secret, AuthMode: "bearer", Enabled: true, ModelsPath: "/v1/models",
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, status, err := DiscoverModels(ctx, p)
	if err == nil {
		t.Fatal("expected discovery error")
	}
	if status != http.StatusForbidden {
		t.Errorf("status=%d want 403", status)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("discovery error leaked the credential: %v", err)
	}
}

func TestParseModelListCommonShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"bare array", `["b","a","a"]`, []string{"a", "b"}},
		{"data objects", `{"data":[{"id":"gpt-x"},{"id":"gpt-y"}]}`, []string{"gpt-x", "gpt-y"}},
		{"models strings", `{"models":["models/abc","def"]}`, []string{"abc", "def"}},
		{"items objects", `{"items":[{"name":"n1"}]}`, []string{"n1"}},
		{"garbage", `not json`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseModelList([]byte(tc.body))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ParseModelList(%s)=%v want %v", tc.body, got, tc.want)
			}
		})
	}
}
