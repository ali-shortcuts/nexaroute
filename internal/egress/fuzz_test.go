package egress

// Wave C-2 (#403) supervisor-takeover fuzz targets for the egress policy
// classifiers.
//
// FuzzIPBlocked differentially tests blocked()/deniedIP()/ipBlocked()
// against an independent reference implementation built on the stdlib
// net package (the production code uses netip). Any disagreement is a
// defect: the two derivations must agree on every parseable address,
// including NAT64/6to4 embeddings, zone suffixes, and 4-in-6 forms.
//
// FuzzEgressURL checks validateURL/NewTransport never panic and that a
// successful validation is consistent with the classifiers (an accepted
// IP-literal target must not be classified blocked).

import (
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// blockedRef is the independent reference for blocked(): same policy data
// (blockedPrefixes) but derived through package net primitives and manual
// embedded-address extraction instead of netip methods.
func blockedRef(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.WithZone("")
	std := net.ParseIP(ip.String())
	if std == nil {
		return true
	}
	if v4 := std.To4(); v4 != nil {
		std = v4 // Unmap, mirroring blocked()
	}
	if std.IsLoopback() {
		return !allowLoopback
	}
	if !std.IsGlobalUnicast() || std.IsPrivate() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip.Unmap()) {
			return true
		}
	}
	if inner, ok := embeddedIPv4Ref(std); ok {
		return blockedRefInner(inner)
	}
	return false
}

// embeddedIPv4Ref extracts the IPv4 address carried in a NAT64
// (64:ff9b::/96) or 6to4 (2002::/16) address using raw bytes, independent
// of the netip-based embeddedIPv4.
func embeddedIPv4Ref(std net.IP) (net.IP, bool) {
	if v4 := std.To4(); v4 != nil {
		return nil, false
	}
	b := std.To16()
	if b == nil {
		return nil, false
	}
	// 64:ff9b::/96
	if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b &&
		b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0 && b[9] == 0 && b[10] == 0 && b[11] == 0 {
		return net.IP{b[12], b[13], b[14], b[15]}, true
	}
	// 2002::/16
	if b[0] == 0x20 && b[1] == 0x02 {
		return net.IP{b[2], b[3], b[4], b[5]}, true
	}
	return nil, false
}

func blockedRefInner(inner net.IP) bool {
	addr, ok := netip.AddrFromSlice(inner)
	if !ok {
		return true
	}
	return blockedRef(addr, false) // embedded loopback never allowed
}

// deniedIPRef is the independent reference for Policy.deniedIP: the
// operator deny list applies to the address itself, matched with package
// net semantics.
func deniedIPRef(nets []*net.IPNet, ip netip.Addr) bool {
	ip = ip.WithZone("")
	std := net.ParseIP(ip.String())
	if std == nil {
		return false
	}
	if v4 := std.To4(); v4 != nil {
		std = v4
	}
	for _, ipnet := range nets {
		if ipnet.Contains(std) {
			return true
		}
	}
	return false
}

func mustParseNets(prefixes []netip.Prefix) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(prefixes))
	for _, prefix := range prefixes {
		if _, ipnet, err := net.ParseCIDR(prefix.String()); err == nil {
			nets = append(nets, ipnet)
		}
	}
	return nets
}

var fuzzDenyPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("64:ff9b::/96"),
}

// fuzzDenyNets is precomputed at init: the fuzz workers run the target
// concurrently, so no per-exec map caching.
var fuzzDenyNets = mustParseNets(fuzzDenyPrefixes)

func FuzzIPBlocked(f *testing.F) {
	for _, seed := range []string{
		"127.0.0.1", "::1", "8.8.8.8", "10.0.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "::",
		"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "2002:a9fe:a9fe::",
		"2002:7f00:1::", "::ffff:127.0.0.1", "::ffff:8.8.8.8",
		"fe80::1", "ff02::1", "2001:db8::1", "192.0.2.1",
		"203.0.113.7", "198.51.100.9", "240.0.0.1", "224.0.0.1",
		"::ffff:0:8.8.8.8", "64:ff9b:1::c000:201",
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, addrStr string, allowLoopback bool) {
		ip, err := netip.ParseAddr(addrStr)
		if err != nil {
			return // unparseable input: nothing to classify
		}
		p := Policy{}
		if got, want := blocked(ip, allowLoopback), blockedRef(ip, allowLoopback); got != want {
			t.Fatalf("blocked(%q, allowLoopback=%v) = %v, reference = %v",
				addrStr, allowLoopback, got, want)
		}
		if got, want := p.deniedIP(ip), deniedIPRef(nil, ip); got != want {
			t.Fatalf("deniedIP(%q) = %v, reference = %v", addrStr, got, want)
		}
		if got, want := p.ipBlocked(ip, allowLoopback), blockedRef(ip, allowLoopback); got != want {
			// With an empty deny list, ipBlocked must reduce to blocked.
			t.Fatalf("ipBlocked(%q, allowLoopback=%v) = %v, reference = %v",
				addrStr, allowLoopback, got, want)
		}
		// With a deny list, the deny check must also agree.
		p2 := Policy{DenyCIDRs: fuzzDenyPrefixes}
		if got, want := p2.deniedIP(ip), deniedIPRef(fuzzDenyNets, ip); got != want {
			t.Fatalf("deniedIP+deny(%q) = %v, reference = %v", addrStr, got, want)
		}
		denyBlocked := p2.deniedIP(ip)
		if inner, ok := embeddedIPv4(ip.WithZone("")); ok && p2.deniedIP(inner) {
			denyBlocked = true
		}
		if got := p2.ipBlocked(ip, allowLoopback); got != (denyBlocked || blockedRef(ip, allowLoopback)) {
			t.Fatalf("ipBlocked+deny(%q, %v) = %v, want deny||blocked = %v",
				addrStr, allowLoopback, got, denyBlocked || blockedRef(ip, allowLoopback))
		}
	})
}

func FuzzEgressURL(f *testing.F) {
	for _, seed := range []string{
		"https://api.anthropic.com/v1",
		"http://localhost:8080/x",
		"https://127.0.0.1/",
		"https://[::1]/",
		"https://8.8.8.8/",
		"https://169.254.169.254/latest",
		"https://user:pass@example.com/",
		"https://example.com/?q=1",
		"https://example.com/#frag",
		"ftp://example.com/",
		"https://",
		"https://64:ff9b::a9fe:a9fe/",
		"http://example.com/",
	} {
		f.Add(seed, "https://proxy.internal:8080")
		f.Add(seed, "")
	}
	policies := []Policy{
		{},
		{AllowLoopback: true},
		{AllowLoopback: true, AllowedHosts: []string{"api.anthropic.com"}},
		{DenyHosts: []string{"evil.example"}},
	}
	f.Fuzz(func(t *testing.T, raw string, proxy string) {
		for _, p := range policies {
			// Must never panic, whatever the input.
			u, err := p.validateURL(raw)
			if _, terr := p.NewTransport(raw, proxy); terr != nil {
				_ = terr
			}
			if err != nil {
				continue
			}
			// Consistency: an accepted IP-literal target must survive
			// the classifiers that validateURL itself applies.
			if ip, perr := netip.ParseAddr(u.Hostname()); perr == nil {
				if p.deniedIP(ip) {
					t.Fatalf("validateURL(%q) accepted a denied IP", raw)
				}
				if p.ipBlocked(ip, p.AllowLoopback) {
					t.Fatalf("validateURL(%q) accepted a blocked IP", raw)
				}
			}
			// Accepted targets must be https, or http to an explicit
			// loopback host when allowed.
			if u.Scheme != "https" {
				if !(u.Scheme == "http" && p.AllowLoopback && isLoopbackHost(u.Hostname())) {
					t.Fatalf("validateURL(%q) accepted scheme %q", raw, u.Scheme)
				}
			}
			if strings.Contains(raw, "@") && u.User == nil {
				// credentials must never survive validation: if the raw
				// URL carries userinfo, validation must have rejected it.
				if uu, perr := url.Parse(raw); perr == nil && uu.User != nil {
					t.Fatalf("validateURL(%q) accepted credentials", raw)
				}
			}
		}
	})
}
