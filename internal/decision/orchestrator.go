package decision

import (
	"context"
	"errors"
	"time"
)

// Decision modes. Only these three exist in Phase F: no smart/hybrid/
// supervisor, and no provider chains (Phase G).
const (
	ModeOff      = "off"
	ModeLocal    = "local"
	ModeAssisted = "assisted"
)

// MaxProviderCalls is the per-request decision budget: exactly one provider
// call, no retries, no second chances. Provider chains belong to Phase G.
const MaxProviderCalls = 1

// DefaultDecisionTimeout bounds a single provider call when the operator did
// not configure one. Fail-open on expiry.
const DefaultDecisionTimeout = 400 * time.Millisecond

// Bounded external outcome classes for events and metrics.
const (
	OutcomeSelected        = "selected"
	OutcomeAbstained       = "abstained"
	OutcomeError           = "error"
	OutcomeTimeout         = "timeout"
	OutcomeInvalid         = "invalid"
	OutcomeUnavailable     = "unavailable"
	OutcomeSkippedAffinity = "skipped_affinity"
	OutcomeSkippedSingle   = "skipped_single"
	OutcomeDisabled        = "disabled"
)

// Outcome is the orchestrator's verdict: a final primary ordering plus the
// bounded evidence needed for events, metrics, and tracing. FinalOrder always
// contains exactly the input candidate IDs (possibly reordered); on any
// failure it equals the input order verbatim.
type Outcome struct {
	FinalOrder        []string
	SelectedID        string
	ProviderCalled    bool
	ProviderCalls     int
	ReasonCodes       []ReasonCode
	ExternalLatencyMS int64
	Confidence        float64
	ProviderID        string
	ProviderType      string
	ExternalOutcome   string
}

// codedError is implemented by typed provider errors (e.g. remote.Error) to
// map failures onto canonical reason codes without leaking details.
type codedError interface {
	error
	DecisionReason() ReasonCode
}

// outcomeForReason maps a canonical reason onto a bounded outcome class.
func outcomeForReason(r ReasonCode) string {
	switch r {
	case ReasonExternalTimeout:
		return OutcomeTimeout
	case ReasonExternalHTTPError, ReasonProviderError:
		return OutcomeError
	case ReasonExternalInvalidResponse, ReasonExternalUnknownCandidate,
		ReasonPrimaryConstraintViolation, ReasonExternalResponseTooLarge:
		return OutcomeInvalid
	case ReasonExternalProviderUnavailable, ReasonExternalRequestTooLarge:
		return OutcomeUnavailable
	case ReasonExternalAbstained, ReasonLocalAbstained:
		return OutcomeAbstained
	case ReasonExternalSelected, ReasonPolicySelected:
		return OutcomeSelected
	default:
		return OutcomeError
	}
}

func isExternalType(t string) bool {
	return t != BuiltinLocalID && t != BuiltinPolicyID
}

// Orchestrator runs the single-provider decision flow: guardrails first,
// then at most one provider call under timeout, then strict validation.
// Every failure path fails open to the existing router order.
type Orchestrator struct {
	mode     string
	provider DecisionProvider
	timeout  time.Duration
}

// NewOrchestrator builds an orchestrator for exactly one provider (nil when
// disabled). A non-positive timeout falls back to DefaultDecisionTimeout.
func NewOrchestrator(mode string, provider DecisionProvider, timeout time.Duration) *Orchestrator {
	if timeout <= 0 {
		timeout = DefaultDecisionTimeout
	}
	return &Orchestrator{mode: mode, provider: provider, timeout: timeout}
}

// ProviderCalls returns the number of provider calls this orchestrator will
// make per request: 0 when disabled, otherwise at most MaxProviderCalls.
func (o *Orchestrator) ProviderCalls() int {
	if o == nil || o.mode == ModeOff || o.provider == nil {
		return 0
	}
	return MaxProviderCalls
}

// Decide computes the final primary ordering for one request.
//
// Flow: eligible set E -> primary-selection guardrails -> (affinity
// short-circuit | single-choice skip | one provider call) -> strict
// validation -> reordered primary or verbatim fail-open order.
func (o *Orchestrator) Decide(ctx context.Context, candidates []Candidate, sessionPin string, features RequestFeatures, requestID string) Outcome {
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	disabled := Outcome{
		FinalOrder:      append([]string(nil), ids...),
		ReasonCodes:     []ReasonCode{ReasonDecisionDisabled},
		ExternalOutcome: OutcomeDisabled,
	}
	if o == nil || o.mode == ModeOff || o.provider == nil || len(candidates) == 0 {
		return disabled
	}
	if len(requestID) > MaxRequestIDBytes {
		requestID = requestID[:MaxRequestIDBytes]
	}
	constraints := ComputeConstraints(candidates, sessionPin)
	req := DecisionRequest{
		RequestID:         requestID,
		Candidates:        candidates,
		AllowedPrimaryIDs: constraints.AllowedPrimaryIDs,
		ForcedPrimaryID:   constraints.ForcedPrimaryID,
		Features:          features,
	}
	// Affinity short-circuit: the eligible pin is authoritative. No provider
	// call, no network cost, no data sharing.
	if constraints.ForcedPrimaryID != "" {
		return Outcome{
			FinalOrder:      append([]string(nil), ids...),
			ReasonCodes:     []ReasonCode{ReasonAffinityPreserved},
			ProviderID:      o.provider.ID(),
			ProviderType:    o.provider.Type(),
			ExternalOutcome: OutcomeSkippedAffinity,
		}
	}
	// Single-choice skip: nothing to select between (also honors the Jev
	// model-route contract, which requires at least two candidates).
	if len(constraints.AllowedPrimaryIDs) < 2 {
		return Outcome{
			FinalOrder:      append([]string(nil), ids...),
			ReasonCodes:     []ReasonCode{ReasonPrimarySingleChoice},
			ProviderID:      o.provider.ID(),
			ProviderType:    o.provider.Type(),
			ExternalOutcome: OutcomeSkippedSingle,
		}
	}

	external := isExternalType(o.provider.Type())
	callCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	start := time.Now()
	res, err := o.provider.Decide(callCtx, req)
	latency := time.Since(start)
	out := Outcome{
		FinalOrder:        append([]string(nil), ids...),
		ProviderCalled:    true,
		ProviderCalls:     1,
		ProviderID:        o.provider.ID(),
		ProviderType:      o.provider.Type(),
		ExternalLatencyMS: latency.Milliseconds(),
	}
	if err != nil {
		reason := ReasonProviderError
		var coded codedError
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			reason = ReasonExternalTimeout
		case errors.As(err, &coded) && coded.DecisionReason().Valid():
			reason = coded.DecisionReason()
		}
		out.ReasonCodes = []ReasonCode{reason}
		out.ExternalOutcome = outcomeForReason(reason)
		return out
	}
	// Never partially trust provider output: validate everything.
	if verr := Validate(req, res); verr != nil {
		reason := ReasonExternalInvalidResponse
		var ve *ValidationError
		if errors.As(verr, &ve) && ve.Reason.Valid() {
			reason = ve.Reason
		}
		out.ReasonCodes = []ReasonCode{reason}
		out.ExternalOutcome = outcomeForReason(reason)
		return out
	}
	if res.Action == ActionAbstain {
		codes := res.ReasonCodes
		if len(codes) == 0 {
			if external {
				codes = []ReasonCode{ReasonExternalAbstained}
			} else {
				codes = []ReasonCode{ReasonLocalAbstained}
			}
		}
		out.ReasonCodes = codes
		out.ExternalOutcome = OutcomeAbstained
		return out
	}
	// Valid SELECT: promote to front, preserve the failover remainder.
	order := make([]string, 0, len(candidates))
	order = append(order, res.SelectedID)
	for _, c := range candidates {
		if c.ID != res.SelectedID {
			order = append(order, c.ID)
		}
	}
	codes := res.ReasonCodes
	if len(codes) == 0 {
		if external {
			codes = []ReasonCode{ReasonExternalSelected}
		} else {
			codes = []ReasonCode{ReasonPolicySelected}
		}
	}
	out.FinalOrder = order
	out.SelectedID = res.SelectedID
	out.Confidence = res.Confidence
	out.ReasonCodes = codes
	out.ExternalOutcome = OutcomeSelected
	return out
}
