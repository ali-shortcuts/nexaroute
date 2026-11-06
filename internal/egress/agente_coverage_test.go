package egress

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// Agent E coverage for the egress dial-policy branches left uncovered
// by the CI baseline (policy.go). No network traffic leaves the test:
// every dial fails closed inside the policy checks except the
// localhost DNS-answer case, which resolves via /etc/hosts only.

func agentETransport(t *testing.T, p Policy) *http.Transport {
	t.Helper()
	tr, err := p.NewTransport("https://example.com", "")
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return tr
}

// policy.go:100-101: the exported bare-address judgment shared with
// callers that classify an address without an operator Policy.
func TestAgentEBlockedIPClassifiesBareAddresses(t *testing.T) {
	if !BlockedIP(netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("loopback must be blocked by the bare-address judgment")
	}
	if !BlockedIP(netip.Addr{}) {
		t.Fatal("invalid address must be blocked by the bare-address judgment")
	}
	if BlockedIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("public unicast address must not be blocked")
	}
	// NAT64 form of a blocked address stays blocked (embedded
	// 169.254.169.254, the link-local metadata address).
	if !BlockedIP(netip.MustParseAddr("64:ff9b::a9fe:a9fe")) {
		t.Fatal("NAT64 form of 169.254.169.254 must be blocked")
	}
}

// policy.go:174-175: an invalid address never matches the operator
// deny-CIDR list.
func TestAgentEDeniedIPIgnoresInvalidAddress(t *testing.T) {
	p := Policy{DenyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	if p.deniedIP(netip.Addr{}) {
		t.Fatal("invalid address must not match the deny list")
	}
	if !p.deniedIP(netip.MustParseAddr("10.1.2.3")) {
		t.Fatal("10.1.2.3 must match the 10.0.0.0/8 deny list")
	}
}

// policy.go:272-273: SplitHostPort failure inside the pinned dialer.
func TestAgentEDialRejectsMalformedAddress(t *testing.T) {
	tr := agentETransport(t, Policy{})
	_, err := tr.DialContext(context.Background(), "tcp", "no-port-here")
	if err == nil {
		t.Fatal("expected SplitHostPort error, got nil")
	}
	if !strings.Contains(err.Error(), "missing port") {
		t.Fatalf("unexpected dial error: %v", err)
	}
}

// policy.go:275-276: the operator deny-list applies at dial time even
// when the host was never a validated target.
func TestAgentEDialRejectsDeniedHost(t *testing.T) {
	tr := agentETransport(t, Policy{DenyHosts: []string{"denied.example"}})
	_, err := tr.DialContext(context.Background(), "tcp", "denied.example:443")
	if err == nil {
		t.Fatal("expected denied-host error, got nil")
	}
	if !strings.Contains(err.Error(), "denied by operator policy") {
		t.Fatalf("unexpected dial error: %v", err)
	}
}

// policy.go:286-287: an IP-literal dial target inside a deny CIDR.
func TestAgentEDialRejectsDeniedIPLiteral(t *testing.T) {
	tr := agentETransport(t, Policy{DenyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	_, err := tr.DialContext(context.Background(), "tcp", "10.1.2.3:443")
	if err == nil {
		t.Fatal("expected denied-address error, got nil")
	}
	if !strings.Contains(err.Error(), "denied by operator policy") {
		t.Fatalf("unexpected dial error: %v", err)
	}
}

// policy.go:303-304: a hostname resolving to a blocked address fails
// closed. localhost resolves via /etc/hosts to loopback, and loopback
// is not allowed under this policy, so the DNS-answer check rejects it
// before any connection is attempted.
func TestAgentEDialRejectsBlockedDNSAnswer(t *testing.T) {
	tr := agentETransport(t, Policy{})
	_, err := tr.DialContext(context.Background(), "tcp", "localhost:8443")
	if err == nil {
		t.Fatal("expected blocked-DNS-answer error, got nil")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("unexpected dial error: %v", err)
	}
}
