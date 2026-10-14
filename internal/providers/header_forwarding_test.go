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

// Header-forwarding security regressions (M3-4A, threat model: header
// allow-list). These tests drive the real adapter request path against
// a local upstream and assert what the upstream actually receives, so
// they fail on a genuine forwarding violation rather than inspecting
// implementation text. Three distinct header classes are kept apart:
// statically configured provider headers (ProviderConfig.Headers),
// client-supplied headers offered for forwarding, and credentials
// injected by applyAuthKey according to AuthMode.

func forwardCaptureUpstream(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[],"usage":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header { return seen }
}

func doWithForward(t *testing.T, p config.ProviderConfig, forward http.Header) http.Header {
	t.Helper()
	srv, seen := forwardCaptureUpstream(t)
	p.BaseURL = srv.URL + "/v1"
	a, err := newHTTPAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Do(context.Background(), []byte(`{"model":"x","messages":[]}`), false, forward)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return seen()
}

func headerValuesContain(h http.Header, needle string) bool {
	for _, vals := range h {
		for _, v := range vals {
			if strings.Contains(v, needle) {
				return true
			}
		}
	}
	return false
}

// Allow-list enforcement plus case-insensitive matching of valid
// configured header names: a listed client header is forwarded, an
// unlisted one is not, and the configured entry matches regardless of
// its casing ("X-Custom-One" in config vs the client's canonical form,
// "x-custom-two" in config vs "X-Custom-Two" from the client).
func TestForwardHeadersAllowListEnforcement(t *testing.T) {
	p := config.ProviderConfig{
		ID: "p", Name: "p", Type: "openai_compatible", AuthMode: "none", Enabled: true,
		ForwardHeaders: []string{"X-Custom-One", "x-custom-two"},
	}
	forward := http.Header{}
	forward.Set("X-Custom-One", "canary-one")
	forward.Set("X-Custom-Two", "canary-two")
	forward.Set("X-Unlisted", "canary-unlisted")
	seen := doWithForward(t, p, forward)
	if got := seen.Get("X-Custom-One"); got != "canary-one" {
		t.Fatalf("listed header (mixed-case config) not forwarded: %q", got)
	}
	if got := seen.Get("X-Custom-Two"); got != "canary-two" {
		t.Fatalf("listed header not forwarded: %q", got)
	}
	if headerValuesContain(seen, "canary-unlisted") {
		t.Fatalf("unlisted client header leaked upstream: %v", seen)
	}
}

// Credential isolation: the four protected credential header names can
// never be forwarded from client input, even when an operator lists
// them in ForwardHeaders. AuthMode is "none" so nothing is injected
// either — the upstream must see none of the client canaries anywhere.
func TestForwardedCredentialHeadersAlwaysBlocked(t *testing.T) {
	protected := []string{"Authorization", "X-API-Key", "X-Goog-API-Key", "X-Admin-Key"}
	for _, name := range protected {
		t.Run(name, func(t *testing.T) {
			canary := "client-canary-" + strings.ToLower(name)
			p := config.ProviderConfig{
				ID: "p", Name: "p", Type: "openai_compatible", AuthMode: "none", Enabled: true,
				ForwardHeaders: []string{name},
			}
			forward := http.Header{}
			forward.Set(name, canary)
			seen := doWithForward(t, p, forward)
			if headerValuesContain(seen, canary) {
				t.Fatalf("client value for protected header %s reached upstream: %v", name, seen)
			}
			if got := seen.Get(name); got != "" {
				t.Fatalf("protected header %s unexpectedly set upstream: %q", name, got)
			}
		})
	}
}

// No overwrite of configured provider values: a statically configured
// credential header keeps its configured value when a client tries to
// forward the same name (and the name is listed). Covers the two
// credential headers an operator can plausibly configure statically;
// Authorization was already blocked, X-Goog-API-Key is the regression.
func TestForwardedCredentialCannotOverwriteConfiguredHeader(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		staticVal  string
		authMode   string
		providerTy string
	}{
		{"authorization", "Authorization", "Bearer static-canary-auth", "none", "openai_compatible"},
		{"x-goog-api-key", "X-Goog-API-Key", "static-canary-goog", "none", "gemini"},
		{"x-api-key", "X-API-Key", "static-canary-apikey", "none", "anthropic_compatible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := config.ProviderConfig{
				ID: "p", Name: "p", Type: tc.providerTy, AuthMode: tc.authMode, Enabled: true,
				Headers:        map[string]string{tc.header: tc.staticVal},
				ForwardHeaders: []string{tc.header},
			}
			forward := http.Header{}
			forward.Set(tc.header, "client-canary-overwrite")
			seen := doWithForward(t, p, forward)
			if got := seen.Get(tc.header); got != tc.staticVal {
				t.Fatalf("configured %s overwritten or lost: got %q want %q", tc.header, got, tc.staticVal)
			}
			if headerValuesContain(seen, "client-canary-overwrite") {
				t.Fatalf("client overwrite value leaked upstream: %v", seen)
			}
		})
	}
}

// Auth injection still wins: with a real Gemini credential configured,
// applyAuthKey (which runs after header application) sets the provider
// key even when the client offers its own X-Goog-API-Key value.
func TestGoogCredentialInjectionWinsOverForwardedValue(t *testing.T) {
	p := config.ProviderConfig{
		ID: "p", Name: "p", Type: "gemini", AuthMode: "x-goog-api-key", APIKey: "real-provider-canary", Enabled: true,
		ForwardHeaders: []string{"X-Goog-API-Key"},
	}
	forward := http.Header{}
	forward.Set("X-Goog-API-Key", "client-canary-goog")
	seen := doWithForward(t, p, forward)
	if got := seen.Get("X-Goog-API-Key"); got != "real-provider-canary" {
		t.Fatalf("provider credential did not win: %q", got)
	}
	if headerValuesContain(seen, "client-canary-goog") {
		t.Fatalf("client goog key leaked upstream: %v", seen)
	}
}
