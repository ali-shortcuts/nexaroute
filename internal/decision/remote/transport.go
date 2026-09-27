package remote

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// SSRF protection for the outbound remote-decision transport.
//
// Policy (fail closed):
//
//   - Hosts are checked before resolution: loopback (v4/v6), localhost and
//     *.localhost, cloud metadata names, and malformed or numeric-lookalike
//     IP spellings are rejected before any lookup.
//   - Every resolved address is validated: loopback, RFC1918, IPv6 unique-local
//     (fc00::/7), link-local, multicast, unspecified, reserved ranges and
//     metadata endpoints are rejected. If ANY answer is blocked the whole
//     resolution is rejected (mixed public/private answers never pass).
//   - The connection is opened ONLY to an IP literal that was resolved and
//     approved above. The original hostname is never passed to the dialer, so
//     no second DNS lookup can happen at connect time. This closes the DNS
//     rebinding / TOCTOU window (lookup #1 sees a public IP, lookup #2 returns
//     a private one): at most one lookup feeds the connection, and it is the
//     one that was validated.
//
// TLS is unaffected by pinning: net/http still performs the handshake against
// the original request hostname (SNI + certificate verification), because the
// request URL is never rewritten. Host header semantics are likewise
// preserved.

// blockedPrefixes lists additional non-routable/reserved ranges beyond the
// category checks in isBlockedIP. 169.254.169.254 (cloud metadata) is covered
// by the link-local check.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network" (RFC 1122)
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT shared space (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking (RFC 2544)
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved + broadcast (RFC 1112)
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known prefix (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (RFC 8215)
	netip.MustParsePrefix("100::/64"),       // discard-only (RFC 6666)
}

// isBlockedIP reports whether an address may not be connected to. IPv4-mapped
// IPv6 addresses (::ffff:x.x.x.x) are unmapped first so they cannot evade the
// IPv4 checks.
func isBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	// Covers unspecified, loopback (127.0.0.0/8, ::1), link-local
	// (169.254.0.0/16 incl. 169.254.169.254, fe80::/10) and multicast.
	if !ip.IsGlobalUnicast() {
		return true
	}
	// RFC 1918 + IPv6 unique-local addresses (fc00::/7).
	if ip.IsPrivate() {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// parseIPLiteral parses host as an IP literal (optionally with a zone, e.g.
// fe80::1%eth0). Brackets and ports must already have been removed.
func parseIPLiteral(host string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// isNumericLookalike reports whether a hostname is a degenerate "IP" spelling
// that some resolvers (libc getaddrinfo, proxies, browsers) reinterpret as an
// address: "2130706433", "127.1", "0177.0.0.1", "017700000001", "0x7f000001",
// "0x7f.0x0.0x0.0x1", "1.2.3.04". None of these parse as strict Go IP
// literals, so they would otherwise take the hostname path.
func isNumericLookalike(host string) bool {
	onlyDigitsDots := true
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !(c >= '0' && c <= '9' || c == '.') {
			onlyDigitsDots = false
			break
		}
	}
	if onlyDigitsDots {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if strings.HasPrefix(label, "0x") {
			return true
		}
	}
	return false
}

// checkDialHost applies the host-level SSRF policy to a hostname or IP
// literal. Blocked targets return an error wrapping ErrSSRFBlocked; malformed
// hosts return an error wrapping ErrInvalidConfig. Zone identifiers are only
// valid on IP literals.
func checkDialHost(host string) error {
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrInvalidConfig)
	}
	if ip, ok := parseIPLiteral(host); ok {
		if isBlockedIP(ip) {
			return fmt.Errorf("%w: %s", ErrSSRFBlocked, host)
		}
		return nil
	}

	h := strings.ToLower(strings.TrimRight(host, "."))
	if h == "" {
		return fmt.Errorf("%w: empty host", ErrInvalidConfig)
	}
	if strings.Contains(h, "%") {
		return fmt.Errorf("%w: malformed host", ErrInvalidConfig)
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("%w: malformed host", ErrInvalidConfig)
		}
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return fmt.Errorf("%w: malformed host", ErrInvalidConfig)
		}
	}

	// Blocked names: loopback aliases and cloud metadata endpoints.
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("%w: %s", ErrSSRFBlocked, host)
	}
	if h == "metadata" || h == "metadata.google.internal" || strings.HasSuffix(h, ".metadata.google.internal") {
		return fmt.Errorf("%w: %s", ErrSSRFBlocked, host)
	}
	// Degenerate numeric "IP" spellings must not reach a resolver.
	if isNumericLookalike(h) {
		return fmt.Errorf("%w: %s", ErrSSRFBlocked, host)
	}
	return nil
}

// resolveFunc resolves a hostname to its addresses. It is the seam used by
// tests to inject deterministic DNS behavior; production always uses
// net.DefaultResolver (see defaultResolve).
type resolveFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

// dialFunc opens a connection to addr. In the production path addr is always
// an approved IP literal ("1.2.3.4:443", "[2001:db8::1]:443").
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// transportConfig carries the small internal injection points for tests.
// There are no production switches here: NewTransport always uses the secure
// defaults (real DNS, real dialer, verified TLS).
type transportConfig struct {
	resolve   resolveFunc // nil -> defaultResolve
	dial      dialFunc    // nil -> net.Dialer with production timeouts
	tlsConfig *tls.Config // nil -> defaultTLSConfig (TLS 1.2+, verification on)
}

func defaultResolve(ctx context.Context, host string) ([]net.IPAddr, error) {
	// Read net.DefaultResolver at call time (never cached) so the resolver is
	// always the process-wide one.
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// ssrfDialContext returns the SSRF-hardened dialer: one resolution per
// connection attempt, every answer validated, and the connection made only to
// an approved IP literal. The hostname is never dialed.
func ssrfDialContext(resolve resolveFunc, dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed dial address", ErrInvalidConfig)
		}

		// IP literal: validate and dial exactly that address (no DNS).
		if ip, ok := parseIPLiteral(host); ok {
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("%w: %s", ErrSSRFBlocked, host)
			}
			return dial(ctx, network, net.JoinHostPort(ip.String(), port))
		}

		// Hostname: pre-DNS policy, single resolution, validate all answers.
		if err := checkDialHost(host); err != nil {
			return nil, err
		}
		addrs, err := resolve(ctx, host)
		if err != nil {
			// DNS failures fail closed.
			return nil, err
		}
		approved := make([]string, 0, len(addrs))
		for _, a := range addrs {
			ip, ok := netip.AddrFromSlice(a.IP)
			if !ok {
				return nil, fmt.Errorf("%w: %s resolved to unparsable address", ErrSSRFBlocked, host)
			}
			ip = ip.Unmap()
			if a.Zone != "" {
				ip = ip.WithZone(a.Zone)
			}
			if isBlockedIP(ip) {
				// Fail closed on mixed answers: one blocked record rejects
				// the entire resolution.
				return nil, fmt.Errorf("%w: %s resolves to blocked %s", ErrSSRFBlocked, host, ip)
			}
			approved = append(approved, net.JoinHostPort(ip.String(), port))
		}
		if len(approved) == 0 {
			return nil, fmt.Errorf("%w: %s resolved to no addresses", ErrUnavailable, host)
		}

		// Pin: dial exactly the approved IP literals, in order. The original
		// hostname is never re-resolved here (DNS rebinding TOCTOU closed).
		var lastErr error
		for _, pinned := range approved {
			conn, err := dial(ctx, network, pinned)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}
}

func defaultTLSConfig() *tls.Config {
	// Verification on: no InsecureSkipVerify, no custom VerifyConnection that
	// could weaken chain validation. ServerName is supplied by net/http from
	// the original request hostname (SNI + certificate check), which pinning
	// does not alter.
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
}

// newTransport builds the hardened transport from internal test seams.
func newTransport(cfg transportConfig) *http.Transport {
	resolve := cfg.resolve
	if resolve == nil {
		resolve = defaultResolve
	}
	dial := cfg.dial
	if dial == nil {
		d := &net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		dial = d.DialContext
	}
	tlsConfig := cfg.tlsConfig
	if tlsConfig == nil {
		tlsConfig = defaultTLSConfig()
	}

	return &http.Transport{
		// Proxy policy: implicit environment proxying (HTTP_PROXY, HTTPS_PROXY,
		// ALL_PROXY and NO_PROXY) is DISABLED for this security-sensitive
		// client. Rationale: with a proxy, NexaRoute would validate and pin the
		// connection to the *proxy*, while the proxy itself can reach blocked
		// targets (loopback, RFC1918, 169.254.169.254 metadata, ...) after the
		// SSRF check has already passed. That silently invalidates the SSRF
		// guarantee, so the only safe policy for this client is direct
		// connections. Operators who must traverse a proxy should do so via an
		// egress gateway that enforces its own policy; do not re-enable
		// http.ProxyFromEnvironment here.
		Proxy:                 nil,
		DialContext:           ssrfDialContext(resolve, dial),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}
}

// NewTransport creates the production transport for the remote-decision
// client: SSRF-hardened (IP-pinned dialing, see above), TLS verification on,
// HTTP/2 enabled, and no environment proxying.
func NewTransport() *http.Transport {
	return newTransport(transportConfig{})
}

// NewTestTransport creates a transport for tests that allows loopback
// (httptest) but still has timeouts and TLS config.
//
// NOTE: this is a test-only seam and must never be used in production code;
// it intentionally does not apply SSRF dial pinning so httptest servers work.
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

// ValidateBaseURL validates BaseURL for SSRF and HTTPS requirements.
// Production must be https and must not point at blocked hosts; URLs with
// userinfo (credentials) are always rejected.
func ValidateBaseURL(raw string, allowHTTPForTest bool) error {
	if raw == "" {
		return nil // default will be used
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: invalid url", ErrInvalidConfig)
	}
	// Reject URLs with credentials (userinfo).
	if u.User != nil {
		return fmt.Errorf("%w: userinfo not allowed", ErrInvalidConfig)
	}
	if u.Scheme != "https" && !(allowHTTPForTest && u.Scheme == "http") {
		return fmt.Errorf("%w: scheme must be https", ErrInvalidConfig)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: host required", ErrInvalidConfig)
	}
	if !allowHTTPForTest {
		if err := checkDialHost(u.Hostname()); err != nil {
			return err
		}
	}
	return nil
}
