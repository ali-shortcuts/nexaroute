package decision

import "time"

// Budget constrains decision execution.
// It is intentionally small: decision plane must be low-latency and fail-open.
type Budget struct {
	// Timeout is the maximum time allowed for Decide.
	Timeout time.Duration `json:"timeout"`
	// MaxCandidates is an optional soft limit for provider work; 0 means no limit.
	MaxCandidates int `json:"max_candidates,omitempty"`
}

// DefaultBudget returns the Phase D default budget.
func DefaultBudget() Budget {
	return Budget{
		Timeout:       10 * time.Millisecond,
		MaxCandidates: 0,
	}
}

// IsZero reports whether budget is unset.
func (b Budget) IsZero() bool {
	return b.Timeout == 0 && b.MaxCandidates == 0
}
