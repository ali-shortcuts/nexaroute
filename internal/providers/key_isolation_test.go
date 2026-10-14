package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Per-provider key isolation regressions (M3-4B). Distinct synthetic
// canary credentials per provider; every assertion scans ALL headers
// an upstream actually received, so a key leaking through any header —
// not just the expected auth header — fails the test.

type capturedRequests struct {
	mu      sync.Mutex
	headers []http.Header
}

func (c *capturedRequests) handler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.headers = append(c.headers, r.Header.Clone())
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func (c *capturedRequests) all() []http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]http.Header(nil), c.headers...)
}

func anyHeaderContains(hs []http.Header, needle string) bool {
	for _, h := range hs {
		for _, vals := range h {
			for _, v := range vals {
				if strings.Contains(v, needle) {
					return true
				}
			}
		}
	}
	return false
}

const chatEnvelope = `{"id":"x","object":"chat.completion","choices":[],"usage":{}}`

// Two providers in one registry, different auth modes and canary keys:
// each upstream receives only its own provider's credential.
func TestProviderKeyIsolationAcrossAdapters(t *testing.T) {
	capA := &capturedRequests{}
	srvA := httptest.NewServer(capA.handler(200, chatEnvelope))
	defer srvA.Close()
	capB := &capturedRequests{}
	srvB := httptest.NewServer(capB.handler(200, chatEnvelope))
	defer srvB.Close()

	cfg := config.Config{Providers: []config.ProviderConfig{
		{ID: "prov-a", Name: "A", Type: "openai_compatible", BaseURL: srvA.URL + "/v1", AuthMode: "bearer", APIKey: "canary-key-alpha", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "model-a", Enabled: true}}},
		{ID: "prov-b", Name: "B", Type: "gemini", BaseURL: srvB.URL, AuthMode: "x-goog-api-key", APIKey: "canary-key-bravo", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "model-b", Enabled: true}}},
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		provider string
		own      string
		other    string
		authHdr  string
	}{
		{"prov-a", "canary-key-alpha", "canary-key-bravo", "Authorization"},
		{"prov-b", "canary-key-bravo", "canary-key-alpha", "X-Goog-Api-Key"},
	} {
		a, ok := reg.Get(tc.provider)
		if !ok {
			t.Fatalf("adapter %s missing", tc.provider)
		}
		resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
		if err != nil {
			t.Fatalf("%s Do: %v", tc.provider, err)
		}
		resp.Body.Close()
		_ = tc
	}
	seenA, seenB := capA.all(), capB.all()
	if len(seenA) != 1 || len(seenB) != 1 {
		t.Fatalf("expected one request per upstream, got A=%d B=%d", len(seenA), len(seenB))
	}
	if got := seenA[0].Get("Authorization"); got != "Bearer canary-key-alpha" {
		t.Fatalf("provider A upstream auth = %q", got)
	}
	if anyHeaderContains(seenA, "canary-key-bravo") {
		t.Fatalf("provider B key leaked into provider A request: %v", seenA[0])
	}
	if got := seenB[0].Get("X-Goog-Api-Key"); got != "canary-key-bravo" {
		t.Fatalf("provider B upstream auth = %q", got)
	}
	if anyHeaderContains(seenB, "canary-key-alpha") {
		t.Fatalf("provider A key leaked into provider B request: %v", seenB[0])
	}
}

// Credential rotation inside one provider (429 on the first key, the
// adapter retries with the second) must use only that provider's own
// credentials — the sequence observed upstream contains A keys only.
func TestProviderCredentialRotationStaysWithinProvider(t *testing.T) {
	var mu sync.Mutex
	var authSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		authSeen = append(authSeen, auth)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if auth == "Bearer canary-rot-one" {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		io.WriteString(w, chatEnvelope)
	}))
	defer srv.Close()

	p := config.ProviderConfig{
		ID: "rot", Name: "rot", Type: "openai_compatible", BaseURL: srv.URL + "/v1", AuthMode: "bearer", Enabled: true,
		Credentials: []config.CredentialConfig{
			{Name: "one", APIKey: "canary-rot-one", Enabled: true},
			{Name: "two", APIKey: "canary-rot-two", Enabled: true},
		},
	}
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected rotation to a 200, got %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authSeen) < 2 {
		t.Fatalf("expected a credential rotation (multiple attempts), saw %v", authSeen)
	}
	for _, auth := range authSeen {
		if auth != "Bearer canary-rot-one" && auth != "Bearer canary-rot-two" {
			t.Fatalf("attempt carried a foreign credential: %q (all: %v)", auth, authSeen)
		}
	}
	if authSeen[len(authSeen)-1] != "Bearer canary-rot-two" {
		t.Fatalf("final attempt should use the second credential, saw %v", authSeen)
	}
}
