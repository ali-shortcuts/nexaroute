package decision

import (
	"fmt"
	"math"
)

// ValidationError is a rejected provider result. It carries only bounded,
// canonical reason codes — never provider free text, payloads, or secrets.
type ValidationError struct {
	Reason  ReasonCode
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("decision validation: %s: %s", e.Reason, e.Message)
}

// Validate proves that a provider result is safe to apply:
//
//   - abstentions are always safe (existing order is preserved);
//   - selections must name a candidate inside the eligible set E;
//   - selections must additionally fall inside AllowedPrimaryIDs (the
//     earliest pool narrowed to the minimum priority tier, or the forced pin);
//   - confidence must be absent (0), finite, and within [0,1];
//   - reason codes must all be canonical.
//
// Any violation rejects the ENTIRE result: the caller must fail open to the
// existing order and must never partially trust invalid output.
func Validate(req DecisionRequest, res DecisionResult) error {
	if !res.Action.Valid() {
		return &ValidationError{Reason: ReasonExternalInvalidResponse, Message: "unknown action"}
	}
	if res.Action == ActionAbstain {
		if !res.ValidReasonCodes() {
			return &ValidationError{Reason: ReasonExternalInvalidResponse, Message: "non-canonical reason codes"}
		}
		return nil
	}
	// SELECT path.
	if res.SelectedID == "" {
		return &ValidationError{Reason: ReasonExternalUnknownCandidate, Message: "empty selection"}
	}
	if _, ok := req.EligibleSet()[res.SelectedID]; !ok {
		return &ValidationError{Reason: ReasonExternalUnknownCandidate, Message: "selection outside eligible set"}
	}
	if _, ok := req.AllowedSet()[res.SelectedID]; !ok {
		return &ValidationError{Reason: ReasonPrimaryConstraintViolation, Message: "selection outside permitted primary band"}
	}
	if math.IsNaN(res.Confidence) || math.IsInf(res.Confidence, 0) ||
		res.Confidence < 0 || res.Confidence > 1 {
		return &ValidationError{Reason: ReasonExternalInvalidResponse, Message: "confidence outside [0,1]"}
	}
	if !res.ValidReasonCodes() {
		return &ValidationError{Reason: ReasonExternalInvalidResponse, Message: "non-canonical reason codes"}
	}
	return nil
}
