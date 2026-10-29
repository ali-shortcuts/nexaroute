package router

import (
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/pricing"
)

// Stable exclusion reason codes used by explainability surfaces
// (Router.Explain and the admin route-preview endpoint). These are
// operator-facing, machine-readable values; once published they must not
// be renamed. They describe deterministic routing decisions only and
// never embed user content or credentials.
const (
	ExcludeModelMismatch           = "model_mismatch"
	ExcludeProviderType            = "provider_type"
	ExcludePrivacyNoTraining       = "privacy_no_training"
	ExcludeProviderCircuit         = "provider_circuit_open"
	ExcludeContextWindow           = "context_window_too_small"
	ExcludeMissingTools            = "capability_tools_unsupported"
	ExcludeMissingVision           = "capability_vision_unsupported"
	ExcludeMissingStreaming        = "capability_streaming_unsupported"
	ExcludeMissingReasoning        = "capability_reasoning_unsupported"
	ExcludeRetired                 = "retired"
	ExcludeNotReady                = "health_not_ready"
	ExcludeCapabilityScopeNotReady = "capability_scope_not_ready"
	// Conformance-manifest exclusions (#436): the deployment's
	// provider dialect is marked UNSUPPORTED for a dimension
	// the request requires, per the conformance manifest.
	ExcludeConformanceStructuredOutput = "conformance_structured_output_unsupported"
	ExcludeConformanceParallelTools    = "conformance_parallel_tools_unsupported"
	ExcludeConformanceReasoning        = "conformance_reasoning_unsupported"
	ExcludeCooldown                    = "health_cooldown"
	// ExcludeFallbackSuperseded marks phase-1 (model-scoped) decisions that
	// were re-evaluated ignoring the unknown model under
	// Routing.FallbackOnUnknownModel; the trailing phase-2 decision is the
	// authoritative verdict for that deployment.
	ExcludeFallbackSuperseded = "fallback_superseded"
)

// decisionLimit bounds the explainability payload. Config validation caps
// providers/models/pools, so real configs stay far below this; the bound is
// a defensive guarantee that a preview can never produce an unbounded API
// response.
const decisionLimit = 1000

// CandidateDecision records one deployment's verdict under the candidate
// funnel for a single requirement: either included (scored and passed to the
// strategy ordering) or excluded with a stable reason.
type CandidateDecision struct {
	DeploymentID   string              `json:"deployment_id"`
	ProviderID     string              `json:"provider_id"`
	Model          string              `json:"model"`
	Priority       int                 `json:"priority"`
	Weight         float64             `json:"weight"`
	TrainsOnData   string              `json:"trains_on_data,omitempty"`
	Capabilities   config.Capabilities `json:"capabilities"`
	Included       bool                `json:"included"`
	ExcludedReason string              `json:"excluded_reason,omitempty"`
	Score          float64             `json:"score,omitempty"`
	// Score breakdown, copied from the canonical scored() result for
	// included candidates: base + health + weight - priority -
	// latency - capacity - failure == Score. Excluded candidates
	// carry no score, because the funnel rejects them before scoring.
	ScoreBase          float64 `json:"score_base,omitempty"`
	HealthAdjustment   float64 `json:"health_adjustment,omitempty"`
	WeightContribution float64 `json:"weight_contribution,omitempty"`
	PriorityPenalty    float64 `json:"priority_penalty,omitempty"`
	LatencyPenalty     float64 `json:"latency_penalty,omitempty"`
	CapacityPenalty    float64 `json:"capacity_penalty,omitempty"`
	FailurePenalty     float64 `json:"failure_penalty,omitempty"`
	HealthStatus       string  `json:"health_status,omitempty"`
	EWMALatencyMS      float64 `json:"ewma_latency_ms,omitempty"`
	EWMAFailureRate    float64 `json:"ewma_failure_rate,omitempty"`
	CapacityPressure   float64 `json:"capacity_pressure,omitempty"`
	EstimatedCostUSD   float64 `json:"estimated_cost_usd,omitempty"`
	PriceKnown         bool    `json:"price_known,omitempty"`
}

// Explanation is the redacted explainability result for one requirement:
// the candidate funnel verdicts plus the exact ordered candidate list the
// data plane would attempt. The same evaluateDeployment/applyStrategy paths
// back both, so the trace cannot drift from real routing behavior.
// IsRotatingStrategy reports whether live selection under strategy
// advances a counter or samples without a stable key, so a simulated
// order cannot be read as the next live pick.
func IsRotatingStrategy(strategy string) bool {
	switch strategy {
	case "round_robin", "adaptive_round_robin":
		return true
	default:
		return false
	}
}

type Explanation struct {
	// Simulated is always true for Explain output: this decision was
	// computed without calling a provider, reserving quota or budget,
	// writing a session pin, or advancing any routing counter. It is
	// not a record of a real request.
	Simulated bool `json:"simulated"`
	// Deterministic means repeating this exact simulation against
	// unchanged runtime state returns the same order. It does NOT
	// mean the simulated pick equals the next real pick.
	Deterministic bool `json:"deterministic"`
	// RotationPredictive is false when live selection rotates
	// (round_robin, adaptive_round_robin, or ready_mesh P2C with no
	// session/selection key): the simulation then shows a
	// deterministically seeded order, not the live rotation slot.
	// When true, the simulated lead candidate is the lead candidate
	// live routing would pick from the same snapshot, assuming no
	// health, config, quota or pin change in between.
	RotationPredictive       bool   `json:"rotation_predictive"`
	Strategy                 string `json:"strategy"`
	Model                    string `json:"model"`
	KnownModel               bool   `json:"known_model"`
	UsedUnknownModelFallback bool   `json:"used_unknown_model_fallback"`
	SessionAffinityEnabled   bool   `json:"session_affinity_enabled"`
	PinnedDeployment         string `json:"pinned_deployment,omitempty"`
	PinnedApplied            bool   `json:"pinned_applied"`
	// Cache-economics quantification (#421 item 1): what the
	// pin is worth, priced from the session's own last turn.
	// Zero when no cache evidence has been observed for the
	// session or the pinned deployment is unpriced — the
	// trace never invents a figure.
	PinnedSessionTurns        int                 `json:"pinned_session_turns,omitempty"`
	PinnedLastCacheReadTokens int                 `json:"pinned_last_cache_read_tokens,omitempty"`
	PinnedCacheBenefitUSD     float64             `json:"pinned_cache_benefit_usd,omitempty"`
	Candidates                []Scored            `json:"candidates,omitempty"`
	CandidateCount            int                 `json:"candidate_count"`
	IncludedCount             int                 `json:"included_count"`
	Decisions                 []CandidateDecision `json:"decisions"`
	ConsideredCount           int                 `json:"considered_count"`
	DecisionsTruncated        bool                `json:"decisions_truncated"`
}

// Explain evaluates the requirement against the current routing state and
// returns both the ordered candidates the request handler would attempt and
// the per-deployment exclusion reasons for everything else. It reads shared
// state (config, health, session pins) and performs no network calls, so it
// is safe as a dry-run preview surface. Unlike Candidates (which silently
// tells the caller nothing it rejected), the same funnel here surfaces a
// reason for every deployment it did not pick. Ordering uses the simulated
// strategy path: rotating strategies are seeded from the requirement's own
// keys instead of the live round-robin counter, so a simulation never
// advances real traffic's rotation and never creates a session pin. The
// only shared-state housekeeping a simulation can trigger is reaping an
// already-expired session pin on a read, exactly as any other read does.
// The result is therefore labelled Simulated, and RotationPredictive says
// whether the simulated lead is also the live lead from this snapshot.
func (r *Router) Explain(req Requirement) Explanation {
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

	exp := Explanation{
		Simulated:              true,
		Deterministic:          true,
		RotationPredictive:     !IsRotatingStrategy(cfg.Routing.Strategy) && !(cfg.Routing.Strategy == "ready_mesh" && req.SelectionKey == "" && req.SessionKey == ""),
		Strategy:               cfg.Routing.Strategy,
		Model:                  req.Model,
		KnownModel:             known,
		SessionAffinityEnabled: cfg.Routing.SessionAffinity,
	}
	if exp.SessionAffinityEnabled {
		exp.PinnedDeployment = r.pinned(req, cfg)
		if pin, ok := r.sessionPinRecord(req, cfg); ok {
			exp.PinnedSessionTurns = pin.Turns
			exp.PinnedLastCacheReadTokens = pin.LastCacheReadTokens
			if pin.LastCacheReadTokens > 0 {
				entry, source := pricing.FromConfig(cfg).ResolveFor(cfg, pin.Deployment, time.Now())
				if source != "unpriced" && entry.InputPerMTok > entry.CacheRead() {
					exp.PinnedCacheBenefitUSD = float64(pin.LastCacheReadTokens) * (entry.InputPerMTok - entry.CacheRead()) / 1e6
				}
			}
		}
	}

	verdicts := map[string]CandidateDecision{}
	addDecision := func(d Deployment, s Scored, reason string) {
		if _, seen := verdicts[d.ID]; seen {
			return
		}
		cd := CandidateDecision{
			DeploymentID: d.ID,
			ProviderID:   d.ProviderID,
			Model:        d.Model,
			Priority:     d.Priority,
			Weight:       d.Weight,
			TrainsOnData: d.TrainsOnData,
			Capabilities: d.Capabilities,
			HealthStatus: string(r.health.Get(d.ID).Status),
		}
		if reason == "" {
			cd.Included = true
			cd.Score = s.Score
			cd.ScoreBase = s.ScoreBase
			cd.HealthAdjustment = s.HealthAdjustment
			cd.WeightContribution = s.WeightContribution
			cd.PriorityPenalty = s.PriorityPenalty
			cd.LatencyPenalty = s.LatencyPenalty
			cd.CapacityPenalty = s.CapacityPenalty
			cd.FailurePenalty = s.FailurePenalty
			cd.HealthStatus = string(s.Health.Status)
			cd.EWMALatencyMS = s.Health.EWMALatencyMS
			cd.EWMAFailureRate = s.Health.EWMAFailureRate
			cd.CapacityPressure = s.CapacityPressure
			cd.EstimatedCostUSD = s.EstimatedCostUSD
			cd.PriceKnown = s.PriceKnown
		} else {
			cd.ExcludedReason = reason
		}
		verdicts[d.ID] = cd
	}

	build := func(in []Deployment, ignore bool) []Scored {
		out := make([]Scored, 0, len(in))
		for _, d := range in {
			s, reason := r.evaluateDeployment(d, req, cfg, ignore, scopes)
			addDecision(d, s, reason)
			if reason == "" {
				out = append(out, s)
			}
		}
		return out
	}

	out := build(source, false)
	if len(out) == 0 && !known && cfg.Routing.FallbackOnUnknownModel {
		// Re-evaluate every deployment ignoring the model, mirroring
		// Candidates. Phase-1 verdicts are superseded: for deployments
		// already in the map the phase-2 verdict wins; for deployments
		// that no longer exist, nothing is emitted.
		exp.UsedUnknownModelFallback = true
		out = build(all, true)
	}

	// Deterministic decision ordering: deployment list order, then the
	// bounded decision output.
	order := all
	if !exp.UsedUnknownModelFallback {
		order = source
	}
	decisions := make([]CandidateDecision, 0, len(order))
	for _, d := range order {
		if cd, ok := verdicts[d.ID]; ok {
			decisions = append(decisions, cd)
			if len(decisions) >= decisionLimit {
				break
			}
		}
	}
	exp.DecisionsTruncated = len(verdicts) > len(decisions)
	exp.ConsideredCount = len(verdicts)
	exp.Decisions = decisions
	exp.IncludedCount = len(out)

	if len(out) > 1 {
		r.applyStrategySimulated(out, req, cfg)
	}
	exp.Candidates = out
	exp.CandidateCount = len(out)
	exp.PinnedApplied = exp.PinnedDeployment != "" && exp.CandidateCount > 0 &&
		exp.Candidates[0].Deployment.ID == exp.PinnedDeployment
	return exp
}
