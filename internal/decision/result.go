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
type DecisionResult struct {
	// Action taken
	Action Action `json:"action"`

	// SELECT: single chosen candidate ID (must be in eligible set)
	SelectedID string `json:"selected_id,omitempty"`

	// RANK: ordered list of candidate IDs (must be subset of eligible, may be partial)
	RankedIDs []string `json:"ranked_ids,omitempty"`

	// Confidence 0.0-1.0 inclusive
	Confidence float64 `json:"confidence"`

	// ReasonCodes bounded cardinality for observability
	ReasonCodes []string `json:"reason_codes,omitempty"`

	// ProviderID is filled by orchestrator from provider.ID()
	ProviderID string `json:"provider_id,omitempty"`

	// Abstained true if provider explicitly abstained (preserves order)
	Abstained bool `json:"abstained,omitempty"`

	// Latency measured by orchestrator
	Latency time.Duration `json:"latency,omitempty"`

	// Error message for debugging (not for client exposure)
	Error string `json:"error,omitempty"`
}

// IsAbstain reports whether result is an abstention.
func (r DecisionResult) IsAbstain() bool {
	return r.Abstained || r.Action == ActionAbstain || (r.Action == "" && len(r.RankedIDs) == 0 && r.SelectedID == "")
}

// ValidAction reports whether Action is known.
func (r DecisionResult) ValidAction() bool {
	switch r.Action {
	case ActionSelect, ActionRank, ActionAbstain, "":
		return true
	default:
		return false
	}
}
