package remote

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type lookupIPFunc func(context.Context, string, string) ([]net.IP, error)
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

var blockedIPv4Nets = mustCIDRs(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
)

var blockedIPv6Nets = mustCIDRs(
	"::/128",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
	"2001:db8::/32",
)

func mustCIDRs(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, n, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

// isBlockedHost performs the non-DNS portion of SSRF validation.
func isBlockedHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	lower := strings.ToLower(host)
	switch lower {
	case "localhost", "metadata", "metadata.google.internal":
		return true
	}
	if strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isLoopbackOrPrivateIP(ip)
	}
	return false
}

// isLoopbackOrPrivateIP rejects local, private, link-local, multicast,
// unspecified and other special-use addresses which must never be reachable by
// the production remote-decision client.
func isLoopbackOrPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range blockedIPv4Nets {
			if n.Contains(v4) {
				return true
			}
		}
		return false
	}
	ip = ip.To16()
	if ip == nil {
		return true
	}
	for _, n := range blockedIPv6Nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func secureDialContext(lookup lookupIPFunc, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		host = strings.Trim(host, "[]")
		if isBlockedHost(host) {
			return nil, fmt.Errorf("%w: %s", ErrSSRFBlocked, host)
		}

		ips, err := lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%w: %s resolved to no addresses", ErrSSRFBlocked, host)
		}

		// Reject the hostname entirely if DNS returns any blocked address. This
		// prevents mixed-answer tricks and ensures an attacker cannot influence
		// which answer the connector happens to choose.
		safe := make([]net.IP, 0, len(ips))
		for _, ip := range ips {
			if isLoopbackOrPrivateIP(ip) {
				return nil, fmt.Errorf("%w: %s resolves to blocked address %s", ErrSSRFBlocked, host, ip.String())
			}
			safe = append(safe, append(net.IP(nil), ip...))
		}

		var lastErr error
		for _, ip := range safe {
			// Critical DNS-rebinding invariant: connect to the exact IP that was
			// validated above. Never pass the hostname back to net.Dialer.
			conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no safe address available")
		}
		return nil, lastErr
	}
}

func newTransportWithDeps(lookup lookupIPFunc, dial dialContextFunc) *http.Transport {
	// Intentionally do not use ProxyFromEnvironment here. An implicit HTTP
	// proxy would move target resolution to the proxy and would make NexaRoute's
	// DNS pinning unable to enforce the SSRF guarantee.
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           secureDialContext(lookup, dial),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// ServerName intentionally remains empty: net/http derives TLS SNI
			// and certificate verification from the original request hostname,
			// while DialContext connects to the pinned validated IP.
		},
	}
}

// NewTransport creates the production transport with DNS-pinned SSRF
// protection and normal TLS certificate verification.
func NewTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return newTransportWithDeps(net.DefaultResolver.LookupIP, dialer.DialContext)
}

// NewTestTransport allows loopback httptest servers. It is never used by the
// production config-driven path.
func NewTestTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          10,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// ValidateBaseURL validates the static portion of the target. DNS-dependent
// validation is repeated at dial time and the connection is pinned to the
// validated IP, closing the DNS-rebinding window.
func ValidateBaseURL(raw string, allowHTTPForTest bool) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: invalid url", ErrInvalidConfig)
	}
	if u.Scheme != "https" && !(allowHTTPForTest && u.Scheme == "http") {
		return fmt.Errorf("%w: scheme must be https", ErrInvalidConfig)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%w: host required", ErrInvalidConfig)
	}
	if u.User != nil {
		return fmt.Errorf("%w: userinfo not allowed", ErrInvalidConfig)
	}
	if isBlockedHost(u.Hostname()) && !allowHTTPForTest {
		return fmt.Errorf("%w: blocked host %s", ErrSSRFBlocked, u.Hostname())
	}
	return nil
}
