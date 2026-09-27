package remote

// SSRF DNS-rebinding (TOCTOU) regression tests.
//
// The production transport must connect only to IP addresses that were
// resolved and approved exactly once per connection attempt. It must never
// re-resolve the original hostname at dial time: a second resolution is the
// classic DNS-rebinding window (validation sees a public IP, the real socket
// is opened to a private/internal IP).

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestSSRF_DNSRebinding_PinnedDial is the deterministic reproduction of the
// DNS rebinding / TOCTOU condition:
//
//	lookup #1 (validation): rebind.test -> 192.0.2.1   (public, passes)
//	lookup #2 (real dial):  rebind.test -> 127.0.0.1   (loopback canary)
//
// A vulnerable implementation validates the first answer and then dials the
// original hostname, triggering the second lookup and connecting to the
// canary. The hardened implementation performs exactly one lookup and dials
// the approved IP literally, so the canary must observe zero connections and
// only one A query may hit the (fake) DNS.
func TestSSRF_DNSRebinding_PinnedDial(t *testing.T) {
	canary := startCanary(t)
	dns := newFakeDNSServer(t)
	dns.setA("rebind.test", net.IPv4(192, 0, 2, 1), net.IPv4(127, 0, 0, 1))

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

	url := fmt.Sprintf("http://rebind.test:%d/", canary.port())
	resp, err := client.Get(url)
	if resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
	}

	if hits := canary.hits(); hits != 0 {
		t.Fatalf("SSRF via DNS rebinding: %d connection(s) reached the private rebinding target", hits)
	}
	if n := dns.aQueryCount("rebind.test"); n != 1 {
		t.Fatalf("unsafe second resolution: expected exactly 1 A lookup for rebind.test (pinned dial), got %d", n)
	}
	if err == nil {
		t.Fatalf("expected the pinned dial to the (unreachable) approved public IP to fail closed")
	}
}

// TestSSRF_NoEnvironmentProxy asserts the security-sensitive transport never
// delegates connections to implicit environment proxies (HTTP_PROXY /
// HTTPS_PROXY / ALL_PROXY). An environment proxy would let the proxy itself
// reach blocked targets after NexaRoute has only validated (and pinned) the
// connection to the proxy, silently invalidating the SSRF guarantee.
func TestSSRF_NoEnvironmentProxy(t *testing.T) {
	tr := NewTransport()
	if tr.Proxy != nil {
		t.Fatalf("production transport must disable implicit environment proxying (Proxy must be nil), got %T", tr.Proxy)
	}
	tt := NewTestTransport()
	if tt.Proxy != nil {
		t.Fatalf("test transport must disable implicit environment proxying (Proxy must be nil), got %T", tt.Proxy)
	}
}
