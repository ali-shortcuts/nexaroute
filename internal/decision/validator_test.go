package decision

import (
	"errors"
	"math"
	"testing"
)

func validationRequest() DecisionRequest {
	cands := []Candidate{cand("A", 0, 0), cand("B", 0, 0), cand("C", 1, 0)}
	return DecisionRequest{
		Candidates:        cands,
		AllowedPrimaryIDs: []string{"A", "B"},
	}
}

func TestValidateAbstain(t *testing.T) {
	req := validationRequest()
	res := DecisionResult{Action: ActionAbstain, ReasonCodes: []ReasonCode{ReasonExternalAbstained}}
	if err := Validate(req, res); err != nil {
		t.Fatalf("abstain must validate: %v", err)
	}
}

func TestValidateAbstainRejectsNonCanonicalReasons(t *testing.T) {
	req := validationRequest()
	res := DecisionResult{Action: ActionAbstain, ReasonCodes: []ReasonCode{"JEV SAYS HI"}}
	if err := Validate(req, res); err == nil {
		t.Fatal("non-canonical reason codes must be rejected")
	}
}

func TestValidateSelectInBand(t *testing.T) {
	req := validationRequest()
	res := DecisionResult{Action: ActionSelect, SelectedID: "B", Confidence: 0.7, ReasonCodes: []ReasonCode{ReasonExternalSelected}}
	if err := Validate(req, res); err != nil {
		t.Fatalf("in-band select must validate: %v", err)
	}
}

func TestValidateSelectOutsideBandRejected(t *testing.T) {
	req := validationRequest() // C is eligible but outside the primary band.
	res := DecisionResult{Action: ActionSelect, SelectedID: "C", ReasonCodes: []ReasonCode{ReasonExternalSelected}}
	err := Validate(req, res)
	if err == nil {
		t.Fatal("out-of-band select must be rejected")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Reason != ReasonPrimaryConstraintViolation {
		t.Fatalf("want PRIMARY_CONSTRAINT_VIOLATION, got %v", err)
	}
}

func TestValidateSelectOutsideEligibleSetRejected(t *testing.T) {
	req := validationRequest()
	res := DecisionResult{Action: ActionSelect, SelectedID: "GHOST", ReasonCodes: []ReasonCode{ReasonExternalSelected}}
	err := Validate(req, res)
	if err == nil {
		t.Fatal("out-of-eligible-set select must be rejected")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Reason != ReasonExternalUnknownCandidate {
		t.Fatalf("want EXTERNAL_UNKNOWN_CANDIDATE, got %v", err)
	}
}

func TestValidateSelectEmptyRejected(t *testing.T) {
	req := validationRequest()
	res := DecisionResult{Action: ActionSelect, ReasonCodes: []ReasonCode{ReasonExternalSelected}}
	if err := Validate(req, res); err == nil {
		t.Fatal("empty select must be rejected")
	}
}

func TestValidateConfidenceBounds(t *testing.T) {
	req := validationRequest()
	for _, c := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 1.1} {
		res := DecisionResult{Action: ActionSelect, SelectedID: "A", Confidence: c, ReasonCodes: []ReasonCode{ReasonExternalSelected}}
		if err := Validate(req, res); err == nil {
			t.Fatalf("confidence %v must be rejected", c)
		}
	}
	for _, c := range []float64{0, 0.5, 1} {
		res := DecisionResult{Action: ActionSelect, SelectedID: "A", Confidence: c, ReasonCodes: []ReasonCode{ReasonExternalSelected}}
		if err := Validate(req, res); err != nil {
			t.Fatalf("confidence %v must validate: %v", c, err)
		}
	}
}

func TestValidateUnknownActionRejected(t *testing.T) {
	req := validationRequest()
	res := DecisionResult{Action: "rank", SelectedID: "A"}
	if err := Validate(req, res); err == nil {
		t.Fatal("unknown action must be rejected")
	}
}

func TestReasonCodesClosed(t *testing.T) {
	if ReasonCode("ANYTHING").Valid() {
		t.Fatal("arbitrary strings must not be valid reason codes")
	}
	for _, c := range AllReasonCodes {
		if !c.Valid() {
			t.Fatalf("listed code %q must validate", c)
		}
	}
}
