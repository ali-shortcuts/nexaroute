package egress

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://a.example/x", "https://a.example/y", true},
		{"HTTPS://a.example/x", "https://A.EXAMPLE/y", true}, // case-insensitive
		{"https://a.example:8443/x", "https://a.example:8443/y", true},
		{"https://a.example/x", "http://a.example/x", false},            // scheme change
		{"https://a.example/x", "https://b.example/x", false},           // host change
		{"https://a.example:8443/x", "https://a.example:9443/x", false}, // port change
	}
	for _, tc := range cases {
		if got := SameOrigin(mustParseURL(t, tc.a), mustParseURL(t, tc.b)); got != tc.want {
			t.Errorf("SameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if SameOrigin(nil, mustParseURL(t, "https://a.example/")) {
		t.Error("SameOrigin(nil, u) = true, want false")
	}
	if SameOrigin(mustParseURL(t, "https://a.example/"), nil) {
		t.Error("SameOrigin(u, nil) = true, want false")
	}
}

func TestRefuseCrossOriginRedirects(t *testing.T) {
	guard := RefuseCrossOriginRedirects(3, "unit")
	origin := mustParseURL(t, "https://origin.example/hook")

	mkReq := func(target string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	first := mkReq(origin.String())

	// No hops yet: nothing to judge.
	if err := guard(first, nil); err != nil {
		t.Fatalf("empty via: %v", err)
	}
	// Same-origin redirect: allowed.
	if err := guard(mkReq("https://origin.example/other"), []*http.Request{first}); err != nil {
		t.Fatalf("same-origin redirect refused: %v", err)
	}
	// Cross-host redirect: refused.
	if err := guard(mkReq("https://evil.example/hook"), []*http.Request{first}); err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("cross-host redirect: got %v, want refusal", err)
	}
	// Scheme downgrade to the same host: refused (origin boundary).
	if err := guard(mkReq("http://origin.example/hook"), []*http.Request{first}); err == nil {
		t.Fatal("scheme-downgrade redirect was not refused")
	}
	// Cap: the third hop onward is stopped even same-origin.
	via := []*http.Request{first, mkReq("https://origin.example/a"), mkReq("https://origin.example/b")}
	if err := guard(mkReq("https://origin.example/c"), via); err == nil || !strings.Contains(err.Error(), "stopped after 3 redirects") {
		t.Fatalf("cap: got %v, want stopped-after-3", err)
	}
	// The service name appears in the error for operability.
	if err := guard(mkReq("https://evil.example/"), []*http.Request{first}); err == nil || !strings.Contains(err.Error(), "unit:") {
		t.Fatalf("service name missing from error: %v", err)
	}
}
