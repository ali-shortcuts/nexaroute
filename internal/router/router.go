package router

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

type Deployment struct {
	ID                string              `json:"id"`
	ProviderID        string              `json:"provider_id"`
	ProviderName      string              `json:"provider_name"`
	ProviderType      string              `json:"provider_type"`
	Model             string              `json:"model"`
	Aliases           []string            `json:"aliases"`
	Priority          int                 `json:"priority"`
	Weight            float64             `json:"weight"`
	ContextWindow     int                 `json:"context_window,omitempty"`
	InputCostPerMTok  float64             `json:"input_cost_per_mtok,omitempty"`
	OutputCostPerMTok float64             `json:"output_cost_per_mtok,omitempty"`
	Capabilities      config.Capabilities `json:"capabilities"`
}

type ProviderLoad struct {
	Active         int64
	Waiting        int64
	Limit          int
	QuotaExhausted bool
	QuotaPressure  float64
}

type Requirement struct {
	Model                               string
	Tools, Vision, Streaming, Reasoning bool
	ProviderType                        string
	SessionKey                          string
	SelectionKey                        string
	ProviderLoad                        map[string]ProviderLoad
	LoadForProvider                     func(string) ProviderLoad
	// MinContextWindow is the minimum usable context window a deployment
	// must advertise to serve this request (estimated prompt tokens plus
	// requested output). Zero disables the check. Deployments with an
	// unknown context window are always eligible.
	MinContextWindow     int
	EstimatedInputTokens int
	MaxOutputTokens      int
}

func (r Requirement) Scopes() []string {
	out := make([]string, 0, 4)
	if r.Tools {
		out = append(out, "tools")
	}
	if r.Vision {
		out = append(out, "vision")
	}
	if r.Streaming {
		out = append(out, "streaming")
	}
	if r.Reasoning {
		out = append(out, "reasoning")
	}
	return out
}

type Scored struct {
	Deployment       Deployment   `json:"deployment"`
	Health           health.State `json:"health"`
	Score            float64      `json:"score"`
	CapacityPressure float64      `json:"capacity_pressure,omitempty"`
	EstimatedCostUSD float64      `json:"estimated_cost_usd,omitempty"`
	PriceKnown       bool         `json:"price_known,omitempty"`
}

type sessionPin struct {
	Deployment string
	Expires    time.Time
}

const maxSessionPins = 10000

type Router struct {
	mu        sync.RWMutex
	cfg       config.Config
	health    *health.Manager
	all       []Deployment
	byID      map[string]Deployment
	byModel   map[string][]Deployment
	rr        atomic.Uint64
	sessionMu sync.RWMutex
	sessions  map[string]sessionPin
}

func New(cfg config.Config, hm *health.Manager) *Router {
	r := &Router{health: hm, sessions: map[string]sessionPin{}}
	r.Reload(cfg)
	return r
}
func IsReadyStrategy(s string) bool {
	return s == "ready_mesh" || s == "ready_queue" || s == "cost_aware"
}

func (r *Router) Reload(cfg config.Config) {
	all := make([]Deployment, 0)
	byID := map[string]Deployment{}
	byModel := map[string][]Deployment{}
	valid := map[string]struct{}{}
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
			d := Deployment{ID: p.ID + "/" + m.ID, ProviderID: p.ID, ProviderName: p.Name, ProviderType: p.Type, Model: m.Model, Aliases: m.Aliases, Priority: m.Priority, Weight: w, ContextWindow: m.ContextWindow, InputCostPerMTok: m.InputCostPerMTok, OutputCostPerMTok: m.OutputCostPerMTok, Capabilities: m.Capabilities}
			all = append(all, d)
			byID[d.ID] = d
			valid[d.ID] = struct{}{}

			seenKeys := map[string]struct{}{}
			keys := append([]string{d.ID, d.Model, m.ID}, d.Aliases...)
			for _, key := range keys {
				if key == "" {
					continue
				}
				if _, seen := seenKeys[key]; seen {
					continue
				}
				seenKeys[key] = struct{}{}
				byModel[key] = append(byModel[key], d)
			}
		}
	}
	r.mu.Lock()
	r.cfg = cfg
	r.all = all
	r.byID = byID
	r.byModel = byModel
	r.mu.Unlock()
	r.sessionMu.Lock()
	now := time.Now()
	if !cfg.Routing.SessionAffinity {
		r.sessions = map[string]sessionPin{}
	} else {
		for k, pin := range r.sessions {
			if _, ok := valid[pin.Deployment]; !ok || now.After(pin.Expires) {
				delete(r.sessions, k)
			}
		}
	}
	r.sessionMu.Unlock()
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
	default:
		return 3
	}
}

func capacityPressure(l ProviderLoad) float64 {
	if l.QuotaExhausted {
		return 4
	}
	p := 0.0
	if l.Limit > 0 {
		p = (float64(l.Active) + 2*float64(l.Waiting)) / float64(l.Limit)
	}
	if p < 0 {
		p = 0
	}
	if p > 4 {
		p = 4
	}
	if l.QuotaPressure > p {
		p = l.QuotaPressure
	}
	if p > 4 {
		return 4
	}
	return p
}

func (r *Router) scored(d Deployment, hs health.State, req Requirement, cfg config.Config) Scored {
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
	load := req.ProviderLoad[d.ProviderID]
	if req.LoadForProvider != nil {
		load = req.LoadForProvider(d.ProviderID)
	}
	pressure := capacityPressure(load)
	score += d.Weight*10 - float64(d.Priority)*3 - hs.EWMALatencyMS*cfg.Routing.LatencyWeight - pressure*cfg.Routing.CapacityWeight
	score -= hs.EWMAFailureRate * cfg.Routing.FailureWeight
	cost, priceKnown := estimatedRequestCost(d, req)
	return Scored{Deployment: d, Health: hs, Score: score, CapacityPressure: pressure, EstimatedCostUSD: cost, PriceKnown: priceKnown}
}

func (r *Router) eligibleDeployment(d Deployment, req Requirement, cfg config.Config, ignoreModel bool, scopes []string) (Scored, bool) {
	if !ignoreModel && !matchesModel(d, req.Model) {
		return Scored{}, false
	}
	if req.ProviderType != "" && d.ProviderType != req.ProviderType {
		return Scored{}, false
	}
	if !r.health.ProviderAvailable(d.ProviderID) {
		return Scored{}, false
	}
	// Pre-call context check: skip deployments that advertise a context
	// window too small for the estimated request. A guaranteed-failure
	// attempt wastes one attempt budget and burns provider quota for an
	// error the gateway can predict in microseconds.
	if req.MinContextWindow > 0 && d.ContextWindow > 0 && d.ContextWindow < req.MinContextWindow {
		return Scored{}, false
	}
	if req.Tools && !d.Capabilities.Tools || req.Vision && !d.Capabilities.Vision || req.Streaming && !d.Capabilities.Streaming || req.Reasoning && !d.Capabilities.Reasoning {
		return Scored{}, false
	}
	healthScopes := []string(nil)
	if cfg.Routing.Strategy == "ready_mesh" || cfg.Routing.Strategy == "cost_aware" {
		healthScopes = scopes
	}
	hs, scopesReady := r.health.GetWithScopes(d.ID, healthScopes)
	if IsReadyStrategy(cfg.Routing.Strategy) {
		if hs.Status != health.Healthy {
			return Scored{}, false
		}
		if cfg.Routing.Strategy == "ready_mesh" && !scopesReady {
			return Scored{}, false
		}
	} else if hs.Status == health.Cooldown {
		return Scored{}, false
	}
	return r.scored(d, hs, req, cfg), true
}

func (r *Router) affinityBucket(req Requirement) string {
	if strings.TrimSpace(req.SessionKey) == "" {
		return ""
	}
	h := sha256.Sum256([]byte(req.SessionKey))
	return hex.EncodeToString(h[:16]) + "|" + req.Model + "|" + req.ProviderType + "|" + strings.Join(req.Scopes(), ",")
}
func (r *Router) pinned(req Requirement, cfg config.Config) string {
	if !cfg.Routing.SessionAffinity {
		return ""
	}
	key := r.affinityBucket(req)
	if key == "" {
		return ""
	}
	now := time.Now()
	r.sessionMu.RLock()
	pin, ok := r.sessions[key]
	if ok && now.Before(pin.Expires) {
		r.sessionMu.RUnlock()
		return pin.Deployment
	}
	r.sessionMu.RUnlock()
	if !ok {
		return ""
	}

	// Upgrade only for the rare expiry path and re-check after acquiring the
	// write lock in case another request refreshed the pin meanwhile.
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	pin, ok = r.sessions[key]
	if !ok {
		return ""
	}
	if time.Now().After(pin.Expires) {
		delete(r.sessions, key)
		return ""
	}
	return pin.Deployment
}

// SessionPin returns the session pin deployment ID for req, or "" when
// session affinity is disabled, no session key is present, or no live pin
// exists. The decision plane uses it to short-circuit external providers when
// an eligible pin is authoritative. It reports the raw pin; eligibility
// against the current candidate set is checked by the caller.
func (r *Router) SessionPin(req Requirement) string {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	return r.pinned(req, cfg)
}
func (r *Router) ObserveSession(req Requirement, id string) {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	if !cfg.Routing.SessionAffinity || strings.TrimSpace(req.SessionKey) == "" || id == "" {
		return
	}
	ttl := time.Duration(cfg.Routing.SessionTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	key := r.affinityBucket(req)
	r.sessionMu.Lock()
	if _, exists := r.sessions[key]; !exists && len(r.sessions) >= maxSessionPins {
		// Affinity is an optimization, not authoritative state. Under a flood of
		// unique session IDs, evict one bounded entry instead of scanning the
		// whole table on every insertion.
		for k := range r.sessions {
			delete(r.sessions, k)
			break
		}
	}
	r.sessions[key] = sessionPin{Deployment: id, Expires: time.Now().Add(ttl)}
	r.sessionMu.Unlock()
}
func (r *Router) SessionCount() int {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	now := time.Now()
	for k, p := range r.sessions {
		if now.After(p.Expires) {
			delete(r.sessions, k)
		}
	}
	return len(r.sessions)
}

func hashIndex(key string, salt byte, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{salt})
	return int(h.Sum64() % uint64(n))
}
func estimatedRequestCost(d Deployment, req Requirement) (float64, bool) {
	// Cost-aware ordering is only trustworthy when the request supplies an
	// output ceiling. OpenAI callers may omit one; in that case all candidates
	// fall back to the ordinary health/quality ordering instead of comparing
	// incomplete input-only estimates.
	if req.EstimatedInputTokens <= 0 || req.MaxOutputTokens <= 0 {
		return 0, false
	}
	if d.InputCostPerMTok <= 0 && d.OutputCostPerMTok <= 0 {
		return 0, false
	}
	cost := (float64(req.EstimatedInputTokens)*d.InputCostPerMTok +
		float64(req.MaxOutputTokens)*d.OutputCostPerMTok) / 1_000_000
	if cost < 0 {
		return 0, false
	}
	return cost, true
}

func better(a, b Scored) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.CapacityPressure != b.CapacityPressure {
		return a.CapacityPressure < b.CapacityPressure
	}
	return a.Deployment.ID < b.Deployment.ID
}

func diversifyProviderFailover(out []Scored, start, attemptLimit int) {
	if start < 1 {
		start = 1
	}
	stop := len(out)
	if attemptLimit > 0 && stop > attemptLimit {
		stop = attemptLimit
	}
	// The primary route is already selected for quality/affinity. Only reorder
	// the bounded prefix the request can actually attempt; scanning/reordering an
	// entire large candidate pool would add hot-path work with no runtime value.
	// Never cross a priority tier.
	for i := start; i < stop; i++ {
		if out[i].Deployment.ProviderID != out[i-1].Deployment.ProviderID {
			continue
		}
		priority := out[i].Deployment.Priority
		pick := -1
		for j := i + 1; j < len(out) && out[j].Deployment.Priority == priority; j++ {
			if out[j].Deployment.ProviderID != out[i-1].Deployment.ProviderID {
				pick = j
				break
			}
		}
		if pick < 0 {
			continue
		}
		chosen := out[pick]
		copy(out[i+1:pick+1], out[i:pick])
		out[i] = chosen
	}
}

func (r *Router) orderCostAware(out []Scored, req Requirement, cfg config.Config) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Deployment.Priority != out[j].Deployment.Priority {
			return out[i].Deployment.Priority < out[j].Deployment.Priority
		}
		if out[i].PriceKnown != out[j].PriceKnown {
			return out[i].PriceKnown
		}
		if out[i].PriceKnown && out[i].EstimatedCostUSD != out[j].EstimatedCostUSD {
			return out[i].EstimatedCostUSD < out[j].EstimatedCostUSD
		}
		return better(out[i], out[j])
	})
	if len(out) < 2 {
		return
	}
	// Session affinity stays authoritative when enabled. Operators who want
	// every turn re-priced can disable affinity explicitly.
	if pin := r.pinned(req, cfg); pin != "" {
		bestPriority := out[0].Deployment.Priority
		for i := range out {
			if out[i].Deployment.ID == pin && out[i].Deployment.Priority == bestPriority {
				chosen := out[i]
				copy(out[1:i+1], out[0:i])
				out[0] = chosen
				diversifyProviderFailover(out, 1, cfg.Routing.MaxAttempts)
				return
			}
		}
	}
	diversifyProviderFailover(out, 1, cfg.Routing.MaxAttempts)
}

func (r *Router) orderReadyMesh(out []Scored, req Requirement, cfg config.Config) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Deployment.Priority != out[j].Deployment.Priority {
			return out[i].Deployment.Priority < out[j].Deployment.Priority
		}
		return better(out[i], out[j])
	})
	if len(out) < 2 {
		return
	}
	if pin := r.pinned(req, cfg); pin != "" {
		for i := range out {
			if out[i].Deployment.ID == pin {
				chosen := out[i]
				copy(out[1:i+1], out[0:i])
				out[0] = chosen
				diversifyProviderFailover(out, 1, cfg.Routing.MaxAttempts)
				return
			}
		}
	}
	window := 1
	p := out[0].Deployment.Priority
	for window < len(out) && out[window].Deployment.Priority == p {
		window++
	}
	if cfg.Routing.P2CWindow > 0 && window > cfg.Routing.P2CWindow {
		window = cfg.Routing.P2CWindow
	}
	if window < 2 {
		diversifyProviderFailover(out, 1, cfg.Routing.MaxAttempts)
		return
	}
	key := req.SelectionKey
	if key == "" {
		key = req.SessionKey
	}
	if key == "" {
		key = strconv.FormatUint(r.rr.Add(1), 10)
	}
	a := hashIndex(key, 'a', window)
	b := hashIndex(key, 'b', window-1)
	if b >= a {
		b++
	}
	winner := a
	if better(out[b], out[a]) {
		winner = b
	}
	out[0], out[winner] = out[winner], out[0]
	diversifyProviderFailover(out, 1, cfg.Routing.MaxAttempts)
}

func (r *Router) Candidates(req Requirement) []Scored {
	r.mu.RLock()
	cfg := r.cfg
	all := r.all
	source := all
	known := req.Model == "" || req.Model == "auto" || req.Model == "claude-auto"
	if !known {
		source = r.byModel[req.Model]
		known = len(source) > 0
	}
	r.mu.RUnlock()
	scopes := req.Scopes()
	build := func(in []Deployment, ignore bool) []Scored {
		out := make([]Scored, 0, len(in))
		for _, d := range in {
			if s, ok := r.eligibleDeployment(d, req, cfg, ignore, scopes); ok {
				out = append(out, s)
			}
		}
		return out
	}
	out := build(source, false)
	if len(out) == 0 && !known && cfg.Routing.FallbackOnUnknownModel {
		out = build(all, true)
	}
	if len(out) <= 1 {
		return out
	}
	switch cfg.Routing.Strategy {
	case "ready_mesh":
		r.orderReadyMesh(out, req, cfg)
	case "cost_aware":
		r.orderCostAware(out, req, cfg)
	case "ready_queue":
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Deployment.Priority != out[j].Deployment.Priority {
				return out[i].Deployment.Priority < out[j].Deployment.Priority
			}
			if out[i].Deployment.Weight != out[j].Deployment.Weight {
				return out[i].Deployment.Weight > out[j].Deployment.Weight
			}
			return out[i].Deployment.ID < out[j].Deployment.ID
		})
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
		rank := healthRank(out[0].Health.Status)
		w := 1
		for w < len(out) && healthRank(out[w].Health.Status) == rank {
			w++
		}
		if w > 1 {
			off := int((r.rr.Add(1) - 1) % uint64(w))
			rot := append([]Scored(nil), out[:w]...)
			for i := 0; i < w; i++ {
				out[i] = rot[(i+off)%w]
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
		w := 1
		best := out[0].Score
		for w < len(out) && w < 32 && out[w].Score >= best-15 && out[w].Health.Status != health.Degraded {
			w++
		}
		if w > 1 {
			off := int((r.rr.Add(1) - 1) % uint64(w))
			rot := append([]Scored(nil), out[:w]...)
			for i := 0; i < w; i++ {
				out[i] = rot[(i+off)%w]
			}
		}
	default:
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

func (r *Router) Eligible(id string, req Requirement) (Scored, bool) {
	r.mu.RLock()
	cfg := r.cfg
	d, ok := r.byID[id]
	r.mu.RUnlock()
	if !ok {
		return Scored{}, false
	}
	return r.eligibleDeployment(d, req, cfg, false, req.Scopes())
}
func (r *Router) Deployment(id string) (Deployment, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byID[id]
	return d, ok
}

func (r *Router) AllLimit(limit int) ([]Deployment, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := len(r.all)
	n := total
	if limit > 0 && n > limit {
		n = limit
	}
	return append([]Deployment(nil), r.all[:n]...), total
}

func (r *Router) All() []Deployment {
	out, _ := r.AllLimit(0)
	return out
}

func (r *Router) Readiness(strategy string) (total, usable int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total = len(r.all)
	readyStrategy := IsReadyStrategy(strategy)
	for _, d := range r.all {
		if !r.health.ProviderAvailable(d.ProviderID) {
			continue
		}
		st := r.health.Get(d.ID).Status
		if readyStrategy {
			if st == health.Healthy {
				usable++
			}
		} else if st != health.Cooldown {
			usable++
		}
	}
	return total, usable
}
