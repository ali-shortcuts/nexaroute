package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// previewMaxTextParam bounds the operator-supplied routing inputs so a
// preview request can never carry oversized keys into requirement
// construction.
const previewMaxTextParam = 512

// adminRoutePreview serves GET /admin/api/route-preview: a routing decision
// preview (issue Phase 4 explainability). It replays the real candidate
// funnel — virtual endpoint resolution, capability/health/privacy gates,
// strategy ordering — for an operator-supplied requirement using the live
// runtime state, without issuing any provider call and without mutating
// anything: no session pin is written, no probe is started, no admission
// counter is touched, and no routing rotation counter is advanced (the
// explanation orders rotating strategies from the requirement's own keys,
// so a preview can never shift the next real pick).
//
// The response contains the ordered candidate list the data plane would
// attempt, the selected provider/model, and a redacted exclusion reason for
// every other deployment. Secrets and user prompts are never part of
// routing decisions, so nothing of that kind can appear here; operator
// session/selection keys are accepted only as bounded anonymous inputs and
// are never echoed back.
//
// The simulated selection is labelled as such everywhere it appears
// (simulated, selected_is_simulated, explanation.simulated). It is not a
// record of a real request, and for rotating strategies it is not even a
// prediction of the next live pick — see explanation.rotation_predictive
// and docs/ROUTING_SIMULATION.md.
//
// With ?compare_profile=<route profile id> the same requirement is
// simulated a second time against that profile's candidate pool
// (primary pool plus fallback-chain pools) and privacy setting, using the
// same canonical funnel. The comparison reports only what changes:
// selected candidate, added/removed deployments, and the alternative
// explanation. It never applies the profile, and it does not simulate
// canary rollout or session-pin promotion for the alternative profile —
// those are session-specific promotions, documented in
// docs/ROUTING_SIMULATION.md.
func (s *Server) adminRoutePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, 405, "method not allowed")
		return
	}
	q := r.URL.Query()
	req := router.Requirement{
		Model:             strings.TrimSpace(q.Get("model")),
		Tools:             previewBool(q, "tools"),
		Vision:            previewBool(q, "vision"),
		Streaming:         previewBool(q, "stream"),
		Reasoning:         previewBool(q, "reasoning"),
		StructuredOutput:  previewBool(q, "structured_output"),
		ParallelTools:     previewBool(q, "parallel_tools"),
		ProviderType:      strings.TrimSpace(q.Get("provider_type")),
		RequireNoTraining: previewBool(q, "require_no_training"),
	}
	for _, k := range []string{"model", "provider_type", "session_key", "selection_key", "compare_profile"} {
		if len(q.Get(k)) > previewMaxTextParam {
			errorJSON(w, 400, fmt.Sprintf("query parameter %q exceeds %d characters", k, previewMaxTextParam))
			return
		}
	}
	// Session/selection keys are used only to evaluate affinity pinning;
	// they are consumed internally and never written or echoed.
	req.SessionKey = strings.TrimSpace(q.Get("session_key"))
	req.SelectionKey = strings.TrimSpace(q.Get("selection_key"))
	windowStr := q.Get("min_context_window")
	if windowStr != "" {
		n, err := strconv.Atoi(windowStr)
		if err != nil || n < 0 {
			errorJSON(w, 400, "min_context_window must be a non-negative integer")
			return
		}
		req.MinContextWindow = n
	}
	if len(req.Model) > 256 {
		errorJSON(w, 400, "model exceeds 256 characters")
		return
	}
	if len(req.ProviderType) > 64 {
		errorJSON(w, 400, "provider_type exceeds 64 characters")
		return
	}

	// One runtime snapshot for the whole simulation: config, resolver and
	// router are read under the same lock the data plane uses, so the
	// baseline and the comparison can never disagree about the config.
	s.runtimeMu.RLock()
	cfg := s.cfg
	cfg.Providers = nil
	resolver := s.routeResolver
	rt := s.rt
	s.runtimeMu.RUnlock()

	compareProfileID := strings.TrimSpace(q.Get("compare_profile"))
	var compareProfile *config.RouteProfileConfig
	if compareProfileID != "" {
		for i := range cfg.RouteProfiles {
			if cfg.RouteProfiles[i].ID == compareProfileID {
				compareProfile = &cfg.RouteProfiles[i]
				break
			}
		}
		if compareProfile == nil {
			errorJSON(w, 400, fmt.Sprintf("unknown route profile %q", compareProfileID))
			return
		}
	}

	protocol := strings.TrimSpace(q.Get("protocol"))
	resolved, isVirtual, routeErr := s.resolveVirtualEndpoint(req.Model)
	if routeErr != nil {
		if isVirtual {
			resp := previewBaseResponse(cfg, req, protocol)
			resp["virtual_endpoint"] = previewVirtualEndpoint(resolved)
			resp["error"] = routeErr.Error()
			resp["selected"] = nil
			resp["selected_is_simulated"] = false
			resp["eligible_candidates"] = []any{}
			resp["excluded_count"] = 0
			writeJSON(w, http.StatusOK, resp)
			return
		}
		errorJSON(w, http.StatusBadRequest, routeErr.Error())
		return
	}
	if isVirtual && resolver != nil {
		if err := checkVirtualEndpointProtocol(resolver, resolved, protocol); err != nil {
			resp := previewBaseResponse(cfg, req, protocol)
			resp["virtual_endpoint"] = previewVirtualEndpoint(resolved)
			resp["error"] = err.Error()
			resp["selected"] = nil
			resp["selected_is_simulated"] = false
			resp["eligible_candidates"] = []any{}
			resp["excluded_count"] = 0
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	funnelReq := req
	var resolvedPtr *route.ResolvedRoute
	if isVirtual {
		// Virtual endpoint routing scans the whole pool funnel with an
		// empty model (mirroring the request path) and the resolver does
		// the public-model/name matching; report both layers.
		funnelReq.Model = ""
		resolvedPtr = &resolved
	}

	// The simulation starts from Router.Explain, never from the live
	// Candidates path: Explain orders with the simulated strategy path,
	// so no rotation counter advances and no provider is contacted.
	explanation := rt.Explain(funnelReq)
	eligible := previewEligibleCandidates(cfg, resolver, rt, req, funnelReq, resolvedPtr, explanation.Candidates)

	// Layer the virtual endpoint pool membership over the funnel verdicts:
	// a deployment the funnel included but the resolved pools did not
	// admit never reaches the retry loop, so its trace must say so.
	eligibleIDs := make(map[string]struct{}, len(eligible))
	for _, c := range eligible {
		eligibleIDs[c.Deployment.ID] = struct{}{}
	}
	if resolvedPtr != nil && resolvedPtr.AllAllowed != nil {
		for i := range explanation.Decisions {
			cd := &explanation.Decisions[i]
			if !cd.Included {
				continue
			}
			if _, ok := eligibleIDs[cd.DeploymentID]; ok {
				continue
			}
			cd.Included = false
			cd.ExcludedReason = "not_in_virtual_endpoint_pools"
		}
	}
	excluded := 0
	for _, cd := range explanation.Decisions {
		if !cd.Included {
			excluded++
		}
	}
	// The response body never carries upstream error text: health state is
	// reduced to status/latency, and the raw Scored entries (which carry
	// unredacted health diagnostics) are dropped in favor of the redacted
	// summaries below.
	explanation.Candidates = nil

	resp := previewBaseResponse(cfg, req, protocol)
	if resolvedPtr != nil {
		resp["virtual_endpoint"] = previewVirtualEndpoint(*resolvedPtr)
	}
	resp["explanation"] = explanation
	resp["eligible_candidates"] = previewCandidateSummaries(eligible)
	resp["excluded_count"] = excluded
	if len(eligible) > 0 {
		resp["selected"] = previewCandidateSummaries(eligible[:1])[0]
		resp["selected_is_simulated"] = true
	} else {
		resp["selected"] = nil
		resp["selected_is_simulated"] = false
	}
	if compareProfile != nil {
		baselineSelected := ""
		if len(eligible) > 0 {
			baselineSelected = eligible[0].Deployment.ID
		}
		resp["comparison"] = previewProfileComparison(cfg, resolver, rt, funnelReq, *compareProfile, eligible, baselineSelected)
	}
	writeJSON(w, http.StatusOK, resp)
}

func previewBaseResponse(cfg config.Config, req router.Requirement, protocol string) map[string]any {
	return map[string]any{
		"preview":          true,
		"simulated":        true,
		"no_provider_call": true,
		"strategy":         cfg.Routing.Strategy,
		"model":            req.Model,
		"protocol":         protocol,
		"requirements": map[string]any{
			"tools":               req.Tools,
			"vision":              req.Vision,
			"stream":              req.Streaming,
			"reasoning":           req.Reasoning,
			"structured_output":   req.StructuredOutput,
			"parallel_tools":      req.ParallelTools,
			"require_no_training": req.RequireNoTraining,
			"min_context_window":  req.MinContextWindow,
			"provider_type":       req.ProviderType,
		},
	}
}

// previewEligibleCandidates derives the ordered, simulated candidate list
// from an Explain result, mirroring the data plane's virtual-endpoint
// path (pool filtering, then canary rollout, then session-pin promotion)
// without ever calling the live Candidates path. For a non-virtual
// requirement the Explain order is the candidate order.
func previewEligibleCandidates(cfg config.Config, resolver *route.Resolver, rt *router.Router, req, funnelReq router.Requirement, resolved *route.ResolvedRoute, explained []router.Scored) []router.Scored {
	if resolved == nil || resolver == nil {
		return explained
	}
	filtered := resolver.AllFilteredCandidates(explained, *resolved)
	out := applyRollout(cfg, resolved, req, filtered)
	// Session affinity (#421): the funnel fetch blanks the model filter,
	// and the router's affinity bucket is keyed by the request's model —
	// so a pin recorded under the public model was invisible to that
	// fetch and virtual-endpoint sessions silently lost their pin.
	// Promote the pinned deployment from the ORIGINAL requirement (the
	// same lookup the decision plane performs) when it survived
	// eligibility and pool filtering.
	if pin := rt.PinnedDeploymentID(req); pin != "" {
		for i := range out {
			if out[i].Deployment.ID == pin && i > 0 {
				chosen := out[i]
				copy(out[1:i+1], out[0:i])
				out[0] = chosen
				break
			}
		}
	}
	return out
}

// previewCandidateSummaries reduces Scored entries to routing metadata
// only: identity, priority/weight, score plus its breakdown, health
// status/latency, capacity pressure and the cost estimate. No upstream
// error text, prompt, or credential can appear here.
func previewCandidateSummaries(candidates []router.Scored) []map[string]any {
	eligible := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		eligible = append(eligible, map[string]any{
			"deployment":    c.Deployment.ID,
			"provider_id":   c.Deployment.ProviderID,
			"provider_type": c.Deployment.ProviderType,
			"model":         c.Deployment.Model,
			"priority":      c.Deployment.Priority,
			"weight":        c.Deployment.Weight,
			"score":         c.Score,
			"score_breakdown": map[string]any{
				"base":                c.ScoreBase,
				"health_adjustment":   c.HealthAdjustment,
				"weight_contribution": c.WeightContribution,
				"priority_penalty":    c.PriorityPenalty,
				"latency_penalty":     c.LatencyPenalty,
				"capacity_penalty":    c.CapacityPenalty,
				"failure_penalty":     c.FailurePenalty,
			},
			"health_status":      string(c.Health.Status),
			"ewma_latency_ms":    c.Health.EWMALatencyMS,
			"capacity_pressure":  c.CapacityPressure,
			"price_known":        c.PriceKnown,
			"estimated_cost_usd": c.EstimatedCostUSD,
			"capability_proofs":  c.Health.Scopes != nil,
		})
	}
	return eligible
}

// previewProfileComparison simulates the same requirement under one
// alternative route profile, reusing the canonical funnel. Pool
// membership is the profile's primary pool plus its fallback-chain pools,
// expanded through the live resolver; the profile's no_training privacy
// setting tightens the requirement the same way the data plane does.
// Canary rollout and session-pin promotion are deliberately NOT applied
// to the alternative: they are session-specific promotions, and the
// comparison must show what the profile itself changes, not what one
// session's history would promote.
func previewProfileComparison(cfg config.Config, resolver *route.Resolver, rt *router.Router, funnelReq router.Requirement, profile config.RouteProfileConfig, baselineEligible []router.Scored, baselineSelected string) map[string]any {
	out := map[string]any{
		"profile_id":     profile.ID,
		"candidate_pool": profile.CandidatePool,
		"simulated":      true,
	}
	if profile.FallbackChain != "" {
		out["fallback_chain"] = profile.FallbackChain
	}
	if profile.Privacy != "" {
		out["privacy"] = profile.Privacy
	}
	members, ok := expandProfilePools(cfg, resolver, profile)
	if !ok {
		out["error"] = fmt.Sprintf("route profile %q pool could not be expanded", profile.ID)
		out["selected"] = nil
		out["selected_changed"] = false
		out["eligible_candidates"] = []any{}
		out["added_deployments"] = []string{}
		out["removed_deployments"] = []string{}
		return out
	}
	altReq := funnelReq
	if profile.Privacy == "no_training" {
		altReq.RequireNoTraining = true
	}
	altExplanation := rt.Explain(altReq)
	altEligible := make([]router.Scored, 0, len(altExplanation.Candidates))
	for _, c := range altExplanation.Candidates {
		if _, ok := members[c.Deployment.ID]; ok {
			altEligible = append(altEligible, c)
		}
	}
	for i := range altExplanation.Decisions {
		cd := &altExplanation.Decisions[i]
		if !cd.Included {
			continue
		}
		if _, ok := members[cd.DeploymentID]; ok {
			continue
		}
		cd.Included = false
		cd.ExcludedReason = "not_in_route_profile_pools"
	}
	excluded := 0
	for _, cd := range altExplanation.Decisions {
		if !cd.Included {
			excluded++
		}
	}
	altExplanation.Candidates = nil

	baselineIDs := make(map[string]struct{}, len(baselineEligible))
	for _, c := range baselineEligible {
		baselineIDs[c.Deployment.ID] = struct{}{}
	}
	altIDs := make(map[string]struct{}, len(altEligible))
	for _, c := range altEligible {
		altIDs[c.Deployment.ID] = struct{}{}
	}
	added := []string{}
	for _, c := range altEligible {
		if _, ok := baselineIDs[c.Deployment.ID]; !ok {
			added = append(added, c.Deployment.ID)
		}
	}
	removed := []string{}
	for _, c := range baselineEligible {
		if _, ok := altIDs[c.Deployment.ID]; !ok {
			removed = append(removed, c.Deployment.ID)
		}
	}
	altSelected := ""
	if len(altEligible) > 0 {
		altSelected = altEligible[0].Deployment.ID
		out["selected"] = previewCandidateSummaries(altEligible[:1])[0]
	} else {
		out["selected"] = nil
	}
	out["selected_changed"] = altSelected != baselineSelected
	out["eligible_count"] = len(altEligible)
	out["excluded_count"] = excluded
	out["eligible_candidates"] = previewCandidateSummaries(altEligible)
	out["added_deployments"] = added
	out["removed_deployments"] = removed
	out["explanation"] = altExplanation
	return out
}

// expandProfilePools unions the expanded deployment sets of a profile's
// primary pool and every pool in its fallback chain. It returns false
// when the resolver is unavailable or any referenced pool does not
// expand, so a comparison never silently treats an unknown pool as empty.
func expandProfilePools(cfg config.Config, resolver *route.Resolver, profile config.RouteProfileConfig) (map[string]struct{}, bool) {
	if resolver == nil {
		return nil, false
	}
	poolIDs := []string{profile.CandidatePool}
	if profile.FallbackChain != "" {
		found := false
		for _, ch := range cfg.FallbackChains {
			if ch.ID == profile.FallbackChain {
				poolIDs = append(poolIDs, ch.Pools...)
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	members := make(map[string]struct{})
	for _, pid := range poolIDs {
		set, ok := resolver.GetExpanded(pid)
		if !ok {
			return nil, false
		}
		for id := range set {
			members[id] = struct{}{}
		}
	}
	return members, true
}

func previewVirtualEndpoint(resolved route.ResolvedRoute) map[string]any {
	return map[string]any{
		"id":                   resolved.VirtualEndpointID,
		"name":                 resolved.VirtualEndpointName,
		"public_model":         resolved.PublicModel,
		"route_profile_id":     resolved.RouteProfileID,
		"primary_pool_id":      resolved.PrimaryPoolID,
		"primary_pool_mode":    resolved.PrimaryMode,
		"fallback_chain_id":    resolved.FallbackChainID,
		"ordered_pool_ids":     resolved.OrderedPoolIDs,
		"admitted_deployments": len(resolved.AllAllowed),
	}
}

func previewBool(q map[string][]string, key string) bool {
	qv, ok := q[key]
	if !ok || len(qv) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(qv[0])) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
