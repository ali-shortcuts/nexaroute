package httpapi

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func adminSecurityRequest(s *Server, method, path string, tlsState *tls.ConnectionState, cookie *http.Cookie, origin, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(`{"draining":true}`))
	req.RemoteAddr = "127.0.0.1:32100"
	req.Header.Set("x-admin-key", "test-admin-key")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if csrf != "" {
		req.Header.Set("X-NexaRoute-CSRF", csrf)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.TLS = tlsState
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAdminCSRFTokenCookieAndMutationGate(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "test-admin-key"
	s := testGateway(t, cfg)

	bootstrap := adminSecurityRequest(s, http.MethodGet, "/admin/api/csrf-token", nil, nil, "", "")
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	cookies := bootstrap.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one csrf cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != adminCSRFCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Secure {
		t.Fatalf("unexpected HTTP csrf cookie properties: %+v", cookie)
	}

	missing := adminSecurityRequest(s, http.MethodPost, "/admin/api/drain", nil, nil, "http://gateway", "")
	if missing.Code != http.StatusForbidden {
		t.Fatalf("state-changing browser request without token status=%d", missing.Code)
	}
	crossOrigin := adminSecurityRequest(s, http.MethodPost, "/admin/api/drain", nil, cookie, "https://attacker.invalid", cookie.Value)
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request status=%d", crossOrigin.Code)
	}
	valid := adminSecurityRequest(s, http.MethodPost, "/admin/api/drain", nil, cookie, "http://gateway", cookie.Value)
	if valid.Code != http.StatusOK {
		t.Fatalf("same-origin request with token status=%d body=%s", valid.Code, valid.Body.String())
	}
}

func TestAdminCSRFTokenCookieSecureOnTLS(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "test-admin-key"
	s := testGateway(t, cfg)
	tlsState := &tls.ConnectionState{Version: tls.VersionTLS13}
	rr := adminSecurityRequest(s, http.MethodGet, "/admin/api/csrf-token", tlsState, nil, "", "")
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("TLS cookie attributes not secure: %+v", cookies)
	}
}

func TestMTLSRequirementsAreScopedToAdminAndDataPlane(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "test-admin-key"
	cfg.TLS.Enabled = true
	cfg.TLS.RequireClientCertAdmin = true
	cfg.TLS.RequireClientCertDataPlane = true
	s := testGateway(t, cfg)

	noCert := adminSecurityRequest(s, http.MethodGet, "/admin/api/snapshot", nil, nil, "", "")
	if noCert.Code != http.StatusForbidden {
		t.Fatalf("admin request without verified certificate status=%d", noCert.Code)
	}
	verified := &tls.ConnectionState{Version: tls.VersionTLS13, VerifiedChains: [][]*x509.Certificate{{{}}}}
	withCert := adminSecurityRequest(s, http.MethodGet, "/admin/api/snapshot", verified, nil, "", "")
	if withCert.Code != http.StatusOK {
		t.Fatalf("admin request with verified certificate status=%d body=%s", withCert.Code, withCert.Body.String())
	}

	dataNoCert := httptest.NewRequest(http.MethodGet, "http://gateway/v1/models", nil)
	dataNoCert.RemoteAddr = "127.0.0.1:32100"
	dataNoCertRecorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(dataNoCertRecorder, dataNoCert)
	if dataNoCertRecorder.Code != http.StatusForbidden {
		t.Fatalf("data-plane request without certificate status=%d", dataNoCertRecorder.Code)
	}
	dataWithCert := httptest.NewRequest(http.MethodGet, "http://gateway/v1/models", nil)
	dataWithCert.RemoteAddr = "127.0.0.1:32100"
	dataWithCert.TLS = verified
	dataWithCertRecorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(dataWithCertRecorder, dataWithCert)
	if dataWithCertRecorder.Code == http.StatusForbidden {
		t.Fatalf("data-plane request with verified certificate was rejected: %s", dataWithCertRecorder.Body.String())
	}
}
