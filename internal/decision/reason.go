package decision

// ReasonCode is a bounded enum for decision observability.
// Only values defined here may become metrics labels or event reasons.
// No arbitrary provider text, chain-of-thought, or prompt content.
type ReasonCode string

const (
	ReasonExistingOrderPreserved ReasonCode = "EXISTING_ORDER_PRESERVED"
	ReasonLocalPassThrough       ReasonCode = "LOCAL_PASS_THROUGH"
	ReasonAbstained              ReasonCode = "ABSTAINED"
	ReasonTimeout                ReasonCode = "TIMEOUT"
	ReasonProviderError          ReasonCode = "PROVIDER_ERROR"
	ReasonProviderPanic          ReasonCode = "PROVIDER_PANIC"
	ReasonInvalidResult          ReasonCode = "INVALID_RESULT"
	ReasonBudgetExceeded         ReasonCode = "BUDGET_EXCEEDED"
	ReasonOffMode                ReasonCode = "OFF_MODE"
	ReasonEligibleSetPreserved   ReasonCode = "ELIGIBLE_SET_PRESERVED"
	ReasonNormalizationApplied   ReasonCode = "NORMALIZATION_APPLIED"
	ReasonValidationFailed       ReasonCode = "VALIDATION_FAILED"
	ReasonSingleCandidate        ReasonCode = "SINGLE_CANDIDATE"
	ReasonEmptyEligible          ReasonCode = "EMPTY_ELIGIBLE"
	ReasonProviderUnhealthy      ReasonCode = "PROVIDER_UNHEALTHY"
	// Phase E: policy engine
	ReasonAffinityPreserved    ReasonCode = "AFFINITY_PRESERVED"
	ReasonPoolBoundaryEnforced ReasonCode = "POOL_BOUNDARY_ENFORCED"
	ReasonPriorityGuardrail    ReasonCode = "PRIORITY_GUARDRAIL_ENFORCED"
	ReasonPolicyScored         ReasonCode = "POLICY_SCORED"
	ReasonPolicySelectFirst    ReasonCode = "POLICY_SELECT_FIRST"
	ReasonTaskAwareWeights     ReasonCode = "TASK_AWARE_WEIGHTS"
	ReasonMinDeltaNotMet       ReasonCode = "MIN_DELTA_NOT_MET"
	ReasonContextGuardrail     ReasonCode = "CONTEXT_GUARDRAIL"
)

// allowedReasonCodes is the canonical set for validation.
var allowedReasonCodes = map[ReasonCode]struct{}{
	ReasonExistingOrderPreserved: {},
	ReasonLocalPassThrough:       {},
	ReasonAbstained:              {},
	ReasonTimeout:                {},
	ReasonProviderError:          {},
	ReasonProviderPanic:          {},
	ReasonInvalidResult:          {},
	ReasonBudgetExceeded:         {},
	ReasonOffMode:                {},
	ReasonEligibleSetPreserved:   {},
	ReasonNormalizationApplied:   {},
	ReasonValidationFailed:       {},
	ReasonSingleCandidate:        {},
	ReasonEmptyEligible:          {},
	ReasonProviderUnhealthy:      {},
	ReasonAffinityPreserved:      {},
	ReasonPoolBoundaryEnforced:   {},
	ReasonPriorityGuardrail:      {},
	ReasonPolicyScored:           {},
	ReasonPolicySelectFirst:      {},
	ReasonTaskAwareWeights:       {},
	ReasonMinDeltaNotMet:         {},
	ReasonContextGuardrail:       {},
}

// IsValidReasonCode reports whether code is in the bounded contract.
func IsValidReasonCode(rc ReasonCode) bool {
	_, ok := allowedReasonCodes[rc]
	return ok
}

// IsValidReasonCodeString validates string form (for backward compat).
func IsValidReasonCodeString(s string) bool {
	_, ok := allowedReasonCodes[ReasonCode(s)]
	return ok
}

// AllReasonCodes returns list of canonical codes for docs/metrics.
func AllReasonCodes() []ReasonCode {
	out := make([]ReasonCode, 0, len(allowedReasonCodes))
	for rc := range allowedReasonCodes {
		out = append(out, rc)
	}
	return out
}

// Bounds for reason code validation
const (
	MaxReasonCodes   = 8
	MaxReasonCodeLen = 64
	MaxSelectedIDLen = 512
	MaxProviderIDLen = 128
	MaxRankedIDs     = 4096 // safety hard limit, but also bounded by eligible len
)
