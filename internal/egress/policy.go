package egress

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

// Policy is the provider egress boundary. AllowedHosts is an optional exact
// host allow-list; AllowLoopback permits only explicitly local HTTP providers.
// DenyHosts and DenyCIDRs are operator-configured deny lists that take
// precedence over everything except the built-in special-purpose blocklist.
type Policy struct {
	AllowedHosts  []string
	AllowLoopback bool
	DenyHosts     []string
	DenyCIDRs     []netip.Prefix
}

// deniedHost reports whether a hostname is on the operator deny-list.
func (p Policy) deniedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, denied := range p.DenyHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(denied), "."), host) {
			return true
		}
	}
	return false
}

func (p Policy) allowsHost(host string) bool {
	if len(p.AllowedHosts) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, allowed := range p.AllowedHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(allowed), "."), host) {
			return true
		}
	}
	return false
}

// blockedPrefixes lists special-purpose ranges that global-unicast and private
// checks do not cover (IANA special-purpose registries, RFC 6890 and friends).
// None of these are legitimate LLM provider addresses.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC 1122)
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT shared space (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 (RFC 5737)
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast (RFC 7526)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 (RFC 5737)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved + limited broadcast (RFC 1112)
	netip.MustParsePrefix("::/96"),           // IPv4-compatible / unspecified / ::1 (deprecated)
	netip.MustParsePrefix("100::/64"),        // discard-only (RFC 6666)
	netip.MustParsePrefix("2001::/32"),       // Teredo (RFC 4380)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation (RFC 3849)
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local (RFC 3879)
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64 local-use (RFC 8215)
}

var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052
	sixToFour      = netip.MustParsePrefix("2002::/16")    // RFC 3056
)

// embeddedIPv4 returns the IPv4 address carried inside a NAT64 (64:ff9b::/96)
// or 6to4 (2002::/16) address. These forms are legitimate on IPv6-only
// networks, so they are judged by the IPv4 address they translate to rather
// than rejected outright; "public name -> synthesized NAT64 answer" keeps
// working while "NAT64 form of 169.254.169.254" does not.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case nat64WellKnown.Contains(ip):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

// BlockedIP reports whether ip is blocked by the built-in special-purpose
// blocklist, judging NAT64 / 6to4 forms by the IPv4 address they embed
// (embedded loopback is never an allowed form). It is the shared judgment
// for callers outside this package that must classify a bare address
// without an operator Policy — e.g. the remote-decision transport (#427,
// audit F5: the two implementations had drifted and the decision transport
// allowed 6to4/Teredo embeddings of blocked addresses).
func BlockedIP(ip netip.Addr) bool {
	return blocked(ip, false)
}

func blocked(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() {
		return true
	}
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	if inner, ok := embeddedIPv4(ip); ok {
		// Embedded loopback is never an allowed form, even with allowLoopback.
		return blocked(inner, false)
	}
	return false
}

// ipBlocked reports whether dialing ip is forbidden under this
// policy: the operator deny list applies to the address itself AND
// to any IPv4 address embedded in it (NAT64 / 6to4), and the
// built-in special-purpose blocklist applies via blocked (which
// itself judges embedded addresses). The deny list is documented
// to take precedence over everything except the built-in
// blocklist; without the embedded check, a synthesized NAT64/6to4
// DNS answer silently bypassed deny_cidrs for public ranges.
func (p Policy) ipBlocked(ip netip.Addr, allowLoopback bool) bool {
	if p.deniedIP(ip) {
		return true
	}
	if inner, ok := embeddedIPv4(ip.WithZone("")); ok && p.deniedIP(inner) {
		return true
	}
	return blocked(ip, allowLoopback)
}

// checkDialAnswers validates the resolved addresses for one dial:
// every answer must survive the deny list and the blocklist, and
// when the host earned loopback exemptions by NAME (localhost or
// *.localhost), every answer must actually be loopback — a
// loopback-spelled name resolving to a public address would
// otherwise send provider credentials over plaintext HTTP to a
// remote host.
func (p Policy) checkDialAnswers(host string, ips []netip.Addr, allowLoopback bool) error {
	for _, ip := range ips {
		if !ip.IsValid() || p.ipBlocked(ip, allowLoopback) {
			return fmt.Errorf("egress DNS answer for %q is blocked", host)
		}
	}
	if allowLoopback {
		for _, ip := range ips {
			if !ip.IsLoopback() {
				return fmt.Errorf("egress DNS answer for %q is blocked", host)
			}
		}
	}
	return nil
}

// isLoopbackHost reports whether a URL/dial host names the local machine
// explicitly: a loopback IP literal, "localhost" or "*.localhost". An
// arbitrary DNS name that merely resolves to 127.0.0.1 does NOT qualify.
// deniedIP reports whether an IP matches the operator deny-CIDR list.
func (p Policy) deniedIP(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() {
		return false
	}
	for _, prefix := range p.DenyCIDRs {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.WithZone("").Unmap().IsLoopback()
}

func (p Policy) validateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("egress target is malformed or contains credentials/query")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !p.AllowLoopback || !isLoopbackHost(u.Hostname())) {
		return nil, fmt.Errorf("egress target must use https (http is only allowed for explicit loopback hosts)")
	}
	if p.deniedHost(u.Hostname()) {
		return nil, fmt.Errorf("egress host %q is denied by operator policy", u.Hostname())
	}
	if !p.allowsHost(u.Hostname()) {
		return nil, fmt.Errorf("egress host %q is not allow-listed", u.Hostname())
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if p.deniedIP(ip) {
			return nil, fmt.Errorf("egress target %q is denied by operator policy", u.Hostname())
		}
		if p.ipBlocked(ip, p.AllowLoopback) {
			return nil, fmt.Errorf("egress target %q is blocked", u.Hostname())
		}
	}
	return u, nil
}

// NewTransport returns a direct, pinned transport. Environment proxies are
// intentionally ignored; proxy egress must be an independently controlled
// gateway rather than an implicit process setting.
func (p Policy) NewTransport(target string, proxyURL string) (*http.Transport, error) {
	if _, err := p.validateURL(target); err != nil {
		return nil, err
	}
	var proxy func(*http.Request) (*url.URL, error)
	var proxyHost string
	if proxyURL != "" {
		proxyTarget, parseErr := url.Parse(proxyURL)
		if parseErr != nil || proxyTarget.Hostname() == "" || (proxyTarget.Scheme != "http" && proxyTarget.Scheme != "https") {
			return nil, fmt.Errorf("provider proxy_url is malformed")
		}
		// Proxy credentials and URL decorations are never sent to the proxy
		// client from provider configuration; the admin API redacts them and the
		// egress layer reduces the runtime target to scheme plus authority.
		proxyTarget.User = nil
		proxyTarget.Path, proxyTarget.RawPath, proxyTarget.RawQuery, proxyTarget.Fragment = "", "", "", ""
		// The proxy receives every provider request (with API keys), so it
		// is policy-checked like a target (#427, audit F6): operator deny
		// lists and the built-in blocklist apply to the proxy host at
		// construction, not only later at dial time. Hostnames are
		// additionally re-checked at dial time by the pinned dialer below,
		// which sees the resolved addresses. (No allow-list check here:
		// the proxy is itself an operator-chosen gateway and the provider
		// adapter allow-lists its hostname explicitly.)
		proxyHost = proxyTarget.Hostname()
		if p.deniedHost(proxyHost) {
			return nil, fmt.Errorf("provider proxy_url host %q is denied by operator policy", proxyHost)
		}
		if ip, parseErr := netip.ParseAddr(proxyHost); parseErr == nil {
			if p.deniedIP(ip) {
				return nil, fmt.Errorf("provider proxy_url %q is denied by operator policy", proxyHost)
			}
			if p.ipBlocked(ip, p.AllowLoopback && isLoopbackHost(proxyHost)) {
				return nil, fmt.Errorf("provider proxy_url %q is blocked", proxyHost)
			}
		}
		proxy = http.ProxyURL(proxyTarget)
	}
	// The proxy hop is not a provider endpoint, so it need not appear in
	// allowed_hosts (docs/CONFIGURATION.md). Permit exactly the configured
	// proxy host at dial time without widening the request-building
	// allow-list. Keep an empty allow-list empty: it means "no restriction".
	dialPolicy := p
	if proxyURL != "" && len(p.AllowedHosts) > 0 {
		dialPolicy.AllowedHosts = append(append([]string(nil), p.AllowedHosts...), proxyHost)
	}
	resolver := net.DefaultResolver
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if p.deniedHost(host) {
			return nil, fmt.Errorf("egress host %q is denied by operator policy", host)
		}
		if !dialPolicy.allowsHost(host) {
			return nil, fmt.Errorf("egress host %q is not allow-listed", host)
		}
		// Loopback is reachable only when the host is spelled as loopback
		// (127.0.0.1, ::1, localhost). A public name that resolves to
		// 127.0.0.1 is a rebinding/SSRF vector and stays blocked.
		allowLoopback := p.AllowLoopback && isLoopbackHost(host)
		if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
			if p.deniedIP(ip) {
				return nil, fmt.Errorf("egress address %q is denied by operator policy", host)
			}
			if p.ipBlocked(ip, allowLoopback) {
				return nil, fmt.Errorf("egress address %q is blocked", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("egress DNS resolution failed for %q: %w", host, err)
		}
		addrs := make([]netip.Addr, 0, len(ips))
		for _, candidate := range ips {
			ip, _ := netip.AddrFromSlice(candidate.IP)
			addrs = append(addrs, ip)
		}
		if err := dialPolicy.checkDialAnswers(host, addrs, allowLoopback); err != nil {
			return nil, err
		}
		var last error
		for _, candidate := range ips {
			ip := netip.MustParseAddr(candidate.IP.String())
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			} else {
				last = dialErr
			}
		}
		return nil, last
	}
	// Do not pin TLSClientConfig.ServerName to the provider's base host.
	// Absolute allow-listed endpoints may target a different HTTPS host;
	// net/http must derive SNI and certificate verification from the host
	// actually being contacted, not from the base URL. Pinning the base name
	// verified the wrong certificate identity (and sent the wrong SNI).
	return &http.Transport{Proxy: proxy, DialContext: dial, ForceAttemptHTTP2: true, MaxIdleConns: 64, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second, ExpectContinueTimeout: time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, nil
}
