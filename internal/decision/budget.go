package decision

import "time"

// Budget constrains decision execution.
// It is intentionally small: decision plane must be low-latency and fail-open.
type Budget struct {
	// Timeout is the maximum time allowed for Decide.
	Timeout time.Duration `json:"timeout"`

	// MaxProviderCalls is the maximum number of DecisionProvider invocations.
	// Phase D default is 1 because only one provider is invoked.
	// If exhausted, orchestrator fails open with BUDGET_EXCEEDED.
	MaxProviderCalls int `json:"max_provider_calls,omitempty"`
}

// DefaultBudget returns the Phase D default budget.
func DefaultBudget() Budget {
	return Budget{
		Timeout:          10 * time.Millisecond,
		MaxProviderCalls: 1,
	}
}

// IsZero reports whether budget is unset.
func (b Budget) IsZero() bool {
	return b.Timeout == 0 && b.MaxProviderCalls == 0
}
