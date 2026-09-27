package remote

// Transport hardening tests: TLS SNI and certificate verification survive
// IP-pinned dialing, HTTP/2 keeps working, production TLS can never disable
// verification, and environment proxies cannot intercept (or receive) the
// security-sensitive client's connections.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 16. Dialing the pinned IP preserves TLS SNI and certificate verification of
// the ORIGINAL hostname, and HTTP/2 still negotiates over the pinned dial.
func TestSSRF_TLSPinnedDial_SNIAndVerification(t *testing.T) {
	srv := startTestTLSServer(t, "secure.test", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	resolver := newFakeResolver()
	resolver.add("secure.test", "192.0.2.10")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		return net.Dial(network, srv.addr)
	}}

	tr := newTransport(transportConfig{
		resolve:   resolver.lookupIPAddr,
		dial:      rec.dial,
		tlsConfig: testTLSConfig(srv.pool),
	})
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := client.Get("https://secure.test/hello")
	if err != nil {
		t.Fatalf("request via pinned dial failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("unexpected body %q", body)
	}

	// The socket must go to the approved IP literal (original port), not the hostname.
	if got := rec.addrs(); len(got) != 1 || got[0] != "192.0.2.10:443" {
		t.Fatalf("expected pinned dial to 192.0.2.10:443, got %v", got)
	}
	// SNI (and thus certificate verification) is keyed on the original hostname.
	if got := srv.sniSeen(); got != "secure.test" {
		t.Fatalf("TLS SNI must remain the original hostname, got %q", got)
	}
	// HTTP/2 behavior preserved over the hardened transport.
	if resp.ProtoMajor != 2 {
		t.Fatalf("expected HTTP/2 over pinned dial, got %s", resp.Proto)
	}
}

// TLS certificate verification is enforced: a certificate for another name
// fails the handshake (no verification bypass anywhere).
func TestSSRF_TLSVerification_Enforced(t *testing.T) {
	srv := startTestTLSServer(t, "other.test", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("must not be read"))
	})

	resolver := newFakeResolver()
	resolver.add("secure.test", "192.0.2.10")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		return net.Dial(network, srv.addr)
	}}

	tr := newTransport(transportConfig{
		resolve:   resolver.lookupIPAddr,
		dial:      rec.dial,
		tlsConfig: testTLSConfig(srv.pool),
	})
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := client.Get("https://secure.test/hello")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("handshake with wrong certificate must fail")
	}
	// x509 errors surface wrapped in url.Error; classify loosely.
	var ce *tls.CertificateVerificationError
	if !errors.As(err, &ce) && !isCertError(err) {
		t.Fatalf("expected certificate verification error, got %v", err)
	}
}

func isCertError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "certificate") || strings.Contains(s, "x509") || strings.Contains(s, "tls")
}

// 18. Production transports never enable InsecureSkipVerify and always set a
// TLS >= 1.2 floor.
func TestSSRF_ProductionTLS_NeverInsecureSkipVerify(t *testing.T) {
	configs := map[string]*tls.Config{
		"NewTransport":          NewTransport().TLSClientConfig,
		"NewTestTransport":      NewTestTransport().TLSClientConfig,
		"newTransport(default)": newTransport(transportConfig{}).TLSClientConfig,
		"defaultTLSConfig":      defaultTLSConfig(),
	}
	for name, cfg := range configs {
		if cfg == nil {
			t.Fatalf("%s: TLS config must not be nil", name)
		}
		if cfg.InsecureSkipVerify {
			t.Fatalf("%s: InsecureSkipVerify must never be enabled", name)
		}
		if cfg.MinVersion < tls.VersionTLS12 {
			t.Fatalf("%s: MinVersion must be at least TLS 1.2, got %x", name, cfg.MinVersion)
		}
		if cfg.ServerName != "" {
			// ServerName must come from the request hostname so SNI/cert
			// checks always match the original hostname.
			t.Fatalf("%s: ServerName must not be preset", name)
		}
	}
}

// Proxy policy (behavioral): even with HTTP_PROXY/HTTPS_PROXY/ALL_PROXY set in
// the environment, the production transport never contacts the proxy.
func TestSSRF_EnvProxy_NeverUsed(t *testing.T) {
	proxyCanary := startCanary(t)
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyCanary.port())
	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("http_proxy", proxyURL)
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("https_proxy", proxyURL)
	t.Setenv("ALL_PROXY", proxyURL)
	t.Setenv("all_proxy", proxyURL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	// The request goes through the fully-default production transport; DNS is
	// faked so the approved IP is unroutable and the dial fails fast.
	dns := newFakeDNSServer(t)
	dns.setA("proxytest.test", net.IPv4(192, 0, 2, 1))
	orig := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, dns.addr())
		},
	}
	t.Cleanup(func() { net.DefaultResolver = orig })

	tr := NewTransport()
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	resp, err := client.Get("https://proxytest.test/")
	if resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("expected failure (no real server), got %v", resp.Status)
	}
	if proxyCanary.hits() != 0 {
		t.Fatalf("environment proxy must never be contacted (%d hits)", proxyCanary.hits())
	}
}

// Context deadline propagation through the client maps to ErrTimeout.
func TestSSRF_ClientDeadline_Timeout(t *testing.T) {
	srv := startTestTLSServer(t, "slow.test", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	resolver := newFakeResolver()
	resolver.add("slow.test", "192.0.2.10")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		return net.Dial(network, srv.addr)
	}}
	tr := newTransport(transportConfig{resolve: resolver.lookupIPAddr, dial: rec.dial, tlsConfig: testTLSConfig(srv.pool)})
	client, err := NewClient(ClientConfig{
		BaseURL:    "https://slow.test",
		APIKey:     "k",
		HTTPClient: &http.Client{Transport: tr},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = client.Do(ctx, "/decide", []byte(`{}`))
	if err == nil {
		t.Fatalf("expected timeout")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected ErrTimeout classification, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("deadline not honored: %v", elapsed)
	}
}
