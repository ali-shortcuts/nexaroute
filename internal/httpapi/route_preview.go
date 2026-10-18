package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
// anything (no session pin is written, no probe is started, no admission
// counter is touched).
//
// The response contains the ordered candidate list the data plane would
// attempt, the selected provider/model, and a redacted exclusion reason for
// every other deployment. Secrets and user prompts are never part of
// routing decisions, so nothing of that kind can appear here; operator
// session/selection keys are accepted only as bounded anonymous inputs and
// are never echoed back.
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
		ProviderType:      strings.TrimSpace(q.Get("provider_type")),
		RequireNoTraining: previewBool(q, "require_no_training"),
	}
	for _, k := range []string{"model", "provider_type", "session_key", "selection_key"} {
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

	protocol := strings.TrimSpace(q.Get("protocol"))
	cfg, candidates, resolved, routeErr := s.candidatesForRequirement(req, protocol)

	resp := map[string]any{
		"preview":          true,
		"no_provider_call": true,
		"strategy":         cfg.Routing.Strategy,
		"model":            req.Model,
		"protocol":         protocol,
		"requirements": map[string]any{
			"tools":               req.Tools,
			"vision":              req.Vision,
			"stream":              req.Streaming,
			"reasoning":           req.Reasoning,
			"require_no_training": req.RequireNoTraining,
			"min_context_window":  req.MinContextWindow,
			"provider_type":       req.ProviderType,
		},
	}
	if routeErr != nil && resolved != nil {
		// Virtual endpoint resolution itself failed (disabled endpoint,
		// protocol restriction). The reason is the explainability answer.
		resp["virtual_endpoint"] = previewVirtualEndpoint(*resolved)
		resp["error"] = routeErr.Error()
		resp["selected"] = nil
		resp["eligible_candidates"] = []any{}
		resp["excluded_count"] = 0
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if routeErr != nil {
		errorJSON(w, http.StatusBadRequest, routeErr.Error())
		return
	}

	funnelReq := req
	if resolved != nil {
		// Virtual endpoint routing scans the whole pool funnel with an
		// empty model (mirroring the request path) and the resolver does
		// the public-model/name matching; report both layers.
		funnelReq.Model = ""
		resp["virtual_endpoint"] = previewVirtualEndpoint(*resolved)
	}

	explanation := s.rt.Explain(funnelReq)
	candidateIDs := make(map[string]struct{}, len(candidates))
	latest := make([]router.Scored, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := candidateIDs[c.Deployment.ID]; ok {
			continue
		}
		candidateIDs[c.Deployment.ID] = struct{}{}
		latest = append(latest, c)
	}

	// Layer the virtual endpoint pool membership over the funnel verdicts:
	// a deployment the funnel included but the resolved pools did not
	// admit never reaches the retry loop, so its trace must say so.
	if resolved != nil && resolved.AllAllowed != nil {
		for i := range explanation.Decisions {
			cd := &explanation.Decisions[i]
			if !cd.Included {
				continue
			}
			if _, ok := candidateIDs[cd.DeploymentID]; ok {
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

	// Redacted candidate summaries: routing metadata only.
	eligible := make([]map[string]any, 0, len(latest))
	for _, c := range latest {
		eligible = append(eligible, map[string]any{
			"deployment":         c.Deployment.ID,
			"provider_id":        c.Deployment.ProviderID,
			"provider_type":      c.Deployment.ProviderType,
			"model":              c.Deployment.Model,
			"priority":           c.Deployment.Priority,
			"score":              c.Score,
			"health_status":      string(c.Health.Status),
			"ewma_latency_ms":    c.Health.EWMALatencyMS,
			"capacity_pressure":  c.CapacityPressure,
			"price_known":        c.PriceKnown,
			"estimated_cost_usd": c.EstimatedCostUSD,
			"capability_proofs":  c.Health.Scopes != nil,
		})
	}

	resp["explanation"] = explanation
	resp["eligible_candidates"] = eligible
	resp["excluded_count"] = excluded
	if len(eligible) > 0 {
		resp["selected"] = eligible[0]
	} else {
		resp["selected"] = nil
	}
	writeJSON(w, http.StatusOK, resp)
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
