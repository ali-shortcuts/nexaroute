package config

import (
	"fmt"
	"strings"
)

// Static no-eligible-deployment analysis (checklist M3-3).
//
// computeEligibilityWarnings derives advisory warnings purely from the
// configuration: enabled providers/models, candidate-pool expansion,
// route-profile fallback chains, and declared data-handling classes. It
// never consults live health, request context, or request-specific
// requirements (model, provider type, capabilities, context window), so
// a fresh ready_mesh install whose deployments have not been probed yet
// produces no warning. The expansion and privacy semantics deliberately
// mirror the runtime (route expansion matching and the fail-closed
// no_training gate) so a warning here means the same thing at request
// time; the runtime packages cannot be imported here without a cycle,
// so the small matching rule is reproduced and pinned by tests.
//
// The results are advisory only: they are stored on
// Config.EligibilityWarnings, recomputed by every ApplyDefaults call,
// and ignored by Validate and ValidateStrict.

// eligibilityDeployment is the static view of one enabled deployment.
type eligibilityDeployment struct {
	id      string // providerID + "/" + modelID, as in the router
	model   string
	aliases []string
	trains  string // lower-cased data_handling.trains_on_data; only "no" satisfies no_training
}

func (c *Config) enabledDeployments() []eligibilityDeployment {
	var out []eligibilityDeployment
	for _, p := range c.Providers {
		if !p.Enabled {
			continue
		}
		trains := strings.ToLower(strings.TrimSpace(p.DataHandling.TrainsOnData))
		for _, m := range p.Models {
			if !m.Enabled {
				continue
			}
			out = append(out, eligibilityDeployment{
				id:      p.ID + "/" + m.ID,
				model:   m.Model,
				aliases: m.Aliases,
				trains:  trains,
			})
		}
	}
	return out
}

// matchesEligibilityEntry reports whether a pool entry selects the
// deployment, mirroring route.matchesPoolEntry: deployment ID, model
// name, alias, or a "/"+entry suffix of the deployment ID.
func matchesEligibilityEntry(d eligibilityDeployment, entry string) bool {
	if d.id == entry || d.model == entry {
		return true
	}
	for _, a := range d.aliases {
		if a == entry {
			return true
		}
	}
	return strings.HasSuffix(d.id, "/"+entry)
}

// expandEligibilityPool resolves a pool to its deployment set. Mode
// "all" selects every enabled deployment; anything else is treated as
// explicit entries (ApplyDefaults has already defaulted the mode).
func expandEligibilityPool(pool CandidatePoolConfig, all []eligibilityDeployment) map[string]eligibilityDeployment {
	set := make(map[string]eligibilityDeployment)
	if pool.Mode == "all" {
		for _, d := range all {
			set[d.id] = d
		}
		return set
	}
	for _, entry := range pool.Deployments {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		for _, d := range all {
			if matchesEligibilityEntry(d, entry) {
				set[d.id] = d
			}
		}
	}
	return set
}

func (c *Config) warnEligibility(msg string) {
	for _, w := range c.EligibilityWarnings {
		if w == msg {
			return
		}
	}
	c.EligibilityWarnings = append(c.EligibilityWarnings, msg)
}

func (c *Config) computeEligibilityWarnings() {
	c.EligibilityWarnings = nil
	deployments := c.enabledDeployments()
	if len(deployments) == 0 {
		c.warnEligibility("providers: no enabled model deployments are configured; every route will fail until at least one provider and one model are enabled")
		return
	}

	poolsByID := make(map[string]CandidatePoolConfig, len(c.CandidatePools))
	for _, p := range c.CandidatePools {
		poolsByID[p.ID] = p
	}
	chainsByID := make(map[string]FallbackChainConfig, len(c.FallbackChains))
	for _, ch := range c.FallbackChains {
		chainsByID[ch.ID] = ch
	}

	expansions := make(map[string]map[string]eligibilityDeployment, len(c.CandidatePools))
	expand := func(poolID string) (map[string]eligibilityDeployment, bool) {
		if set, ok := expansions[poolID]; ok {
			return set, true
		}
		pool, ok := poolsByID[poolID]
		if !ok {
			return nil, false // unknown pools are a Validate error, not a warning
		}
		set := expandEligibilityPool(pool, deployments)
		expansions[poolID] = set
		return set, true
	}

	// chainPoolIDs lists a profile's primary pool followed by its
	// fallback chain's pools, in routing order.
	chainPoolIDs := func(rp RouteProfileConfig) []string {
		ids := []string{rp.CandidatePool}
		if strings.TrimSpace(rp.FallbackChain) != "" {
			if ch, ok := chainsByID[rp.FallbackChain]; ok {
				ids = append(ids, ch.Pools...)
			}
		}
		return ids
	}
	chainUnion := func(rp RouteProfileConfig) map[string]eligibilityDeployment {
		union := make(map[string]eligibilityDeployment)
		for _, pid := range chainPoolIDs(rp) {
			if set, ok := expand(pid); ok {
				for id, d := range set {
					union[id] = d
				}
			}
		}
		return union
	}

	// Pool-level warnings only for pools a route can actually reach; an
	// unreferenced empty pool is not, by itself, a routing problem.
	referenced := make([]string, 0, len(c.CandidatePools))
	seenPool := make(map[string]bool, len(c.CandidatePools))
	for _, rp := range c.RouteProfiles {
		for _, pid := range chainPoolIDs(rp) {
			if pid != "" && !seenPool[pid] {
				seenPool[pid] = true
				referenced = append(referenced, pid)
			}
		}
	}
	for _, pid := range referenced {
		if set, ok := expand(pid); ok && len(set) == 0 {
			c.warnEligibility(fmt.Sprintf("candidate_pools[%s]: pool matches no enabled deployments; check its entries for typos, renamed models, or disabled providers or models", pid))
		}
	}

	profilesByID := make(map[string]RouteProfileConfig, len(c.RouteProfiles))
	for _, rp := range c.RouteProfiles {
		profilesByID[rp.ID] = rp
	}
	satisfying := func(set map[string]eligibilityDeployment) int {
		n := 0
		for _, d := range set {
			if d.trains == "no" {
				n++
			}
		}
		return n
	}

	for _, rp := range c.RouteProfiles {
		union := chainUnion(rp)
		if len(union) == 0 {
			c.warnEligibility(fmt.Sprintf("route_profiles[%s]: no usable deployments across its candidate pool and fallback chain; requests routed by this profile will fail until a pool in its chain matches an enabled deployment", rp.ID))
			continue
		}
		if rp.Privacy == "no_training" && satisfying(union) == 0 {
			c.warnEligibility(fmt.Sprintf("route_profiles[%s]: profile requires no_training but none of its %d reachable deployments declare data_handling.trains_on_data \"no\"; requests will fail closed", rp.ID, len(union)))
		}
	}

	// Virtual keys: only the no_training zero-satisfying case, where the
	// runtime conclusion is exact (the same fail-closed rule as M3-2).
	allSet := make(map[string]eligibilityDeployment, len(deployments))
	for _, d := range deployments {
		allSet[d.id] = d
	}
	for _, k := range c.ClientAuth.VirtualKeys {
		if k.Revoked || k.RequirePrivacy != "no_training" {
			continue
		}
		reachable := allSet
		if len(k.AllowedRoutes) > 0 {
			reachable = make(map[string]eligibilityDeployment)
			for _, routeID := range k.AllowedRoutes {
				rp, ok := profilesByID[routeID]
				if !ok {
					continue
				}
				for id, d := range chainUnion(rp) {
					reachable[id] = d
				}
			}
		}
		if len(reachable) == 0 {
			continue // dead routes are already reported at the profile level
		}
		if satisfying(reachable) == 0 {
			c.warnEligibility(fmt.Sprintf("client_auth.virtual_keys[%s]: key requires no_training but none of its %d reachable deployments declare data_handling.trains_on_data \"no\"; its requests will fail closed", k.ID, len(reachable)))
		}
	}
}
