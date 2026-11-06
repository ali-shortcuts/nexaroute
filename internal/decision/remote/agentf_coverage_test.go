package remote

import (
	"context"
	"net"
	"testing"
)

// Agent F coverage: the DNS-answer zone branch in ssrfDialContext.

// TestAgentFSSRFZonedAddressPreservesZone verifies a DNS answer carrying a
// zone (e.g. a link-local-style scoped address) keeps the zone on the pinned
// dial address instead of dropping it.
func TestAgentFSSRFZonedAddressPreservesZone(t *testing.T) {
	resolve := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		// 2606:4700:4700::1111 is a globally routable address (Cloudflare
		// DNS); 2001:db8::/32 is a blocked documentation prefix.
		return []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111"), Zone: "eth0"}}, nil
	}
	rec := &recordDialer{}
	d := ssrfDialContext(resolve, rec.dial)
	conn, err := d(context.Background(), "tcp", "zoned.example.com:443")
	if err != nil {
		t.Fatalf("zoned answer must be dialled, got error: %v", err)
	}
	_ = conn.Close()
	got := rec.addrs()
	if len(got) != 1 || got[0] != "[2606:4700:4700::1111%eth0]:443" {
		t.Fatalf("dialled addresses = %v, want [[2606:4700:4700::1111%%eth0]:443]", got)
	}
}
