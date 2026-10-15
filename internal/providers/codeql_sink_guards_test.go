package providers

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// The request-dispatch sink only ever dials hosts the operator
// configured: the base URL host or an allowed_hosts entry. A
// cross-host absolute endpoint outside that set is refused while
// the request is being built, before any credential is attached.
func TestHostAllowedMirrorsEgressAllowlist(t *testing.T) {
	mk := func(base string, allowed []string) *httpAdapter {
		return &httpAdapter{p: config.ProviderConfig{BaseURL: base, AllowedHosts: allowed}}
	}
	cases := []struct {
		name string
		a    *httpAdapter
		url  string
		want bool
	}{
		{"base host", mk("https://api.example.com/v1", nil), "https://api.example.com/v1/models", true},
		{"base host case + trailing dot", mk("https://api.example.com", nil), "https://API.example.com./v1", true},
		{"other host, no list", mk("https://api.example.com", nil), "https://evil.example.com/v1", false},
		{"listed host", mk("https://api.example.com", []string{"catalog.example.com"}), "https://catalog.example.com/models", true},
		{"listed host replaces base default", mk("https://api.example.com", []string{"catalog.example.com"}), "https://api.example.com/v1", false},
		{"unlisted host with list", mk("https://api.example.com", []string{"catalog.example.com"}), "https://evil.example.com/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			if err != nil {
				t.Fatal(err)
			}
			if got := tc.a.hostAllowed(u); got != tc.want {
				t.Fatalf("hostAllowed(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestDoPathRefusesNonAllowlistedAbsoluteEndpoint(t *testing.T) {
	var hits atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.2: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	other := "http://" + ln.Addr().String() + "/v1/models"

	base := "http://127.0.0.1:1" // unroutable on purpose: refusal must precede dialing
	a := &httpAdapter{p: config.ProviderConfig{BaseURL: base}, c: &http.Client{}, sem: make(chan struct{}, 1)}
	_, err = a.DoPath(context.Background(), http.MethodGet, other, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "allowed hosts") {
		t.Fatalf("cross-host endpoint must be refused by the allowlist, err=%v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("refused endpoint was dialed anyway")
	}

	// With the host allowlisted, the same absolute endpoint is served.
	b := &httpAdapter{p: config.ProviderConfig{BaseURL: base, AllowedHosts: []string{"127.0.0.2"}}, c: &http.Client{Timeout: 5 * time.Second}, sem: make(chan struct{}, 1), forwardAllowed: map[string]struct{}{}}
	resp, err := b.DoPath(context.Background(), http.MethodGet, other, nil, false, nil)
	if err != nil {
		t.Fatalf("allowlisted endpoint must be served: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("allowlisted endpoint hits=%d, want 1", hits.Load())
	}
}
