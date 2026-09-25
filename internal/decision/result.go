package decision

// Action is the bounded decision outcome from a provider.
type Action string

const (
	// ActionAbstain keeps the existing router order unchanged.
	ActionAbstain Action = "abstain"
	// ActionSelect moves the validated selection to the front, preserving
	// the remainder of the existing order for failover.
	ActionSelect Action = "select"
)

// Valid reports whether a is a canonical action.
func (a Action) Valid() bool {
	return a == ActionAbstain || a == ActionSelect
}

// DecisionResult is the single output of a DecisionProvider call. External
// adapters MUST resolve opaque choices back to physical deployment IDs before
// returning: SelectedID is always a physical ID (or empty for abstain).
type DecisionResult struct {
	Action Action
	// SelectedID is the physical deployment ID to promote. It must belong to
	// AllowedPrimaryIDs or the entire result is rejected (fail open).
	SelectedID string
	// Confidence is the provider-supplied selection confidence in [0,1], or 0
	// when unknown/absent. A missing confidence is represented as 0 and must
	// never be fabricated (e.g. 0.5) or labeled as calibrated.
	Confidence float64
	// ReasonCodes are canonical NexaRoute reason codes. Free-form provider
	// text (guidance/messages) must never be copied here.
	ReasonCodes  []ReasonCode
	ProviderID   string
	ProviderType string
}

// ValidReasonCodes reports whether every reason code is canonical.
func (r DecisionResult) ValidReasonCodes() bool {
	for _, c := range r.ReasonCodes {
		if !c.Valid() {
			return false
		}
	}
	return true
}
