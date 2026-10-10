package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Privacy fail-closed routing (PRIVACY P2).
//
// A no_training requirement is an absolute guarantee: when it is set, the
// request FAILS rather than falling back to a lower privacy class. The
// enforcement itself lives in router.eligibleDeployment (single choke
// point); this file only computes the requirement, maps the empty-result
// to a privacy-safe 503, and emits privacy-safe observability.

const (
	privacyUnavailableType    = "privacy_unavailable"
	privacyUnavailableMessage = "no deployment satisfies the required privacy class"
	privacyExcludedReason     = "privacy_excluded"
)

// requireNoTrainingForRequest computes router.Requirement.RequireNoTraining.
// It is true when the matched route profile has privacy "no_training" OR the
// authenticated virtual key has require_privacy "no_training" (strictest
// wins). Neither source widens the other; empty/"any" means no requirement.
func (s *Server) requireNoTrainingForRequest(r *http.Request, model string) bool {
	// Virtual-key source (strictest wins with route profile).
	if id, ok := clientIdentityFromRequest(r); ok {
		if strings.ToLower(strings.TrimSpace(id.RequirePrivacy)) == "no_training" {
			return true
		}
	}
	// Route-profile source.
	s.runtimeMu.RLock()
	resolver := s.routeResolver
	cfg := s.cfg
	s.runtimeMu.RUnlock()
	var profileID string
	if resolver != nil {
		if resolved, ok := resolver.Resolve(strings.TrimSpace(model)); ok {
			profileID = resolved.RouteProfileID
		}
	}
	if profileID == "" {
		return false
	}
	for _, rp := range cfg.RouteProfiles {
		if rp.ID == profileID {
			return strings.ToLower(strings.TrimSpace(rp.Privacy)) == "no_training"
		}
	}
	return false
}

// applyPrivacyRequirement stamps the fail-closed flag onto a requirement.
// It must be called BEFORE candidatesForRequirement so every downstream
// path (initial selection, failover, pools/fallbacks, affinity re-checks,
// hedges, credential selection via currentRouteCandidate, decision-plane
// candidate lists) observes the same flag.
func (s *Server) applyPrivacyRequirement(req router.Requirement, r *http.Request) router.Requirement {
	req.RequireNoTraining = s.requireNoTrainingForRequest(r, req.Model)
	return req
}

// privacyExcludedTotal counts deployments excluded solely by the privacy
// gate among otherwise-eligible candidates. It re-resolves without the
// privacy flag; callers only invoke it when the privacy-filtered set is
// empty, so a non-zero result proves privacy is the sole cause.
func (s *Server) privacyExcludedTotal(req router.Requirement, protocol string, resolved *route.ResolvedRoute) int {
	if !req.RequireNoTraining {
		return 0
	}
	relaxed := req
	relaxed.RequireNoTraining = false
	_, relaxedCandidates, _, err := s.candidatesForRequirement(relaxed, protocol)
	if err != nil || len(relaxedCandidates) == 0 {
		return 0
	}
	// candidatesForRequirement already applies pool/fallback filtering, so
	// every relaxed candidate here would have served absent privacy.
	// Count only those the privacy gate actually excludes (defensive: the
	// relaxed set may still contain trains_on_data="no" entries when the
	// empty result came from another dimension, e.g. health flapping
	// between the two resolutions).
	excluded := 0
	for _, c := range relaxedCandidates {
		if !router.SatisfiesNoTraining(c.Deployment.TrainsOnData) {
			excluded++
		}
	}
	if excluded == 0 {
		return 0
	}
	_ = resolved
	return excluded
}

// emitPrivacyExclusion records a privacy-safe bus event when the privacy
// gate excludes deployments. It carries only the excluded count and route
// identity; never prompt content, provider names, or reasons beyond the
// privacy_excluded reason code.
func (s *Server) emitPrivacyExclusion(requestID string, excluded int, resolved *route.ResolvedRoute) {
	if excluded <= 0 {
		return
	}
	ev := events.Event{
		RequestID:  requestID,
		Kind:       "candidate_exhausted",
		Message:    fmt.Sprintf("privacy excluded %d deployment(s): %s", excluded, privacyUnavailableMessage),
		ErrorType:  privacyExcludedReason,
		StatusCode: http.StatusServiceUnavailable,
	}
	if resolved != nil {
		ev.VirtualEndpoint = resolved.VirtualEndpointID
		ev.PublicModel = resolved.PublicModel
		ev.RouteProfile = resolved.RouteProfileID
		ev.Pool = resolved.PrimaryPoolID
	}
	s.bus.Add(ev)
}

// isPrivacyUnavailable reports whether an empty candidate set is caused
// solely by the privacy gate (fail-closed 503 with privacy_unavailable),
// emitting the privacy-safe event as a side effect.
func (s *Server) isPrivacyUnavailable(req router.Requirement, protocol string, resolved *route.ResolvedRoute, requestID string) bool {
	excluded := s.privacyExcludedTotal(req, protocol, resolved)
	if excluded == 0 {
		return false
	}
	s.emitPrivacyExclusion(requestID, excluded, resolved)
	return true
}

// Per-protocol privacy 503 helpers. Each reuses the existing per-protocol
// error envelope shape for its ingress and carries exactly the required
// type/message without leaking provider names.

func openAIPrivacyUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{
			"message": privacyUnavailableMessage,
			"type":    privacyUnavailableType,
			"param":   nil,
			"code":    privacyUnavailableType,
		},
	})
}

func anthropicPrivacyUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": privacyUnavailableType, "message": privacyUnavailableMessage},
	})
}

func responsesPrivacyUnavailable(w http.ResponseWriter) {
	canonicalErrorJSON(w, "openai_responses", http.StatusServiceUnavailable, privacyUnavailableType, privacyUnavailableMessage)
}
