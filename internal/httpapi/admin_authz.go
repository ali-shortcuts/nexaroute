package httpapi

import (
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// adminPermissionForRequest maps every registered Admin API family to a
// server-side permission. Unknown paths fail closed instead of inheriting the
// broad Admin authentication boundary. For known paths, method validation is
// left to the handler so unsupported methods retain the normal 405 response.
func adminPermissionForRequest(r *http.Request) (authz.Permission, bool) {
	path := r.URL.Path
	read := r.Method == http.MethodGet || r.Method == http.MethodHead

	switch {
	case path == "/admin/api/snapshot" || path == "/admin/api/csrf-token":
		return authz.ReadConfig, true
	case path == "/admin/api/events/stream":
		return authz.ReadAudit, true
	case path == "/admin/api/providers" || strings.HasPrefix(path, "/admin/api/providers/") ||
		path == "/admin/api/provider-presets" || path == "/admin/api/provider-check" ||
		path == "/admin/api/provider-test" || path == "/admin/api/provider-discover" ||
		path == "/admin/api/compat" || path == "/admin/api/compat/reset":
		if read && path != "/admin/api/compat/reset" {
			return authz.ReadProviders, true
		}
		return authz.WriteProviders, true
	case path == "/admin/api/settings" || path == "/admin/api/config/history" || path == "/admin/api/drain":
		if read {
			return authz.ReadConfig, true
		}
		return authz.WriteConfig, true
	case path == "/admin/api/usage/identities":
		return authz.ReadUsage, true
	case path == "/admin/api/client-keys" || strings.HasPrefix(path, "/admin/api/client-keys/"):
		return authz.ManageKeys, true
	case path == "/admin/api/virtual-endpoints" || strings.HasPrefix(path, "/admin/api/virtual-endpoints/") ||
		path == "/admin/api/route-profiles" || strings.HasPrefix(path, "/admin/api/route-profiles/") ||
		path == "/admin/api/candidate-pools" || strings.HasPrefix(path, "/admin/api/candidate-pools/") ||
		path == "/admin/api/simple-routes" || strings.HasPrefix(path, "/admin/api/simple-routes/") ||
		path == "/admin/api/fallback-chains" || strings.HasPrefix(path, "/admin/api/fallback-chains/"):
		if read {
			return authz.ReadRouting, true
		}
		return authz.WriteRouting, true
	case path == "/admin/api/scorecards" || strings.HasPrefix(path, "/admin/api/scorecards/") ||
		path == "/admin/api/evaluation/suites" || path == "/admin/api/evaluation/runs" || path == "/admin/api/evaluation/run":
		if read {
			return authz.ReadRouting, true
		}
		return authz.RunEvaluation, true
	case path == "/admin/api/probe":
		if read {
			return authz.ReadProviders, true
		}
		return authz.WriteProviders, true
	case path == "/admin/api/endpoint":
		if read {
			return authz.ReadConfig, true
		}
		// This compatibility endpoint can rotate client keys; require the
		// sensitive permission rather than treating the mixed-purpose POST as a
		// routine configuration edit.
		return authz.ManageKeys, true
	}
	return "", false
}

// legacyAdminIdentity deliberately maps the existing static Admin credential
// (and the existing loopback-only keyless mode) to the documented break-glass
// owner. It never accepts role claims from request headers or query parameters.
func legacyAdminIdentity() authz.Identity {
	return authz.Identity{Subject: "legacy-admin-break-glass", Roles: []authz.Role{authz.RoleOwner}}
}
