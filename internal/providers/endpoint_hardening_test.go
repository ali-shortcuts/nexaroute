package providers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestDoPathRejectsUserinfoAndNonHTTPEndpoints(t *testing.T) {
	cases := []struct{ name, base, path string }{
		{"userinfo in base", "https://user:pass@example.test", "/v1/models"},
		{"userinfo in absolute path", "https://example.test", "https://user:pass@other.test/v1/models"},
		{"file scheme", "file:///etc/passwd", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &httpAdapter{p: config.ProviderConfig{BaseURL: tc.base}, c: &http.Client{}}
			_, err := a.DoPath(context.Background(), http.MethodGet, tc.path, nil, false, nil)
			if err == nil || !strings.Contains(err.Error(), "invalid endpoint") {
				t.Fatalf("endpoint must be rejected before any request, err=%v", err)
			}
			if strings.Contains(err.Error(), "pass") {
				t.Fatalf("error leaks credentials: %v", err)
			}
		})
	}
}

func TestAdapterConcurrencyIsClamped(t *testing.T) {
	a, err := newHTTPAdapter(config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: "https://example.test", MaxConcurrency: 1 << 30, Enabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := cap(a.sem); got != maxAdapterConcurrency {
		t.Fatalf("semaphore capacity=%d, want %d", got, maxAdapterConcurrency)
	}
}
