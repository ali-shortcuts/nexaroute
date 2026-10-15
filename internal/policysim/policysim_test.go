package policysim

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// M6-2: two candidates — "a" has the higher router baseline but
// 5x the latency of "b". baseline-first must pick a; latency-first
// must pick b.
func simConfig() config.Config {
	cfg := config.Config{}
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		{ID: "baseline-first", Weights: config.DecisionPolicyWeights{RouterBaseline: 1}},
		{ID: "latency-first", Weights: config.DecisionPolicyWeights{Latency: 1}},
	}
	return cfg
}

func simFixture() Fixture {
	return Fixture{
		Name: "two-candidates",
		Request: decision.DecisionRequest{
			TaskProfile: decision.TaskProfile{},
			Candidates: []decision.Candidate{
				{ID: "a", ProviderID: "p", RouterScore: 100, EWMALatencyMS: 50, HealthStatus: "healthy"},
				{ID: "b", ProviderID: "p", RouterScore: 90, EWMALatencyMS: 10, HealthStatus: "healthy"},
			},
		},
	}
}

func TestSimulatePolicyOutcomes(t *testing.T) {
	cfg := simConfig()
	rep, err := Simulate(context.Background(), cfg, "baseline-first", []Fixture{simFixture()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Cases) != 1 || rep.Cases[0].Winner != "a" {
		t.Fatalf("baseline-first winner: %+v", rep.Cases)
	}
	if rep.Cases[0].Breakdown == "" {
		t.Fatal("breakdown (policy trace) missing")
	}
	rep2, err := Simulate(context.Background(), cfg, "latency-first", []Fixture{simFixture()})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Cases[0].Winner != "b" {
		t.Fatalf("latency-first winner: %+v", rep2.Cases)
	}
	if _, err := Simulate(context.Background(), cfg, "ghost", []Fixture{simFixture()}); err == nil {
		t.Fatal("undefined policy accepted")
	}
}

// Determinism: the same simulation run repeatedly — and from a
// JSON round trip of the fixture — yields the identical digest.
func TestSimulateDeterministicDigest(t *testing.T) {
	cfg := simConfig()
	first, err := Simulate(context.Background(), cfg, "latency-first", []Fixture{simFixture()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		next, err := Simulate(context.Background(), cfg, "latency-first", []Fixture{simFixture()})
		if err != nil {
			t.Fatal(err)
		}
		if next.Digest != first.Digest {
			t.Fatalf("run %d digest %s != %s", i, next.Digest, first.Digest)
		}
	}
	raw, err := json.Marshal(FixtureFile{Cases: []Fixture{simFixture()}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded FixtureFile
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	roundTripped, err := Simulate(context.Background(), cfg, "latency-first", decoded.Cases)
	if err != nil {
		t.Fatal(err)
	}
	if roundTripped.Digest != first.Digest {
		t.Fatalf("JSON round trip changed the digest: %s != %s", roundTripped.Digest, first.Digest)
	}
}

func TestReplayAgreementAndDivergence(t *testing.T) {
	cfg := simConfig()
	fx := simFixture()
	records := []ReplayRecord{
		{Request: fx.Request, RecordedWinner: "a"}, // production ran baseline-first
		{Request: fx.Request, RecordedWinner: "b"},
	}
	rep, err := Replay(context.Background(), cfg, "latency-first", records)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 2 || rep.Agreed != 1 {
		t.Fatalf("replay agreement: %+v", rep)
	}
	if rep.Cases[0].Agree || !rep.Cases[1].Agree {
		t.Fatalf("per-case agreement wrong: %+v", rep.Cases)
	}
	// Replaying under the production policy agrees everywhere.
	rep2, err := Replay(context.Background(), cfg, "baseline-first", records[:1])
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Agreed != 1 {
		t.Fatalf("same-policy replay should agree: %+v", rep2)
	}
}
