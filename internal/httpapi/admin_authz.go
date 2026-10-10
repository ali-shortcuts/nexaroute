package httpapi

import (
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// adminPermissionForRequest is a fail-closed server-side route/method matrix.
// It never accepts OPTIONS, TRACE, CONNECT, PATCH, or an unknown method by
// inheriting the permission of a route family. Individual handlers still
// validate their resource-specific method and return 405 where applicable.
func adminPermissionForRequest(r *http.Request) (authz.Permission, bool) {
	path := r.URL.Path
	readWrite := func(read, write authz.Permission) (authz.Permission, bool) {
		switch r.Method {
		case http.MethodGet:
			return read, true
		case http.MethodPost, http.MethodPut, http.MethodDelete:
			return write, true
		default:
			return "", false
		}
	}
	readOnly := func(permission authz.Permission) (authz.Permission, bool) {
		if r.Method == http.MethodGet {
			return permission, true
		}
		return "", false
	}
	writeOnly := func(permission authz.Permission) (authz.Permission, bool) {
		if r.Method == http.MethodPost {
			return permission, true
		}
		return "", false
	}

	switch {
	case path == "/admin/api/snapshot" || path == "/admin/api/csrf-token":
		return readOnly(authz.ReadConfig)
	case path == "/admin/api/audit" || path == "/admin/api/events/stream":
		return readOnly(authz.ReadAudit)
	case path == "/admin/api/usage/identities":
		return readOnly(authz.ReadUsage)
	case path == "/admin/api/provider-presets":
		return readOnly(authz.ReadProviders)
	case path == "/admin/api/provider-check" || path == "/admin/api/provider-test" ||
		path == "/admin/api/provider-discover" || path == "/admin/api/probe":
		return writeOnly(authz.WriteProviders)
	case path == "/admin/api/compat/reset":
		return writeOnly(authz.WriteProviders)
	case path == "/admin/api/compat":
		return readWrite(authz.ReadProviders, authz.WriteProviders)
	case path == "/admin/api/providers" || strings.HasPrefix(path, "/admin/api/providers/"):
		return readWrite(authz.ReadProviders, authz.WriteProviders)
	case path == "/admin/api/settings" || path == "/admin/api/config/history" || path == "/admin/api/drain":
		return readWrite(authz.ReadConfig, authz.WriteConfig)
	case path == "/admin/api/client-keys" || strings.HasPrefix(path, "/admin/api/client-keys/"):
		if r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
			return authz.ManageKeys, true
		}
		return "", false
	case path == "/admin/api/virtual-endpoints" || strings.HasPrefix(path, "/admin/api/virtual-endpoints/") ||
		path == "/admin/api/route-profiles" || strings.HasPrefix(path, "/admin/api/route-profiles/") ||
		path == "/admin/api/candidate-pools" || strings.HasPrefix(path, "/admin/api/candidate-pools/") ||
		path == "/admin/api/simple-routes" || strings.HasPrefix(path, "/admin/api/simple-routes/") ||
		path == "/admin/api/fallback-chains" || strings.HasPrefix(path, "/admin/api/fallback-chains/"):
		return readWrite(authz.ReadRouting, authz.WriteRouting)
	case path == "/admin/api/scorecards" || strings.HasPrefix(path, "/admin/api/scorecards/"):
		return readWrite(authz.ReadRouting, authz.RunEvaluation)
	case path == "/admin/api/evaluation/suites" || path == "/admin/api/evaluation/runs":
		return readOnly(authz.ReadRouting)
	case path == "/admin/api/evaluation/run":
		return writeOnly(authz.RunEvaluation)
	case path == "/admin/api/endpoint":
		if r.Method == http.MethodGet {
			return authz.ReadConfig, true
		}
		if r.Method == http.MethodPost {
			// This compatibility endpoint can rotate client credentials.
			return authz.ManageKeys, true
		}
		return "", false
	default:
		return "", false
	}
}

// legacyAdminIdentity is reserved for the explicitly configured emergency
// mechanism. It never accepts role claims from request headers or query values.
func legacyAdminIdentity() authz.Identity {
	return authz.Identity{Subject: "legacy-admin-break-glass", Roles: []authz.Role{authz.RoleOwner}}
}
