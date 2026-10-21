package httpapi

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func trustedProxyConfig(t *testing.T, cidrs ...string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Admin.APIKey = "test-admin-key"
	cfg.TLS.TrustedProxyCIDRs = cidrs
	return testGateway(t, cfg)
}

func csrfBootstrapRequest(remote, forwardedProto string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/csrf-token", nil)
	req.RemoteAddr = remote
	req.Header.Set("x-admin-key", "test-admin-key")
	if forwardedProto != "" {
		req.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	return req
}

func csrfCookieSecure(t *testing.T, rr *httptest.ResponseRecorder) bool {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("csrf bootstrap status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == adminCSRFCookie {
			return cookie.Secure
		}
	}
	t.Fatalf("csrf cookie not set")
	return false
}

func TestTrustedProxySuppliesSecureScheme(t *testing.T) {
	s := trustedProxyConfig(t, "127.0.0.1/32")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, csrfBootstrapRequest("127.0.0.1:32100", "https"))
	if !csrfCookieSecure(t, rr) {
		t.Fatal("trusted proxy with X-Forwarded-Proto=https should produce a Secure cookie")
	}
}

func TestTrustedProxyPlainForwardedSchemeStaysInsecure(t *testing.T) {
	s := trustedProxyConfig(t, "127.0.0.1/32")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, csrfBootstrapRequest("127.0.0.1:32100", "http"))
	if csrfCookieSecure(t, rr) {
		t.Fatal("trusted proxy reporting http must not produce a Secure cookie")
	}
}

func TestUntrustedPeerCannotSupplySecureScheme(t *testing.T) {
	s := trustedProxyConfig(t, "10.0.0.0/8")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, csrfBootstrapRequest("127.0.0.1:32100", "https"))
	if csrfCookieSecure(t, rr) {
		t.Fatal("untrusted peer must not control the cookie Secure attribute")
	}
}

func TestTrustedProxyRejectsAmbiguousForwardedScheme(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{name: "comma chain", values: []string{"https, http"}},
		{name: "duplicate header", values: []string{"https", "https"}},
		{name: "empty", values: []string{""}},
		{name: "unknown token", values: []string{"wss"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := trustedProxyConfig(t, "127.0.0.1/32")
			req := csrfBootstrapRequest("127.0.0.1:32100", "")
			for _, value := range tt.values {
				req.Header.Add("X-Forwarded-Proto", value)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if csrfCookieSecure(t, rr) {
				t.Fatalf("ambiguous forwarding header %q was trusted", tt.values)
			}
		})
	}
}

func TestForwardedHeaderAloneIsIgnored(t *testing.T) {
	s := trustedProxyConfig(t, "127.0.0.1/32")
	req := csrfBootstrapRequest("127.0.0.1:32100", "")
	req.Header.Set("Forwarded", "proto=https")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if csrfCookieSecure(t, rr) {
		t.Fatal("the Forwarded header must not control cookie security")
	}
}

func TestDirectTLSIsAlwaysSecure(t *testing.T) {
	s := trustedProxyConfig(t, "10.0.0.0/8")
	req := csrfBootstrapRequest("127.0.0.1:32100", "")
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if !csrfCookieSecure(t, rr) {
		t.Fatal("a direct TLS connection should always produce a Secure cookie")
	}
}

func TestTrustedProxySessionAndOIDFCookies(t *testing.T) {
	s := trustedProxyConfig(t, "127.0.0.1/32")
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/", nil)
	req.RemoteAddr = "127.0.0.1:32100"
	req = s.withTrustedProxyContext(req)
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	setAdminSessionCookie(rr, req, "session-token", time.Now().Add(time.Hour))
	setOIDCFlowCookie(rr, req, "binding-token")
	clearAdminSessionCookie(rr, req)
	for _, cookie := range rr.Result().Cookies() {
		if !cookie.Secure {
			t.Fatalf("trusted proxy cookie %q should be Secure", cookie.Name)
		}
	}
}

func TestTrustedProxySameOriginScheme(t *testing.T) {
	s := trustedProxyConfig(t, "127.0.0.1/32")
	request := func(remote string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/drain", nil)
		req.RemoteAddr = remote
		req.Header.Set("Origin", "https://gateway")
		req.Header.Set("X-Forwarded-Proto", "https")
		return s.withTrustedProxyContext(req)
	}
	if !sameOrigin(request("127.0.0.1:32100")) {
		t.Fatal("https origin should match a trusted proxy's https scheme")
	}
	if sameOrigin(request("127.0.0.2:32100")) {
		t.Fatal("untrusted peer must not make an https origin match a plain-HTTP listener")
	}
}

func TestTrustedProxyRejectsHTTPOriginWhenProxySaysHTTPS(t *testing.T) {
	s := trustedProxyConfig(t, "127.0.0.1/32")
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/drain", nil)
	req.RemoteAddr = "127.0.0.1:32100"
	req.Header.Set("Origin", "http://gateway")
	req.Header.Set("X-Forwarded-Proto", "https")
	req = s.withTrustedProxyContext(req)
	if sameOrigin(req) {
		t.Fatal("an http origin must not match when the trusted proxy reports https")
	}
}
