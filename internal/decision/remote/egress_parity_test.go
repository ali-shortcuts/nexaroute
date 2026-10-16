package remote

import (
	"net/netip"
	"testing"
)

// Issue #427 (audit F5): the remote-decision transport must judge NAT64 /
// 6to4 / Teredo forms by the IPv4 address they embed, exactly like the
// provider egress policy (internal/egress). Before the fix, isBlockedIP had
// no embedded-IPv4 recursion and no Teredo prefix, so a decision-provider
// hostname rebinding to a 6to4 form of loopback or the cloud metadata
// address was allowed to connect with the decision bearer key attached.
func TestIsBlockedIPEmbeddedForms(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"2002:7f00:0001::", true},  // 6to4 -> 127.0.0.1
		{"2002:a9fe:a9fe::", true},  // 6to4 -> 169.254.169.254 (metadata)
		{"2002:0a00:0001::", true},  // 6to4 -> 10.0.0.1 (RFC1918)
		{"2002:0801:0101::", false}, // 6to4 -> 8.1.1.1 (public)
		{"64:ff9b::a9fe:a9fe", true},
		{"64:ff9b::7f00:1", true},
		{"64:ff9b::808:808", false},                    // NAT64 -> 8.8.8.8 (public)
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", true}, // Teredo
		{"2001:db8::1", true},                          // documentation range
		{"8.8.8.8", false},
		{"127.0.0.1", true},
		{"169.254.169.254", true},
		{"10.1.2.3", true},
	}
	for _, c := range cases {
		ip := netip.MustParseAddr(c.addr)
		if got := isBlockedIP(ip); got != c.want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", c.addr, got, c.want)
		}
	}
}
