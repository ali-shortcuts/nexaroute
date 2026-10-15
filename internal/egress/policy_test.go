package egress

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
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
