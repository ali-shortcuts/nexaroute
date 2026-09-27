package route

import (
	"strings"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// ResolvedRoute carries the virtual endpoint resolution result.
// It contains only what the existing router needs plus stable route identity
// for observability. It does NOT contain a second routing core.
type ResolvedRoute struct {
	VirtualEndpointID   string
	VirtualEndpointName string
	PublicModel         string
	RouteProfileID      string
	RouteProfileName    string
	PrimaryPoolID       string
	PrimaryPoolName     string
	PrimaryMode         string // explicit or all
	FallbackChainID     string
	FallbackChainName   string
	// Ordered pool IDs to try: primary first, then fallback chain pools.
	OrderedPoolIDs []string
	// AllowedDeployments maps deployment ID -> struct{} for the primary pool.
	// If nil and PrimaryMode=="all", it means all deployments allowed.
	AllowedDeployments map[string]struct{}
	// FallbackAllowed is parallel to fallback pools (excluding primary) with their allowed sets.
	FallbackAllowed []map[string]struct{}
	// AllAllowed is union of all pools (for quick checks / observability)
	AllAllowed map[string]struct{}
	// Resolved pools for UI / debugging
	Pools map[string]config.CandidatePoolConfig
}

// Resolver holds indexes for virtual endpoint resolution.
// It is rebuilt on config reload and is safe for concurrent reads.
type Resolver struct {
	mu sync.RWMutex

	// Indexes
	byPublicModel map[string]config.VirtualEndpointConfig
	byID          map[string]config.VirtualEndpointConfig
	profiles      map[string]config.RouteProfileConfig
	pools         map[string]config.CandidatePoolConfig
	chains        map[string]config.FallbackChainConfig

	// Expanded pool membership: poolID -> set of deployment IDs
	expanded map[string]map[string]struct{}

	// For observability: list of VEs
	virtualEndpoints []config.VirtualEndpointConfig
}

// NewResolver builds a resolver from config and current deployments.
// Deployments are used to expand pool entries (model IDs, aliases, deployment IDs).
func NewResolver(cfg config.Config, allDeployments []router.Deployment) *Resolver {
	r := &Resolver{
		byPublicModel:    make(map[string]config.VirtualEndpointConfig, len(cfg.VirtualEndpoints)),
		byID:             make(map[string]config.VirtualEndpointConfig, len(cfg.VirtualEndpoints)),
		profiles:         make(map[string]config.RouteProfileConfig, len(cfg.RouteProfiles)),
		pools:            make(map[string]config.CandidatePoolConfig, len(cfg.CandidatePools)),
		chains:           make(map[string]config.FallbackChainConfig, len(cfg.FallbackChains)),
		expanded:         make(map[string]map[string]struct{}, len(cfg.CandidatePools)),
		virtualEndpoints: append([]config.VirtualEndpointConfig(nil), cfg.VirtualEndpoints...),
	}
	for _, ve := range cfg.VirtualEndpoints {
		r.byID[ve.ID] = ve
		r.byPublicModel[ve.PublicModel] = ve
	}
	for _, rp := range cfg.RouteProfiles {
		r.profiles[rp.ID] = rp
	}
	for _, cp := range cfg.CandidatePools {
		r.pools[cp.ID] = cp
	}
	for _, fc := range cfg.FallbackChains {
		r.chains[fc.ID] = fc
	}
	// Expand pools
	for _, cp := range cfg.CandidatePools {
		if strings.ToLower(cp.Mode) == "all" {
			// all mode: nil means allow all; but store explicit all for debugging
			allSet := make(map[string]struct{}, len(allDeployments))
			for _, d := range allDeployments {
				allSet[d.ID] = struct{}{}
			}
			r.expanded[cp.ID] = allSet
			continue
		}
		set := make(map[string]struct{})
		if len(cp.Deployments) == 0 {
			// explicit empty pool -> no deployments
			r.expanded[cp.ID] = set
			continue
		}
		for _, entry := range cp.Deployments {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			for _, d := range allDeployments {
				if matchesPoolEntry(d, entry) {
					set[d.ID] = struct{}{}
				}
			}
		}
		r.expanded[cp.ID] = set
	}
	return r
}

// matchesPoolEntry checks if a deployment matches a pool entry.
// Pool entry may be deployment ID, model ID, alias, or suffix.
func matchesPoolEntry(d router.Deployment, entry string) bool {
	if d.ID == entry {
		return true
	}
	if d.Model == entry {
		return true
	}
	for _, a := range d.Aliases {
		if a == entry {
			return true
		}
	}
	// suffix match: deployment ID ends with "/"+entry (like router.matchesModel)
	if strings.HasSuffix(d.ID, "/"+entry) {
		return true
	}
	return false
}

// Resolve returns the resolved route for a requested model if it matches a virtual endpoint.
// Second return bool indicates whether model is a virtual endpoint.
// If virtual endpoint is disabled, it returns (ResolvedRoute, true) but with IsEnabled false check left to caller,
// or we can return error. Here we return disabled as not found? Caller should check disabled separately.
// We return resolved even if disabled, caller decides.
func (r *Resolver) Resolve(model string) (ResolvedRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ve, ok := r.byPublicModel[model]
	if !ok {
		return ResolvedRoute{}, false
	}
	// Build resolved route
	rp, ok := r.profiles[ve.RouteProfile]
	if !ok {
		// Should not happen if config validated, but return not found
		return ResolvedRoute{}, false
	}
	primaryPool, ok := r.pools[rp.CandidatePool]
	if !ok {
		return ResolvedRoute{}, false
	}
	primaryAllowed := r.expanded[primaryPool.ID]

	ordered := []string{primaryPool.ID}
	var fallbackChain *config.FallbackChainConfig
	var fallbackAllowed []map[string]struct{}
	var fallbackChainID, fallbackChainName string
	if rp.FallbackChain != "" {
		if fc, ok := r.chains[rp.FallbackChain]; ok {
			fallbackChain = &fc
			fallbackChainID = fc.ID
			fallbackChainName = fc.Name
			for _, pid := range fc.Pools {
				// Avoid duplicate primary in fallback (if present, skip)
				if pid == primaryPool.ID {
					continue
				}
				ordered = append(ordered, pid)
				if set, ok := r.expanded[pid]; ok {
					// copy set to avoid mutation
					cpy := make(map[string]struct{}, len(set))
					for k := range set {
						cpy[k] = struct{}{}
					}
					fallbackAllowed = append(fallbackAllowed, cpy)
				} else {
					fallbackAllowed = append(fallbackAllowed, map[string]struct{}{})
				}
			}
		}
	}

	// AllAllowed union
	allAllowed := make(map[string]struct{})
	for k := range primaryAllowed {
		allAllowed[k] = struct{}{}
	}
	for _, set := range fallbackAllowed {
		for k := range set {
			allAllowed[k] = struct{}{}
		}
	}

	// Pools map for UI
	poolsMap := make(map[string]config.CandidatePoolConfig, len(r.pools))
	for id, p := range r.pools {
		poolsMap[id] = p
	}

	// If primary mode is "all", AllowedDeployments nil means all, but we have expanded all set.
	// Keep expanded set for filtering; if mode all, we treat nil as all but we have set.
	res := ResolvedRoute{
		VirtualEndpointID:   ve.ID,
		VirtualEndpointName: ve.Name,
		PublicModel:         ve.PublicModel,
		RouteProfileID:      rp.ID,
		RouteProfileName:    rp.Name,
		PrimaryPoolID:       primaryPool.ID,
		PrimaryPoolName:     primaryPool.Name,
		PrimaryMode:         primaryPool.Mode,
		FallbackChainID:     fallbackChainID,
		FallbackChainName:   fallbackChainName,
		OrderedPoolIDs:      ordered,
		AllowedDeployments:  primaryAllowed,
		FallbackAllowed:     fallbackAllowed,
		AllAllowed:          allAllowed,
		Pools:               poolsMap,
	}
	// If fallbackChain exists but we filtered primary out, we still have it in ordered.
	_ = fallbackChain
	return res, true
}

// ResolveByID resolves by virtual endpoint ID (for admin APIs)
func (r *Resolver) ResolveByID(id string) (config.VirtualEndpointConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ve, ok := r.byID[id]
	return ve, ok
}

// ListVirtualEndpoints returns all virtual endpoints (copy)
func (r *Resolver) ListVirtualEndpoints() []config.VirtualEndpointConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.VirtualEndpointConfig, len(r.virtualEndpoints))
	copy(out, r.virtualEndpoints)
	return out
}

// ListRouteProfiles returns all route profiles
func (r *Resolver) ListRouteProfiles() []config.RouteProfileConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.RouteProfileConfig, 0, len(r.profiles))
	for _, v := range r.profiles {
		out = append(out, v)
	}
	return out
}

// ListCandidatePools returns all candidate pools
func (r *Resolver) ListCandidatePools() []config.CandidatePoolConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.CandidatePoolConfig, 0, len(r.pools))
	for _, v := range r.pools {
		out = append(out, v)
	}
	return out
}

// ListFallbackChains returns all fallback chains
func (r *Resolver) ListFallbackChains() []config.FallbackChainConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.FallbackChainConfig, 0, len(r.chains))
	for _, v := range r.chains {
		out = append(out, v)
	}
	return out
}

// GetExpanded returns expanded set for a pool ID
func (r *Resolver) GetExpanded(poolID string) (map[string]struct{}, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set, ok := r.expanded[poolID]
	if !ok {
		return nil, false
	}
	// copy
	cpy := make(map[string]struct{}, len(set))
	for k := range set {
		cpy[k] = struct{}{}
	}
	return cpy, true
}

// FilterCandidates filters a candidate list by allowed set.
// If allowed is nil and mode is all, it means allow all (return original).
// If allowed is empty map, no candidates.
func FilterCandidates(candidates []router.Scored, allowed map[string]struct{}, mode string) []router.Scored {
	if mode == "all" {
		// If allowed is nil or empty but mode all, treat as all.
		// However expanded all set is non-nil with all IDs; so we can just check if allowed nil.
		if allowed == nil {
			return candidates
		}
		// If allowed is the all set (contains all), filtering still works but returns all eligible.
	}
	if allowed == nil {
		return candidates
	}
	if len(allowed) == 0 {
		return nil
	}
	out := make([]router.Scored, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := allowed[c.Deployment.ID]; ok {
			out = append(out, c)
		}
	}
	return out
}

// ResolveCandidates tries primary pool, then fallback pools in order, returning first non-empty filtered list.
// Returns filtered candidates and the pool ID that provided them.
func (r *Resolver) ResolveCandidates(candidates []router.Scored, resolved ResolvedRoute) ([]router.Scored, string) {
	// Primary
	filtered := FilterCandidates(candidates, resolved.AllowedDeployments, resolved.PrimaryMode)
	if len(filtered) > 0 {
		return filtered, resolved.PrimaryPoolID
	}
	// Fallbacks in order
	fallbackIndex := 0
	for i := 1; i < len(resolved.OrderedPoolIDs); i++ {
		poolID := resolved.OrderedPoolIDs[i]
		var allowed map[string]struct{}
		if fallbackIndex < len(resolved.FallbackAllowed) {
			allowed = resolved.FallbackAllowed[fallbackIndex]
			fallbackIndex++
		} else {
			if set, ok := r.GetExpanded(poolID); ok {
				allowed = set
			}
		}
		mode := "explicit"
		if p, ok := r.pools[poolID]; ok {
			mode = p.Mode
		}
		f := FilterCandidates(candidates, allowed, mode)
		if len(f) > 0 {
			return f, poolID
		}
	}
	return nil, ""
}

// AllFilteredCandidates returns candidates from all pools in order (primary then fallbacks),
// preserving the router's ordering within each pool and deduplicating deployments.
// This allows the retry loop to failover from primary to fallback at runtime.
func (r *Resolver) AllFilteredCandidates(candidates []router.Scored, resolved ResolvedRoute) []router.Scored {
	seen := make(map[string]struct{}, len(candidates))
	out := make([]router.Scored, 0, len(candidates))
	// Helper to append filtered pool
	appendPool := func(allowed map[string]struct{}, mode string) {
		filtered := FilterCandidates(candidates, allowed, mode)
		for _, c := range filtered {
			if _, ok := seen[c.Deployment.ID]; ok {
				continue
			}
			seen[c.Deployment.ID] = struct{}{}
			out = append(out, c)
		}
	}
	appendPool(resolved.AllowedDeployments, resolved.PrimaryMode)
	fallbackIndex := 0
	for i := 1; i < len(resolved.OrderedPoolIDs); i++ {
		poolID := resolved.OrderedPoolIDs[i]
		var allowed map[string]struct{}
		if fallbackIndex < len(resolved.FallbackAllowed) {
			allowed = resolved.FallbackAllowed[fallbackIndex]
			fallbackIndex++
		} else {
			if set, ok := r.GetExpanded(poolID); ok {
				allowed = set
			}
		}
		mode := "explicit"
		if p, ok := r.pools[poolID]; ok {
			mode = p.Mode
		}
		appendPool(allowed, mode)
	}
	return out
}
