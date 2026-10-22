package policy

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestFromConfig_VersionExplicitIsPreserved(t *testing.T) {
	p, err := FromConfig(config.DecisionPolicyConfig{
		ID:      "balanced",
		Version: "2026-10-18.1",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
	})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if p.Version != "2026-10-18.1" {
		t.Fatalf("explicit version not preserved: %q", p.Version)
	}
}

func TestFromConfig_DerivedVersionIsStableAndContentSensitive(t *testing.T) {
	base := config.DecisionPolicyConfig{ID: "balanced", Weights: config.DecisionPolicyWeights{RouterBaseline: 1}}
	a, err := FromConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version == "" || !strings.HasPrefix(a.Version, VersionPrefix) {
		t.Fatalf("derived version must be a content fingerprint, got %q", a.Version)
	}
	if a.Version != b.Version {
		t.Fatalf("derived version is not deterministic: %q vs %q", a.Version, b.Version)
	}

	changed := base
	changed.Weights.Reliability = 1
	c, err := FromConfig(changed)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version == a.Version {
		t.Fatalf("changing weights must change the derived version")
	}

	// Task overrides are part of the effective policy and must affect the version.
	overridden := base
	overridden.TaskOverrides = map[string]config.DecisionPolicyWeights{"coding": {Latency: 1}}
	d, err := FromConfig(overridden)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version == a.Version {
		t.Fatalf("changing task overrides must change the derived version")
	}
}

func TestScoreCandidatesWithObservations_MinEvidenceDecaysTowardNeutral(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.HealthStatus = "degraded" // healthScore 0.2
			c.Successes = 0
			c.Failures = 1
			c.EWMAFailureRate = 1.0 // failure-rate score 0
			c.OriginalRank = 0
		}),
	}

	raw := ScoreCandidatesWithObservations(cands, 0, 0)
	if !approxEq(raw[0].Components[CompReliability], 0.1) {
		t.Fatalf("raw reliability = %v, want 0.1", raw[0].Components[CompReliability])
	}
	if raw[0].Evidence != 1 {
		t.Fatalf("disabled gate evidence = %v, want 1", raw[0].Evidence)
	}

	// n=1 of minObs=4 -> evidence 0.25 -> 0.5 + 0.25*(0.1-0.5) = 0.4
	gated := ScoreCandidatesWithObservations(cands, 0, 4)
	if !approxEq(gated[0].Evidence, 0.25) {
		t.Fatalf("gated evidence = %v, want 0.25", gated[0].Evidence)
	}
	if !approxEq(gated[0].Components[CompEvidence], 0.25) {
		t.Fatalf("gated evidence component = %v, want 0.25", gated[0].Components[CompEvidence])
	}
	if !approxEq(gated[0].Components[CompReliability], 0.4) {
		t.Fatalf("gated reliability = %v, want 0.4", gated[0].Components[CompReliability])
	}

	// At the threshold the raw score is trusted fully.
	met := ScoreCandidatesWithObservations(cands, 0, 1)
	if !approxEq(met[0].Components[CompReliability], 0.1) {
		t.Fatalf("threshold-met reliability = %v, want 0.1", met[0].Components[CompReliability])
	}
}

func TestScoreCandidatesWithObservations_ZeroSamplesStayNeutral(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.HealthStatus = "healthy"
			c.Successes = 0
			c.Failures = 0
			c.EWMAFailureRate = 0
		}),
	}
	gated := ScoreCandidatesWithObservations(cands, 0, 5)
	if gated[0].Evidence != 0 {
		t.Fatalf("zero-sample evidence = %v, want 0", gated[0].Evidence)
	}
	if !approxEq(gated[0].Components[CompReliability], 0.5) {
		t.Fatalf("zero-sample reliability = %v, want neutral 0.5", gated[0].Components[CompReliability])
	}
}

// TestDecide_MinEvidencePreventsFlapping proves that a single observation
// cannot move traffic away from the incumbent when the evidence gate is on:
// the same policy that switches with the gate disabled preserves order once a
// minimum observation count is required.
func TestDecide_MinEvidencePreventsFlapping(t *testing.T) {
	makeReq := func() decision.DecisionRequest {
		return decision.DecisionRequest{
			Candidates: []decision.Candidate{
				makeCandidate("victim", func(c *decision.Candidate) {
					c.HealthStatus = "degraded"
					c.Successes = 0
					c.Failures = 1
					c.EWMAFailureRate = 1.0
					c.OriginalRank = 0
				}),
				makeCandidate("candidate", func(c *decision.Candidate) {
					c.HealthStatus = "healthy"
					c.Successes = 2
					c.Failures = 0
					c.EWMAFailureRate = 0
					c.OriginalRank = 1
				}),
			},
		}
	}

	mkPolicy := func(minObs int) Policy {
		p, err := FromConfig(config.DecisionPolicyConfig{
			ID:              "flap",
			Weights:         config.DecisionPolicyWeights{Reliability: 1},
			MinScoreDelta:   0.6,
			MinObservations: minObs,
		})
		if err != nil {
			t.Fatalf("FromConfig: %v", err)
		}
		return p
	}

	// Without the gate, the single failing sample pushes the policy to switch
	// to the "candidate" (improvement 0.9 >= 0.6).
	noGate := NewProvider([]Policy{mkPolicy(0)}, "flap")
	res, err := noGate.Decide(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if res.Action != decision.ActionSelect || res.SelectedID != "candidate" {
		t.Fatalf("gate off: got action=%s selected=%q, want SELECT candidate", res.Action, res.SelectedID)
	}

	// With the gate, evidence confidence shrinks the improvement below the
	// switch margin, so the incumbent order is preserved (no flap).
	withGate := NewProvider([]Policy{mkPolicy(4)}, "flap")
	res, err = withGate.Decide(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	// Note: only ActionAbstain with Abstained=true is a valid preserve;
	// the assertions below pin the required outcome.
	if res.Action != decision.ActionAbstain {
		t.Fatalf("gate on: got action=%s selected=%q, want ABSTAIN preserving order", res.Action, res.SelectedID)
	}
	if res.PolicyTrace != nil && res.PolicyTrace.ChangedPrimary {
		t.Fatalf("gate on: primary must not change")
	}
}

func TestDecide_PolicyTraceCarriesVersion(t *testing.T) {
	p, err := FromConfig(config.DecisionPolicyConfig{
		ID:      "balanced",
		Version: "v9",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	prov := NewProvider([]Policy{p}, "balanced")
	res, err := prov.Decide(context.Background(), decision.DecisionRequest{
		Candidates: []decision.Candidate{
			makeCandidate("a", func(c *decision.Candidate) { c.RouterScore = 1; c.OriginalRank = 0 }),
			makeCandidate("b", func(c *decision.Candidate) { c.RouterScore = 1; c.OriginalRank = 1 }),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PolicyTrace == nil {
		t.Fatal("expected policy trace")
	}
	if res.PolicyTrace.PolicyVersion != "v9" {
		t.Fatalf("trace policy version = %q, want v9", res.PolicyTrace.PolicyVersion)
	}
}
