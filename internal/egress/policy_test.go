package egress

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestPolicyRejectsPrivateAndAllowListMismatch(t *testing.T) {
	p := Policy{AllowedHosts: []string{"api.example.com"}}
	if _, err := p.NewTransport("https://127.0.0.1:8443", ""); err == nil {
		t.Fatal("private literal must be rejected")
	}
	if _, err := p.NewTransport("https://other.example.com", ""); err == nil {
		t.Fatal("host outside allow-list must be rejected")
	}
}

func TestPolicyAllowsExplicitLoopbackHTTPOnly(t *testing.T) {
	p := Policy{AllowLoopback: true}
	tr, err := p.NewTransport("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proxy != nil || tr.DialContext == nil {
		t.Fatal("expected direct pinned transport")
	}
	if _, err := (Policy{}).NewTransport("http://127.0.0.1:8080", ""); err == nil {
		t.Fatal("loopback HTTP must require explicit opt-in")
	}
}

func TestPolicyValidatesExplicitProxyConfiguration(t *testing.T) {
	tr, err := (Policy{}).NewTransport("https://api.example.com", "http://proxy.example.com:8080")
	if err != nil || tr.Proxy == nil {
		t.Fatalf("explicit proxy should be preserved: transport=%v err=%v", tr, err)
	}
	if _, err := (Policy{}).NewTransport("https://api.example.com", "not a url"); err == nil {
		t.Fatal("malformed proxy must be rejected")
	}
}

func TestPolicyTransportIsHTTPTransport(t *testing.T) {
	tr, err := (Policy{}).NewTransport("https://api.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: tr}
	if client.Transport == nil {
		t.Fatal("transport missing")
	}
}

func TestCheckDialAnswersPinsLoopbackSpelledNamesToLoopback(t *testing.T) {
	loopback := netip.MustParseAddr("127.0.0.1")
	public := netip.MustParseAddr("93.184.216.34")
	private := netip.MustParseAddr("10.0.0.1")

	if err := checkDialAnswers("localhost", []netip.Addr{loopback}, true); err != nil {
		t.Fatalf("legit loopback answer rejected: %v", err)
	}
	if err := checkDialAnswers("localhost", []netip.Addr{public}, true); err == nil {
		t.Fatal("loopback-spelled name resolving to a public address was allowed")
	}
	if err := checkDialAnswers("localhost", []netip.Addr{loopback, public}, true); err == nil {
		t.Fatal("mixed loopback/public answers were allowed for a loopback-spelled name")
	}
	if err := checkDialAnswers("example.com", []netip.Addr{public}, false); err != nil {
		t.Fatalf("ordinary public answer rejected: %v", err)
	}
	if err := checkDialAnswers("example.com", []netip.Addr{private}, false); err == nil {
		t.Fatal("private answer was allowed")
	}
}
