package router

import "github.com/ali-shortcuts/nexaroute/internal/config"

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
	ExcludeCooldown                = "health_cooldown"
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
	DeploymentID     string              `json:"deployment_id"`
	ProviderID       string              `json:"provider_id"`
	Model            string              `json:"model"`
	Priority         int                 `json:"priority"`
	Weight           float64             `json:"weight"`
	TrainsOnData     string              `json:"trains_on_data,omitempty"`
	Capabilities     config.Capabilities `json:"capabilities"`
	Included         bool                `json:"included"`
	ExcludedReason   string              `json:"excluded_reason,omitempty"`
	Score            float64             `json:"score,omitempty"`
	HealthStatus     string              `json:"health_status,omitempty"`
	EWMALatencyMS    float64             `json:"ewma_latency_ms,omitempty"`
	EWMAFailureRate  float64             `json:"ewma_failure_rate,omitempty"`
	CapacityPressure float64             `json:"capacity_pressure,omitempty"`
	EstimatedCostUSD float64             `json:"estimated_cost_usd,omitempty"`
	PriceKnown       bool                `json:"price_known,omitempty"`
}

// Explanation is the redacted explainability result for one requirement:
// the candidate funnel verdicts plus the exact ordered candidate list the
// data plane would attempt. The same evaluateDeployment/applyStrategy paths
// back both, so the trace cannot drift from real routing behavior.
type Explanation struct {
	Strategy                 string              `json:"strategy"`
	Model                    string              `json:"model"`
	KnownModel               bool                `json:"known_model"`
	UsedUnknownModelFallback bool                `json:"used_unknown_model_fallback"`
	SessionAffinityEnabled   bool                `json:"session_affinity_enabled"`
	PinnedDeployment         string              `json:"pinned_deployment,omitempty"`
	PinnedApplied            bool                `json:"pinned_applied"`
	Candidates               []Scored            `json:"candidates,omitempty"`
	CandidateCount           int                 `json:"candidate_count"`
	IncludedCount            int                 `json:"included_count"`
	Decisions                []CandidateDecision `json:"decisions"`
	ConsideredCount          int                 `json:"considered_count"`
	DecisionsTruncated       bool                `json:"decisions_truncated"`
}

func decisionFromScored(s Scored) CandidateDecision {
	return CandidateDecision{
		DeploymentID:     s.Deployment.ID,
		ProviderID:       s.Deployment.ProviderID,
		Model:            s.Deployment.Model,
		Priority:         s.Deployment.Priority,
		Weight:           s.Deployment.Weight,
		TrainsOnData:     s.Deployment.TrainsOnData,
		Capabilities:     s.Deployment.Capabilities,
		Included:         true,
		Score:            s.Score,
		HealthStatus:     string(s.Health.Status),
		EWMALatencyMS:    s.Health.EWMALatencyMS,
		EWMAFailureRate:  s.Health.EWMAFailureRate,
		CapacityPressure: s.CapacityPressure,
		EstimatedCostUSD: s.EstimatedCostUSD,
		PriceKnown:       s.PriceKnown,
	}
}

// Explain evaluates the requirement against the current routing state and
// returns both the ordered candidates the request handler would attempt and
// the per-deployment exclusion reasons for everything else. It reads shared
// state (config, health, session pins) but mutates nothing and performs no
// network calls, so it is safe as a dry-run preview surface. Unlike
// Candidates (which silently tells the caller nothing it rejected), the
// same funnel here surfaces a reason for every deployment it did not pick.
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
		Strategy:               cfg.Routing.Strategy,
		Model:                  req.Model,
		KnownModel:             known,
		SessionAffinityEnabled: cfg.Routing.SessionAffinity,
	}
	if exp.SessionAffinityEnabled {
		exp.PinnedDeployment = r.pinned(req, cfg)
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
		r.applyStrategy(out, req, cfg)
	}
	exp.Candidates = out
	exp.CandidateCount = len(out)
	exp.PinnedApplied = exp.PinnedDeployment != "" && exp.CandidateCount > 0 &&
		exp.Candidates[0].Deployment.ID == exp.PinnedDeployment
	return exp
}
