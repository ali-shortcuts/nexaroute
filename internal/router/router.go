package router

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ali-shortcuts/universal-llm-gateway/internal/config"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/health"
)

type Deployment struct {
	ID           string              `json:"id"`
	ProviderID   string              `json:"provider_id"`
	ProviderName string              `json:"provider_name"`
	ProviderType string              `json:"provider_type"`
	Model        string              `json:"model"`
	Aliases      []string            `json:"aliases"`
	Priority     int                 `json:"priority"`
	Weight       float64             `json:"weight"`
	Capabilities config.Capabilities `json:"capabilities"`
}

type Requirement struct {
	Model                               string
	Tools, Vision, Streaming, Reasoning bool
}
type Scored struct {
	Deployment Deployment   `json:"deployment"`
	Health     health.State `json:"health"`
	Score      float64      `json:"score"`
}

type Router struct {
	mu     sync.RWMutex
	cfg    config.Config
	health *health.Manager
	all    []Deployment
	rr     atomic.Uint64
}

func New(cfg config.Config, hm *health.Manager) *Router {
	r := &Router{health: hm}
	r.Reload(cfg)
	return r
}

func (r *Router) Reload(cfg config.Config) {
	all := make([]Deployment, 0)
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if !m.Enabled {
				continue
			}
			w := m.Weight
			if w <= 0 {
				w = 1
			}
			all = append(all, Deployment{ID: p.ID + "/" + m.ID, ProviderID: p.ID, ProviderName: p.Name, ProviderType: p.Type, Model: m.Model, Aliases: m.Aliases, Priority: m.Priority, Weight: w, Capabilities: m.Capabilities})
		}
	}
	r.mu.Lock()
	r.cfg = cfg
	r.all = all
	r.mu.Unlock()
}

func matchesModel(d Deployment, model string) bool {
	if model == "" || model == "auto" || model == "claude-auto" {
		return true
	}
	if d.ID == model || d.Model == model {
		return true
	}
	for _, a := range d.Aliases {
		if a == model {
			return true
		}
	}
	return strings.HasSuffix(d.ID, "/"+model)
}

func healthRank(st health.Status) int {
	switch st {
	case health.Healthy:
		return 0
	case health.Unknown:
		return 1
	case health.HalfOpen:
		return 2
	case health.Degraded:
		return 3
	default:
		return 3
	}
}

func (r *Router) Candidates(req Requirement) []Scored {
	r.mu.RLock()
	cfg := r.cfg
	all := append([]Deployment(nil), r.all...)
	r.mu.RUnlock()

	build := func(ignoreModel bool) []Scored {
		out := make([]Scored, 0, len(all))
		for _, d := range all {
			if !ignoreModel && !matchesModel(d, req.Model) {
				continue
			}
			if req.Tools && !d.Capabilities.Tools {
				continue
			}
			if req.Vision && !d.Capabilities.Vision {
				continue
			}
			if req.Streaming && !d.Capabilities.Streaming {
				continue
			}
			if req.Reasoning && !d.Capabilities.Reasoning {
				continue
			}
			hs := r.health.Get(d.ID)
			if hs.Status == health.Cooldown {
				continue
			}
			score := 100.0
			switch hs.Status {
			case health.Healthy:
				score += 35
			case health.Unknown:
				score += 10
			case health.HalfOpen:
				score -= 10
			case health.Degraded:
				score -= 20
			}
			score += d.Weight*10 - float64(d.Priority)*3 - hs.EWMALatencyMS*cfg.Routing.LatencyWeight
			total := hs.Successes + hs.Failures
			if total > 0 {
				score -= (float64(hs.Failures) / float64(total)) * cfg.Routing.FailureWeight
			}
			out = append(out, Scored{Deployment: d, Health: hs, Score: score})
		}
		return out
	}

	out := build(false)
	if len(out) == 0 && cfg.Routing.FallbackOnUnknownModel && req.Model != "" && req.Model != "auto" && req.Model != "claude-auto" {
		out = build(true)
	}
	if len(out) <= 1 {
		return out
	}

	strategy := cfg.Routing.Strategy
	switch strategy {
	case "priority":
		sort.SliceStable(out, func(i, j int) bool {
			ri, rj := healthRank(out[i].Health.Status), healthRank(out[j].Health.Status)
			if ri != rj {
				return ri < rj
			}
			if out[i].Deployment.Priority != out[j].Deployment.Priority {
				return out[i].Deployment.Priority < out[j].Deployment.Priority
			}
			return out[i].Score > out[j].Score
		})
	case "least_latency":
		sort.SliceStable(out, func(i, j int) bool {
			ri, rj := healthRank(out[i].Health.Status), healthRank(out[j].Health.Status)
			if ri != rj {
				return ri < rj
			}
			li, lj := out[i].Health.EWMALatencyMS, out[j].Health.EWMALatencyMS
			// Unknown latency goes after measured latency inside the same health band.
			if li == 0 && lj != 0 {
				return false
			}
			if li != 0 && lj == 0 {
				return true
			}
			if li != lj {
				return li < lj
			}
			return out[i].Score > out[j].Score
		})
	case "round_robin":
		sort.SliceStable(out, func(i, j int) bool {
			ri, rj := healthRank(out[i].Health.Status), healthRank(out[j].Health.Status)
			if ri != rj {
				return ri < rj
			}
			return out[i].Deployment.ID < out[j].Deployment.ID
		})
		// Rotate only inside the best currently available health band. Rotating
		// the entire list can promote a degraded/half-open deployment ahead of a
		// healthy one once the round-robin offset advances far enough.
		bestRank := healthRank(out[0].Health.Status)
		window := 1
		for window < len(out) && healthRank(out[window].Health.Status) == bestRank {
			window++
		}
		if window > 1 {
			off := int(r.rr.Add(1)-1) % window
			rot := append([]Scored(nil), out[:window]...)
			for i := 0; i < window; i++ {
				out[i] = rot[(i+off)%window]
			}
		}
	case "adaptive_round_robin":
		sort.SliceStable(out, func(i, j int) bool {
			ri, rj := healthRank(out[i].Health.Status), healthRank(out[j].Health.Status)
			if ri != rj {
				return ri < rj
			}
			return out[i].Score > out[j].Score
		})
		window := 1
		best := out[0].Score
		for window < len(out) && window < 32 && out[window].Score >= best-15 && out[window].Health.Status != health.Degraded {
			window++
		}
		if window > 1 {
			off := int(r.rr.Add(1)-1) % window
			rot := append([]Scored(nil), out[:window]...)
			for i := 0; i < window; i++ {
				out[i] = rot[(i+off)%window]
			}
		}
	default: // adaptive
		sort.SliceStable(out, func(i, j int) bool {
			ri, rj := healthRank(out[i].Health.Status), healthRank(out[j].Health.Status)
			if ri != rj {
				return ri < rj
			}
			return out[i].Score > out[j].Score
		})
	}
	return out
}

func (r *Router) All() []Deployment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Deployment(nil), r.all...)
}
