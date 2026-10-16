package egress

import (
	"net/netip"
	"strings"
	"testing"
)

// Issue #427 (audit F6): a provider proxy_url must pass the same deny/IP
// policy as the base_url at transport construction. Before the fix,
// NewTransport only parsed the proxy URL, so a proxy pointing at the cloud
// metadata address was accepted even under an explicit DenyCIDRs policy and
// all provider traffic (with API keys) would egress through it.
func TestNewTransportProxyPolicy(t *testing.T) {
	denyMeta := Policy{DenyCIDRs: []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16")}}
	cases := []struct {
		name    string
		policy  Policy
		proxy   string
		wantErr string // substring; "" means construction must succeed
	}{
		{"denied CIDR literal", denyMeta, "http://169.254.169.254:8080/", "denied"},
		{"built-in blocked literal without deny list", Policy{}, "http://169.254.169.254:8080/", "blocked"},
		{"loopback literal", Policy{}, "http://127.0.0.1:8080/", "blocked"},
		{"6to4 embedded metadata", Policy{}, "http://[2002:a9fe:a9fe::]:8080/", "blocked"},
		{"NAT64 embedded metadata", Policy{}, "http://[64:ff9b::a9fe:a9fe]:8080/", "blocked"},
		{"denied host name", Policy{DenyHosts: []string{"proxy.evil.example"}}, "http://proxy.evil.example:8080/", "denied"},
		{"public literal proxy", Policy{}, "http://8.8.8.8:8080/", ""},
		{"public named proxy", Policy{}, "http://proxy.example.com:8080/", ""},
		{"no proxy", Policy{}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.policy.NewTransport("https://api.example.com", c.proxy)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("proxy %q should be accepted, got %v", c.proxy, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("proxy %q should be rejected (%s), got %v", c.proxy, c.wantErr, err)
			}
		})
	}
}
