package decision

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision/providerstate"
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
	// Phase E: optional policy trace
	PolicyTrace *PolicyTrace `json:"policy_trace,omitempty"`
	// Phase G: optional chain trace
	ChainTrace *ChainTrace `json:"chain_trace,omitempty"`
	// Input/output IDs may be included only if bounded and useful; omitted for privacy/brevity in Phase D
}

// Orchestrator enforces budget, timeout, panic recovery, fail-open,
// and eligible-set invariant. It is safe for concurrent use and hot-reload.
type Orchestrator struct {
	registry      *Registry
	metrics       *Metrics
	mu            sync.RWMutex // protects cfg, chains, healthCfg
	cfg           config.DecisionConfig
	chains        map[string]config.DecisionChainConfig
	healthCfg     config.DecisionProviderHealthConfig
	providerState *providerstate.Manager
	chainExec     *ChainExecutor
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
	// Default health config
	healthCfg := config.DecisionProviderHealthConfig{
		FailureThreshold:     3,
		FailureWindowSeconds: 30,
		CooldownSeconds:      60,
	}
	ps := providerstate.New(providerstate.Config{
		FailureThreshold: healthCfg.FailureThreshold,
		FailureWindow:    time.Duration(healthCfg.FailureWindowSeconds) * time.Second,
		Cooldown:         time.Duration(healthCfg.CooldownSeconds) * time.Second,
	}, nil)
	chainExec := NewChainExecutor(reg, ps, metrics)
	return &Orchestrator{
		registry:      reg,
		metrics:       metrics,
		cfg:           cfg,
		chains:        make(map[string]config.DecisionChainConfig),
		healthCfg:     healthCfg,
		providerState: ps,
		chainExec:     chainExec,
	}
}

// NewOrchestratorWithConfig creates an orchestrator with full config (chains + health) — preferred for Phase G server wiring.
func NewOrchestratorWithConfig(reg *Registry, fullCfg config.Config, metrics *Metrics) *Orchestrator {
	if reg == nil {
		reg = NewRegistry()
	}
	if metrics == nil {
		metrics = &Metrics{}
	}
	decCfg := fullCfg.Decision
	if decCfg.Mode == "" {
		decCfg.Mode = "off"
	}
	if decCfg.Provider == "" {
		decCfg.Provider = "local"
	}
	if decCfg.TimeoutMS == 0 {
		decCfg.TimeoutMS = 10
	}
	healthCfg := fullCfg.DecisionProviderHealth
	if healthCfg.FailureThreshold == 0 {
		healthCfg.FailureThreshold = 3
	}
	if healthCfg.FailureWindowSeconds == 0 {
		healthCfg.FailureWindowSeconds = 30
	}
	if healthCfg.CooldownSeconds == 0 {
		healthCfg.CooldownSeconds = 60
	}
	chainsMap := make(map[string]config.DecisionChainConfig, len(fullCfg.DecisionChains))
	for _, ch := range fullCfg.DecisionChains {
		chainsMap[ch.ID] = ch
	}
	ps := providerstate.New(providerstate.Config{
		FailureThreshold: healthCfg.FailureThreshold,
		FailureWindow:    time.Duration(healthCfg.FailureWindowSeconds) * time.Second,
		Cooldown:         time.Duration(healthCfg.CooldownSeconds) * time.Second,
	}, nil)
	chainExec := NewChainExecutor(reg, ps, metrics)
	return &Orchestrator{
		registry:      reg,
		metrics:       metrics,
		cfg:           decCfg,
		chains:        chainsMap,
		healthCfg:     healthCfg,
		providerState: ps,
		chainExec:     chainExec,
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

// UpdateFullConfig hot-reloads decision config plus chains and provider health atomically.
func (o *Orchestrator) UpdateFullConfig(fullCfg config.Config) {
	decCfg := fullCfg.Decision
	if decCfg.Mode == "" {
		decCfg.Mode = "off"
	}
	if decCfg.Provider == "" {
		decCfg.Provider = "local"
	}
	if decCfg.TimeoutMS == 0 {
		decCfg.TimeoutMS = 10
	}
	healthCfg := fullCfg.DecisionProviderHealth
	if healthCfg.FailureThreshold == 0 {
		healthCfg.FailureThreshold = 3
	}
	if healthCfg.FailureWindowSeconds == 0 {
		healthCfg.FailureWindowSeconds = 30
	}
	if healthCfg.CooldownSeconds == 0 {
		healthCfg.CooldownSeconds = 60
	}
	chainsMap := make(map[string]config.DecisionChainConfig, len(fullCfg.DecisionChains))
	for _, ch := range fullCfg.DecisionChains {
		chainsMap[ch.ID] = ch
	}
	o.mu.Lock()
	o.cfg = decCfg
	o.chains = chainsMap
	o.healthCfg = healthCfg
	// Update provider state manager config
	if o.providerState != nil {
		o.providerState.UpdateConfig(providerstate.Config{
			FailureThreshold: healthCfg.FailureThreshold,
			FailureWindow:    time.Duration(healthCfg.FailureWindowSeconds) * time.Second,
			Cooldown:         time.Duration(healthCfg.CooldownSeconds) * time.Second,
		})
	}
	o.mu.Unlock()
}

// Config returns current config snapshot (coherent).
func (o *Orchestrator) Config() config.DecisionConfig {
	o.mu.RLock()
	c := o.cfg
	o.mu.RUnlock()
	return c
}

// Chains returns copy of chain map snapshot
func (o *Orchestrator) Chains() map[string]config.DecisionChainConfig {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]config.DecisionChainConfig, len(o.chains))
	for k, v := range o.chains {
		// deep copy steps
		steps := make([]config.DecisionChainStep, len(v.Steps))
		copy(steps, v.Steps)
		v.Steps = steps
		out[k] = v
	}
	return out
}

// ProviderState returns the manager (for admin snapshot and tests)
func (o *Orchestrator) ProviderState() *providerstate.Manager {
	return o.providerState
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
	chainsSnapshot := o.chains
	// copy chain map for request-local immutability
	chainsCopy := make(map[string]config.DecisionChainConfig, len(chainsSnapshot))
	for k, v := range chainsSnapshot {
		steps := make([]config.DecisionChainStep, len(v.Steps))
		copy(steps, v.Steps)
		v.Steps = steps
		chainsCopy[k] = v
	}
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

	// Compute primary selection constraints (Phase F generalization) — once per request
	primaryConstraints := ComputePrimaryConstraints(req.Candidates, req.PinnedCandidateID)

	// Phase G hybrid mode: ordered chain execution
	if cfg.Mode == "hybrid" {
		chainID := cfg.Chain
		chCfg, ok := chainsCopy[chainID]
		if !ok || chainID == "" {
			// Chain not found → fail-open, no provider calls
			ordered = CloneCandidates(req.Candidates)
			result = DecisionResult{
				Action:      ActionAbstain,
				Abstained:   true,
				Confidence:  0,
				ReasonCodes: []ReasonCode{ReasonProviderError, ReasonChainExhausted, ReasonExistingOrderPreserved},
				ProviderID:  chainID,
				Error:       "chain not found",
			}
			trace.ProviderID = chainID
			trace.Action = ActionAbstain
			trace.ReasonCodes = result.ReasonCodes
			trace.FallbackUsed = true
			// Chain trace for observability
			ct := &ChainTrace{
				ChainID:   chainID,
				StepCount: 0,
				CallsUsed: 0,
				Outcome:   ChainOutcomeExhausted,
			}
			trace.ChainTrace = ct
			trace.Duration = 0
			if o.metrics != nil {
				o.metrics.Record(result, fmt.Errorf("chain not found"), false, false)
				o.metrics.RecordChainOutcome("exhausted")
			}
			return ordered, result, trace
		}
		// Build chain config for executor (immutable snapshot)
		chainForExec := ChainConfig{
			ID: chCfg.ID,
		}
		for _, step := range chCfg.Steps {
			chainForExec.Steps = append(chainForExec.Steps, ChainStepConfig{
				Provider:  step.Provider,
				TimeoutMS: step.TimeoutMS,
			})
		}
		// Budget for chain: global chain budget
		budget := req.Budget
		if budget.IsZero() {
			// Derive from cfg
			maxCalls := cfg.MaxProviderCalls
			if maxCalls == 0 {
				maxCalls = len(chainForExec.Steps)
			}
			budget = Budget{
				Timeout:          time.Duration(cfg.TimeoutMS) * time.Millisecond,
				MaxProviderCalls: maxCalls,
			}
		} else {
			// If budget already set, ensure timeout is min(cfg timeout, budget timeout) will be handled inside executor
			if cfg.MaxProviderCalls > 0 && budget.MaxProviderCalls == 0 {
				budget.MaxProviderCalls = cfg.MaxProviderCalls
			}
			if budget.MaxProviderCalls == 0 {
				budget.MaxProviderCalls = len(chainForExec.Steps)
			}
		}

		chainStart := time.Now()
		orderedChain, chainResult, chainTrace := o.chainExec.Execute(ctx, chainForExec, req, primaryConstraints, budget, time.Duration(cfg.TimeoutMS)*time.Millisecond)
		latency := time.Since(chainStart)
		chainResult.Latency = latency
		trace.ProviderID = chainTrace.SelectedProviderID
		if trace.ProviderID == "" {
			// For exhausted/budget etc, set to chain id
			trace.ProviderID = chainID
		}
		trace.Action = chainResult.Action
		trace.SelectedID = chainResult.SelectedID
		trace.ReasonCodes = chainResult.ReasonCodes
		trace.Duration = latency
		trace.ChainTrace = &chainTrace
		// Fallback used true for exhausted cases
		switch chainTrace.Outcome {
		case ChainOutcomeSelected:
			trace.FallbackUsed = false
		case ChainOutcomeAffinityPreserved:
			trace.FallbackUsed = false
		default:
			trace.FallbackUsed = true
		}
		// Copy policy trace if present from result
		if chainResult.PolicyTrace != nil {
			trace.PolicyTrace = chainResult.PolicyTrace
		}
		// Record generic metric
		if o.metrics != nil {
			// TimedOut detection is included in chainResult reason codes
			timedOut := false
			for _, rc := range chainResult.ReasonCodes {
				if rc == ReasonTimeout || rc == ReasonChainDeadlineExhausted {
					timedOut = true
					break
				}
			}
			o.metrics.Record(chainResult, nil, timedOut, false)
		}
		return orderedChain, chainResult, trace
	}

	// Non-hybrid: single provider path (off/local/assisted)
	// Budget: MaxProviderCalls — Phase D default 1, but for non-hybrid it's always 1 unless overridden via req.Budget
	providerCallsBudget := 1
	if !req.Budget.IsZero() {
		providerCallsBudget = req.Budget.MaxProviderCalls
	} else if cfg.MaxProviderCalls > 0 {
		providerCallsBudget = cfg.MaxProviderCalls
	}
	if providerCallsBudget <= 0 {
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

	// Phase G: generalize affinity short-circuit to entire chain / all providers
	// If authoritative eligible pin exists, no provider should be called
	if primaryConstraints.ForcedPrimaryID != "" {
		forcedID := primaryConstraints.ForcedPrimaryID
		forcedCandidate := Candidate{}
		found := false
		for _, c := range req.Candidates {
			if c.ID == forcedID {
				forcedCandidate = c
				found = true
				break
			}
		}
		if found {
			ordered = make([]Candidate, 0, len(req.Candidates))
			ordered = append(ordered, forcedCandidate)
			for _, c := range req.Candidates {
				if c.ID != forcedID {
					ordered = append(ordered, c)
				}
			}
			result = DecisionResult{
				Action:      ActionSelect,
				SelectedID:  forcedID,
				Confidence:  1.0,
				ReasonCodes: []ReasonCode{ReasonAffinityPreserved, ReasonEligibleSetPreserved},
				ProviderID:  provider.ID(),
			}
			trace.ProviderID = provider.ID()
			trace.Action = ActionSelect
			trace.SelectedID = forcedID
			trace.ReasonCodes = result.ReasonCodes
			trace.FallbackUsed = false
			trace.ChainTrace = &ChainTrace{
				ChainID:   "",
				StepCount: 0,
				CallsUsed: 0,
				Outcome:   ChainOutcomeAffinityPreserved,
			}
			if o.metrics != nil {
				o.metrics.Record(result, nil, false, false)
				o.metrics.RecordChainOutcome("affinity_preserved")
			}
			return ordered, result, trace
		}
	}

	// Provider health check before invocation (also check providerstate cooldown for external single provider?)
	// For single provider mode, also respect cooldown if provider is external and in cooldown
	if provider.ID() != "local" && provider.ID() != "policy" && o.providerState != nil && o.providerState.IsCooldown(provider.ID()) {
		ordered = CloneCandidates(req.Candidates)
		result = DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonDecisionProviderCooldown, ReasonExistingOrderPreserved},
			ProviderID:  provider.ID(),
		}
		trace.ProviderID = provider.ID()
		trace.Action = ActionAbstain
		trace.ReasonCodes = result.ReasonCodes
		trace.FallbackUsed = true
		if o.metrics != nil {
			o.metrics.Record(result, nil, false, false)
			o.metrics.RecordChainStep(providerType(provider.ID()), "cooldown")
			o.metrics.RecordChainOutcome("exhausted")
		}
		return ordered, result, trace
	}

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
				if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
					o.providerState.RecordFailure(provider.ID())
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
	if decideResult.Error != "" && len(decideResult.Error) > 256 {
		decideResult.Error = decideResult.Error[:256]
	}

	trace.ProviderID = provider.ID()
	trace.Duration = latency

	// Check timeout
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
		if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
			o.providerState.RecordFailure(provider.ID())
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
		if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
			o.providerState.RecordFailure(provider.ID())
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
		if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
			o.providerState.RecordFailure(provider.ID())
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
		if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
			o.providerState.RecordFailure(provider.ID())
		}
		return ordered, decideResult, trace
	}

	// Validate result
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
		if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
			o.providerState.RecordFailure(provider.ID())
		}
		return ordered, decideResult, trace
	}

	if decideResult.Action == ActionSelect {
		if !primaryConstraints.IsAllowedPrimary(decideResult.SelectedID) {
			ordered = CloneCandidates(req.Candidates)
			decideResult = DecisionResult{
				Action:      ActionAbstain,
				Abstained:   true,
				Confidence:  0,
				ReasonCodes: []ReasonCode{ReasonPrimaryConstraintViolation, ReasonExistingOrderPreserved},
				ProviderID:  provider.ID(),
				Latency:     latency,
				Error:       "primary constraint violation",
			}
			trace.Action = ActionAbstain
			trace.ReasonCodes = decideResult.ReasonCodes
			trace.FallbackUsed = true
			if o.metrics != nil {
				o.metrics.Record(decideResult, fmt.Errorf("primary constraint violation"), false, false)
			}
			if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
				o.providerState.RecordFailure(provider.ID())
			}
			return ordered, decideResult, trace
		}
	}

	// Record success for healthy SELECT/ABSTAIN before normalize (ABSTAIN is healthy)
	if o.providerState != nil && provider.ID() != "local" && provider.ID() != "policy" {
		// ABSTAIN and SELECT valid are successes (not failures)
		o.providerState.RecordSuccess(provider.ID())
	}

	// Normalize
	normalized, applied, normReason := NormalizeResult(req.Candidates, decideResult)
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
	if decideResult.PolicyTrace != nil {
		trace.PolicyTrace = decideResult.PolicyTrace
	}

	if o.metrics != nil {
		o.metrics.Record(decideResult, nil, false, false)
	}
	return normalized, decideResult, trace
}
