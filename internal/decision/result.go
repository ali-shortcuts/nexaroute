package decision

import "time"

// Action defines what the provider decided.
type Action string

const (
	ActionSelect  Action = "SELECT"
	ActionRank    Action = "RANK"
	ActionAbstain Action = "ABSTAIN"
)

// DecisionResult is the output of a DecisionProvider.
// It MUST obey strict contract validated by ValidateResult.
type DecisionResult struct {
	// Action taken — must be SELECT, RANK, or ABSTAIN (empty/unknown is INVALID)
	Action Action `json:"action"`

	// SELECT: single chosen candidate ID (must be in eligible set)
	SelectedID string `json:"selected_id,omitempty"`

	// RANK: ordered list of candidate IDs (must be subset of eligible, may be partial but bounded)
	RankedIDs []string `json:"ranked_ids,omitempty"`

	// Confidence 0.0-1.0 inclusive, finite, not NaN/Inf
	Confidence float64 `json:"confidence"`

	// ReasonCodes bounded cardinality for observability — only canonical values allowed
	ReasonCodes []ReasonCode `json:"reason_codes,omitempty"`

	// ProviderID is filled by orchestrator from provider.ID()
	ProviderID string `json:"provider_id,omitempty"`

	// Abstained true if provider explicitly abstained (preserves order)
	Abstained bool `json:"abstained,omitempty"`

	// Latency measured by orchestrator
	Latency time.Duration `json:"latency,omitempty"`

	// Error message for debugging — internal only, bounded, not for client exposure.
	// Must not become unbounded telemetry or privacy channel.
	// Serialized only for internal debug, but truncated and sanitized.
	Error string `json:"error,omitempty"`
}

// IsAbstain reports whether result is an abstention.
// Only explicit ABSTAIN action with empty payload is considered abstain after strict validation.
// Internal orchestrator fallbacks use ActionAbstain explicitly.
func (r DecisionResult) IsAbstain() bool {
	if r.Action == ActionAbstain {
		return true
	}
	if r.Abstained && r.Action == ActionAbstain {
		return true
	}
	// For backward compat during validation, treat explicit abstained flag with ABSTAIN action as abstain
	if r.Abstained && r.Action == "" {
		// Empty action is invalid per strict contract, but orchestrator may generate internal fallback with explicit ABSTAIN
		// This path is only for internal fail-open results that set Action=ABSTAIN
		return false
	}
	return false
}

// ValidAction reports whether Action is known and non-empty.
func (r DecisionResult) ValidAction() bool {
	switch r.Action {
	case ActionSelect, ActionRank, ActionAbstain:
		return true
	default:
		return false
	}
}

// IsStrictAbstain checks ABSTAIN contract: selected_id empty, ranked_ids empty
func (r DecisionResult) IsStrictAbstain() bool {
	return r.Action == ActionAbstain && r.SelectedID == "" && len(r.RankedIDs) == 0
}
