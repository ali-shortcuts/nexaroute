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

// SSRF protection: reject loopback, private, link-local, metadata endpoints for production
func isBlockedHost(host string) bool {
	// Strip port
	if strings.Contains(host, ":") {
		h, _, err := net.SplitHostPort(host)
		if err == nil {
			host = h
		}
	}
	// Remove brackets for IPv6
	host = strings.Trim(host, "[]")

	// Check for metadata service
	if host == "169.254.169.254" || host == "metadata.google.internal" {
		return true
	}

	// Try parse IP
	ip := net.ParseIP(host)
	if ip != nil {
		// Loopback
		if ip.IsLoopback() {
			return true
		}
		// Private
		if ip.IsPrivate() {
			return true
		}
		// Link-local
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return true
		}
		// Unspecified
		if ip.IsUnspecified() {
			return true
		}
		// Multicast, etc.
		if ip.IsMulticast() {
			return true
		}
		// Check 0.0.0.0/8, etc.
		// Additional: 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 already covered by IsPrivate
		// 169.254.0.0/16 link-local already covered
		// 127.0.0.0/8 loopback covered
		return false
	}

	// For hostnames, block localhost and internal names? For production we require HTTPS and official domain
	// We block localhost explicitly
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if lower == "metadata" {
		return true
	}
	// Allow official Jev domain and testing domains via override, but still block private IPs after DNS resolution?
	// DNS resolution safety is best-effort: we check IP after lookup in dialer
	return false
}

// isLoopbackOrPrivateIP checks if resolved IP is blocked
func isLoopbackOrPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return true
	}
	return false
}

// NewTransport creates a secure transport with SSRF protections and TLS verification
func NewTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	// Custom dial context with DNS resolution safety
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// Check host before DNS
		if isBlockedHost(host) {
			return nil, fmt.Errorf("%w: %s", ErrSSRFBlocked, host)
		}
		// Resolve and check IPs
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if isLoopbackOrPrivateIP(ip) {
				// Allow 127.0.0.1 and ::1 only if explicitly testing? For production we block.
				// We check if host is jevai.org official — if so, allow after verification? But official should not resolve to private.
				// For safety, block private IPs
				// However, for tests with httptest server (127.0.0.1), we need to allow when BaseURL override is used
				// The caller will bypass this check for test mode via custom transport injection
				// Here we block to prevent SSRF in production
				// We allow if host is explicitly 127.0.0.1 or localhost for test? The spec says tests may inject httptest transport/base URL through internal constructor without weakening production validation.
				// So in production transport we block, but test transport will be custom and bypass
				return nil, fmt.Errorf("%w: %s resolves to private %s", ErrSSRFBlocked, host, ip.String())
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// No InsecureSkipVerify
		},
	}
}

// NewTestTransport creates a transport for tests that allows loopback (httptest) but still has timeouts and TLS config
func NewTestTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // no proxy for tests
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          10,
		IdleConnTimeout:       10 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

// ValidateBaseURL validates BaseURL for SSRF and HTTPS requirements
// Production must be HTTPS and official domain or safe
func ValidateBaseURL(raw string, allowHTTPForTest bool) error {
	if raw == "" {
		return nil // default will be used
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: invalid url", ErrInvalidConfig)
	}
	if u.Scheme != "https" && !(allowHTTPForTest && u.Scheme == "http") {
		return fmt.Errorf("%w: scheme must be https", ErrInvalidConfig)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: host required", ErrInvalidConfig)
	}
	if isBlockedHost(u.Host) && !allowHTTPForTest {
		return fmt.Errorf("%w: blocked host %s", ErrSSRFBlocked, u.Host)
	}
	// Reject URLs with credentials
	if u.User != nil {
		return fmt.Errorf("%w: userinfo not allowed", ErrInvalidConfig)
	}
	return nil
}
