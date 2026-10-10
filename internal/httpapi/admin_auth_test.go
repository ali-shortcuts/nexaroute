package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

func installTestOIDCSession(t *testing.T, srv *Server, role authz.Role) string {
	t.Helper()
	const issuer = "https://issuer.example.test"
	const policy = "test-policy-fingerprint"
	srv.runtimeMu.Lock()
	srv.cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: issuer, RoleClaim: "groups", RoleMappings: map[string]string{"gateway-admin": "admin"}}
	srv.cfg.Admin.EmergencyAccessEnabled = false
	srv.cfg.Admin.SessionTTLSeconds = 3600
	srv.cfg.Admin.IdleTimeoutSeconds = 300
	srv.runtimeMu.Unlock()
	srv.oidc = &oidcAuthenticator{issuer: issuer, policyHash: policy}
	srv.oidcErr = nil

	rawID := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	now := time.Now().UTC().Truncate(time.Second)
	cfg := srv.adminConfigSnapshot()
	session := securitystore.Session{
		Subject: "user-123", Issuer: issuer, Roles: []string{string(role)}, PolicyHash: policy,
		CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Duration(cfg.SessionTTLSeconds) * time.Second),
	}
	event := securitystore.AuditEvent{Timestamp: now, Actor: "user-123", Action: "auth.login.succeeded", Outcome: "success", Status: http.StatusSeeOther}
	if err := srv.securityDB.CreateSession(context.Background(), opaqueHash(rawID), session, event); err != nil {
		t.Fatal(err)
	}
	return rawID
}

func TestViewerSessionCanReadConfigButNotKeysOrAudit(t *testing.T) {
	srv := testGateway(t, config.Default())
	cookieValue := installTestOIDCSession(t, srv, authz.RoleViewer)
	serve := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
		req.RemoteAddr = "127.0.0.1:10001"
		req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookieValue})
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}

	snapshot := serve("/admin/api/snapshot?events=20")
	if snapshot.Code != http.StatusOK {
		t.Fatalf("viewer snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(snapshot.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if events, ok := payload["events"].([]any); !ok || len(events) != 0 {
		t.Fatalf("viewer snapshot leaked audit-like events: %#v", payload["events"])
	}
	if usage, ok := payload["usage"]; !ok || usage == nil {
		t.Fatalf("viewer should retain the explicitly granted ReadUsage permission: %#v", usage)
	}

	keys := serve("/admin/api/client-keys")
	if keys.Code != http.StatusForbidden {
		t.Fatalf("viewer client-key read status=%d, want forbidden: %s", keys.Code, keys.Body.String())
	}
	audit := serve("/admin/api/audit")
	if audit.Code != http.StatusForbidden {
		t.Fatalf("viewer audit read status=%d, want forbidden: %s", audit.Code, audit.Body.String())
	}
}

func TestOIDCRolePermissionsReachActualAdminHandlers(t *testing.T) {
	roles := []authz.Role{authz.RoleViewer, authz.RoleOperator, authz.RoleAdmin}
	for _, role := range roles {
		cfg := config.Default()
		cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "default", Mode: "all"}}
		cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "default", CandidatePool: "default"}}
		srv := testGateway(t, cfg)
		handler := srv.Handler()
		cookieValue := installTestOIDCSession(t, srv, role)
		request := func(method, path, body string) *httptest.ResponseRecorder {
			t.Helper()
			req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(body))
			if body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			ip := oidcTestRequestIP.Add(1)%250 + 1
			req.RemoteAddr = fmt.Sprintf("127.0.0.%d:10100", ip)
			req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookieValue})
			const csrf = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if isStateChanging(method) {
				req.Header.Set("Origin", "http://gateway")
				req.Header.Set("X-NexaRoute-CSRF", csrf)
				req.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: csrf})
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			return response
		}

		if response := request(http.MethodGet, "/admin/api/snapshot", ""); response.Code != http.StatusOK {
			t.Fatalf("%s could not read permitted snapshot: %d %s", role, response.Code, response.Body.String())
		}
		settings := request(http.MethodGet, "/admin/api/settings", "")
		if settings.Code != http.StatusOK {
			t.Fatalf("%s could not read settings: %d %s", role, settings.Code, settings.Body.String())
		}
		keys := request(http.MethodGet, "/admin/api/client-keys", "")
		if role == authz.RoleAdmin {
			if keys.Code != http.StatusOK {
				t.Fatalf("admin client-key read failed: %d %s", keys.Code, keys.Body.String())
			}
		} else if keys.Code != http.StatusForbidden {
			t.Fatalf("%s read admin client keys: %d %s", role, keys.Code, keys.Body.String())
		}

		audit := request(http.MethodGet, "/admin/api/audit", "")
		if role == authz.RoleAdmin {
			if audit.Code != http.StatusOK {
				t.Fatalf("admin audit read failed: %d %s", audit.Code, audit.Body.String())
			}
		} else if audit.Code != http.StatusForbidden {
			t.Fatalf("%s read privileged audit records: %d %s", role, audit.Code, audit.Body.String())
		}

		configWrite := request(http.MethodPut, "/admin/api/settings", settings.Body.String())
		if role == authz.RoleAdmin {
			if configWrite.Code != http.StatusOK {
				t.Fatalf("admin settings mutation failed: %d %s", configWrite.Code, configWrite.Body.String())
			}
			events, err := srv.securityDB.AuditRecords(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			foundCompletion := false
			for _, event := range events {
				if event.Action == "admin.operation.completed" && event.Target == "/admin/api/settings" && event.Outcome == "success" {
					foundCompletion = true
				}
			}
			if !foundCompletion {
				t.Fatalf("successful privileged settings update was not durably audited: %#v", events)
			}
		} else if configWrite.Code != http.StatusForbidden {
			t.Fatalf("%s reached privileged config mutation: %d %s", role, configWrite.Code, configWrite.Body.String())
		}

		if role == authz.RoleViewer {
			routingWrite := request(http.MethodPost, "/admin/api/virtual-endpoints", `{}`)
			if routingWrite.Code != http.StatusForbidden {
				t.Fatalf("viewer reached routing mutation handler: %d %s", routingWrite.Code, routingWrite.Body.String())
			}
		} else if role == authz.RoleOperator {
			routingWrite := request(http.MethodPost, "/admin/api/virtual-endpoints", `{"id":"wp4-test-endpoint","public_model":"wp4-test-model","route_profile":"default","protocols":["anthropic"]}`)
			if routingWrite.Code != http.StatusCreated {
				t.Fatalf("operator could not create an authorized virtual endpoint: %d %s", routingWrite.Code, routingWrite.Body.String())
			}
			foundCompletion := false
			events, err := srv.securityDB.AuditRecords(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Action == "admin.operation.completed" && event.Target == "/admin/api/virtual-endpoints" && event.Outcome == "success" {
					foundCompletion = true
				}
			}
			if !foundCompletion {
				t.Fatalf("successful operator routing update was not durably audited: %#v", events)
			}
		}
	}
}

func TestSessionExpiryAndIdleTimeoutAreEnforcedByMiddleware(t *testing.T) {
	srv := testGateway(t, config.Default())
	installTestOIDCSession(t, srv, authz.RoleViewer)
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name      string
		lastSeen  time.Time
		expiresAt time.Time
	}{
		{name: "absolute expiry", lastSeen: now.Add(-2 * time.Minute), expiresAt: now.Add(-time.Minute)},
		{name: "idle timeout", lastSeen: now.Add(-time.Hour), expiresAt: now.Add(time.Hour)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rawID := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string(rune('b'+i)), 32)))
			session := securitystore.Session{
				Subject: "expired-user", Issuer: srv.oidc.issuer, Roles: []string{string(authz.RoleViewer)},
				PolicyHash: srv.oidc.policyHash, CreatedAt: now.Add(-2 * time.Hour), LastSeen: tc.lastSeen, ExpiresAt: tc.expiresAt,
			}
			event := securitystore.AuditEvent{Actor: "expired-user", Action: "auth.login.succeeded", Outcome: "success", Status: http.StatusSeeOther}
			if err := srv.securityDB.CreateSession(context.Background(), opaqueHash(rawID), session, event); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
			req.RemoteAddr = fmt.Sprintf("127.0.0.%d:10120", 40+i)
			req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: rawID})
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, req)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s session was accepted: %d %s", tc.name, response.Code, response.Body.String())
			}
		})
	}
}

func TestAdminSessionCookieHasSecureScopedAttributes(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://gateway/admin/", nil)
	response := httptest.NewRecorder()
	setAdminSessionCookie(response, request, "opaque-session-value", time.Now().Add(time.Hour))
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("session cookie count=%d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != adminSessionCookie || !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/admin/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe session cookie attributes: %#v", cookie)
	}
}

func TestSessionMutationsRequireStrictDoubleSubmitCSRF(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://gateway.example/admin/api/settings", nil)
	request.Header.Set("Origin", "https://gateway.example")
	if strictSessionCSRFAllowed(request) {
		t.Fatal("session mutation accepted without a CSRF cookie and header")
	}
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	request.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: token})
	request.Header.Set("X-NexaRoute-CSRF", token)
	if !strictSessionCSRFAllowed(request) {
		t.Fatal("valid same-origin double-submit token was rejected")
	}
	request.Header.Set("Origin", "https://attacker.example")
	if strictSessionCSRFAllowed(request) {
		t.Fatal("cross-origin mutation accepted with a matching token")
	}
}

func TestOIDCFailureNeverDowngradesToEmergencyAPIKey(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "test-break-glass-key"
	cfg.Admin.EmergencyAccessEnabled = true
	srv := testGateway(t, cfg)
	srv.runtimeMu.Lock()
	srv.cfg.Admin.OIDC = config.OIDCConfig{Enabled: true, IssuerURL: "https://issuer.example.test"}
	srv.runtimeMu.Unlock()
	srv.oidc = nil
	srv.oidcErr = errors.New("provider discovery failed")

	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.RemoteAddr = "127.0.0.1:10002"
	req.Header.Set("X-Admin-Key", "test-break-glass-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("OIDC outage downgraded to emergency auth: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionPolicyChangeRevokesExistingSession(t *testing.T) {
	srv := testGateway(t, config.Default())
	cookieValue := installTestOIDCSession(t, srv, authz.RoleViewer)
	srv.oidc.policyHash = "changed-policy-fingerprint"
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.RemoteAddr = "127.0.0.3:10003"
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookieValue})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("session survived OIDC policy change: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, found, err := srv.securityDB.Session(context.Background(), opaqueHash(cookieValue)); err != nil || found {
		t.Fatalf("policy-mismatched session was not revoked: found=%v err=%v", found, err)
	}
}

func TestMalformedUnknownAndHeaderForgedSessionsAreRejected(t *testing.T) {
	srv := testGateway(t, config.Default())
	installTestOIDCSession(t, srv, authz.RoleViewer)
	handler := srv.Handler()
	cases := []struct {
		name       string
		cookie     string
		roleHeader string
	}{
		{name: "malformed cookie", cookie: "not-a-session"},
		{name: "unknown session", cookie: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("z", 32)))},
		{name: "untrusted role header", roleHeader: "admin"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
			req.RemoteAddr = fmt.Sprintf("127.0.0.%d:10008", 10+i)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: tc.cookie})
			}
			if tc.roleHeader != "" {
				req.Header.Set("X-Admin-Role", tc.roleHeader)
				req.Header.Set("X-Admin-Subject", "attacker")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("untrusted identity accepted: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestEmergencyAccessIsExplicitAndEverySuccessfulUseIsAudited(t *testing.T) {
	key := "emergency-test-key"

	disabled := config.Default()
	disabled.Admin.APIKey = key
	disabled.Admin.EmergencyAccessEnabled = false
	disabledServer := testGateway(t, disabled)
	disabledRequest := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	disabledRequest.RemoteAddr = "127.0.0.4:10004"
	disabledRequest.Header.Set("X-Admin-Key", key)
	disabledResponse := httptest.NewRecorder()
	disabledServer.Handler().ServeHTTP(disabledResponse, disabledRequest)
	if disabledResponse.Code != http.StatusUnauthorized {
		t.Fatalf("API key bypassed disabled emergency access: %d %s", disabledResponse.Code, disabledResponse.Body.String())
	}

	enabled := config.Default()
	enabled.Admin.APIKey = key
	enabled.Admin.EmergencyAccessEnabled = true
	enabledServer := testGateway(t, enabled)
	request := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	request.RemoteAddr = "127.0.0.5:10005"
	request.Header.Set("X-Admin-Key", key)
	response := httptest.NewRecorder()
	enabledServer.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("explicit emergency access failed: %d %s", response.Code, response.Body.String())
	}
	events, err := enabledServer.securityDB.AuditRecords(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Actor != "legacy-admin-break-glass" || events[0].Action != "admin.operation.completed" || events[1].Action != "admin.operation.authorized" {
		t.Fatalf("emergency use was not durably audited: %#v", events)
	}
}

func TestAuditStoreFailureBlocksEmergencyAdminHandler(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "emergency-test-key"
	cfg.Admin.EmergencyAccessEnabled = true
	srv := testGateway(t, cfg)
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	request.RemoteAddr = "127.0.0.6:10006"
	request.Header.Set("X-Admin-Key", "emergency-test-key")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin operation proceeded without durable audit: %d %s", response.Code, response.Body.String())
	}
}
