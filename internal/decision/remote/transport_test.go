package remote

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

func pipeConn() net.Conn {
	a, b := net.Pipe()
	go func() {
		_ = b.Close()
	}()
	return a
}

func TestSecureDialPinsValidatedDNSAddress(t *testing.T) {
	var lookups atomic.Int32
	var dialed string
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		lookups.Add(1)
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	dial := func(_ context.Context, _ string, addr string) (net.Conn, error) {
		dialed = addr
		return pipeConn(), nil
	}

	conn, err := secureDialContext(lookup, dial)(context.Background(), "tcp", "safe.example:443")
	if err != nil {
		t.Fatalf("secure dial failed: %v", err)
	}
	_ = conn.Close()
	if lookups.Load() != 1 {
		t.Fatalf("DNS lookups=%d want 1", lookups.Load())
	}
	if dialed != "8.8.8.8:443" {
		t.Fatalf("dial address=%q; hostname was re-used or wrong IP pinned", dialed)
	}
}

func TestSecureDialRejectsMixedPublicPrivateAnswers(t *testing.T) {
	var dials atomic.Int32
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return pipeConn(), nil
	}
	_, err := secureDialContext(lookup, dial)(context.Background(), "tcp", "safe.example:443")
	if err == nil || !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("mixed DNS answer error=%v want ErrSSRFBlocked", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("dial occurred despite blocked DNS answer")
	}
}

func TestSecureDialTriesOnlyValidatedSafeIPs(t *testing.T) {
	lookup := func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")}, nil
	}
	var got []string
	dial := func(_ context.Context, _ string, addr string) (net.Conn, error) {
		got = append(got, addr)
		if strings.HasPrefix(addr, "8.8.8.8:") {
			return nil, errors.New("first address unavailable")
		}
		return pipeConn(), nil
	}
	conn, err := secureDialContext(lookup, dial)(context.Background(), "tcp", "safe.example:443")
	if err != nil {
		t.Fatalf("safe fallback dial failed: %v", err)
	}
	_ = conn.Close()
	if len(got) != 2 || got[0] != "8.8.8.8:443" || got[1] != "1.1.1.1:443" {
		t.Fatalf("dial sequence=%v", got)
	}
}

func TestSecureDialResolverFailureAndCancellation(t *testing.T) {
	lookupErr := errors.New("resolver failed")
	lookup := func(context.Context, string, string) ([]net.IP, error) { return nil, lookupErr }
	_, err := secureDialContext(lookup, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial should not run after resolver failure")
		return nil, nil
	})(context.Background(), "tcp", "safe.example:443")
	if !errors.Is(err, lookupErr) {
		t.Fatalf("resolver error=%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lookup2 := func(ctx context.Context, _, _ string) ([]net.IP, error) {
		return nil, ctx.Err()
	}
	_, err = secureDialContext(lookup2, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial should not run after cancellation")
		return nil, nil
	})(ctx, "tcp", "safe.example:443")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestBlockedAddressFamilies(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1",
		"169.254.169.254", "172.16.1.1", "192.168.1.1", "224.0.0.1",
		"::", "::1", "fc00::1", "fe80::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254",
	}
	for _, raw := range blocked {
		if !isLoopbackOrPrivateIP(net.ParseIP(raw)) {
			t.Errorf("%s was not blocked", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if isLoopbackOrPrivateIP(net.ParseIP(raw)) {
			t.Errorf("public IP %s was blocked", raw)
		}
	}
}

func TestValidateBaseURLSecurityRules(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1/x",
		"https://[::1]/x",
		"https://169.254.169.254/latest/meta-data",
		"https://metadata.google.internal/",
		"https://localhost/",
		"https://foo.localhost/",
		"https://user:pass@example.com/",
	} {
		if err := ValidateBaseURL(raw, false); err == nil {
			t.Errorf("ValidateBaseURL(%q) unexpectedly allowed", raw)
		}
	}
	if err := ValidateBaseURL("https://example.com/api", false); err != nil {
		t.Fatalf("safe hostname rejected: %v", err)
	}
}

func TestProductionTransportDoesNotUseImplicitProxyOrDisableTLSVerification(t *testing.T) {
	tr := NewTransport()
	if tr.Proxy != nil {
		t.Fatal("production remote transport must not use an implicit environment proxy")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLS config missing")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("production transport disables TLS certificate verification")
	}
	if tr.TLSClientConfig.ServerName != "" {
		t.Fatalf("ServerName=%q: it must be derived by net/http from the original request hostname", tr.TLSClientConfig.ServerName)
	}
}
