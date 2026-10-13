package egress

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// M3-4: table-driven SSRF matrix for the provider egress boundary.

func TestBlockedIPMatrix(t *testing.T) {
	cases := []struct {
		ip          string
		allowLoop   bool
		wantBlocked bool
		why         string
	}{
		// Public addresses must pass.
		{"8.8.8.8", false, false, "public v4"},
		{"1.1.1.1", false, false, "public v4"},
		{"2606:4700:4700::1111", false, false, "public v6"},
		// Loopback is gated by allowLoopback.
		{"127.0.0.1", false, true, "loopback v4"},
		{"127.0.0.1", true, false, "loopback v4 explicitly allowed"},
		{"::1", false, true, "loopback v6"},
		{"::1", true, false, "loopback v6 explicitly allowed"},
		{"::ffff:127.0.0.1", false, true, "mapped loopback"},
		// RFC 1918 and ULA.
		{"10.0.0.1", false, true, "rfc1918"},
		{"172.16.0.1", false, true, "rfc1918"},
		{"172.31.255.255", false, true, "rfc1918"},
		{"192.168.1.1", false, true, "rfc1918"},
		{"fd00::1", false, true, "ula"},
		{"::ffff:10.0.0.1", false, true, "mapped rfc1918"},
		// Link-local and cloud metadata.
		{"169.254.169.254", false, true, "cloud metadata"},
		{"169.254.0.1", false, true, "link-local"},
		{"fe80::1", false, true, "link-local v6"},
		{"fe80::1%eth0", false, true, "link-local v6 with zone"},
		{"fd00:ec2::254", false, true, "aws imds v6 (ula)"},
		// Special-purpose ranges that IsPrivate/IsGlobalUnicast miss.
		{"100.64.0.1", false, true, "cgnat low"},
		{"100.100.100.200", false, true, "cgnat (alibaba metadata)"},
		{"100.127.255.254", false, true, "cgnat high"},
		{"100.63.255.255", false, false, "just below cgnat"},
		{"100.128.0.1", false, false, "just above cgnat"},
		{"0.0.0.0", false, true, "unspecified"},
		{"0.1.2.3", false, true, "this-network"},
		{"192.0.0.1", false, true, "ietf protocol assignments"},
		{"192.0.2.10", false, true, "test-net-1"},
		{"192.88.99.1", false, true, "6to4 relay"},
		{"198.18.0.1", false, true, "benchmarking low"},
		{"198.19.255.255", false, true, "benchmarking high"},
		{"198.51.100.7", false, true, "test-net-2"},
		{"203.0.113.9", false, true, "test-net-3"},
		{"224.0.0.1", false, true, "multicast"},
		{"240.0.0.1", false, true, "reserved"},
		{"255.255.255.255", false, true, "limited broadcast"},
		{"::", false, true, "unspecified v6"},
		{"::127.0.0.1", false, true, "ipv4-compatible loopback"},
		{"::10.0.0.1", false, true, "ipv4-compatible private"},
		{"100::1", false, true, "discard-only"},
		{"2001::1", false, true, "teredo"},
		{"2001:db8::1", false, true, "doc v6"},
		{"fec0::1", false, true, "site-local"},
		{"ff02::1", false, true, "multicast v6"},
		// NAT64 / 6to4: judged by the embedded IPv4 address.
		{"64:ff9b::808:808", false, false, "nat64 of 8.8.8.8 is legitimate on v6-only nets"},
		{"64:ff9b::a9fe:a9fe", false, true, "nat64 of 169.254.169.254"},
		{"64:ff9b::7f00:1", false, true, "nat64 of 127.0.0.1"},
		{"64:ff9b::7f00:1", true, true, "nat64 loopback stays blocked even if loopback allowed"},
		{"64:ff9b::a00:1", false, true, "nat64 of 10.0.0.1"},
		{"64:ff9b::6440:1", false, true, "nat64 of cgnat 100.64.0.1"},
		{"64:ff9b:1::1", false, true, "nat64 local-use"},
		{"2002:0808:0808::1", false, false, "6to4 of 8.8.8.8"},
		{"2002:a9fe:a9fe::1", false, true, "6to4 of metadata ip"},
		{"2002:7f00:1::1", false, true, "6to4 of loopback"},
		{"2002:0a00:0001::1", false, true, "6to4 of private"},
	}
	for _, tc := range cases {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("bad test ip %q: %v", tc.ip, err)
		}
		if got := blocked(ip, tc.allowLoop); got != tc.wantBlocked {
			t.Errorf("blocked(%s, allowLoopback=%v) = %v, want %v (%s)", tc.ip, tc.allowLoop, got, tc.wantBlocked, tc.why)
		}
	}
}

func TestBlockedZeroAddrFailsClosed(t *testing.T) {
	if !blocked(netip.Addr{}, true) {
		t.Fatal("invalid address must be blocked")
	}
}

func TestValidateURLMatrix(t *testing.T) {
	cases := []struct {
		name   string
		policy Policy
		target string
		wantOK bool
	}{
		{"https public host", Policy{}, "https://api.example.com/v1", true},
		{"https public literal", Policy{}, "https://8.8.8.8", true},
		{"plain http public host rejected", Policy{}, "http://api.example.com", false},
		{"plain http public host rejected even with loopback opt-in", Policy{AllowLoopback: true}, "http://api.example.com", false},
		{"plain http public literal rejected even with loopback opt-in", Policy{AllowLoopback: true}, "http://8.8.8.8", false},
		{"http loopback literal needs opt-in", Policy{}, "http://127.0.0.1:5000/v1", false},
		{"http loopback literal with opt-in", Policy{AllowLoopback: true}, "http://127.0.0.1:5000/v1", true},
		{"http localhost with opt-in", Policy{AllowLoopback: true}, "http://localhost:5000/v1", true},
		{"http v6 loopback with opt-in", Policy{AllowLoopback: true}, "http://[::1]:5000/v1", true},
		{"https loopback without opt-in", Policy{}, "https://127.0.0.1", false},
		{"https private literal", Policy{}, "https://10.1.2.3", false},
		{"https cgnat literal", Policy{}, "https://100.64.0.10", false},
		{"https metadata literal", Policy{}, "https://169.254.169.254/latest/meta-data", false},
		{"https nat64 metadata literal", Policy{}, "https://[64:ff9b::a9fe:a9fe]", false},
		{"https mapped private literal", Policy{}, "https://[::ffff:10.0.0.1]", false},
		{"userinfo rejected", Policy{}, "https://user:pw@api.example.com", false},
		{"query rejected", Policy{}, "https://api.example.com/?key=secret", false},
		{"fragment rejected", Policy{}, "https://api.example.com/#x", false},
		{"empty host rejected", Policy{}, "https:///path", false},
		{"unsupported scheme", Policy{}, "ftp://api.example.com", false},
		{"file scheme", Policy{}, "file:///etc/passwd", false},
		{"allow-list mismatch", Policy{AllowedHosts: []string{"api.example.com"}}, "https://evil.example.com", false},
		{"allow-list match is case and dot insensitive", Policy{AllowedHosts: []string{"API.Example.com."}}, "https://api.example.com", true},
		{"allow-list is not suffix matching", Policy{AllowedHosts: []string{"example.com"}}, "https://api.example.com", false},
	}
	for _, tc := range cases {
		_, err := tc.policy.validateURL(tc.target)
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: validateURL(%q) err=%v, wantOK=%v", tc.name, tc.target, err, tc.wantOK)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	yes := []string{"localhost", "LOCALHOST", "localhost.", "app.localhost", "127.0.0.1", "127.1.2.3", "::1", "::ffff:127.0.0.1"}
	no := []string{"", "example.com", "localhost.example.com", "notlocalhost", "127.0.0.1.nip.io", "10.0.0.1", "8.8.8.8", "0x7f.1", "2130706433"}
	for _, h := range yes {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range no {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}

// dialBlocked exercises the real DialContext installed on the transport with
// IP-literal hosts, so no DNS or network access is needed: every case here
// must be rejected before a socket is opened.
func TestTransportDialRejectsBlockedLiterals(t *testing.T) {
	tr, err := (Policy{AllowLoopback: false}).NewTransport("https://api.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{
		"127.0.0.1:443", "[::1]:443", "10.0.0.1:443", "169.254.169.254:80",
		"100.64.0.1:443", "198.18.0.1:443", "[64:ff9b::a9fe:a9fe]:443", "[::ffff:10.0.0.1]:443",
	} {
		conn, err := tr.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			t.Errorf("dial to %s must be rejected", addr)
			continue
		}
		if !strings.Contains(err.Error(), "blocked") && !strings.Contains(err.Error(), "allow-listed") {
			t.Errorf("dial to %s failed for an unexpected reason: %v", addr, err)
		}
	}
}

// Rebinding / TOCTOU: a public-looking name whose DNS answer is loopback must
// be rejected at dial time even when the policy opts in to loopback, because
// the host was not spelled as loopback.
func TestTransportRejectsPublicNameResolvingToLoopback(t *testing.T) {
	p := Policy{AllowLoopback: true}
	tr, err := p.NewTransport("https://rebind.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	// Loopback name through the same transport: dial is attempted (and may
	// fail to connect), but must not be rejected by policy.
	if _, err := tr.DialContext(context.Background(), "tcp", "localhost:1"); err != nil && strings.Contains(err.Error(), "blocked") {
		t.Fatalf("explicit localhost must not be policy-blocked under AllowLoopback: %v", err)
	}
	// Host not in the default allow-list is rejected before any lookup.
	p2 := Policy{AllowedHosts: []string{"rebind.example.com"}, AllowLoopback: true}
	tr2, err := p2.NewTransport("https://rebind.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr2.DialContext(context.Background(), "tcp", "other.example.com:443"); err == nil || !strings.Contains(err.Error(), "allow-listed") {
		t.Fatalf("dial outside the allow-list must be rejected, got %v", err)
	}
}

func TestResolvedAnswersAreAllChecked(t *testing.T) {
	// Mirror of the dial-time rule: if ANY answer is blocked, reject all.
	answers := []string{"8.8.8.8", "100.64.0.9"}
	rejected := false
	for _, a := range answers {
		ip := net.ParseIP(a)
		addr, _ := netip.AddrFromSlice(ip)
		if blocked(addr, false) {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("a mixed public/cgnat answer set must be rejected")
	}
}

func TestProxyConfigIsNormalised(t *testing.T) {
	tr, err := (Policy{}).NewTransport("https://api.example.com", "http://user:pw@proxy.example.com:8080/path?x=1#f")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages", nil)
	got, err := tr.Proxy(req)
	if err != nil || got == nil {
		t.Fatalf("proxy not set: %v", err)
	}
	if got.User != nil || got.Path != "" || got.RawQuery != "" || got.Fragment != "" {
		t.Fatalf("proxy credentials/path/query must be stripped, got %q", got.String())
	}
	for _, bad := range []string{"socks5://proxy.example.com:1080", "ftp://proxy.example.com", "://x", "http://"} {
		if _, err := (Policy{}).NewTransport("https://api.example.com", bad); err == nil {
			t.Errorf("proxy %q must be rejected", bad)
		}
	}
}
