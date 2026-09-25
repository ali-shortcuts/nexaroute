package decision

import (
	"context"
	"fmt"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Orchestrator enforces budget, timeout, panic recovery, fail-open,
// and eligible-set invariant.
type Orchestrator struct {
	registry *Registry
	metrics  *Metrics
	// config snapshot for hot-reload coherence
	cfg config.DecisionConfig
}

// NewOrchestrator creates an orchestrator with given registry and config.
func NewOrchestrator(reg *Registry, cfg config.DecisionConfig, metrics *Metrics) *Orchestrator {
	if reg == nil {
		reg = NewRegistry()
	}
	if metrics == nil {
		metrics = GlobalMetrics
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
	o.cfg = cfg
}

// Config returns current config snapshot.
func (o *Orchestrator) Config() config.DecisionConfig {
	return o.cfg
}

// MetricsSnapshot returns metrics snapshot.
func (o *Orchestrator) MetricsSnapshot() map[string]int64 {
	if o.metrics == nil {
		return GlobalMetrics.Snapshot()
	}
	return o.metrics.Snapshot()
}

// Decide executes the decision plane for given request and eligible set.
// It returns the final ordered candidate list (always same elements as eligible,
// possibly reordered) and the DecisionResult.
//
// Invariants:
// - If mode=off: returns eligible unchanged, zero overhead path.
// - On timeout, error, panic, invalid result: fail-open → eligible unchanged.
// - No candidate outside eligible is ever returned.
// - Omitted candidates are appended in original order to preserve failover.
func (o *Orchestrator) Decide(ctx context.Context, req DecisionRequest) (ordered []Candidate, result DecisionResult, err error) {
	// OFF zero-overhead path
	if o.cfg.Mode == "off" {
		ordered = CloneCandidates(req.Candidates)
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  1.0,
			ReasonCodes: []string{ReasonOffMode, ReasonExistingOrderPreserved},
			ProviderID:  "off",
		}
		if o.metrics != nil {
			o.metrics.Record(result, nil, false, true)
		}
		return ordered, result, nil
	}

	// Resolve provider
	providerName := o.cfg.Provider
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
			ReasonCodes: []string{ReasonProviderError, ReasonExistingOrderPreserved},
			ProviderID:  providerName,
			Error:       perr.Error(),
		}
		if o.metrics != nil {
			o.metrics.Record(result, perr, false, false)
		}
		return ordered, result, nil
	}

	// Budget / timeout
	timeout := time.Duration(o.cfg.TimeoutMS) * time.Millisecond
	if req.Budget.Timeout > 0 && req.Budget.Timeout < timeout {
		timeout = req.Budget.Timeout
	}
	if timeout <= 0 {
		timeout = 10 * time.Millisecond
	}
	// Enforce max timeout bound (5s from config validation, but also guard here)
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}

	// Context with timeout, preserving parent cancellation
	ctxTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Panic recovery + timing
	start := time.Now()
	var decideResult DecisionResult
	var decideErr error
	timedOut := false

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				decideErr = fmt.Errorf("decision provider panic: %v", rec)
				decideResult = DecisionResult{
					Action:      ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []string{ReasonProviderPanic, ReasonExistingOrderPreserved},
					ProviderID:  provider.ID(),
					Error:       fmt.Sprintf("%v", rec),
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

	// Check timeout
	if ctxTimeout.Err() == context.DeadlineExceeded {
		timedOut = true
		decideErr = ctxTimeout.Err()
		decideResult = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []string{ReasonTimeout, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
			Latency:     latency,
			Error:       "timeout",
		}
		ordered = CloneCandidates(req.Candidates)
		if o.metrics != nil {
			o.metrics.Record(decideResult, decideErr, timedOut, false)
		}
		return ordered, decideResult, nil
	}

	if decideErr != nil {
		// Provider error → fail-open
		ordered = CloneCandidates(req.Candidates)
		// Ensure reason codes indicate error
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
		if decideResult.Action == "" {
			decideResult.Action = ActionAbstain
		}
		if o.metrics != nil {
			o.metrics.Record(decideResult, decideErr, timedOut, false)
		}
		return ordered, decideResult, nil
	}

	// Validate result against eligible set
	if verr := ValidateResult(req.Candidates, decideResult); verr != nil {
		ordered = CloneCandidates(req.Candidates)
		decideResult = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []string{ReasonInvalidResult, ReasonValidationFailed, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
			Latency:     latency,
			Error:       verr.Error(),
		}
		if o.metrics != nil {
			o.metrics.Record(decideResult, verr, false, false)
		}
		return ordered, decideResult, nil
	}

	// Normalize (preserve failover coverage)
	normalized, applied, normReason := NormalizeResult(req.Candidates, decideResult)
	_ = applied
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

	if o.metrics != nil {
		o.metrics.Record(decideResult, nil, false, false)
	}
	return normalized, decideResult, nil
}
