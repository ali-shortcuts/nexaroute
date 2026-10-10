package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
	"golang.org/x/oauth2"
)

func TestCoverageAdminAuthPathResolutionAndValidation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "config.json")
	got, err := adminSecurityStorePath("  data/../security.db  ", configPath)
	if err != nil || got != filepath.Join(filepath.Dir(configPath), "security.db") {
		t.Fatalf("relative path = %q, err=%v", got, err)
	}
	absolute := filepath.Join(t.TempDir(), "absolute.db")
	got, err = adminSecurityStorePath(absolute, configPath)
	if err != nil || got != absolute {
		t.Fatalf("absolute path = %q, err=%v", got, err)
	}
	got, err = adminSecurityStorePath("", configPath)
	if err != nil || got != filepath.Join(filepath.Dir(configPath), "security", "nexaroute-security.db") {
		t.Fatalf("default path = %q, err=%v", got, err)
	}
	if _, err := adminSecurityStorePath("", " "); err == nil {
		t.Fatal("missing configured and config paths accepted")
	}
}

func TestCoverageAdminAuthCookieHashAndLoopbackParsing(t *testing.T) {
	value := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	if _, ok := opaqueSessionHash(value); !ok {
		t.Fatal("canonical 32-byte URL token rejected")
	}
	for _, invalid := range []string{"", "short", value + "=", strings.Repeat("!", 43)} {
		if _, ok := opaqueSessionHash(invalid); ok {
			t.Fatalf("invalid opaque session token accepted: %q", invalid)
		}
	}
	for _, tc := range []struct {
		addr string
		want bool
	}{{"127.0.0.1:80", true}, {"[::1]:443", true}, {"::1", true}, {"192.0.2.1:80", false}, {"not-an-ip", false}} {
		if got := remoteIsLoopback(tc.addr); got != tc.want {
			t.Errorf("remoteIsLoopback(%q)=%v, want %v", tc.addr, got, tc.want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
	req.AddCookie(&http.Cookie{Name: "other", Value: "ignored"})
	if value, present, valid := cookieValue(req, adminSessionCookie); present || !valid || value != "" {
		t.Fatalf("missing cookie = %q,%v,%v", value, present, valid)
	}
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "one"})
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "two"})
	if value, present, valid := cookieValue(req, adminSessionCookie); !present || valid || value != "" {
		t.Fatalf("duplicate cookie = %q,%v,%v", value, present, valid)
	}
}

func TestCoverageAdminAuthEmergencyKeyAndKeylessHostRestrictions(t *testing.T) {
	cfg := config.AdminConfig{APIKey: "constant-time-test-key", EmergencyAccessEnabled: true}
	request := httptest.NewRequest(http.MethodGet, "http://gateway/admin", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("X-Admin-Key", "constant-time-test-key")
	if !legacyAdminAuthorized(request, cfg) {
		t.Fatal("matching explicit emergency key was rejected")
	}
	request.Header.Del("X-Admin-Key")
	request.Header.Set("Authorization", "Bearer constant-time-test-key")
	if !legacyAdminAuthorized(request, cfg) {
		t.Fatal("matching bearer emergency key was rejected")
	}
	request.Header.Set("X-Admin-Key", "constant-time-test-keX")
	if legacyAdminAuthorized(request, cfg) {
		t.Fatal("wrong emergency key accepted")
	}
	request.Header.Del("X-Admin-Key")
	request.RemoteAddr = "198.51.100.10:1234"
	cfg.BindLocalOnly = true
	if legacyAdminAuthorized(request, cfg) {
		t.Fatal("non-loopback emergency client accepted")
	}
	keyless := config.AdminConfig{EmergencyAccessEnabled: true}
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Del("Authorization")
	request.Host = "localhost:8080"
	if !legacyAdminAuthorized(request, keyless) {
		t.Fatal("safe loopback-host keyless emergency request rejected")
	}
	request.Host = "attacker.example"
	if legacyAdminAuthorized(request, keyless) {
		t.Fatal("DNS-rebound host accepted for keyless emergency access")
	}
}

func TestCoverageAdminAuthUnavailableAndLocalOnlyFailClosed(t *testing.T) {
	zeroServer := &Server{}
	if err := zeroServer.CloseSecurityStore(); err != nil {
		t.Fatalf("zero server close: %v", err)
	}
	if zeroServer.SecurityStoreError() != nil {
		t.Fatal("zero server reported a security-store error")
	}
	if _, _, ok, err := zeroServer.authenticateAdmin(httptest.NewRequest(http.MethodGet, "http://gateway/", nil)); ok || err == nil {
		t.Fatalf("nil security store authentication = ok %v, err %v", ok, err)
	}

	cfg := config.Default()
	cfg.Admin.BindLocalOnly = true
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.RemoteAddr = "203.0.113.10:90"
	if srv.adminAuthorized(req) {
		t.Fatal("remote address passed local-only guard")
	}
	req.RemoteAddr = "127.0.0.1:90"
	if srv.adminAuthorized(req) {
		t.Fatal("unauthenticated loopback request was authorized")
	}
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	if srv.adminAuthorized(req) {
		t.Fatal("closed security store authorized admin")
	}
}

func TestCoverageAdminAuthContextAuditAndCookieClearInvariants(t *testing.T) {
	identity := authz.Identity{Subject: "coverage-user", Roles: []authz.Role{authz.RoleViewer}}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin", nil)
	if got := adminIdentityFromRequest(withAdminIdentity(req, identity)); got.Subject != identity.Subject || len(got.Roles) != 1 {
		t.Fatalf("identity context round-trip = %#v", got)
	}
	if got := adminIdentityFromRequest(req); got.Subject != "" {
		t.Fatalf("identity unexpectedly present in original context: %#v", got)
	}
	req.Header.Set("X-Trace-ID", "trace-fallback")
	event := auditEvent(req, "subject", "action", "success", http.StatusOK, "oidc-session", authz.ReadConfig)
	if event.RequestID != "trace-fallback" || event.Actor != "subject" || event.Permission != string(authz.ReadConfig) {
		t.Fatalf("audit event fallback fields = %#v", event)
	}
	req.Header.Set("X-Request-ID", "request-preferred")
	if got := auditEvent(req, "subject", "action", "success", http.StatusOK, "oidc-session", "").RequestID; got != "request-preferred" {
		t.Fatalf("request id precedence = %q", got)
	}

	for _, tc := range []struct {
		name   string
		url    string
		secure bool
	}{{"tls", "https://gateway/admin", true}, {"plain", "http://gateway/admin", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			w := httptest.NewRecorder()
			setAdminSessionCookie(w, r, "coverage-token", time.Now().Add(time.Hour))
			setOIDCFlowCookie(w, r, "binding-token")
			cookies := w.Result().Cookies()
			if len(cookies) != 2 || cookies[0].Secure != tc.secure || cookies[1].Secure != tc.secure || !cookies[0].HttpOnly || cookies[0].Path != "/admin/" {
				t.Fatalf("set cookies = %#v", cookies)
			}
			w = httptest.NewRecorder()
			clearAdminSessionCookie(w, r)
			cookies = w.Result().Cookies()
			if len(cookies) != 3 {
				t.Fatalf("cleared cookie count = %d", len(cookies))
			}
			for _, cookie := range cookies {
				if cookie.Value != "" || cookie.MaxAge >= 0 || cookie.Secure != tc.secure {
					t.Errorf("cookie not cleared safely: %#v", cookie)
				}
			}
		})
	}
	w := httptest.NewRecorder()
	setAdminSessionCookie(w, httptest.NewRequest(http.MethodGet, "http://gateway/", nil), "expired", time.Now().Add(-time.Hour))
	if got := w.Result().Cookies()[0].MaxAge; got != 0 {
		t.Fatalf("expired cookie max-age=%d, want zero", got)
	}
}

func TestCoverageAdminAuthStatusAndLoginBadRequests(t *testing.T) {
	srv := testGateway(t, config.Default())
	w := httptest.NewRecorder()
	srv.adminAuthStatus(w, httptest.NewRequest(http.MethodPost, "http://gateway/admin/auth/status", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("auth status method response: %d headers=%v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	srv.adminAuthStatus(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/status", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"authenticated":false`) || w.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("disabled OIDC status: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	srv.authLogin(w, httptest.NewRequest(http.MethodPost, "http://gateway/admin/auth/login", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("login method response: %d allow=%q", w.Code, w.Header().Get("Allow"))
	}
	w = httptest.NewRecorder()
	srv.authLogin(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/login", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled OIDC login status=%d body=%s", w.Code, w.Body.String())
	}

	srv.runtimeMu.Lock()
	srv.cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: "https://issuer.example.test"}
	srv.runtimeMu.Unlock()
	srv.oidc = nil
	srv.oidcErr = errors.New("discovery is intentionally unavailable")
	w = httptest.NewRecorder()
	srv.authLogin(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/login", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable OIDC login status=%d body=%s", w.Code, w.Body.String())
	}
	srv.oidc = &oidcAuthenticator{issuer: "https://issuer.example.test", policyHash: "coverage-policy"}
	srv.oidcErr = nil
	r := httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/login", nil)
	r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "first"})
	r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "second"})
	w = httptest.NewRecorder()
	srv.authLogin(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate session cookie login status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCoverageAdminAuthStatusProviderAndStoreFailures(t *testing.T) {
	srv := testGateway(t, config.Default())
	srv.runtimeMu.Lock()
	srv.cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: "https://issuer.example.test"}
	srv.runtimeMu.Unlock()
	srv.oidc = nil
	srv.oidcErr = errors.New("offline")
	w := httptest.NewRecorder()
	srv.adminAuthStatus(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/status", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"oidc_available":false`) || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("unavailable-provider status response: %d %s headers=%v", w.Code, w.Body.String(), w.Header())
	}
	srv.oidc = &oidcAuthenticator{issuer: "https://issuer.example.test", policyHash: "coverage-policy"}
	srv.oidcErr = nil
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/status", nil)
	r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))})
	srv.adminAuthStatus(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed session store status=%d body=%s", w.Code, w.Body.String())
	}
	srv.securityErr = errors.New("unavailable")
	w = httptest.NewRecorder()
	srv.adminAuthStatus(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/status", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("configured store error status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCoverageAdminAuthSessionRejectsInvalidStoredRoleClaims(t *testing.T) {
	srv := testGateway(t, config.Default())
	installTestOIDCSession(t, srv, authz.RoleViewer)
	now := time.Now().UTC().Truncate(time.Second)
	badRoles := [][]string{{"superuser"}, {}, {"viewer", "admin"}, {"operator", "unknown"}}
	for i, roles := range badRoles {
		id := make([]byte, 32)
		for j := range id {
			id[j] = byte(0x70 + i)
		}
		cookie := base64.RawURLEncoding.EncodeToString(id)
		session := securitystore.Session{Subject: "invalid-role-user", Issuer: srv.oidc.issuer, Roles: roles, PolicyHash: srv.oidc.policyHash, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
		if err := srv.securityDB.CreateSession(context.Background(), opaqueHash(cookie), session, securitystore.AuditEvent{Actor: "invalid-role-user", Action: "login", Outcome: "success", Status: http.StatusSeeOther}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "http://gateway/admin", nil)
		r.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookie})
		if _, _, present, ok, err := srv.sessionIdentity(r); err != nil || !present || ok {
			t.Errorf("unsafe role set %v accepted: present=%v ok=%v err=%v", roles, present, ok, err)
		}
	}
}

func TestCoverageAdminAuthLoginAuditFailureFailsClosed(t *testing.T) {
	srv := testGateway(t, config.Default())
	srv.runtimeMu.Lock()
	srv.cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: "https://issuer.example.test"}
	srv.runtimeMu.Unlock()
	srv.oidc = &oidcAuthenticator{
		issuer: "https://issuer.example.test", policyHash: "coverage-policy",
		oauth:   &oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://issuer.example.test/authorize"}},
		pending: make(map[string]pendingOIDCLogin),
	}
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.authLogin(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/login", nil))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "authorization_code") || len(w.Result().Cookies()) != 0 {
		t.Fatalf("login continued after audit failure: %d body=%s cookies=%#v", w.Code, w.Body.String(), w.Result().Cookies())
	}
}

func TestCoverageAdminAuthCallbackRejectsBadStateAndBinding(t *testing.T) {
	srv := testGateway(t, config.Default())
	srv.runtimeMu.Lock()
	srv.cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: "https://issuer.example.test"}
	srv.runtimeMu.Unlock()
	srv.oidc = &oidcAuthenticator{issuer: "https://issuer.example.test", policyHash: "coverage-policy"}
	srv.oidcErr = nil
	cases := []struct {
		name    string
		url     string
		cookies []*http.Cookie
	}{
		{name: "missing state", url: "http://gateway/admin/auth/callback?code=code"},
		{name: "repeated state", url: "http://gateway/admin/auth/callback?state=a&state=b", cookies: []*http.Cookie{{Name: adminOIDCFlowCookie, Value: "binding"}}},
		{name: "missing binding", url: "http://gateway/admin/auth/callback?state=unknown"},
		{name: "duplicate binding", url: "http://gateway/admin/auth/callback?state=unknown", cookies: []*http.Cookie{{Name: adminOIDCFlowCookie, Value: "a"}, {Name: adminOIDCFlowCookie, Value: "b"}}},
		{name: "unknown state", url: "http://gateway/admin/auth/callback?state=unknown&code=code", cookies: []*http.Cookie{{Name: adminOIDCFlowCookie, Value: "binding"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			for _, cookie := range tc.cookies {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			srv.authCallback(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("bad callback accepted: %d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("failure response cache policy = %q", w.Header().Get("Cache-Control"))
			}
		})
	}
	w := httptest.NewRecorder()
	srv.authCallback(w, httptest.NewRequest(http.MethodPost, "http://gateway/admin/auth/callback", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("callback method response: %d allow=%q", w.Code, w.Header().Get("Allow"))
	}
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	srv.authCallback(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/callback?state=x", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("callback with unavailable store status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCoverageAdminAuthLogoutCSRFAndAuditQueryValidation(t *testing.T) {
	srv := testGateway(t, config.Default())
	w := httptest.NewRecorder()
	srv.adminAuthLogout(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/logout", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("logout method response: %d headers=%v", w.Code, w.Header())
	}
	r := httptest.NewRequest(http.MethodPost, "http://gateway/admin/auth/logout", nil)
	r.Header.Set("Origin", "http://gateway")
	w = httptest.NewRecorder()
	srv.adminAuthLogout(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF status=%d", w.Code)
	}
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: token})
	r.Header.Set("X-NexaRoute-CSRF", token)
	w = httptest.NewRecorder()
	srv.adminAuthLogout(w, r)
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 3 {
		t.Fatalf("anonymous logout response: %d cookies=%#v body=%s", w.Code, w.Result().Cookies(), w.Body.String())
	}

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		w = httptest.NewRecorder()
		srv.adminAuditRecords(w, httptest.NewRequest(method, "http://gateway/admin/audit", nil))
		if method != http.MethodGet && w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("audit method %s status=%d", method, w.Code)
		}
	}
	for _, query := range []string{"?limit=0", "?limit=1001", "?limit=not-a-number"} {
		w = httptest.NewRecorder()
		srv.adminAuditRecords(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/audit"+query, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("audit query %q status=%d body=%s", query, w.Code, w.Body.String())
		}
	}
}

func TestCoverageAdminAuthFailedAuditIsUnavailable(t *testing.T) {
	srv := testGateway(t, config.Default())
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	if err := srv.recordAudit(securitystore.AuditEvent{Actor: "a", Action: "x", Outcome: "denied", Status: 401}); err == nil {
		t.Fatal("recordAudit succeeded after database close")
	}
	w := httptest.NewRecorder()
	srv.writeLoginFailure(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/callback", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed-login audit outage status=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	srv.denyAdminRequest(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/x", nil), authz.Identity{}, "", "", http.StatusForbidden, "denied")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("denial audit outage status=%d body=%s", w.Code, w.Body.String())
	}
}
