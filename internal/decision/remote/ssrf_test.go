package remote

// SSRF policy regression tests: direct dial policy (literals, hostnames,
// numeric-lookalike spellings), DNS pinning, mixed answers, userinfo and
// resolver failures. All tests are deterministic and offline: DNS behavior is
// injected through the internal resolver seam.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// mustNotResolve returns a resolver that fails the test if it is ever used.
func mustNotResolve(t *testing.T) resolveFunc {
	t.Helper()
	return func(ctx context.Context, host string) ([]net.IPAddr, error) {
		t.Errorf("resolver must not be called (pre-DNS rejection) for %q", host)
		return nil, fmt.Errorf("unexpected resolve of %q", host)
	}
}

func assertBlockedDial(t *testing.T, addr string) {
	t.Helper()
	rec := &recordDialer{}
	d := ssrfDialContext(mustNotResolve(t), rec.dial)
	conn, err := d(context.Background(), "tcp", addr)
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("%s: expected rejection, got a connection", addr)
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("%s: expected ErrSSRFBlocked, got %v", addr, err)
	}
	if rec.count() != 0 {
		t.Fatalf("%s: dialer must not be called for blocked targets, got %v", addr, rec.addrs())
	}
}

// 1. IPv4 loopback / IPv6 loopback rejected.
func TestSSRF_LoopbackRejected(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:80",
		"127.1.2.3:443",
		"127.255.255.255:80",
		"[::1]:80",
		"[::1]:443",
		"[0:0:0:0:0:0:0:1]:80",
		"[::ffff:127.0.0.1]:80", // IPv4-mapped loopback
	} {
		assertBlockedDial(t, addr)
	}
}

// localhost and *.localhost (all spellings, incl. trailing dot and case).
func TestSSRF_LocalhostNamesRejected(t *testing.T) {
	for _, addr := range []string{
		"localhost:80",
		"LOCALHOST:80",
		"LocalHost:443",
		"localhost.:80", // trailing-dot FQDN form
		"foo.localhost:80",
		"a.b.localhost:443",
		"LOCALHOST.:80",
	} {
		assertBlockedDial(t, addr)
	}
}

// 2. RFC1918 rejected.
func TestSSRF_RFC1918Rejected(t *testing.T) {
	for _, addr := range []string{
		"10.0.0.1:80",
		"10.255.255.255:443",
		"172.16.0.1:80",
		"172.31.255.255:80",
		"192.168.0.1:80",
		"192.168.255.255:443",
	} {
		assertBlockedDial(t, addr)
	}
}

// 3+4. Link-local (incl. the 169.254.169.254 cloud metadata IP) rejected.
func TestSSRF_LinkLocalAndMetadataIPRejected(t *testing.T) {
	for _, addr := range []string{
		"169.254.0.1:80",
		"169.254.169.254:80",
		"169.254.169.254:443",
		"[fe80::1]:80",
		"[fe80::1%eth0]:80", // zone form
		"[::ffff:169.254.169.254]:80",
	} {
		assertBlockedDial(t, addr)
	}
}

// 5. Metadata hostnames rejected.
func TestSSRF_MetadataHostnamesRejected(t *testing.T) {
	for _, addr := range []string{
		"metadata.google.internal:80",
		"Metadata.Google.Internal:443",
		"metadata.google.internal.:80", // trailing dot
		"foo.metadata.google.internal:80",
		"metadata:80",
		"METADATA:80",
	} {
		assertBlockedDial(t, addr)
	}
}

// Multicast / unspecified / reserved / CGNAT rejected (v4+v6).
func TestSSRF_MulticastUnspecifiedReservedRejected(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:80",
		"[::]:80",
		"224.0.0.1:80",
		"239.255.255.255:80",
		"[ff02::1]:80",
		"[ff01::1]:80",
		"255.255.255.255:80",
		"100.64.0.1:80",
		"192.0.0.1:80",
		"198.18.0.1:80",
		"240.0.0.1:80",
	} {
		assertBlockedDial(t, addr)
	}
}

// IPv6 unique-local (fc00::/7) rejected.
func TestSSRF_IPv6UniqueLocalRejected(t *testing.T) {
	for _, addr := range []string{
		"[fd00::1]:80",
		"[fc00::1]:443",
		"[fd12:3456:789a::1]:80",
		"[::ffff:10.0.0.1]:80", // mapped RFC1918
	} {
		assertBlockedDial(t, addr)
	}
}

// Alternate/odd IP spellings that libc-style parsers accept must be rejected
// as suspicious numeric lookalikes before any resolver can reinterpret them.
func TestSSRF_NumericLookalikeHostsRejected(t *testing.T) {
	for _, addr := range []string{
		"2130706433:80",   // decimal 127.0.0.1
		"017700000001:80", // octal 127.0.0.1
		"0x7f000001:80",   // hex 127.0.0.1
		"0X7F000001:80",   // hex uppercase
		"127.1:80",        // short form 127.0.0.1
		"0177.0.0.1:80",   // octal-dotted
		"0x7f.0x0.0x0.0x1:80",
		"1.2.3.04:80", // leading-zero dotted
		"0:80",        // 0.0.0.0 short
	} {
		assertBlockedDial(t, addr)
	}
}

// Malformed host:port rejected fail-closed.
func TestSSRF_MalformedAddressesRejected(t *testing.T) {
	for _, addr := range []string{
		"no-port",
		"a:b:c:80",
		"[::1",
		"",
		"bad host:80",
		"ho%st:80",
	} {
		rec := &recordDialer{}
		d := ssrfDialContext(mustNotResolve(t), rec.dial)
		conn, err := d(context.Background(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil {
			t.Fatalf("%q: expected error for malformed address", addr)
		}
		var e *Error
		if errors.As(err, &e) && e.Kind == ErrSSRFBlocked {
			t.Fatalf("%q: malformed must not be classified as SSRF host block: %v", addr, err)
		}
		if !errors.Is(err, ErrInvalidConfig) && !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("%q: expected ErrInvalidConfig (or SSRF), got %v", addr, err)
		}
		if rec.count() != 0 {
			t.Fatalf("%q: dialer must not be called", addr)
		}
	}
}

// 6. Safe public IP literal accepted (dial reached, exact address preserved).
func TestSSRF_SafePublicIPAccepted(t *testing.T) {
	for _, addr := range []string{
		"8.8.8.8:443",
		"93.184.216.34:8443",
		"[2606:2800:220:1:248:1893:25c8:1946]:443",
	} {
		rec := &recordDialer{}
		d := ssrfDialContext(mustNotResolve(t), rec.dial)
		conn, err := d(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", addr, err)
		}
		_ = conn.Close()
		if got := rec.addrs(); len(got) != 1 || got[0] != addr {
			t.Fatalf("%s: expected dial of exactly %s, got %v", addr, addr, got)
		}
	}
}

// 7. Hostname resolving only to safe public IPs accepted, dial pinned to the
// approved IP literal (never the hostname).
func TestSSRF_HostnameSafePublicAccepted_Pinned(t *testing.T) {
	resolver := newFakeResolver()
	resolver.add("api.example.com", "93.184.216.34")
	rec := &recordDialer{}
	d := ssrfDialContext(resolver.lookupIPAddr, rec.dial)

	conn, err := d(context.Background(), "tcp", "api.example.com:443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = conn.Close()
	if got := rec.addrs(); len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("expected pinned dial to 93.184.216.34:443, got %v", got)
	}
	if resolver.callCount() != 1 {
		t.Fatalf("expected exactly 1 lookup, got %d", resolver.callCount())
	}
}

// Hostname resolving to a blocked IP is rejected even though the name itself
// is fine.
func TestSSRF_HostnameResolvingPrivateRejected(t *testing.T) {
	cases := map[string]string{
		"internal.test":  "10.0.0.5",
		"loop.test":      "127.0.0.1",
		"meta.test":      "169.254.169.254",
		"ula.test":       "fd00::1",
		"ll6.test":       "fe80::1",
		"mapped6.test":   "::ffff:192.168.1.1",
		"unspec.test":    "0.0.0.0",
		"multicast.test": "224.0.0.1",
	}
	for host, ip := range cases {
		resolver := newFakeResolver()
		resolver.add(host, ip)
		rec := &recordDialer{}
		d := ssrfDialContext(resolver.lookupIPAddr, rec.dial)
		_, err := d(context.Background(), "tcp", host+":80")
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("%s -> %s: expected ErrSSRFBlocked, got %v", host, ip, err)
		}
		if rec.count() != 0 {
			t.Fatalf("%s: dialer must not be called, got %v", host, rec.addrs())
		}
	}
}

// 8. DNS rebinding simulation at the dial level: the resolver is consulted
// exactly once per connection attempt and the dial receives only the pinned,
// approved IP. A second lookup (which would return loopback) can never happen.
func TestSSRF_DNSRebinding_ResolverCalledOnce_PinnedDial(t *testing.T) {
	resolver := newFakeResolver()
	resolver.add("rebind.test", "93.184.216.34") // lookup #1: public
	resolver.add("rebind.test", "127.0.0.1")     // lookup #2 would rebind
	rec := &recordDialer{err: errors.New("dial refused")}
	d := ssrfDialContext(resolver.lookupIPAddr, rec.dial)

	_, err := d(context.Background(), "tcp", "rebind.test:443")
	if err == nil {
		t.Fatalf("expected dial error")
	}
	if resolver.callCount() != 1 {
		t.Fatalf("unsafe second resolution: resolver called %d times", resolver.callCount())
	}
	got := rec.addrs()
	if len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("expected pinned dial to 93.184.216.34:443, got %v", got)
	}
}

// 9. Mixed public/private DNS answers: one blocked record rejects the whole
// resolution, regardless of order.
func TestSSRF_MixedDNSAnswersRejected(t *testing.T) {
	for _, batch := range [][]string{
		{"93.184.216.34", "10.0.0.1"},
		{"10.0.0.1", "93.184.216.34"},
		{"93.184.216.34", "127.0.0.1", "93.184.216.35"},
		{"8.8.8.8", "fd00::1"},
		{"8.8.8.8", "169.254.169.254"},
	} {
		resolver := newFakeResolver()
		resolver.add("mixed.test", batch...)
		rec := &recordDialer{}
		d := ssrfDialContext(resolver.lookupIPAddr, rec.dial)
		_, err := d(context.Background(), "tcp", "mixed.test:443")
		if !errors.Is(err, ErrSSRFBlocked) {
			t.Fatalf("%v: expected ErrSSRFBlocked, got %v", batch, err)
		}
		if rec.count() != 0 {
			t.Fatalf("%v: dialer must not be called on mixed answers, got %v", batch, rec.addrs())
		}
	}
}

// 14. Resolver failure fails closed.
func TestSSRF_ResolverFailureFailsClosed(t *testing.T) {
	resolver := newFakeResolver()
	resolver.setErr(errors.New("dns backend down"))
	rec := &recordDialer{}
	d := ssrfDialContext(resolver.lookupIPAddr, rec.dial)
	_, err := d(context.Background(), "tcp", "whatever.test:443")
	if err == nil {
		t.Fatalf("expected error on resolver failure")
	}
	if rec.count() != 0 {
		t.Fatalf("dialer must not be called when resolution fails, got %v", rec.addrs())
	}

	// Empty answer set is also fail-closed.
	resolver2 := newFakeResolver()
	rec2 := &recordDialer{}
	d2 := ssrfDialContext(resolver2.lookupIPAddr, rec2.dial)
	_, err = d2(context.Background(), "tcp", "empty.test:443")
	if err == nil {
		t.Fatalf("expected error on empty resolution")
	}
	if rec2.count() != 0 {
		t.Fatalf("dialer must not be called on empty resolution")
	}
}

// 15. Connection failure across multiple safe IPs: every approved IP is
// attempted in order and the final error is returned.
func TestSSRF_MultipleSafeIPs_AllTriedOnFailure(t *testing.T) {
	resolver := newFakeResolver()
	resolver.add("multi.test", "192.0.2.1", "192.0.2.2", "192.0.2.3")
	rec := &recordDialer{err: errors.New("connection refused")}
	d := ssrfDialContext(resolver.lookupIPAddr, rec.dial)

	_, err := d(context.Background(), "tcp", "multi.test:443")
	if err == nil {
		t.Fatalf("expected error when all dials fail")
	}
	want := []string{"192.0.2.1:443", "192.0.2.2:443", "192.0.2.3:443"}
	got := rec.addrs()
	if len(got) != len(want) {
		t.Fatalf("expected %d dial attempts %v, got %v", len(want), want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected dials %v, got %v", want, got)
		}
	}
}

// Partial failure: first IP fails, second succeeds; only approved IP literals
// are dialed and the loop stops at the first success.
func TestSSRF_MultipleSafeIPs_FailoverToSecond(t *testing.T) {
	resolver := newFakeResolver()
	resolver.add("multi.test", "192.0.2.1", "192.0.2.2", "192.0.2.3")
	rec := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		if addr == "192.0.2.1:443" {
			return nil, errors.New("connection refused")
		}
		return stubConn{}, nil
	}}
	d := ssrfDialContext(resolver.lookupIPAddr, rec.dial)

	conn, err := d(context.Background(), "tcp", "multi.test:443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = conn.Close()
	got := rec.addrs()
	if len(got) != 2 || got[0] != "192.0.2.1:443" || got[1] != "192.0.2.2:443" {
		t.Fatalf("expected dials [192.0.2.1:443 192.0.2.2:443], got %v", got)
	}
}

// 17. Context cancellation/deadline is honored during resolution and dialing.
func TestSSRF_ContextCancellation(t *testing.T) {
	// Cancellation during lookup.
	blockingResolve := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	d := ssrfDialContext(blockingResolve, (&recordDialer{}).dial)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d(ctx, "tcp", "slow.test:443")
	if err == nil {
		t.Fatalf("expected error on canceled context")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation not honored promptly: took %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context error, got %v", err)
	}

	// Cancellation during dial (with a safe, approved host).
	resolver := newFakeResolver()
	resolver.add("slow.test", "93.184.216.34")
	slowDial := &recordDialer{hook: func(network, addr string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	d2 := ssrfDialContext(resolver.lookupIPAddr, slowDial.dial)
	_, err = d2(ctx, "tcp", "slow.test:443")
	if err == nil {
		t.Fatalf("expected error on canceled dial")
	}
}

// 13. URL userinfo (credentials) rejected everywhere.
func TestSSRF_UserinfoRejected(t *testing.T) {
	for _, raw := range []string{
		"https://user:pass@www.jevai.org/",
		"https://user@www.jevai.org/",
		"https://user:pass@127.0.0.1:8443/",
		"https://user@safe.example:8443/path",
	} {
		if err := ValidateBaseURL(raw, false); err == nil {
			t.Fatalf("%s: expected rejection of userinfo", raw)
		}
		if err := ValidateBaseURL(raw, true); err == nil {
			t.Fatalf("%s: expected rejection of userinfo in test mode", raw)
		}
		if _, err := NewClient(ClientConfig{BaseURL: raw, APIKey: "k"}); err == nil {
			t.Fatalf("%s: NewClient must reject userinfo", raw)
		}
	}
}

// Base URL validation: production requires https and safe hosts; test mode
// allows http and loopback.
func TestSSRF_ValidateBaseURL(t *testing.T) {
	if err := ValidateBaseURL("https://www.jevai.org", false); err != nil {
		t.Fatalf("safe https base must pass: %v", err)
	}
	if err := ValidateBaseURL("", false); err != nil {
		t.Fatalf("empty base (default) must pass: %v", err)
	}
	if err := ValidateBaseURL("http://www.jevai.org", false); err == nil {
		t.Fatalf("http must fail in production mode")
	}
	if err := ValidateBaseURL("http://127.0.0.1:9000", false); err == nil {
		t.Fatalf("loopback must fail in production mode")
	}
	if err := ValidateBaseURL("https://169.254.169.254/", false); err == nil {
		t.Fatalf("metadata IP must fail in production mode")
	}
	if err := ValidateBaseURL("https://metadata.google.internal/", false); err == nil {
		t.Fatalf("metadata host must fail in production mode")
	}
	if err := ValidateBaseURL("http://127.0.0.1:9000", true); err != nil {
		t.Fatalf("test mode must allow http loopback: %v", err)
	}
	if err := ValidateBaseURL("not a url ://", false); err == nil {
		t.Fatalf("invalid URL must fail")
	}
}
