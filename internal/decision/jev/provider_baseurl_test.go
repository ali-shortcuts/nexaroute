package jev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// The Jev base_url is operator-supplied configuration, which makes it a trust
// boundary, not a test hook. Only loopback — the documented local-testing
// override — may run over plain HTTP. Everything else keeps the production
// transport guarantees: HTTPS, and no link-local, private or metadata target.
//
// Before this was enforced, any "http://" base_url produced a plain-HTTP client
// with no SSRF dial guard at all, so a base_url of "http://169.254.169.254/..."
// sent the Jev API key in a Bearer header to a cloud metadata endpoint from
// configuration alone.
func TestNewProviderRejectsInsecureBaseURLFromConfig(t *testing.T) {
	urls := []string{
		"http://jev.example.com",                   // plaintext: the credential crosses the network in the clear
		"http://169.254.169.254/latest/meta-data/", // link-local metadata: credential exfiltration via SSRF
		"http://[::ffff:169.254.169.254]/",         // same target, IPv4-mapped form
		"http://10.0.0.1",                          // private range
		"http://192.168.1.1",                       // private range
		"http://127.0.0.1.nip.io",                  // name that used to resolve towards loopback
		"https://169.254.169.254",                  // blocked host even over TLS
		"https://10.0.0.1",                         // private range over TLS
	}
	for _, raw := range urls {
		t.Run(raw, func(t *testing.T) {
			p, err := NewProvider(ProviderConfig{
				ID:      "jev-main",
				Type:    "jev",
				Enabled: true,
				APIKey:  "sk-test-key",
				BaseURL: raw,
			})
			if err == nil {
				t.Fatalf("base_url %q produced a usable client without an injected test client: %v", raw, p)
			}
			if p != nil {
				t.Fatalf("base_url %q must not return a provider", raw)
			}
		})
	}
}

// TestNewProviderAcceptsLoopbackBaseURL documents the one relaxation config is
// allowed: a plaintext loopback endpoint, which is the documented local-testing
// override and the only way to point a config-driven gateway at a local server.
func TestNewProviderAcceptsLoopbackBaseURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:65535", "http://localhost:65535", "http://[::1]:65535"} {
		t.Run(raw, func(t *testing.T) {
			p, err := NewProvider(ProviderConfig{ID: "jev-main", Type: "jev", Enabled: true, APIKey: "k", BaseURL: raw})
			if err != nil {
				t.Fatalf("loopback base_url must stay supported: %v", err)
			}
			if p.BaseURL() != raw {
				t.Fatalf("base url = %q, want %q", p.BaseURL(), raw)
			}
		})
	}
}

// TestNewProviderKeepsInjectedTestClient documents the intended escape hatch:
// tests inject their own http.Client (httptest), which is the only way to point
// a provider at a loopback plaintext server.
func TestNewProviderKeepsInjectedTestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`))
	}))
	defer srv.Close()

	p, err := NewProvider(ProviderConfig{
		ID:         "jev-main",
		Type:       "jev",
		Enabled:    true,
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("injected test client must stay supported: %v", err)
	}
	res, err := p.Decide(context.Background(), decision.DecisionRequest{
		Candidates: []decision.Candidate{{ID: "p1/m1"}, {ID: "p2/m2"}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if res.Action != decision.ActionSelect || res.SelectedID != "p1/m1" {
		t.Fatalf("unexpected decision: %+v", res)
	}
}

// TestUpdateFromConfigRejectsInsecureBaseURL pins the same boundary on the hot
// reload path: an operator edit must not be able to move a provider onto a
// plaintext or link-local endpoint.
func TestUpdateFromConfigRejectsInsecureBaseURL(t *testing.T) {
	p, err := NewProvider(ProviderConfig{
		ID:      "jev-main",
		Type:    "jev",
		Enabled: true,
		APIKey:  "test-key",
		BaseURL: "https://jev.example.com",
	})
	if err != nil {
		t.Fatalf("https base url must be accepted: %v", err)
	}
	if err := p.UpdateFromConfig("test-key", "http://169.254.169.254/latest/meta-data/", true); err == nil {
		t.Fatal("hot reload must reject a plaintext link-local base url")
	}
}

// TestNewProviderShortBaseURLDoesNotPanic covers the same code path from the
// other side: the old constructor sliced base_url at a fixed offset to sniff the
// scheme, which panics on any string shorter than seven bytes.
func TestNewProviderShortBaseURLDoesNotPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("NewProvider panicked on a short base_url: %v", rec)
		}
	}()
	if _, err := NewProvider(ProviderConfig{ID: "jev-main", Type: "jev", Enabled: true, APIKey: "k", BaseURL: "ab"}); err == nil {
		t.Fatal("a two byte base_url must be rejected, not accepted")
	}
}
