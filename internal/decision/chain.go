package decision

import (
	"context"
	"fmt"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision/providerstate"
)

// Chain outcome canonical strings.
const (
	ChainOutcomeSelected          = "SELECTED"
	ChainOutcomeExhausted         = "EXHAUSTED"
	ChainOutcomeBudgetExhausted   = "BUDGET_EXHAUSTED"
	ChainOutcomeDeadlineExhausted = "DEADLINE_EXHAUSTED"
	ChainOutcomeAffinityPreserved = "AFFINITY_PRESERVED"
)

// Step outcome canonical strings.
const (
	StepOutcomeSelected      = "SELECTED"
	StepOutcomeAbstained     = "ABSTAINED"
	StepOutcomeError         = "ERROR"
	StepOutcomeTimeout       = "TIMEOUT"
	StepOutcomeInvalid       = "INVALID"
	StepOutcomeUnavailable   = "UNAVAILABLE"
	StepOutcomeCooldown      = "COOLDOWN"
	StepOutcomeSkippedBudget = "SKIPPED_BUDGET"
)

// ChainStepTrace is a bounded structured step record.
type ChainStepTrace struct {
	Index        int           `json:"index"`
	ProviderID   string        `json:"provider_id"`
	ProviderType string        `json:"provider_type"` // jev | policy | local
	Outcome      string        `json:"outcome"`
	Action       Action        `json:"action,omitempty"`
	Called       bool          `json:"called"`
	Duration     time.Duration `json:"duration"`
	ReasonCodes  []ReasonCode  `json:"reason_codes,omitempty"`
}

// ChainTrace extends DecisionTrace with optional chain execution details.
type ChainTrace struct {
	ChainID            string           `json:"chain_id"`
	StepCount          int              `json:"step_count"`
	CallsUsed          int              `json:"calls_used"`
	SelectedProviderID string           `json:"selected_provider_id,omitempty"`
	Outcome            string           `json:"outcome"`
	Steps              []ChainStepTrace `json:"steps,omitempty"`
}

// ChainExecutor orchestrates ordered DecisionProvider chain execution.
type ChainExecutor struct {
	registry *Registry
	state    *providerstate.Manager
	metrics  *Metrics
}

// NewChainExecutor creates executor. registry and state may be nil (tests).
func NewChainExecutor(reg *Registry, state *providerstate.Manager, metrics *Metrics) *ChainExecutor {
	if reg == nil {
		reg = NewRegistry()
	}
	return &ChainExecutor{
		registry: reg,
		state:    state,
		metrics:  metrics,
	}
}

// providerType maps provider ID to bounded type string for metrics/trace.
func providerType(id string) string {
	switch id {
	case "local":
		return "local"
	case "policy":
		return "policy"
	default:
		// For Phase G, all externals are jev; fallback to jev for bounded label.
		return "jev"
	}
}

// ChainConfig is a request-local immutable snapshot of chain definition.
type ChainConfig struct {
	ID    string
	Steps []ChainStepConfig
}

// ChainStepConfig is a request-local step snapshot.
type ChainStepConfig struct {
	Provider  string
	TimeoutMS int
}

// Execute runs chain with global timeout and call budget.
// Constraints are computed once before calling and applied to every step.
// ctx is parent request context; executor derives chain deadline from budget.Timeout or cfgTimeout.
// Returns ordered candidates (same elements as input, possibly reordered), final DecisionResult, ChainTrace.
// It never modifies eligible set; always returns same elements.
func (e *ChainExecutor) Execute(
	ctx context.Context,
	chain ChainConfig,
	req DecisionRequest,
	constraints PrimarySelectionConstraints,
	budget Budget,
	cfgTimeout time.Duration,
) (ordered []Candidate, result DecisionResult, trace ChainTrace) {

	trace = ChainTrace{
		ChainID:   chain.ID,
		StepCount: len(chain.Steps),
		Outcome:   ChainOutcomeExhausted,
		Steps:     make([]ChainStepTrace, 0, len(chain.Steps)),
	}

	// Affinity short-circuit: chain-level invariant, zero calls for any provider
	if constraints.ForcedPrimaryID != "" {
		forcedID := constraints.ForcedPrimaryID
		// Build ordered list: forced first + rest in original order
		found := false
		var forcedCandidate Candidate
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
				ProviderID:  chain.ID, // chain-level provider? Use empty? We'll set to "chain"
			}
			trace.Outcome = ChainOutcomeAffinityPreserved
			trace.CallsUsed = 0
			// No steps recorded as called; but we should emit one pseudo step for affinity? Spec says 0 provider calls, outcome AFFINITY_PRESERVED
			if e.metrics != nil {
				e.metrics.RecordChainOutcome("affinity_preserved")
			}
			return ordered, result, trace
		}
		// If forced not found (should not happen), fall through to normal execution
	}

	// Global deadline: decision.timeout_ms becomes whole chain deadline
	// cfgTimeout is decision timeout; budget.Timeout may be smaller (or zero means use cfgTimeout)
	timeout := cfgTimeout
	if budget.Timeout > 0 && budget.Timeout < timeout {
		timeout = budget.Timeout
	}
	if timeout <= 0 {
		timeout = 10 * time.Millisecond
	}
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	// Max provider calls budget
	maxCalls := budget.MaxProviderCalls
	if maxCalls <= 0 {
		maxCalls = len(chain.Steps)
		if maxCalls == 0 {
			maxCalls = 1
		}
		if maxCalls > 8 {
			maxCalls = 8
		}
	}

	// Parent chain context with global deadline
	chainCtx, chainCancel := context.WithTimeout(ctx, timeout)
	defer chainCancel()

	callsUsed := 0
	chainStart := time.Now()
	_ = chainStart

	// Loop steps
	for idx, step := range chain.Steps {
		stepTrace := ChainStepTrace{
			Index:        idx,
			ProviderID:   step.Provider,
			ProviderType: providerType(step.Provider),
			Called:       false,
		}

		// Check global deadline before step
		select {
		case <-chainCtx.Done():
			// Deadline exhausted before next step
			stepTrace.Outcome = StepOutcomeTimeout
			stepTrace.ReasonCodes = []ReasonCode{ReasonTimeout, ReasonChainDeadlineExhausted}
			trace.Steps = append(trace.Steps, stepTrace)
			trace.Outcome = ChainOutcomeDeadlineExhausted
			trace.CallsUsed = callsUsed
			if e.metrics != nil {
				e.metrics.RecordChainOutcome("deadline_exhausted")
			}
			ordered = CloneCandidates(req.Candidates)
			result = DecisionResult{
				Action:      ActionAbstain,
				Abstained:   true,
				Confidence:  0,
				ReasonCodes: []ReasonCode{ReasonTimeout, ReasonChainDeadlineExhausted, ReasonExistingOrderPreserved},
				ProviderID:  step.Provider,
			}
			return ordered, result, trace
		default:
		}

		// Budget check: skipped steps do NOT count, but actual Decide calls do
		if callsUsed >= maxCalls {
			stepTrace.Outcome = StepOutcomeSkippedBudget
			stepTrace.ReasonCodes = []ReasonCode{ReasonBudgetExceeded, ReasonChainBudgetExhausted}
			trace.Steps = append(trace.Steps, stepTrace)
			// Continue to mark remaining steps as skipped_budget as well for trace completeness, but we can break after first budget exhaustion?
			// Spec 85: BUDGET_EXHAUSTED stops further calls. So mark remaining as skipped as well.
			// For remaining steps, add skipped traces without calling.
			for j := idx + 1; j < len(chain.Steps); j++ {
				remaining := ChainStepTrace{
					Index:        j,
					ProviderID:   chain.Steps[j].Provider,
					ProviderType: providerType(chain.Steps[j].Provider),
					Outcome:      StepOutcomeSkippedBudget,
					ReasonCodes:  []ReasonCode{ReasonBudgetExceeded, ReasonChainBudgetExhausted},
					Called:       false,
				}
				trace.Steps = append(trace.Steps, remaining)
				if e.metrics != nil {
					e.metrics.RecordChainStep(remaining.ProviderType, "skipped_budget")
				}
			}
			trace.Outcome = ChainOutcomeBudgetExhausted
			trace.CallsUsed = callsUsed
			if e.metrics != nil {
				e.metrics.RecordChainOutcome("budget_exhausted")
				e.metrics.RecordChainStep(stepTrace.ProviderType, "skipped_budget")
			}
			ordered = CloneCandidates(req.Candidates)
			result = DecisionResult{
				Action:      ActionAbstain,
				Abstained:   true,
				Confidence:  0,
				ReasonCodes: []ReasonCode{ReasonBudgetExceeded, ReasonChainBudgetExhausted, ReasonExistingOrderPreserved},
				ProviderID:  step.Provider,
			}
			return ordered, result, trace
		}

		// Check provider existence
		provider, ok := e.registry.Get(step.Provider)
		if !ok {
			stepTrace.Outcome = StepOutcomeUnavailable
			stepTrace.ReasonCodes = []ReasonCode{ReasonProviderUnhealthy, ReasonExternalProviderUnavailable}
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "unavailable")
			}
			continue
		}

		// Check provider health unavailable (config disabled/missing key) — do not count as failure, skip
		ph := provider.Health()
		if ph.Status == HealthUnavailable {
			stepTrace.Outcome = StepOutcomeUnavailable
			stepTrace.ReasonCodes = []ReasonCode{ReasonProviderUnhealthy, ReasonExternalProviderUnavailable}
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "unavailable")
			}
			continue
		}

		// Check cooldown via providerstate manager (only for external providers)
		// Local/policy bypass cooldown per spec 30/31
		isExternal := step.Provider != "local" && step.Provider != "policy"
		if isExternal && e.state != nil && e.state.IsCooldown(step.Provider) {
			stepTrace.Outcome = StepOutcomeCooldown
			stepTrace.ReasonCodes = []ReasonCode{ReasonDecisionProviderCooldown, ReasonExternalProviderUnavailable}
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "cooldown")
			}
			continue
		}

		// Prepare per-step context: min(remaining global deadline, step timeout)
		stepCtx := chainCtx
		var stepCancel context.CancelFunc
		if step.TimeoutMS > 0 {
			stepTimeout := time.Duration(step.TimeoutMS) * time.Millisecond
			// Compute remaining deadline
			if deadline, hasDeadline := chainCtx.Deadline(); hasDeadline {
				remaining := time.Until(deadline)
				if stepTimeout < remaining {
					stepCtx, stepCancel = context.WithTimeout(chainCtx, stepTimeout)
					defer stepCancel()
				}
			} else {
				stepCtx, stepCancel = context.WithTimeout(chainCtx, stepTimeout)
				defer stepCancel()
			}
		}

		// Invoke provider.Decide
		stepTrace.Called = true
		callsUsed++
		start := time.Now()
		var decideResult DecisionResult
		var decideErr error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					decideErr = fmt.Errorf("decision provider panic: %v", rec)
					decideResult = DecisionResult{
						Action:      ActionAbstain,
						Abstained:   true,
						Confidence:  0,
						ReasonCodes: []ReasonCode{ReasonProviderPanic, ReasonExistingOrderPreserved},
						ProviderID:  provider.ID(),
						Error:       fmt.Sprintf("%v", rec),
					}
				}
			}()
			decideResult, decideErr = provider.Decide(stepCtx, req)
		}()
		duration := time.Since(start)
		stepTrace.Duration = duration
		decideResult.Latency = duration
		if decideResult.ProviderID == "" {
			decideResult.ProviderID = provider.ID()
		}
		// Bound error string not needed for trace but for state decision

		// Check timeout: step context deadline exceeded
		if chainCtx.Err() == context.DeadlineExceeded || stepCtx.Err() == context.DeadlineExceeded {
			stepTrace.Outcome = StepOutcomeTimeout
			stepTrace.ReasonCodes = []ReasonCode{ReasonTimeout, ReasonExternalTimeout, ReasonChainStepFailed}
			stepTrace.Action = ActionAbstain
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "timeout")
			}
			// Record failure for external providers (timeout counts as failure)
			if isExternal && e.state != nil {
				e.state.RecordFailure(provider.ID())
			}
			// Continue if budget and deadline remain
			// Check if global deadline still has remaining before next iteration
			select {
			case <-chainCtx.Done():
				trace.Outcome = ChainOutcomeDeadlineExhausted
				trace.CallsUsed = callsUsed
				if e.metrics != nil {
					e.metrics.RecordChainOutcome("deadline_exhausted")
				}
				ordered = CloneCandidates(req.Candidates)
				result = DecisionResult{
					Action:      ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []ReasonCode{ReasonTimeout, ReasonChainDeadlineExhausted, ReasonExistingOrderPreserved},
					ProviderID:  provider.ID(),
				}
				return ordered, result, trace
			default:
			}
			continue
		}

		if decideErr != nil {
			stepTrace.Outcome = StepOutcomeError
			stepTrace.Action = decideResult.Action
			if !decideResult.ValidAction() {
				stepTrace.Action = ActionAbstain
			}
			stepTrace.ReasonCodes = []ReasonCode{ReasonProviderError, ReasonChainStepFailed}
			// Preserve some code from result if present
			if len(decideResult.ReasonCodes) > 0 {
				// Keep first canonical if available, but don't leak arbitrary error strings
				// Use generic failed
			}
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "error")
			}
			if isExternal && e.state != nil {
				e.state.RecordFailure(provider.ID())
			}
			continue
		}

		// Capabilities check done inside orchestrator single path; for chain we also enforce
		caps := provider.Capabilities()
		if decideResult.Action == ActionRank && !caps.CanRank {
			stepTrace.Outcome = StepOutcomeInvalid
			stepTrace.ReasonCodes = []ReasonCode{ReasonInvalidResult, ReasonValidationFailed, ReasonChainStepFailed}
			stepTrace.Action = decideResult.Action
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "invalid")
			}
			if isExternal && e.state != nil {
				e.state.RecordFailure(provider.ID())
			}
			continue
		}
		if decideResult.Action == ActionSelect && !caps.CanSelect {
			stepTrace.Outcome = StepOutcomeInvalid
			stepTrace.ReasonCodes = []ReasonCode{ReasonInvalidResult, ReasonValidationFailed, ReasonChainStepFailed}
			stepTrace.Action = decideResult.Action
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "invalid")
			}
			if isExternal && e.state != nil {
				e.state.RecordFailure(provider.ID())
			}
			continue
		}

		// Validate result against eligible set (strict)
		if verr := ValidateResult(req.Candidates, decideResult); verr != nil {
			stepTrace.Outcome = StepOutcomeInvalid
			stepTrace.ReasonCodes = []ReasonCode{ReasonInvalidResult, ReasonValidationFailed, ReasonChainStepFailed}
			stepTrace.Action = decideResult.Action
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "invalid")
			}
			if isExternal && e.state != nil {
				e.state.RecordFailure(provider.ID())
			}
			continue
		}

		// Primary band validation for SELECT
		if decideResult.Action == ActionSelect {
			if !constraints.IsAllowedPrimary(decideResult.SelectedID) {
				stepTrace.Outcome = StepOutcomeInvalid
				stepTrace.ReasonCodes = []ReasonCode{ReasonPrimaryConstraintViolation, ReasonInvalidResult, ReasonChainStepFailed}
				stepTrace.Action = decideResult.Action
				trace.Steps = append(trace.Steps, stepTrace)
				if e.metrics != nil {
					e.metrics.RecordChainStep(stepTrace.ProviderType, "invalid")
				}
				if isExternal && e.state != nil {
					e.state.RecordFailure(provider.ID())
				}
				continue
			}
		}

		// Success cases: SELECT valid or ABSTAIN valid
		if decideResult.Action == ActionSelect {
			stepTrace.Outcome = StepOutcomeSelected
			stepTrace.Action = ActionSelect
			stepTrace.ReasonCodes = decideResult.ReasonCodes
			if len(stepTrace.ReasonCodes) == 0 {
				stepTrace.ReasonCodes = []ReasonCode{ReasonChainSelected}
			}
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "selected")
				e.metrics.RecordChainOutcome("selected")
			}
			if isExternal && e.state != nil {
				e.state.RecordSuccess(provider.ID())
			} else if e.state != nil {
				// For local/policy, also record success but they are not cooldown-gated; still update for observability
				// We choose not to record for policy/local to keep them always healthy (see spec 30)
				// But we can still record success for metrics if needed — not needed
			}
			// Normalize and return
			normalized, _, _ := NormalizeResult(req.Candidates, decideResult)
			// Ensure reason codes include chain selected
			hasChainSelected := false
			for _, rc := range decideResult.ReasonCodes {
				if rc == ReasonChainSelected {
					hasChainSelected = true
					break
				}
			}
			if !hasChainSelected {
				decideResult.ReasonCodes = append(decideResult.ReasonCodes, ReasonChainSelected)
				if len(decideResult.ReasonCodes) > MaxReasonCodes {
					decideResult.ReasonCodes = decideResult.ReasonCodes[:MaxReasonCodes]
				}
			}
			trace.Outcome = ChainOutcomeSelected
			trace.CallsUsed = callsUsed
			trace.SelectedProviderID = provider.ID()
			return normalized, decideResult, trace
		}

		if decideResult.Action == ActionAbstain {
			stepTrace.Outcome = StepOutcomeAbstained
			stepTrace.Action = ActionAbstain
			// Preserve provider reason codes plus chain abstract
			rc := decideResult.ReasonCodes
			hasAbstain := false
			for _, c := range rc {
				if c == ReasonChainStepAbstained {
					hasAbstain = true
					break
				}
			}
			if !hasAbstain {
				if len(rc) < MaxReasonCodes {
					rc = append(rc, ReasonChainStepAbstained)
				}
			}
			stepTrace.ReasonCodes = rc
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "abstained")
			}
			// ABSTAIN is healthy, not failure
			if isExternal && e.state != nil {
				e.state.RecordSuccess(provider.ID())
			}
			// Continue to next provider
			continue
		}

		if decideResult.Action == ActionRank {
			// Phase G contract: chain chooses PRIMARY only. RANK must NOT become a full-list chain result.
			// Treat valid RANK as invalid for chain semantics: do not reorder full failover list, continue to next provider if budget/deadline allow.
			stepTrace.Outcome = StepOutcomeInvalid
			stepTrace.Action = ActionRank
			stepTrace.ReasonCodes = []ReasonCode{ReasonInvalidResult, ReasonValidationFailed, ReasonChainStepFailed}
			trace.Steps = append(trace.Steps, stepTrace)
			if e.metrics != nil {
				e.metrics.RecordChainStep(stepTrace.ProviderType, "invalid")
			}
			if isExternal && e.state != nil {
				e.state.RecordFailure(provider.ID())
			}
			continue
		}
	}

	// Exhausted: all steps abstained/failed/timeout/unavailable/cooldown/invalid
	trace.CallsUsed = callsUsed
	trace.Outcome = ChainOutcomeExhausted
	if e.metrics != nil {
		e.metrics.RecordChainOutcome("exhausted")
	}
	ordered = CloneCandidates(req.Candidates)
	result = DecisionResult{
		Action:      ActionAbstain,
		Abstained:   true,
		Confidence:  0,
		ReasonCodes: []ReasonCode{ReasonChainExhausted, ReasonExistingOrderPreserved},
		ProviderID:  chain.ID,
	}
	return ordered, result, trace
}
