package httpapi

import (
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// routeEligibilityView builds the per-route-profile eligibility section
// of the admin snapshot (M3-3). It reports pool membership (expanded
// through the live route resolver) and current health-based exclusions
// only. Per-request filters — model matching, provider type, capability
// and context-window requirements — are deliberately NOT applied, so
// eligible_now is an upper bound for any single request, never a
// per-request prediction. The response carries a note saying so.
//
// Exclusion precedence mirrors the health/privacy portion of
// router.eligibleDeployment: the fail-closed privacy gate first, then
// provider availability, then deployment health status (retired, then
// the ready-strategy health requirement, then cooldown).
func (s *Server) routeEligibilityView(cfg config.Config, healthAll []health.State) map[string]any {
	healthByID := make(map[string]health.State, len(healthAll))
	for _, st := range healthAll {
		healthByID[st.Deployment] = st
	}
	deploymentsByID := make(map[string]router.Deployment)
	if s.rt != nil {
		for _, d := range s.rt.All() {
			deploymentsByID[d.ID] = d
		}
	}
	chainsByID := make(map[string]config.FallbackChainConfig, len(cfg.FallbackChains))
	for _, ch := range cfg.FallbackChains {
		chainsByID[ch.ID] = ch
	}
	readyStrategy := router.IsReadyStrategy(cfg.Routing.Strategy)

	profiles := make([]map[string]any, 0, len(cfg.RouteProfiles))
	for _, rp := range cfg.RouteProfiles {
		poolIDs := []string{rp.CandidatePool}
		fallbackPools := []string{}
		if rp.FallbackChain != "" {
			if ch, ok := chainsByID[rp.FallbackChain]; ok {
				fallbackPools = append(fallbackPools, ch.Pools...)
				poolIDs = append(poolIDs, ch.Pools...)
			}
		}

		members := make(map[string]struct{})
		if s.routeResolver != nil {
			for _, pid := range poolIDs {
				if set, ok := s.routeResolver.GetExpanded(pid); ok {
					for id := range set {
						members[id] = struct{}{}
					}
				}
			}
		}

		excluded := map[string]int{
			"privacy_filtered":       0,
			"provider_unavailable":   0,
			"retired":                0,
			"cooldown":               0,
			"health_not_established": 0,
			"not_healthy":            0,
		}
		eligibleNow := 0
		privacySatisfying := 0
		for id := range members {
			d, ok := deploymentsByID[id]
			if !ok {
				continue
			}
			satisfies := router.SatisfiesNoTraining(d.TrainsOnData)
			if satisfies {
				privacySatisfying++
			}
			if rp.Privacy == "no_training" && !satisfies {
				excluded["privacy_filtered"]++
				continue
			}
			if s.hm != nil && !s.hm.ProviderAvailable(d.ProviderID) {
				excluded["provider_unavailable"]++
				continue
			}
			st, known := healthByID[id]
			status := health.Unknown
			if known {
				status = st.Status
			}
			// Cooldown and Retired exclude under every strategy, so
			// they keep their own buckets; the ready-strategy health
			// requirement then separates not-yet-established health
			// (Unknown) from other non-healthy states.
			switch {
			case status == health.Retired:
				excluded["retired"]++
			case status == health.Cooldown:
				excluded["cooldown"]++
			case readyStrategy && status != health.Healthy:
				if status == health.Unknown {
					excluded["health_not_established"]++
				} else {
					excluded["not_healthy"]++
				}
			default:
				eligibleNow++
			}
		}

		state := "ok"
		switch {
		case len(members) == 0:
			state = "empty"
		case eligibleNow == 0:
			state = "unavailable"
		case eligibleNow < len(members):
			state = "degraded"
		}

		row := map[string]any{
			"profile_id":         rp.ID,
			"candidate_pool":     rp.CandidatePool,
			"fallback_pools":     fallbackPools,
			"candidates":         len(members),
			"privacy_satisfying": privacySatisfying,
			"eligible_now":       eligibleNow,
			"excluded":           excluded,
			"state":              state,
		}
		if rp.Privacy != "" {
			row["privacy"] = rp.Privacy
		}
		if rp.FallbackChain != "" {
			row["fallback_chain"] = rp.FallbackChain
		}
		profiles = append(profiles, row)
	}

	return map[string]any{
		"note":     "Counts reflect candidate-pool membership and current health only. Per-request filters (model, provider type, capabilities, context window) are not applied, so eligible_now is an upper bound for any single request.",
		"strategy": cfg.Routing.Strategy,
		"profiles": profiles,
	}
}

// eligibilityWarningsForSnapshot returns the static M3-3 warnings of the
// current config as a non-nil slice for JSON output.
func eligibilityWarningsForSnapshot(cfg config.Config) []string {
	if len(cfg.EligibilityWarnings) == 0 {
		return []string{}
	}
	return append([]string(nil), cfg.EligibilityWarnings...)
}
