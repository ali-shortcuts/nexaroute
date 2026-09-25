package decision

// Reason codes for DecisionResult. Bounded cardinality, suitable for metrics.
const (
	ReasonExistingOrderPreserved = "EXISTING_ORDER_PRESERVED"
	ReasonLocalPassThrough       = "LOCAL_PASS_THROUGH"
	ReasonAbstained              = "ABSTAINED"
	ReasonTimeout                = "TIMEOUT"
	ReasonProviderError          = "PROVIDER_ERROR"
	ReasonProviderPanic          = "PROVIDER_PANIC"
	ReasonInvalidResult          = "INVALID_RESULT"
	ReasonBudgetExceeded         = "BUDGET_EXCEEDED"
	ReasonOffMode                = "OFF_MODE"
	ReasonEligibleSetPreserved   = "ELIGIBLE_SET_PRESERVED"
	ReasonNormalizationApplied   = "NORMALIZATION_APPLIED"
	ReasonValidationFailed       = "VALIDATION_FAILED"
)
