package decision

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// DecisionTrace is a small bounded trace for route explanation and observability.
// It contains only safe, bounded fields — no raw prompts, chain-of-thought, or secrets.
type DecisionTrace struct {
	Mode           string        `json:"mode"`
	ProviderID     string        `json:"provider_id"`
	CandidateCount int           `json:"candidate_count"`
	Action         Action        `json:"action"`
	SelectedID     string        `json:"selected_id,omitempty"`
	ReasonCodes    []ReasonCode  `json:"reason_codes,omitempty"`
	Duration       time.Duration `json:"duration"`
	FallbackUsed   bool          `json:"fallback_used"`
	// Input/output IDs may be included only if bounded and useful; omitted for privacy/brevity in Phase D
}

// Orchestrator enforces budget, timeout, panic recovery, fail-open,
// and eligible-set invariant. It is safe for concurrent use and hot-reload.
type Orchestrator struct {
	registry *Registry
	metrics  *Metrics
	mu       sync.RWMutex // protects cfg
	cfg      config.DecisionConfig
}

// NewOrchestrator creates an orchestrator with given registry and config.
func NewOrchestrator(reg *Registry, cfg config.DecisionConfig, metrics *Metrics) *Orchestrator {
	if reg == nil {
		reg = NewRegistry()
	}
	if metrics == nil {
		metrics = &Metrics{} // per-instance, not global, to avoid cross-server leakage
	}
	// Normalize config
	if cfg.Mode == "" {
		cfg.Mode = "off"
	}
	if cfg.Provider == "" {
		cfg.Provider = "local"
	}
	if cfg.TimeoutMS == 0 {
		cfg.TimeoutMS = 10
	}
	return &Orchestrator{
		registry: reg,
		metrics:  metrics,
		cfg:      cfg,
	}
}

// UpdateConfig hot-reloads decision config atomically (coherent snapshot).
// It is safe for concurrent use with Decide.
func (o *Orchestrator) UpdateConfig(cfg config.DecisionConfig) {
	if cfg.Mode == "" {
		cfg.Mode = "off"
	}
	if cfg.Provider == "" {
		cfg.Provider = "local"
	}
	if cfg.TimeoutMS == 0 {
		cfg.TimeoutMS = 10
	}
	o.mu.Lock()
	o.cfg = cfg
	o.mu.Unlock()
}

// Config returns current config snapshot (coherent).
func (o *Orchestrator) Config() config.DecisionConfig {
	o.mu.RLock()
	c := o.cfg
	o.mu.RUnlock()
	return c
}

// MetricsSnapshot returns metrics snapshot.
func (o *Orchestrator) MetricsSnapshot() map[string]int64 {
	if o.metrics == nil {
		return map[string]int64{}
	}
	return o.metrics.Snapshot()
}

// boundedError truncates error strings to prevent unbounded telemetry.
func boundedError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const maxErrLen = 256
	if len(s) > maxErrLen {
		s = s[:maxErrLen]
	}
	return s
}

func boundedPanic(rec any) string {
	s := fmt.Sprintf("%v", rec)
	const maxErrLen = 256
	if len(s) > maxErrLen {
		s = s[:maxErrLen]
	}
	return s
}

// Decide executes the decision plane for given request and eligible set.
// It returns the final ordered candidate list (always same elements as eligible,
// possibly reordered), the DecisionResult, and a DecisionTrace.
//
// Invariants:
// - If mode=off: returns eligible unchanged, zero overhead path.
// - Empty eligible: returns empty, no provider call, reason EMPTY_ELIGIBLE
// - Single candidate: returns single without calling provider, reason SINGLE_CANDIDATE
// - On timeout, error, panic, invalid result, budget exhausted, provider unhealthy: fail-open → eligible unchanged.
// - No candidate outside eligible is ever returned.
// - Omitted candidates are appended in original order to preserve failover.
// - Provider must obey ctx.Done() — contract documented in provider.go
func (o *Orchestrator) Decide(ctx context.Context, req DecisionRequest) (ordered []Candidate, result DecisionResult, trace DecisionTrace) {
	// Snapshot config atomically for coherent request handling
	o.mu.RLock()
	cfg := o.cfg
	o.mu.RUnlock()

	trace = DecisionTrace{
		Mode:           cfg.Mode,
		CandidateCount: len(req.Candidates),
	}

	// Empty eligible handling — must not call provider
	if len(req.Candidates) == 0 {
		ordered = nil
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonEmptyEligible, ReasonExistingOrderPreserved},
			ProviderID:  "none",
		}
		trace.ProviderID = "none"
		trace.Action = ActionAbstain
		trace.ReasonCodes = result.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(result, nil, false, false)
		}
		return ordered, result, trace
	}

	// Single candidate optimization — no provider call
	if len(req.Candidates) == 1 {
		ordered = CloneCandidates(req.Candidates)
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  1.0,
			ReasonCodes: []ReasonCode{ReasonSingleCandidate, ReasonExistingOrderPreserved},
			ProviderID:  "none",
		}
		trace.ProviderID = "none"
		trace.Action = ActionAbstain
		trace.ReasonCodes = result.ReasonCodes
		trace.FallbackUsed = false
		if o.metrics != nil {
			o.metrics.Record(result, nil, false, false)
		}
		return ordered, result, trace
	}

	// OFF zero-overhead path
	if cfg.Mode == "off" {
		ordered = CloneCandidates(req.Candidates)
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  1.0,
			ReasonCodes: []ReasonCode{ReasonOffMode, ReasonExistingOrderPreserved},
			ProviderID:  "off",
		}
		trace.ProviderID = "off"
		trace.Action = ActionAbstain
		trace.ReasonCodes = result.ReasonCodes
		if o.metrics != nil {
			o.metrics.Record(result, nil, false, true)
		}
		return ordered, result, trace
	}

	// Budget: MaxProviderCalls — Phase D default 1
	providerCallsBudget := 1
	if !req.Budget.IsZero() {
		// If budget explicitly set, use its MaxProviderCalls (0 means exhausted)
		providerCallsBudget = req.Budget.MaxProviderCalls
	} else if req.Budget.MaxProviderCalls > 0 {
		providerCallsBudget = req.Budget.MaxProviderCalls
	}
	if providerCallsBudget <= 0 {
		// Budget exhausted before any call
		ordered = CloneCandidates(req.Candidates)
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonBudgetExceeded, ReasonExistingOrderPreserved},
			ProviderID:  cfg.Provider,
		}
		trace.ProviderID = cfg.Provider
		trace.Action = ActionAbstain
		trace.ReasonCodes = result.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(result, nil, false, false)
		}
		return ordered, result, trace
	}

	// Resolve provider
	providerName := cfg.Provider
	if providerName == "" {
		providerName = "local"
	}
	provider, perr := o.registry.Resolve(providerName)
	if perr != nil {
		// Unknown provider → fail-open
		ordered = CloneCandidates(req.Candidates)
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonProviderError, ReasonExistingOrderPreserved},
			ProviderID:  providerName,
			Error:       boundedError(perr),
		}
		trace.ProviderID = providerName
		trace.Action = ActionAbstain
		trace.ReasonCodes = result.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(result, perr, false, false)
		}
		return ordered, result, trace
	}

	// Provider health check before invocation
	ph := provider.Health()
	if ph.Status == HealthUnavailable {
		ordered = CloneCandidates(req.Candidates)
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonProviderUnhealthy, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
		}
		trace.ProviderID = provider.ID()
		trace.Action = ActionAbstain
		trace.ReasonCodes = result.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(result, nil, false, false)
		}
		return ordered, result, trace
	}
	// Degraded: for Phase D, allow local if operational, but if provider is not local and degraded, fail-open? Spec says allow degraded local if operational, or fail open if explicitly unavailable.
	// We allow degraded for local, but for other providers we also allow but could be configured to fail-open in future. For now, allow degraded.

	// Budget / timeout
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	if req.Budget.Timeout > 0 && req.Budget.Timeout < timeout {
		timeout = req.Budget.Timeout
	}
	if timeout <= 0 {
		timeout = 10 * time.Millisecond
	}
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var decideResult DecisionResult
	var decideErr error
	timedOut := false

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				decideErr = fmt.Errorf("decision provider panic: %s", boundedPanic(rec))
				decideResult = DecisionResult{
					Action:      ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []ReasonCode{ReasonProviderPanic, ReasonExistingOrderPreserved},
					ProviderID:  provider.ID(),
					Error:       boundedPanic(rec),
				}
			}
		}()
		decideResult, decideErr = provider.Decide(ctxTimeout, req)
	}()

	latency := time.Since(start)
	decideResult.Latency = latency
	if decideResult.ProviderID == "" {
		decideResult.ProviderID = provider.ID()
	}
	// Bound error string
	if decideResult.Error != "" && len(decideResult.Error) > 256 {
		decideResult.Error = decideResult.Error[:256]
	}

	trace.ProviderID = provider.ID()
	trace.Duration = latency

	// Check timeout — must check context error after Decide returns, because provider may have respected ctx
	if ctxTimeout.Err() == context.DeadlineExceeded {
		timedOut = true
		decideErr = ctxTimeout.Err()
		decideResult = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonTimeout, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
			Latency:     latency,
			Error:       "timeout",
		}
		ordered = CloneCandidates(req.Candidates)
		trace.Action = ActionAbstain
		trace.ReasonCodes = decideResult.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(decideResult, decideErr, timedOut, false)
		}
		return ordered, decideResult, trace
	}

	if decideErr != nil {
		ordered = CloneCandidates(req.Candidates)
		hasErrReason := false
		for _, rc := range decideResult.ReasonCodes {
			if rc == ReasonProviderError {
				hasErrReason = true
				break
			}
		}
		if !hasErrReason {
			decideResult.ReasonCodes = append(decideResult.ReasonCodes, ReasonProviderError, ReasonExistingOrderPreserved)
		}
		decideResult.Abstained = true
		if !decideResult.ValidAction() {
			decideResult.Action = ActionAbstain
		}
		decideResult.Error = boundedError(decideErr)
		trace.Action = decideResult.Action
		trace.ReasonCodes = decideResult.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(decideResult, decideErr, timedOut, false)
		}
		return ordered, decideResult, trace
	}

	// Capabilities enforcement
	caps := provider.Capabilities()
	if decideResult.Action == ActionRank && !caps.CanRank {
		ordered = CloneCandidates(req.Candidates)
		decideResult = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonInvalidResult, ReasonValidationFailed, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
			Latency:     latency,
			Error:       "provider cannot rank",
		}
		trace.Action = ActionAbstain
		trace.ReasonCodes = decideResult.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(decideResult, fmt.Errorf("capability mismatch"), false, false)
		}
		return ordered, decideResult, trace
	}
	if decideResult.Action == ActionSelect && !caps.CanSelect {
		ordered = CloneCandidates(req.Candidates)
		decideResult = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonInvalidResult, ReasonValidationFailed, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
			Latency:     latency,
			Error:       "provider cannot select",
		}
		trace.Action = ActionAbstain
		trace.ReasonCodes = decideResult.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(decideResult, fmt.Errorf("capability mismatch"), false, false)
		}
		return ordered, decideResult, trace
	}

	// Validate result against eligible set (strict)
	if verr := ValidateResult(req.Candidates, decideResult); verr != nil {
		ordered = CloneCandidates(req.Candidates)
		decideResult = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonInvalidResult, ReasonValidationFailed, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
			Latency:     latency,
			Error:       boundedError(verr),
		}
		trace.Action = ActionAbstain
		trace.ReasonCodes = decideResult.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(decideResult, verr, false, false)
		}
		return ordered, decideResult, trace
	}

	// Normalize
	normalized, applied, normReason := NormalizeResult(req.Candidates, decideResult)
	// Ensure norm reason is in codes if not already
	if normReason != "" {
		found := false
		for _, rc := range decideResult.ReasonCodes {
			if rc == normReason {
				found = true
				break
			}
		}
		if !found {
			decideResult.ReasonCodes = append(decideResult.ReasonCodes, normReason)
		}
	}

	trace.Action = decideResult.Action
	trace.ReasonCodes = decideResult.ReasonCodes
	trace.FallbackUsed = !applied || normReason == ReasonNormalizationApplied || normReason == ReasonAbstained
	if decideResult.Action == ActionSelect {
		trace.SelectedID = decideResult.SelectedID
	}

	if o.metrics != nil {
		o.metrics.Record(decideResult, nil, false, false)
	}
	return normalized, decideResult, trace
}
