package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestAdminPermissionForRequest(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		want       authz.Permission
		wantMapped bool
	}{
		{"snapshot read", http.MethodGet, "/admin/api/snapshot", authz.ReadConfig, true},
		{"provider list read", http.MethodGet, "/admin/api/providers", authz.ReadProviders, true},
		{"provider change write", http.MethodPut, "/admin/api/providers/p1", authz.WriteProviders, true},
		{"provider check write class", http.MethodPost, "/admin/api/provider-check", authz.WriteProviders, true},
		{"config update", http.MethodPut, "/admin/api/settings", authz.WriteConfig, true},
		{"usage read", http.MethodGet, "/admin/api/usage/identities", authz.ReadUsage, true},
		{"client key management", http.MethodDelete, "/admin/api/client-keys/k1", authz.ManageKeys, true},
		{"route change", http.MethodPost, "/admin/api/simple-routes", authz.WriteRouting, true},
		{"evaluation run", http.MethodPost, "/admin/api/evaluation/run", authz.RunEvaluation, true},
		{"audit read", http.MethodGet, "/admin/api/audit", authz.ReadAudit, true},
		{"unknown admin path", http.MethodGet, "/admin/api/unregistered", "", false},
		{"unsupported provider method denied", http.MethodOptions, "/admin/api/providers", "", false},
		{"unsupported snapshot method denied", http.MethodTrace, "/admin/api/snapshot", "", false},
		{"viewer cannot stream audit", http.MethodGet, "/admin/api/events/stream", authz.ReadAudit, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "http://localhost"+tt.path, nil)
			got, ok := adminPermissionForRequest(r)
			if ok != tt.wantMapped || got != tt.want {
				t.Fatalf("adminPermissionForRequest(%s %s) = (%q, %v), want (%q, %v)", tt.method, tt.path, got, ok, tt.want, tt.wantMapped)
			}
		})
	}
}

func TestLegacyAdminIdentityIsExplicitBreakGlassOwner(t *testing.T) {
	identity := legacyAdminIdentity()
	if !identity.Authenticated() || identity.Subject != "legacy-admin-break-glass" || !identity.HasRole(authz.RoleOwner) {
		t.Fatalf("unexpected break-glass identity: %#v", identity)
	}
	if !identity.Authorize(authz.ManageKeys) || !identity.Authorize(authz.WriteConfig) {
		t.Fatal("break-glass owner must retain existing privileged Admin capabilities")
	}
	if (authz.Identity{}).Authorize(authz.WriteConfig) {
		t.Fatal("unauthenticated identity must not authorize a privileged operation")
	}
}

func TestAdminMiddlewareDeniesUnmappedRouteAndKeepsOwnerAccess(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "test-break-glass-key"
	s := testGateway(t, cfg)
	h := s.Handler()

	request := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if got := request("/admin/api/snapshot", map[string]string{"X-Admin-Key": "test-break-glass-key"}); got.Code != http.StatusOK {
		t.Fatalf("owner should retain snapshot access, got %d: %s", got.Code, got.Body.String())
	}
	if got := request("/admin/api/unregistered", map[string]string{"X-Admin-Key": "test-break-glass-key"}); got.Code != http.StatusForbidden {
		t.Fatalf("unmapped route must fail closed, got %d: %s", got.Code, got.Body.String())
	}
	other := testGateway(t, cfg).Handler()
	wrong := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/client-keys", nil)
	wrong.RemoteAddr = "127.0.0.1:12346"
	wrong.Header.Set("X-Admin-Key", "wrong")
	wrong.Header.Set("X-Admin-Role", "owner")
	wrongRR := httptest.NewRecorder()
	other.ServeHTTP(wrongRR, wrong)
	if wrongRR.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted role header must not authenticate, got %d: %s", wrongRR.Code, wrongRR.Body.String())
	}
}
