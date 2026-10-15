package egress

import (
	"net/netip"
	"testing"
)

// Regression (audit 2026-10-15): the operator deny list was only
// consulted for the outer address, so a NAT64/6to4 answer embedding
// a denied PUBLIC IPv4 silently bypassed deny_cidrs (the built-in
// blocklist judged the embedded address only against its own
// special-purpose ranges). The documented precedence — deny lists
// over everything except the built-in blocklist — must hold for
// embedded addresses too, at URL validation and at dial time.
func TestDenyCIDRsApplyToEmbeddedIPv4(t *testing.T) {
	p := Policy{DenyCIDRs: []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")}}
	for _, addr := range []string{
		"8.8.8.8",               // plain
		"64:ff9b::808:808",      // NAT64 embedding 8.8.8.8
		"2002:808:808::",        // 6to4 embedding 8.8.8.8
		"64:ff9b::808:808%eth0", // zone-scoped NAT64 embedding 8.8.8.8
		"2002:808:808::%eth0",   // zone-scoped 6to4 embedding 8.8.8.8
	} {
		if !p.ipBlocked(netip.MustParseAddr(addr), false) {
			t.Errorf("ipBlocked(%s) = false, want true (denied directly or embedded)", addr)
		}
	}
	for _, addr := range []string{
		"1.1.1.1",
		"64:ff9b::101:101",      // NAT64 embedding 1.1.1.1
		"2002:101:101::",        // 6to4 embedding 1.1.1.1
		"64:ff9b::101:101%eth0", // zone-scoped NAT64 embedding 1.1.1.1
	} {
		if p.ipBlocked(netip.MustParseAddr(addr), false) {
			t.Errorf("ipBlocked(%s) = true, want false (not denied, not blocklisted)", addr)
		}
	}
	// A literal NAT64 base URL for a denied host must fail validation.
	if _, err := p.validateURL("https://[64:ff9b::808:808]/v1"); err == nil {
		t.Error("validateURL accepted a NAT64 literal embedding a denied address")
	}
	if _, err := p.validateURL("https://[64:ff9b::101:101]/v1"); err != nil {
		t.Errorf("validateURL rejected a NAT64 literal embedding an allowed address: %v", err)
	}
}

// Regression (audit 2026-10-15): a loopback-SPELLED name
// (localhost / *.localhost) earns the http scheme exception and the
// loopback exemption by name, but nothing required its DNS answers
// to actually be loopback — a public answer would have been dialed
// over plaintext HTTP with the provider credential attached.
func TestLoopbackNameAnswersMustBeLoopback(t *testing.T) {
	p := Policy{AllowLoopback: true}
	public := []netip.Addr{netip.MustParseAddr("93.184.216.34")}
	loop := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	if err := p.checkDialAnswers("svc.localhost", public, true); err == nil {
		t.Error("loopback-spelled name resolving to a public address must be refused")
	}
	if err := p.checkDialAnswers("svc.localhost", loop, true); err != nil {
		t.Errorf("loopback-spelled name resolving to loopback must pass: %v", err)
	}
	mixed := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("93.184.216.34")}
	if err := p.checkDialAnswers("localhost", mixed, true); err == nil {
		t.Error("any non-loopback answer for a loopback-spelled name must refuse the dial")
	}
	// The rebinding direction stays blocked: a public name resolving
	// to loopback without the loopback opt-in is refused.
	if err := p.checkDialAnswers("example.com", loop, false); err == nil {
		t.Error("public name resolving to loopback must stay blocked")
	}
	if err := p.checkDialAnswers("example.com", public, false); err != nil {
		t.Errorf("ordinary public dial must pass: %v", err)
	}
}
