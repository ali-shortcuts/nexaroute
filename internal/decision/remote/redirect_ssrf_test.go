package remote

// Redirect SSRF tests. Redirect handling lives at the http.Client level, so
// these tests exercise the real client construction:
//
//   - the production client (NewClient + hardened transport) must reject
//     redirect destinations that are blocked targets, must never follow any
//     redirect (so Authorization can never leak and chains cannot form), and
//   - even a redirect-following client driving the production transport must
//     be stopped at the transport: every hop is SSRF-validated and IP-pinned.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func mustNotContain(t *testing.T, haystack []string, needle string) {
	t.Helper()
	for _, s := range haystack {
		if s == needle {
			t.Fatalf("unexpected %q in %v", needle, haystack)
		}
	}
}

// 11. Safe hostname + redirect to a private/literal target is rejected and the
// private target is never contacted.
func TestSSRF_RedirectToPrivateLiteral_Rejected(t *testing.T) {
	canary := startCanary(t)

	srv := startTestTLSServer(t, "safe.test", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("https://127.0.0.1:%d/private", canary.port()), http.StatusFound)
	})

	resolver := newFakeResolver()
	resolver.add("safe.test", "192.0.2.10")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		return net.Dial(network, srv.addr)
	}}

	tr := newTransport(transportConfig{resolve: resolver.lookupIPAddr, dial: rec.dial, tlsConfig: testTLSConfig(srv.pool)})
	client, err := NewClient(ClientConfig{
		BaseURL:    "https://safe.test",
		APIKey:     "secret-key-abc",
		HTTPClient: &http.Client{Transport: tr, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, _, err = client.Do(context.Background(), "/api/v1/decisions/model-route", []byte(`{}`))
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("redirect to 127.0.0.1 must be rejected as SSRF, got %v", err)
	}
	if canary.hits() != 0 {
		t.Fatalf("redirect target must never be contacted, got %d hits", canary.hits())
	}
	// Only the initial host was resolved; nothing else.
	if got := resolver.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 lookup (initial host), got %d", got)
	}
}

// 11b. Redirect to a metadata hostname is rejected before any lookup/dial.
func TestSSRF_RedirectToMetadataHostname_Rejected(t *testing.T) {
	srv := startTestTLSServer(t, "safe.test", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://metadata.google.internal/computeMetadata/v1/instance/", http.StatusFound)
	})

	resolver := newFakeResolver()
	resolver.add("safe.test", "192.0.2.10")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		return net.Dial(network, srv.addr)
	}}

	tr := newTransport(transportConfig{resolve: resolver.lookupIPAddr, dial: rec.dial, tlsConfig: testTLSConfig(srv.pool)})
	client, err := NewClient(ClientConfig{
		BaseURL:    "https://safe.test",
		APIKey:     "secret-key-abc",
		HTTPClient: &http.Client{Transport: tr, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, _, err = client.Do(context.Background(), "/decide", []byte(`{}`))
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("redirect to metadata.google.internal must be rejected as SSRF, got %v", err)
	}
	mustNotContain(t, resolver.hostsSeen(), "metadata.google.internal")
	if rec.count() != 1 {
		t.Fatalf("only the initial dial may happen, got %v", rec.addrs())
	}
}

// 11c. Redirect to a hostname that resolves privately is never followed by
// the production client (ErrRedirectNotAllowed) and never resolved/dialed.
func TestSSRF_RedirectToPrivateHostname_NotFollowed(t *testing.T) {
	srv := startTestTLSServer(t, "safe.test", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://internal.example/secret", http.StatusFound)
	})

	resolver := newFakeResolver()
	resolver.add("safe.test", "192.0.2.10")
	resolver.add("internal.example", "10.0.0.5") // would be blocked at the dialer
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		return net.Dial(network, srv.addr)
	}}

	tr := newTransport(transportConfig{resolve: resolver.lookupIPAddr, dial: rec.dial, tlsConfig: testTLSConfig(srv.pool)})
	client, err := NewClient(ClientConfig{
		BaseURL:    "https://safe.test",
		APIKey:     "secret-key-abc",
		HTTPClient: &http.Client{Transport: tr, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	body, status, err := client.Do(context.Background(), "/decide", []byte(`{}`))
	if err == nil {
		t.Fatalf("expected redirect rejection, got body %q", body)
	}
	if !errors.Is(err, ErrRedirectNotAllowed) || status != http.StatusFound {
		t.Fatalf("expected ErrRedirectNotAllowed with 302, got status=%d err=%v", status, err)
	}
	mustNotContain(t, resolver.hostsSeen(), "internal.example")
	if rec.count() != 1 {
		t.Fatalf("redirect must never be dialed, got %v", rec.addrs())
	}
}

// 12. Redirect chain toward a metadata target is stopped at the first blocked
// hop even when redirects ARE followed (production transport enforcement).
// safe.test -> relay.test -> metadata.google.internal
func TestSSRF_RedirectChain_ToMetadata_TransportBlocks(t *testing.T) {
	canary := startCanary(t) // stands in for the metadata endpoint

	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	portB := lnB.Addr().(*net.TCPAddr).Port
	srvB := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://metadata.google.internal:%d/step3", canary.port()), http.StatusFound)
	})}
	go func() { _ = srvB.Serve(lnB) }()
	t.Cleanup(func() { _ = srvB.Close() })

	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	portA := lnA.Addr().(*net.TCPAddr).Port
	srvA := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://relay.test:%d/step2", portB), http.StatusFound)
	})}
	go func() { _ = srvA.Serve(lnA) }()
	t.Cleanup(func() { _ = srvA.Close() })

	resolver := newFakeResolver()
	resolver.add("safe.test", "192.0.2.1")
	resolver.add("relay.test", "192.0.2.2")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		switch {
		case addr == fmt.Sprintf("192.0.2.1:%d", portA):
			return net.Dial(network, lnA.Addr().String())
		case addr == fmt.Sprintf("192.0.2.2:%d", portB):
			return net.Dial(network, lnB.Addr().String())
		default:
			return nil, fmt.Errorf("unexpected dial target %s", addr)
		}
	}}

	// NOTE: this client intentionally FOLLOWS redirects (default policy) to
	// prove the transport-level SSRF enforcement covers every hop. The
	// production client built by NewClient never follows redirects at all.
	tr := newTransport(transportConfig{resolve: resolver.lookupIPAddr, dial: rec.dial})
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	url := fmt.Sprintf("http://safe.test:%d/start", portA)
	resp, err := client.Get(url)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("redirect chain to metadata must end in SSRF rejection, got %v", err)
	}
	if canary.hits() != 0 {
		t.Fatalf("metadata target must never be contacted, got %d hits", canary.hits())
	}
	mustNotContain(t, resolver.hostsSeen(), "metadata.google.internal")
	// Both safe hops were validated and dialed via pinned IPs only.
	if got := rec.addrs(); len(got) != 2 {
		t.Fatalf("expected 2 pinned dials (safe hops only), got %v", got)
	}
}

// 11d. Even with a redirect-following client, a redirect to a private-resolving
// hostname is stopped by the transport (validation on every destination).
func TestSSRF_RedirectToPrivateHostname_FollowClientTransportBlocks(t *testing.T) {
	canary := startCanary(t)

	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	portA := lnA.Addr().(*net.TCPAddr).Port
	srvA := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://internal.example:%d/secret", canary.port()), http.StatusFound)
	})}
	go func() { _ = srvA.Serve(lnA) }()
	t.Cleanup(func() { _ = srvA.Close() })

	resolver := newFakeResolver()
	resolver.add("safe.test", "192.0.2.1")
	resolver.add("internal.example", "10.0.0.5") // private answer
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		if addr == fmt.Sprintf("192.0.2.1:%d", portA) {
			return net.Dial(network, lnA.Addr().String())
		}
		return nil, fmt.Errorf("unexpected dial target %s", addr)
	}}

	tr := newTransport(transportConfig{resolve: resolver.lookupIPAddr, dial: rec.dial})
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := client.Get(fmt.Sprintf("http://safe.test:%d/start", portA))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("redirect to private-resolving host must be SSRF-rejected, got %v", err)
	}
	if canary.hits() != 0 {
		t.Fatalf("private target must never be contacted, got %d hits", canary.hits())
	}
	if got := rec.addrs(); len(got) != 1 {
		t.Fatalf("only the first hop may be dialed, got %v", got)
	}
}

// Redirect destinations with userinfo or non-https schemes are rejected by the
// redirect policy (production mode).
func TestSSRF_RedirectUserinfoAndScheme_Rejected(t *testing.T) {
	srv := startTestTLSServer(t, "safe.test", func(w http.ResponseWriter, r *http.Request) {
		loc := r.URL.Query().Get("to")
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	})

	resolver := newFakeResolver()
	resolver.add("safe.test", "192.0.2.10")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		return net.Dial(network, srv.addr)
	}}

	tr := newTransport(transportConfig{resolve: resolver.lookupIPAddr, dial: rec.dial, tlsConfig: testTLSConfig(srv.pool)})
	client, err := NewClient(ClientConfig{
		BaseURL:    "https://safe.test",
		APIKey:     "k",
		HTTPClient: &http.Client{Transport: tr, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	for _, to := range []string{
		"https://user:pass@evil.example/x",
		"http://evil.example/x", // plain http downgrade
		"file:///etc/passwd",
	} {
		path := "/decide?to=" + urlQueryEscape(to)
		_, _, err := client.Do(context.Background(), path, []byte(`{}`))
		if err == nil {
			t.Fatalf("redirect to %q must be rejected", to)
		}
		if !errors.Is(err, ErrInvalidConfig) && !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("redirect to %q: expected policy rejection, got %v", to, err)
		}
	}
	// Keep-alive may share one connection across the calls; what matters is
	// that only the pinned initial-hop IP was ever dialed.
	for _, a := range rec.addrs() {
		if a != "192.0.2.10:443" {
			t.Fatalf("redirect destinations must never be dialed, got %v", rec.addrs())
		}
	}
}

func urlQueryEscape(s string) string {
	// minimal escape for test URLs
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/', c == ':', c == '@':
			out = append(out, c)
		default:
			out = append(out, '%')
			const hex = "0123456789ABCDEF"
			out = append(out, hex[c>>4], hex[c&0xF])
		}
	}
	return string(out)
}
