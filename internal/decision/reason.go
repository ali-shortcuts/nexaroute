// Package decision owns NexaRoute's generic model-routing decision contract.
//
// External intelligence (Jev or any future DecisionProvider) is NEVER
// eligibility authority: the router computes the eligible set E, the shared
// primary-selection guardrails narrow the permitted primary band, and only
// then may exactly one configured DecisionProvider select a new primary from
// inside that band. Every failure fails open to the existing router order.
package decision

// ReasonCode is a bounded, canonical decision reason. Arbitrary strings must
// never be used as reason codes: they flow into events, metrics-adjacent
// counters, and the admin snapshot, so the set is closed and validated.
type ReasonCode string

const (
	// Built-in provider outcomes.
	ReasonLocalAbstained ReasonCode = "LOCAL_ABSTAINED"
	ReasonPolicySelected ReasonCode = "POLICY_SELECTED"

	// Guardrail outcomes.
	ReasonAffinityPreserved ReasonCode = "AFFINITY_PRESERVED"
	// ReasonPrimarySingleChoice marks a skipped provider call: fewer than two
	// candidates compete for primary (e.g. a single allowed primary), so no
	// selection is needed. This also honors the Jev model-route contract,
	// which requires at least two candidates.
	ReasonPrimarySingleChoice ReasonCode = "PRIMARY_SINGLE_CHOICE"
	// ReasonPrimaryConstraintViolation marks a provider selection that was
	// rejected because it fell outside the permitted primary band
	// (AllowedPrimaryIDs), even when it was inside the eligible set E.
	ReasonPrimaryConstraintViolation ReasonCode = "PRIMARY_CONSTRAINT_VIOLATION"
	ReasonDecisionDisabled           ReasonCode = "DECISION_DISABLED"
	// ReasonProviderError marks an unexpected, untyped provider failure. Typed
	// failures always map to a more specific code; this is the bounded
	// fallback so fail-open never needs an arbitrary string.
	ReasonProviderError ReasonCode = "DECISION_PROVIDER_ERROR"

	// External provider outcomes.
	ReasonExternalSelected            ReasonCode = "EXTERNAL_SELECTED"
	ReasonExternalAbstained           ReasonCode = "EXTERNAL_ABSTAINED"
	ReasonExternalTimeout             ReasonCode = "EXTERNAL_TIMEOUT"
	ReasonExternalHTTPError           ReasonCode = "EXTERNAL_HTTP_ERROR"
	ReasonExternalInvalidResponse     ReasonCode = "EXTERNAL_INVALID_RESPONSE"
	ReasonExternalUnknownCandidate    ReasonCode = "EXTERNAL_UNKNOWN_CANDIDATE"
	ReasonExternalRequestTooLarge     ReasonCode = "EXTERNAL_REQUEST_TOO_LARGE"
	ReasonExternalResponseTooLarge    ReasonCode = "EXTERNAL_RESPONSE_TOO_LARGE"
	ReasonExternalProviderUnavailable ReasonCode = "EXTERNAL_PROVIDER_UNAVAILABLE"
)

// AllReasonCodes is the closed set of valid reason codes.
var AllReasonCodes = []ReasonCode{
	ReasonLocalAbstained,
	ReasonPolicySelected,
	ReasonAffinityPreserved,
	ReasonPrimarySingleChoice,
	ReasonPrimaryConstraintViolation,
	ReasonDecisionDisabled,
	ReasonProviderError,
	ReasonExternalSelected,
	ReasonExternalAbstained,
	ReasonExternalTimeout,
	ReasonExternalHTTPError,
	ReasonExternalInvalidResponse,
	ReasonExternalUnknownCandidate,
	ReasonExternalRequestTooLarge,
	ReasonExternalResponseTooLarge,
	ReasonExternalProviderUnavailable,
}

var validReasonCodes = func() map[ReasonCode]struct{} {
	m := make(map[ReasonCode]struct{}, len(AllReasonCodes))
	for _, c := range AllReasonCodes {
		m[c] = struct{}{}
	}
	return m
}()

// Valid reports whether c is a canonical reason code.
func (c ReasonCode) Valid() bool {
	_, ok := validReasonCodes[c]
	return ok
}

// Strings converts reason codes to their string form for events/metrics.
func Strings(codes []ReasonCode) []string {
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		if c.Valid() {
			out = append(out, string(c))
		}
	}
	return out
}
