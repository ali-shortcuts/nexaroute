package egress

import (
	"net/netip"
	"testing"
)

func TestDenyHostsRejectsListedHost(t *testing.T) {
	p := Policy{DenyHosts: []string{"evil.example.com", "Bad.Actor.net"}}
	if !p.deniedHost("evil.example.com") {
		t.Error("expected evil.example.com to be denied")
	}
	if !p.deniedHost("EVIL.EXAMPLE.COM") {
		t.Error("expected case-insensitive deny match")
	}
	if !p.deniedHost("bad.actor.net.") {
		t.Error("expected trailing-dot tolerant deny match")
	}
	if p.deniedHost("good.example.com") {
		t.Error("good.example.com should not be denied")
	}
}

func TestDenyHostsEnforcedInValidateURL(t *testing.T) {
	p := Policy{DenyHosts: []string{"blocked.example.com"}}
	if _, err := p.validateURL("https://blocked.example.com/v1"); err == nil {
		t.Error("expected validateURL to reject deny-listed host")
	}
	if _, err := p.validateURL("https://allowed.example.com/v1"); err != nil {
		t.Errorf("expected validateURL to allow non-listed host, got: %v", err)
	}
}

func TestDenyCIDRsRejectMatchingIP(t *testing.T) {
	p := Policy{DenyCIDRs: []netip.Prefix{
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("198.51.100.7/32"),
	}}
	if !p.deniedIP(netip.MustParseAddr("203.0.113.44")) {
		t.Error("expected 203.0.113.44 to be denied")
	}
	if !p.deniedIP(netip.MustParseAddr("198.51.100.7")) {
		t.Error("expected 198.51.100.7 to be denied")
	}
	if p.deniedIP(netip.MustParseAddr("198.51.100.8")) {
		t.Error("198.51.100.8 should not be denied")
	}
	if p.deniedIP(netip.MustParseAddr("8.8.8.8")) {
		t.Error("8.8.8.8 should not be denied")
	}
}

func TestDenyCIDRsEnforcedForLiteralIP(t *testing.T) {
	p := Policy{DenyCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}}
	if _, err := p.validateURL("https://203.0.113.10/v1"); err == nil {
		t.Error("expected validateURL to reject deny-listed IP literal")
	}
}

func TestEmptyDenyListsDenyNothing(t *testing.T) {
	p := Policy{}
	if p.deniedHost("anything.example.com") {
		t.Error("empty deny list should deny nothing")
	}
	if p.deniedIP(netip.MustParseAddr("1.2.3.4")) {
		t.Error("empty deny CIDR list should deny nothing")
	}
}
