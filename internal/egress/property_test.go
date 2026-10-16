package egress

// Wave C-7 (#408) supervisor-takeover property tests: egress classification
// versus an independent reference model.
//
// Mechanism is stdlib testing/quick with FIXED seeds (fully reproducible;
// no new dependencies). Every property runs in the default test pass with
// -quickchecks cases (default 10000); scale up manually with:
//
//	go test ./internal/egress/ -run 'TestProperty_Egress' -quickchecks=100000
//
// Properties:
//  P1 TestProperty_EgressURLClassification — Policy.validateURL and
//     Policy.NewTransport accept/reject exactly when the reference model
//     says so, over randomized URLs (schemes, IPv4/IPv6 literals incl.
//     NAT64/6to4/IPv4-mapped/zone forms, hostnames, userinfo/query/fragment
//     decorations, malformed strings), randomized policies, and randomized
//     proxy URLs (the proxy-exemption rules: proxy parse validity is
//     independent, target validation still applies).
//  P2 TestProperty_EgressDialAnswers — Policy.checkDialAnswers agrees with
//     the reference over randomized hostnames and DNS answers (fake
//     resolver model), including the loopback-by-spelling pinning: a
//     loopback-spelled host with AllowLoopback requires EVERY answer to be
//     loopback.
//  P3 TestProperty_EgressEmbeddedIP — Policy.ipBlocked agrees with the
//     reference over randomized addresses (heavy on NAT64/6to4/Teredo/
//     mapped/zone forms) and randomized deny-CIDR sets, covering the
//     embedded-IPv4 interpretation of deny_cidrs.
//
// Reference model design: derived from net/netip primitives plus the
// RFC 6890 special-purpose registries re-declared below (not reused from
// the production var, so a wrong production list is caught) and the
// documented policy composition. It shares only the url.Parse front end
// with production: the property under test is the policy logic AFTER
// parsing, not URL parsing itself.

import (
	"flag"
	"fmt"
	"math/rand"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

func init() {
	// testing/quick defines the -quickchecks flag itself (default 100).
	// It is the scale-up control for these properties; raise its default
	// to the brief's 10k floor. This runs before testing's flag.Parse, so
	// an explicit -quickchecks=N on the command line still wins.
	_ = flag.Set("quickchecks", "10000")
}

// propQuickChecks returns the effective -quickchecks value.
func propQuickChecks() int {
	if f := flag.Lookup("quickchecks"); f != nil {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 0 {
			return n
		}
	}
	return 10000
}

// propSeedEgress is the fixed base seed for the quick.Config Rand: every
// run reproduces the identical case sequence.
const propSeedEgress = int64(0xC708)

// refBlockedPrefixes re-declares the special-purpose registry from
// RFC 6890 and friends, independently of the production blockedPrefixes.
// Any divergence between the two lists fails the properties loudly.
var refBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network" (RFC 1122)
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT shared space (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("192.0.2.0/24"),   // TEST-NET-1 (RFC 5737)
	netip.MustParsePrefix("192.88.99.0/24"), // deprecated 6to4 relay anycast (RFC 7526)
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), // TEST-NET-3 (RFC 5737)
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved + limited broadcast (RFC 1112)
	netip.MustParsePrefix("::/96"),          // IPv4-compatible / unspecified / ::1 (deprecated)
	netip.MustParsePrefix("100::/64"),       // discard-only (RFC 6666)
	netip.MustParsePrefix("2001::/32"),      // Teredo (RFC 4380)
	netip.MustParsePrefix("2001:db8::/32"),  // documentation (RFC 3849)
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local (RFC 3879)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (RFC 8215)
}

// refEmbeddedIPv4 extracts the IPv4 address carried in a NAT64
// (64:ff9b::/96, RFC 6052) or 6to4 (2002::/16, RFC 3056) address from raw
// bytes, independently of the production embeddedIPv4.
func refEmbeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.IsValid() || ip.Is4() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	nat64 := b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b
	for i := 4; i < 12 && nat64; i++ {
		if b[i] != 0 {
			nat64 = false
		}
	}
	if nat64 {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	if b[0] == 0x20 && b[1] == 0x02 {
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

// refBlocked is the independent model of blocked(): built-in
// special-purpose blocklist with embedded-address recursion.
func refBlocked(ip netip.Addr, allowLoopback bool) bool {
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
	for _, prefix := range refBlockedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	if inner, ok := refEmbeddedIPv4(ip); ok {
		// Embedded loopback is never an allowed form, even with allowLoopback.
		return refBlocked(inner, false)
	}
	return false
}

// refDeniedIP is the independent model of Policy.deniedIP: the operator
// deny-CIDR list matched against the zone-stripped, unmapped address.
func refDeniedIP(deny []netip.Prefix, ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() {
		return false
	}
	for _, prefix := range deny {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// refIPBlocked is the independent model of Policy.ipBlocked: the operator
// deny list applies to the address itself AND to any IPv4 address embedded
// in it (NAT64/6to4 interpretation), then the built-in blocklist.
func refIPBlocked(p Policy, ip netip.Addr, allowLoopback bool) bool {
	if refDeniedIP(p.DenyCIDRs, ip) {
		return true
	}
	if inner, ok := refEmbeddedIPv4(ip.WithZone("")); ok && refDeniedIP(p.DenyCIDRs, inner) {
		return true
	}
	return refBlocked(ip, allowLoopback)
}

// refIsLoopbackHost is the independent model of isLoopbackHost: explicit
// loopback spellings only (loopback literal, "localhost", "*.localhost").
func refIsLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.WithZone("").Unmap().IsLoopback()
}

// refDeniedHost mirrors Policy.deniedHost: case-insensitive,
// space-trimmed, single-trailing-dot-trimmed exact match.
func refDeniedHost(p Policy, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, denied := range p.DenyHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(denied), "."), host) {
			return true
		}
	}
	return false
}

// refAllowsHost mirrors Policy.allowsHost: empty allow-list allows all;
// otherwise exact case-insensitive match after ONE trailing-dot trim
// (note: no space trim, matching production exactly).
func refAllowsHost(p Policy, host string) bool {
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

// refValidateURL is the independent model of Policy.validateURL.
func refValidateURL(p Policy, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && p.AllowLoopback && refIsLoopbackHost(host):
	default:
		return false
	}
	if refDeniedHost(p, host) {
		return false
	}
	if !refAllowsHost(p, host) {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if refDeniedIP(p.DenyCIDRs, ip) {
			return false
		}
		if refIPBlocked(p, ip, p.AllowLoopback) {
			return false
		}
	}
	return true
}

// refProxyOK is the independent model of the proxy_url parse gate in
// Policy.NewTransport: empty is fine; otherwise the proxy URL must parse
// with a non-empty host and an http/https scheme. (Proxy exemptions: when
// a proxy is configured the target's DNS answers are never resolved at
// dial time — the dial closure checks the PROXY host — while the target
// URL validation above still applies.)
func refProxyOK(proxyURL string) bool {
	if proxyURL == "" {
		return true
	}
	pu, err := url.Parse(proxyURL)
	if err != nil || pu.Hostname() == "" || (pu.Scheme != "http" && pu.Scheme != "https") {
		return false
	}
	return true
}

// refCheckDialAnswers is the independent model of
// Policy.checkDialAnswers: every answer must be valid and survive the
// deny/block lists, and when the host earned loopback by spelling, every
// answer must actually be loopback (stdlib IsLoopback covers 4-in-6 and
// zoned forms, as production relies on).
func refCheckDialAnswers(p Policy, host string, ips []netip.Addr, allowLoopback bool) bool {
	for _, ip := range ips {
		if !ip.IsValid() || refIPBlocked(p, ip, allowLoopback) {
			return false
		}
	}
	if allowLoopback {
		for _, ip := range ips {
			if !ip.IsLoopback() {
				return false
			}
		}
	}
	return true
}

// ---- generators (seeded; each case i gets its own sub-RNG) ----

func propPick(rnd *rand.Rand, n int) int { return rnd.Intn(n) }

func genIPv4Literal(rnd *rand.Rand) string {
	switch propPick(rnd, 16) {
	case 0:
		return fmt.Sprintf("%d.%d.%d.%d", 1+rnd.Intn(223), rnd.Intn(256), rnd.Intn(256), 1+rnd.Intn(254))
	case 1:
		return fmt.Sprintf("127.%d.%d.%d", rnd.Intn(256), rnd.Intn(256), rnd.Intn(256))
	case 2:
		return fmt.Sprintf("10.%d.%d.%d", rnd.Intn(256), rnd.Intn(256), rnd.Intn(256))
	case 3:
		return fmt.Sprintf("172.%d.%d.%d", 16+rnd.Intn(16), rnd.Intn(256), rnd.Intn(256))
	case 4:
		return fmt.Sprintf("192.168.%d.%d", rnd.Intn(256), rnd.Intn(256))
	case 5:
		return "0.0.0.0"
	case 6:
		return fmt.Sprintf("169.254.%d.%d", rnd.Intn(256), rnd.Intn(256))
	case 7:
		return fmt.Sprintf("100.%d.%d.%d", 64+rnd.Intn(64), rnd.Intn(256), rnd.Intn(256))
	case 8:
		return fmt.Sprintf("192.0.2.%d", rnd.Intn(256))
	case 9:
		return fmt.Sprintf("192.0.0.%d", rnd.Intn(256))
	case 10:
		return fmt.Sprintf("192.88.99.%d", rnd.Intn(256))
	case 11:
		return fmt.Sprintf("198.%d.%d.%d", 18+rnd.Intn(2), rnd.Intn(256), rnd.Intn(256))
	case 12:
		return fmt.Sprintf("198.51.100.%d", rnd.Intn(256))
	case 13:
		return fmt.Sprintf("203.0.113.%d", rnd.Intn(256))
	case 14:
		return fmt.Sprintf("%d.%d.%d.%d", 224+rnd.Intn(16), rnd.Intn(256), rnd.Intn(256), rnd.Intn(256))
	default:
		return fmt.Sprintf("%d.%d.%d.%d", 240+rnd.Intn(16), rnd.Intn(256), rnd.Intn(256), rnd.Intn(256))
	}
}

// nat64Of renders an IPv4 literal in NAT64 form (64:ff9b::/96, RFC 6052):
// the v4 bytes become the last 32 bits as two hex groups.
func nat64Of(v4 string) string {
	var a, b, c, d int
	fmt.Sscanf(v4, "%d.%d.%d.%d", &a, &b, &c, &d)
	return fmt.Sprintf("64:ff9b::%02x%02x:%02x%02x", a, b, c, d)
}

// genIPv6Literal returns a bare IPv6 literal (no brackets, % zone NOT
// URL-escaped; the URL assembler escapes it).
func genIPv6Literal(rnd *rand.Rand) string {
	switch propPick(rnd, 14) {
	case 0:
		return "2001:4860:4860::8888"
	case 1:
		return fmt.Sprintf("2606:4700:%x::%x", rnd.Intn(65536), rnd.Intn(65536))
	case 2:
		return "::1"
	case 3:
		return "::"
	case 4:
		return "fe80::1"
	case 5:
		return "fe80::1%eth0"
	case 6: // NAT64 with random embedded IPv4
		return nat64Of(genIPv4Literal(rnd))
	case 7: // 6to4 with random embedded IPv4
		v4 := genIPv4Literal(rnd)
		var a, b, c, d int
		fmt.Sscanf(v4, "%d.%d.%d.%d", &a, &b, &c, &d)
		return fmt.Sprintf("2002:%02x%02x:%02x%02x::1", a, b, c, d)
	case 8: // Teredo
		return fmt.Sprintf("2001::%x:%x", rnd.Intn(65536), rnd.Intn(65536))
	case 9: // IPv4-mapped
		return fmt.Sprintf("::ffff:%s", genIPv4Literal(rnd))
	case 10:
		return "2001:db8::1"
	case 11:
		return "100::1"
	case 12:
		return "fec0::1"
	default: // IPv4-compatible
		return fmt.Sprintf("::%s", genIPv4Literal(rnd))
	}
}

func genHostname(rnd *rand.Rand) string {
	switch propPick(rnd, 14) {
	case 0:
		return "localhost"
	case 1:
		return "LOCALHOST"
	case 2:
		return "localhost."
	case 3:
		return "a.localhost"
	case 4:
		return "deep.a.localhost"
	case 5:
		return "x.LOCALHOST."
	case 6:
		return "evil.example"
	case 7:
		return "blocked.example"
	case 8:
		return "api.anthropic.com"
	case 9:
		return "api.anthropic.com."
	case 10:
		return "EXAMPLE.com"
	case 11:
		return fmt.Sprintf("%s.example", randomLabel(rnd))
	case 12:
		return fmt.Sprintf("%s.%s.invalid", randomLabel(rnd), randomLabel(rnd))
	default:
		return "127.0.0.1.nip.io"
	}
}

func randomLabel(rnd *rand.Rand) string {
	const letters = "abcdefgh"
	n := 1 + rnd.Intn(8)
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rnd.Intn(len(letters))]
	}
	return string(b)
}

func genMalformedHost(rnd *rand.Rand) string {
	switch propPick(rnd, 8) {
	case 0:
		return ""
	case 1:
		return "[::1" // unclosed bracket
	case 2:
		return "exa mple.com"
	case 3:
		return "192.168.1.256" // invalid IPv4 -> hostname
	case 4:
		return "1.2.3.4.5"
	case 5:
		return "[v1.fe80::]"
	case 6:
		return "exämple.com"
	default:
		return "..."
	}
}

// genURLHost returns a host token ready for URL assembly (IPv6 already
// bracketed, zone % escaped).
func genURLHost(rnd *rand.Rand) string {
	switch propPick(rnd, 10) {
	case 0, 1, 2:
		return genIPv4Literal(rnd)
	case 3, 4:
		return "[" + strings.ReplaceAll(genIPv6Literal(rnd), "%", "%25") + "]"
	case 5, 6, 7:
		return genHostname(rnd)
	default:
		return genMalformedHost(rnd)
	}
}

func genURL(rnd *rand.Rand) string {
	// Fully malformed raw strings.
	if propPick(rnd, 20) == 0 {
		return []string{
			"", "notaurl", "https://", "://", "https://[::1",
			"https:///path", ":", "https://?q=1",
		}[propPick(rnd, 8)]
	}
	scheme := []string{"https", "https", "https", "http", "http", "HTTPS", "ftp", "gopher", "ht!tp"}[propPick(rnd, 9)]
	userinfo := []string{"", "", "", "user@", "user:pass@", ":pass@"}[propPick(rnd, 6)]
	host := genURLHost(rnd)
	port := []string{"", "", "", ":443", ":0", ":8080", ":99999", ":abc"}[propPick(rnd, 8)]
	tail := []string{"", "", "/", "/v1/chat", "?q=1", "#frag", "/?q=1#f", "?"}[propPick(rnd, 8)]
	return scheme + "://" + userinfo + host + port + tail
}

func genProxyURL(rnd *rand.Rand) string {
	return []string{
		"",
		"",
		"http://proxy.internal:8080",
		"https://proxy.internal:3128",
		"http://user:pass@proxy.internal:8080/path?q=1#frag",
		"ftp://proxy.internal:21",
		"socks5://proxy.internal:1080",
		"://bad",
		"http://",
		"http://proxy.internal:notaport",
	}[propPick(rnd, 10)]
}

var propAllowHostPool = []string{"api.anthropic.com", "example.com", "localhost"}
var propDenyHostPool = []string{"evil.example", "blocked.example", "api.anthropic.com"}
var propDenyCIDRPool = []string{
	"10.0.0.0/8", "192.168.0.0/16", "203.0.113.0/24", "2001:db8::/32",
	"64:ff9b::/96", "8.8.8.0/24", "100.64.0.0/10", "2002::/16",
}

func genPolicy(rnd *rand.Rand) Policy {
	p := Policy{AllowLoopback: rnd.Intn(2) == 0}
	for _, h := range propAllowHostPool {
		if rnd.Intn(3) == 0 {
			p.AllowedHosts = append(p.AllowedHosts, h)
		}
	}
	for _, h := range propDenyHostPool {
		if rnd.Intn(3) == 0 {
			p.DenyHosts = append(p.DenyHosts, h)
		}
	}
	for _, c := range propDenyCIDRPool {
		if rnd.Intn(4) == 0 {
			p.DenyCIDRs = append(p.DenyCIDRs, netip.MustParsePrefix(c))
		}
	}
	return p
}

// genIPAddr draws from the full address space with heavy weight on the
// embedding/edge forms that shipped the historical defects.
func genIPAddr(rnd *rand.Rand) netip.Addr {
	var s string
	switch propPick(rnd, 12) {
	case 0, 1, 2:
		s = genIPv6Literal(rnd)
		s = strings.ReplaceAll(s, "%eth0", "") // zones handled separately below
	case 3, 4, 5:
		s = genIPv4Literal(rnd)
	case 6:
		s = "fe80::1%eth0"
	case 7:
		s = "::1%lo"
	case 8:
		s = nat64Of(genIPv4Literal(rnd))
	case 9:
		v4 := genIPv4Literal(rnd)
		var a, b, c, d int
		fmt.Sscanf(v4, "%d.%d.%d.%d", &a, &b, &c, &d)
		s = fmt.Sprintf("2002:%02x%02x:%02x%02x::1%%eth0", a, b, c, d)
	case 10:
		s = fmt.Sprintf("::ffff:%s", genIPv4Literal(rnd))
	default:
		s = fmt.Sprintf("2001:%x::1", rnd.Intn(65536))
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{} // invalid: also a meaningful case
	}
	return ip
}

func genAnswers(rnd *rand.Rand) []netip.Addr {
	n := 1 + rnd.Intn(4)
	out := make([]netip.Addr, 0, n)
	for i := 0; i < n; i++ {
		if rnd.Intn(10) == 0 {
			out = append(out, netip.Addr{}) // invalid answer
			continue
		}
		out = append(out, genIPAddr(rnd))
	}
	return out
}

// ---- properties (testing/quick drivers) ----

// quickConfig returns a deterministic quick.Config: the seeded Rand makes
// every run reproduce the identical case sequence. MaxCount comes from
// the -quickchecks flag (default raised to 10000 in init).
func quickConfig(seed int64) *quick.Config {
	return &quick.Config{MaxCount: propQuickChecks(), Rand: rand.New(rand.NewSource(seed))}
}

type egressURLCase struct {
	policy Policy
	raw    string
	proxy  string
}

func (egressURLCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(egressURLCase{policy: genPolicy(r), raw: genURL(r), proxy: genProxyURL(r)})
}

func TestProperty_EgressURLClassification(t *testing.T) {
	n := propQuickChecks()
	var firstFail string
	idx := -1
	err := quick.Check(func(c egressURLCase) bool {
		idx++
		p, raw, proxy := c.policy, c.raw, c.proxy
		if _, verr := p.validateURL(raw); (verr == nil) != refValidateURL(p, raw) {
			if firstFail == "" {
				firstFail = fmt.Sprintf("case %d: validateURL(%q) accepted=%v, reference=%v (policy %+v)",
					idx, raw, verr == nil, refValidateURL(p, raw), p)
			}
			return false
		}
		_, terr := p.NewTransport(raw, proxy)
		if want := refValidateURL(p, raw) && refProxyOK(proxy); (terr == nil) != want {
			if firstFail == "" {
				firstFail = fmt.Sprintf("case %d: NewTransport(%q, proxy %q) accepted=%v, reference=%v (policy %+v)",
					idx, raw, proxy, terr == nil, want, p)
			}
			return false
		}
		return true
	}, quickConfig(propSeedEgress))
	if err != nil {
		t.Fatalf("P1 url-classification property failed: %v\n%s", err, firstFail)
	}
	t.Logf("P1 url-classification: %d generated (url, policy, proxy) cases, production == reference on all", n)
}

type egressDialCase struct {
	policy  Policy
	host    string
	answers []netip.Addr
}

func (egressDialCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(egressDialCase{policy: genPolicy(r), host: genHostname(r), answers: genAnswers(r)})
}

func TestProperty_EgressDialAnswers(t *testing.T) {
	n := propQuickChecks()
	var firstFail string
	idx := -1
	err := quick.Check(func(c egressDialCase) bool {
		idx++
		p, host, answers := c.policy, c.host, c.answers
		// Effective loopback permission exactly as the dial closure computes it.
		allowLoopback := p.AllowLoopback && refIsLoopbackHost(host)
		got := p.checkDialAnswers(host, answers, allowLoopback) == nil
		if want := refCheckDialAnswers(p, host, answers, allowLoopback); got != want {
			if firstFail == "" {
				firstFail = fmt.Sprintf("case %d: checkDialAnswers(%q, %v, allowLoopback=%v) accepted=%v, reference=%v (policy %+v)",
					idx, host, answers, allowLoopback, got, want, p)
			}
			return false
		}
		return true
	}, quickConfig(propSeedEgress+1))
	if err != nil {
		t.Fatalf("P2 dial-answers property failed: %v\n%s", err, firstFail)
	}
	t.Logf("P2 dial-answers: %d generated (host, answers, policy) cases, production == reference on all", n)
}

type egressIPCase struct {
	policy        Policy
	ip            netip.Addr
	allowLoopback bool
}

func (egressIPCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(egressIPCase{policy: genPolicy(r), ip: genIPAddr(r), allowLoopback: r.Intn(2) == 0})
}

func TestProperty_EgressEmbeddedIP(t *testing.T) {
	n := propQuickChecks()
	var firstFail string
	idx := -1
	err := quick.Check(func(c egressIPCase) bool {
		idx++
		if got, want := c.policy.ipBlocked(c.ip, c.allowLoopback), refIPBlocked(c.policy, c.ip, c.allowLoopback); got != want {
			if firstFail == "" {
				firstFail = fmt.Sprintf("case %d: ipBlocked(%v, allowLoopback=%v)=%v, reference=%v (policy %+v)",
					idx, c.ip, c.allowLoopback, got, want, c.policy)
			}
			return false
		}
		return true
	}, quickConfig(propSeedEgress+2))
	if err != nil {
		t.Fatalf("P3 embedded-ip property failed: %v\n%s", err, firstFail)
	}
	t.Logf("P3 embedded-ip: %d generated (addr, policy) cases, production == reference on all", n)
}
