package httpapi

import (
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// OpenCode model-entitlement diagnostic (HTTP 403 on an opencode
// provider). Classification is status-based for opencode types: the
// documented Console/Go material publishes no distinguishing error
// body, so a genuinely invalid opencode key returning 403 surfaces the
// same diagnostic on that request. The distinction that IS enforced:
// the credential is not cooled, no other credential is tried for the
// same restricted model, and no deployment failover or quarantine
// follows — the next request re-evaluates independently, and other
// models on the same key are not poisoned.
const opencodeEntitlementDiagnostic = "OpenCode model entitlement denied (HTTP 403): this model is not available on the current account or plan " +
	"(for example, a free-tier model outside OpenCode, or a model requiring a paid model or a Go subscription). " +
	"No credential was cooled and no other credential was tried for this model. " +
	"Next step: use a paid model or a Go subscription, or pick a model your plan includes."

// applyOpencodeEntitlementOverride rewrites a classified upstream
// failure into the entitlement policy when it is an opencode 403.
// It returns true when it applied. Generic providers are untouched.
func applyOpencodeEntitlementOverride(providerType string, status int, cls *compat.Classified, policy *upstreamFailurePolicy) bool {
	if !providers.IsOpencodeType(providerType) || status != http.StatusForbidden {
		return false
	}
	cls.Message = opencodeEntitlementDiagnostic
	policy.ErrorType = "opencode_model_entitlement"
	policy.Failover = false
	policy.QuarantineDeployment = false
	policy.SignalProvider = false
	policy.HardCooldown = false
	return true
}
