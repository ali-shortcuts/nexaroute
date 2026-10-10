package egress

import (
	"net/http"
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
