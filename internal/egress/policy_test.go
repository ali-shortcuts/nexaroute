package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestPolicyRejectsPrivateAndAllowListMismatch(t *testing.T) {
	p := Policy{AllowedHosts: []string{"api.example.com"}}
	if _, err := p.NewTransport("https://127.0.0.1:8443", ""); err == nil {
		t.Fatal("private literal must be rejected")
	}
	if _, err := p.NewTransport("https://other.example.com", ""); err == nil {
		t.Fatal("host outside allow-list must be rejected")
	}
}

func TestPolicyAllowsExplicitLoopbackHTTPOnly(t *testing.T) {
	p := Policy{AllowLoopback: true}
	tr, err := p.NewTransport("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proxy != nil || tr.DialContext == nil {
		t.Fatal("expected direct pinned transport")
	}
	if _, err := (Policy{}).NewTransport("http://127.0.0.1:8080", ""); err == nil {
		t.Fatal("loopback HTTP must require explicit opt-in")
	}
}

func TestPolicyValidatesExplicitProxyConfiguration(t *testing.T) {
	tr, err := (Policy{}).NewTransport("https://api.example.com", "http://user:pass@proxy.example.com:8080/path?q=x")
	if err != nil || tr.Proxy == nil {
		t.Fatalf("explicit proxy should be preserved: transport=%v err=%v", tr, err)
	}
	if _, err := (Policy{}).NewTransport("https://api.example.com", "not a url"); err == nil {
		t.Fatal("malformed proxy must be rejected")
	}
	if _, err := (Policy{}).NewTransport("https://api.example.com", "file:///tmp/proxy"); err == nil {
		t.Fatal("unsupported proxy scheme accepted")
	}
}

func TestPolicyRejectsURLDecorationsAndBlockedAddressClasses(t *testing.T) {
	p := Policy{AllowLoopback: true}
	for _, raw := range []string{
		"https://user:pass@example.com", "https://example.com/path?q=secret", "https://example.com/#fragment",
		"ftp://example.com", "https://192.168.1.10:443",
	} {
		if _, err := p.NewTransport(raw, ""); err == nil {
			t.Fatalf("accepted unsafe URL %q", raw)
		}
	}
	if _, err := (Policy{}).NewTransport("http://example.com", ""); err == nil {
		t.Fatal("HTTP external target accepted")
	}
	if _, err := (Policy{}).NewTransport("https://[::1]:443", ""); err == nil {
		t.Fatal("loopback IPv6 accepted without opt-in")
	}
	if !blocked(netip.MustParseAddr("10.0.0.1"), false) || !blocked(netip.MustParseAddr("127.0.0.1"), false) || blocked(netip.MustParseAddr("8.8.8.8"), false) {
		t.Fatal("unexpected address classification")
	}
}

func TestPolicyTransportIsHTTPTransport(t *testing.T) {
	tr, err := (Policy{}).NewTransport("https://api.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: tr}
	if client.Transport == nil {
		t.Fatal("transport missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.DialContext(ctx, "tcp", "api.example.com:443"); err == nil {
		t.Fatal("cancelled dial unexpectedly succeeded")
	}
}

// Regression: docs promise that a configured proxy_url host is permitted at
// dial time in addition to allowed_hosts. The allow-list check used to run on
// every dial, including the proxy hop, so enabling allowed_hosts silently
// broke provider proxy_url.
func TestNewTransportAllowsProxyHostOutsideAllowedHosts(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()

	p := Policy{AllowedHosts: []string{"localhost"}, AllowLoopback: true}
	tr, err := p.NewTransport("http://localhost:1", proxy.URL)
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://localhost:1/")
	if err != nil {
		t.Fatalf("request through configured proxy failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || proxied.Load() != 1 {
		t.Fatalf("status=%d proxied=%d, want 200/1", resp.StatusCode, proxied.Load())
	}
}
