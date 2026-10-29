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
	"github.com/ali-shortcuts/nexaroute/internal/pricing"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
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
	// TrainsOnData is the provider's declared data-handling class
	// ("yes" | "no" | "unknown"). It is fail-closed metadata: privacy
	// routing only trusts the exact value "no".
	TrainsOnData string `json:"trains_on_data,omitempty"`
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
	// StructuredOutput and ParallelTools carry the request's
	// dialect-sensitive demands (#436): a deployment whose
	// provider dialect the conformance manifest marks
	// UNSUPPORTED for a required dimension is ineligible, even
	// when the model itself declares the capability — the flag
	// would be dropped on encode.
	StructuredOutput, ParallelTools bool
	ProviderType                    string
	SessionKey                      string
	SelectionKey                    string
	ProviderLoad                    map[string]ProviderLoad
	LoadForProvider                 func(string) ProviderLoad
	// MinContextWindow is the minimum usable context window a deployment
	// must advertise to serve this request (estimated prompt tokens plus
	// requested output). Zero disables the check. Deployments with an
	// unknown context window are always eligible.
	MinContextWindow     int
	EstimatedInputTokens int
	MaxOutputTokens      int
	// RequireNoTraining is true when the matched route profile has
	// privacy "no_training" OR the authenticated virtual key has
	// require_privacy "no_training" (strictest wins). When true, only
	// deployments whose provider declares trains_on_data="no" are
	// eligible. "unknown" and "yes" are both ineligible (fail closed).
	RequireNoTraining bool
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
	if r.StructuredOutput {
		out = append(out, "structured_output")
	}
	if r.ParallelTools {
		out = append(out, "parallel_tools")
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
	// ScoreBreakdown decomposes Score into the exact terms scored()
	// adds, so an explanation can say *why* a candidate won instead of
	// only reporting the total. Score == ScoreBase + HealthAdjustment +
	// WeightContribution - PriorityPenalty - LatencyPenalty -
	// CapacityPenalty - FailurePenalty. The breakdown is computed once,
	// inside scored(), so it can never drift from the ordering.
	ScoreBase          float64 `json:"score_base,omitempty"`
	HealthAdjustment   float64 `json:"health_adjustment,omitempty"`
	WeightContribution float64 `json:"weight_contribution,omitempty"`
	PriorityPenalty    float64 `json:"priority_penalty,omitempty"`
	LatencyPenalty     float64 `json:"latency_penalty,omitempty"`
	CapacityPenalty    float64 `json:"capacity_penalty,omitempty"`
	FailurePenalty     float64 `json:"failure_penalty,omitempty"`
}

type sessionPin struct {
	Deployment string
	Expires    time.Time
	// Cache economics (#421 item 1): how many turns this pin
	// has served and the last turn's observed token counts, so
	// the decision trace can price the warm prefix instead of
	// merely naming the pin.
	Turns               int
	LastInputTokens     int
	LastCacheReadTokens int
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

// SatisfiesNoTraining reports whether a provider trains_on_data value meets
// a no_training requirement. Only the exact value "no" (case-insensitive,
// trimmed) satisfies it. "unknown", "yes", and "" all fail closed.
func SatisfiesNoTraining(trainsOnData string) bool {
	return strings.ToLower(strings.TrimSpace(trainsOnData)) == "no"
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
	// The versioned price book (M5-6) governs routing prices when
	// it prices a deployment: the active version's rates replace
	// the model-config rates at build time. Resolved per Reload,
	// so a version flip takes effect on the next config swap.
	priceBook := pricing.FromConfig(cfg)
	bookNow := time.Now()
	all := make([]Deployment, 0)
	byID := map[string]Deployment{}
	byModel := map[string][]Deployment{}
	valid := map[string]struct{}{}
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		trains := strings.ToLower(strings.TrimSpace(p.DataHandling.TrainsOnData))
		if trains == "" {
			trains = "unknown"
		}
		for _, m := range p.Models {
			if !m.Enabled {
				continue
			}
			w := m.Weight
			if w <= 0 {
				w = 1
			}
			d := Deployment{ID: p.ID + "/" + m.ID, ProviderID: p.ID, ProviderName: p.Name, ProviderType: p.Type, Model: m.Model, Aliases: m.Aliases, Priority: m.Priority, Weight: w, ContextWindow: m.ContextWindow, InputCostPerMTok: m.InputCostPerMTok, OutputCostPerMTok: m.OutputCostPerMTok, Capabilities: m.Capabilities, TrainsOnData: trains}
			if entry, _, ok := priceBook.Resolve(d.ID, bookNow); ok {
				d.InputCostPerMTok = entry.InputPerMTok
				d.OutputCostPerMTok = entry.OutputPerMTok
			}
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
	var healthAdj float64
	switch hs.Status {
	case health.Healthy:
		healthAdj = 35
	case health.Unknown:
		healthAdj = 10
	case health.HalfOpen:
		healthAdj = -10
	case health.Degraded:
		healthAdj = -20
	}
	load := req.ProviderLoad[d.ProviderID]
	if req.LoadForProvider != nil {
		load = req.LoadForProvider(d.ProviderID)
	}
	pressure := capacityPressure(load)
	weightContrib := d.Weight * 10
	priorityPenalty := float64(d.Priority) * 3
	latencyPenalty := hs.EWMALatencyMS * cfg.Routing.LatencyWeight
	capacityPenalty := pressure * cfg.Routing.CapacityWeight
	failurePenalty := hs.EWMAFailureRate * cfg.Routing.FailureWeight
	score := 100.0 + healthAdj + weightContrib - priorityPenalty - latencyPenalty - capacityPenalty - failurePenalty
	cost, priceKnown := estimatedRequestCost(d, req)
	return Scored{Deployment: d, Health: hs, Score: score, CapacityPressure: pressure, EstimatedCostUSD: cost, PriceKnown: priceKnown,
		ScoreBase: 100, HealthAdjustment: healthAdj, WeightContribution: weightContrib, PriorityPenalty: priorityPenalty,
		LatencyPenalty: latencyPenalty, CapacityPenalty: capacityPenalty, FailurePenalty: failurePenalty}
}

// simulatedRotationOffset derives a deterministic rotation offset from
// the requirement's own keys, never from the live round-robin counter.
// The same keys always simulate the same pick; with neither a key nor
// a model the offset is 0, i.e. the simulation shows the head of the
// window rather than predicting which slot live rotation will land on
// next.
func simulatedRotationOffset(req Requirement, window int) int {
	if window <= 1 {
		return 0
	}
	key := req.SelectionKey
	if key == "" {
		key = req.SessionKey
	}
	if key == "" && req.Model == "" {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(req.Model))
	return int(h.Sum64() % uint64(window)) // #nosec G115 -- modulo result is strictly less than window.
}

func (r *Router) eligibleDeployment(d Deployment, req Requirement, cfg config.Config, ignoreModel bool, scopes []string) (Scored, bool) {
	s, reason := r.evaluateDeployment(d, req, cfg, ignoreModel, scopes)
	return s, reason == ""
}

// evaluateDeployment is the same funnel as eligibleDeployment but adds an
// operator-facing exclusion reason, so explainability (Router.Explain and the
// admin route-preview surface) can never disagree with what the data plane
// actually routes. Everything that picks a deployment funnels through here
// (initial selection, retry/failover, fallback chains/candidate pools via
// Candidates, session-affinity re-checks via Eligible, hedged partners via
// currentRouteCandidate, and credential selection which only runs after a
// deployment is deemed eligible). A non-empty reason means excluded.
func (r *Router) evaluateDeployment(d Deployment, req Requirement, cfg config.Config, ignoreModel bool, scopes []string) (Scored, string) {
	if !ignoreModel && !matchesModel(d, req.Model) {
		return Scored{}, ExcludeModelMismatch
	}
	if req.ProviderType != "" && d.ProviderType != req.ProviderType {
		return Scored{}, ExcludeProviderType
	}
	// Fail-closed privacy gate: a no_training requirement only trusts
	// providers that declare trains_on_data="no". "unknown" is NOT
	// acceptable. This single gate covers every path that picks a
	// deployment because all of them funnel through evaluateDeployment
	// (initial selection, retry/failover, fallback chains/candidate pools
	// via Candidates, session-affinity re-checks via Eligible, hedged
	// partners via currentRouteCandidate, and credential selection which
	// only runs after a deployment is deemed eligible).
	if req.RequireNoTraining && !SatisfiesNoTraining(d.TrainsOnData) {
		return Scored{}, ExcludePrivacyNoTraining
	}
	if !r.health.ProviderAvailable(d.ProviderID) {
		return Scored{}, ExcludeProviderCircuit
	}
	// Pre-call context check: skip deployments that advertise a context
	// window too small for the estimated request. A guaranteed-failure
	// attempt wastes one attempt budget and burns provider quota for an
	// error the gateway can predict in microseconds.
	if req.MinContextWindow > 0 && d.ContextWindow > 0 && d.ContextWindow < req.MinContextWindow {
		return Scored{}, ExcludeContextWindow
	}
	if req.Tools && !d.Capabilities.Tools {
		return Scored{}, ExcludeMissingTools
	}
	if req.Vision && !d.Capabilities.Vision {
		return Scored{}, ExcludeMissingVision
	}
	if req.Streaming && !d.Capabilities.Streaming {
		return Scored{}, ExcludeMissingStreaming
	}
	if req.Reasoning && !d.Capabilities.Reasoning {
		return Scored{}, ExcludeMissingReasoning
	}
	// Conformance-manifest filter (#436): the manifest is the
	// evidence contract for dialect-level claims. A dimension
	// it marks UNSUPPORTED for this deployment's provider
	// dialect cannot be honored by translation — the request
	// must not be silently degraded, so the deployment is
	// ineligible for requests that require the dimension.
	if entry, ok := providers.ManifestEntry(d.ProviderType); ok {
		if req.StructuredOutput && entry.Dimensions[providers.DimStructuredOutput].State == providers.StateUnsupported {
			return Scored{}, ExcludeConformanceStructuredOutput
		}
		if req.ParallelTools && entry.Dimensions[providers.DimParallelTools].State == providers.StateUnsupported {
			return Scored{}, ExcludeConformanceParallelTools
		}
		if req.Reasoning && entry.Dimensions[providers.DimReasoning].State == providers.StateUnsupported {
			return Scored{}, ExcludeConformanceReasoning
		}
	}
	healthScopes := []string(nil)
	if cfg.Routing.Strategy == "ready_mesh" || cfg.Routing.Strategy == "cost_aware" {
		healthScopes = scopes
	}
	hs, scopesReady := r.health.GetWithScopes(d.ID, healthScopes)
	// Retired/EOL deployments are permanently ineligible until their deployment
	// identity is invalidated by a config/model change.
	if hs.Status == health.Retired {
		return Scored{}, ExcludeRetired
	}
	if IsReadyStrategy(cfg.Routing.Strategy) {
		if hs.Status != health.Healthy {
			return Scored{}, ExcludeNotReady
		}
		if cfg.Routing.Strategy == "ready_mesh" && !scopesReady {
			return Scored{}, ExcludeCapabilityScopeNotReady
		}
	} else if hs.Status == health.Cooldown {
		return Scored{}, ExcludeCooldown
	}
	return r.scored(d, hs, req, cfg), ""
}

func (r *Router) affinityBucket(req Requirement) string {
	if strings.TrimSpace(req.SessionKey) == "" {
		return ""
	}
	h := sha256.Sum256([]byte(req.SessionKey))
	privacy := "any"
	if req.RequireNoTraining {
		privacy = "no_training"
	}
	return hex.EncodeToString(h[:16]) + "|" + req.Model + "|" + req.ProviderType + "|" + strings.Join(req.Scopes(), ",") + "|" + privacy
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

// PinnedDeploymentID returns the pinned deployment ID for given requirement without exposing raw session key.
// It is safe for decision plane use — only returns deployment ID, not session key.
func (r *Router) PinnedDeploymentID(req Requirement) string {
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
	pin := r.sessions[key]
	if pin.Deployment != id {
		// The pin moved: the new deployment starts a fresh
		// cache-economics record.
		pin = sessionPin{Deployment: id}
	}
	pin.Expires = time.Now().Add(ttl)
	pin.Turns++
	r.sessions[key] = pin
	r.sessionMu.Unlock()
}

// ObserveSessionCache records a completed turn's observed token
// counts on the session pin (#421 item 1), so Explain can
// quantify the cache benefit of keeping the pin. Only positive
// observations update the record; a turn that reports no cache
// reads does not erase the last measured warm prefix.
func (r *Router) ObserveSessionCache(req Requirement, id string, inputTokens, cacheReadTokens int) {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	if !cfg.Routing.SessionAffinity || strings.TrimSpace(req.SessionKey) == "" || id == "" {
		return
	}
	key := r.affinityBucket(req)
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	pin, ok := r.sessions[key]
	if !ok || pin.Deployment != id {
		return
	}
	if inputTokens > 0 {
		pin.LastInputTokens = inputTokens
	}
	if cacheReadTokens > 0 {
		pin.LastCacheReadTokens = cacheReadTokens
	}
	r.sessions[key] = pin
}

// sessionPinRecord returns the live pin record for a
// requirement, or false when no unexpired pin exists.
func (r *Router) sessionPinRecord(req Requirement, cfg config.Config) (sessionPin, bool) {
	if !cfg.Routing.SessionAffinity {
		return sessionPin{}, false
	}
	key := r.affinityBucket(req)
	if key == "" {
		return sessionPin{}, false
	}
	r.sessionMu.RLock()
	defer r.sessionMu.RUnlock()
	pin, ok := r.sessions[key]
	if !ok || !time.Now().Before(pin.Expires) {
		return sessionPin{}, false
	}
	return pin, true
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
	return int(h.Sum64() % uint64(n)) // #nosec G115 -- the modulo result is strictly less than n, and n is a non-negative slice/index bound.
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

func (r *Router) orderReadyMesh(out []Scored, req Requirement, cfg config.Config, simulated bool) {
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
		bestPriority := out[0].Deployment.Priority
		for i := range out {
			if out[i].Deployment.ID == pin && out[i].Deployment.Priority == bestPriority {
				chosen := out[i]
				copy(out[1:i+1], out[0:i])
				out[0] = chosen
				diversifyProviderFailover(out, 1, cfg.Routing.MaxAttempts)
				// Session affinity stays authoritative when enabled
				// (#421): a promoted pin must NOT be silently
				// overridden by the P2C load-spreading choice below,
				// or the pin's cache-warmth purpose is defeated in
				// the default strategy (orderCostAware already
				// returns here). Unpinned traffic keeps P2C.
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
	if key == "" && !simulated {
		// Live traffic rotates the P2C window on every unkeyed
		// request. A simulation must never consume that counter:
		// with no key it hashes the empty key instead, which is
		// deterministic but explicitly NOT a prediction of the
		// next live pick (see Explanation.RotationPredictive).
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
	r.applyStrategy(out, req, cfg)
	return out
}

// applyStrategy orders an already-eligibility-filtered candidate list with the
// configured strategy. Extracted from Candidates so explainability surfaces
// (Router.Explain / admin route preview) observe exactly the ordering the data
// plane uses — never a second, drifting copy.
func (r *Router) applyStrategy(out []Scored, req Requirement, cfg config.Config) {
	r.applyStrategyMode(out, req, cfg, false)
}

// applyStrategySimulated is the ordering Explain uses: identical
// strategy code, but rotation comes from the requirement's own keys
// instead of the live round-robin counter, so a simulation can never
// advance real traffic's rotation.
func (r *Router) applyStrategySimulated(out []Scored, req Requirement, cfg config.Config) {
	r.applyStrategyMode(out, req, cfg, true)
}

func (r *Router) applyStrategyMode(out []Scored, req Requirement, cfg config.Config, simulated bool) {
	rotation := func(window int) int {
		if simulated {
			return simulatedRotationOffset(req, window)
		}
		return int((r.rr.Add(1) - 1) % uint64(window)) // #nosec G115 -- the modulo result is strictly less than window, the bounded rotation window size.
	}
	switch cfg.Routing.Strategy {
	case "ready_mesh":
		r.orderReadyMesh(out, req, cfg, simulated)
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
			off := rotation(w)
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
			off := rotation(w)
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
}

func (r *Router) Eligible(id string, req Requirement) (Scored, bool) {
	r.mu.RLock()
	cfg := r.cfg
	d, ok := r.byID[id]
	known := req.Model == "" || req.Model == "auto" || req.Model == "claude-auto"
	if ok && !known {
		_, known = r.byModel[req.Model]
	}
	r.mu.RUnlock()
	if !ok {
		return Scored{}, false
	}
	// Mirror Candidates: when the requested model is unknown and
	// fallback-on-unknown-model is enabled, Candidates selects fallback
	// deployments with the model-match check relaxed. The per-attempt
	// re-check must apply the same rule, or every fallback candidate is
	// silently dropped here and the request dies without any attempt.
	ignoreModel := !known && cfg.Routing.FallbackOnUnknownModel
	return r.eligibleDeployment(d, req, cfg, ignoreModel, req.Scopes())
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
