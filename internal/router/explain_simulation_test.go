package router

import (
	"math"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// simFixture builds a two-deployment router with one shared alias and no
// session affinity, so rotation behaviour is observable in isolation.
func simFixture(t *testing.T, strategy string) *Router {
	t.Helper()
	cfg := config.Default()
	cfg.Routing.Strategy = strategy
	cfg.Routing.SessionAffinity = false
	for _, name := range []string{"A", "B"} {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "p" + name, Name: name, Type: "openai_compatible", BaseURL: "https://example.invalid", Enabled: true,
			Models: []config.ModelConfig{{
				ID: "m", Model: "physical-" + name, Aliases: []string{"sim"}, Enabled: true,
				Priority: 0, Weight: 1, Capabilities: config.Capabilities{Streaming: true},
			}},
		})
	}
	h := health.New(4, time.Hour)
	r := New(cfg, h)
	for _, d := range r.All() {
		h.RecordSuccess(d.ID, 10*time.Millisecond)
		h.RecordSuccess(d.ID, 10*time.Millisecond)
	}
	return r
}

func simCandidateIDs(cands []Scored) []string {
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Deployment.ID)
	}
	return ids
}

func simEqualIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A simulation must never advance the live round-robin counter: one
// Explain between two real Candidates calls must leave the second real
// pick exactly where the first real pick left it (one rotation step on),
// not two steps on.
func TestExplainDoesNotAdvanceLiveRoundRobinCounter(t *testing.T) {
	r := simFixture(t, "round_robin")
	req := Requirement{Model: "sim"}
	first := simCandidateIDs(r.Candidates(req))
	if len(first) != 2 {
		t.Fatalf("first candidates = %v, want 2", first)
	}
	explained := simCandidateIDs(r.Explain(req).Candidates)
	if len(explained) != 2 {
		t.Fatalf("explained candidates = %v, want 2", explained)
	}
	second := simCandidateIDs(r.Candidates(req))
	if simEqualIDs(first, second) {
		t.Fatalf("second real pick %v equals first %v; a simulation advanced the rotation counter", second, first)
	}
	// The simulated order is repeatable and independent of live rotation.
	again := simCandidateIDs(r.Explain(req).Candidates)
	if !simEqualIDs(explained, again) {
		t.Fatalf("repeated simulation changed order: %v then %v", explained, again)
	}
	third := simCandidateIDs(r.Candidates(req))
	if !simEqualIDs(first, third) {
		t.Fatalf("third real pick %v, want rotation back to first %v", third, first)
	}
}

// A simulation must never create a session pin, even when the
// requirement carries a session key and affinity is enabled.
func TestExplainDoesNotCreateSessionPin(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.SessionAffinity = true
	cfg.Providers = []config.ProviderConfig{{
		ID: "pA", Name: "A", Type: "openai_compatible", BaseURL: "https://example.invalid", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Aliases: []string{"sim"}, Enabled: true,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	h := health.New(4, time.Hour)
	r := New(cfg, h)
	h.RecordSuccess("pA/m", 10*time.Millisecond)
	h.RecordSuccess("pA/m", 10*time.Millisecond)
	if got := r.SessionCount(); got != 0 {
		t.Fatalf("session count before simulation = %d, want 0", got)
	}
	r.Explain(Requirement{Model: "sim", SessionKey: "operator-session"})
	if got := r.SessionCount(); got != 0 {
		t.Fatalf("simulation created a session pin: count = %d, want 0", got)
	}
}

func TestExplainLabelsSimulationAndDecomposesScore(t *testing.T) {
	r, _, _ := explainFixture(t, "ready_queue")
	exp := r.Explain(Requirement{Model: "coding", Streaming: true})
	if !exp.Simulated || !exp.Deterministic || !exp.RotationPredictive {
		t.Fatalf("explanation labels = simulated %v deterministic %v rotation_predictive %v, want all true for ready_queue",
			exp.Simulated, exp.Deterministic, exp.RotationPredictive)
	}
	for _, cd := range exp.Decisions {
		if !cd.Included {
			if cd.Score != 0 || cd.ScoreBase != 0 {
				t.Fatalf("excluded decision %q carries a score", cd.DeploymentID)
			}
			continue
		}
		rebuilt := cd.ScoreBase + cd.HealthAdjustment + cd.WeightContribution - cd.PriorityPenalty - cd.LatencyPenalty - cd.CapacityPenalty - cd.FailurePenalty
		if math.Abs(rebuilt-cd.Score) > 1e-9 {
			t.Fatalf("decision %q score %v != breakdown sum %v (base %v health %v weight %v priority %v latency %v capacity %v failure %v)",
				cd.DeploymentID, cd.Score, rebuilt, cd.ScoreBase, cd.HealthAdjustment, cd.WeightContribution, cd.PriorityPenalty, cd.LatencyPenalty, cd.CapacityPenalty, cd.FailurePenalty)
		}
		if cd.ScoreBase != 100 {
			t.Fatalf("decision %q score base = %v, want 100", cd.DeploymentID, cd.ScoreBase)
		}
		if cd.WeightContribution != cd.Weight*10 {
			t.Fatalf("decision %q weight contribution = %v, want weight*10 = %v", cd.DeploymentID, cd.WeightContribution, cd.Weight*10)
		}
		if cd.PriorityPenalty != float64(cd.Priority)*3 {
			t.Fatalf("decision %q priority penalty = %v, want priority*3", cd.DeploymentID, cd.PriorityPenalty)
		}
	}
}

func TestExplainRotationPredictiveFlags(t *testing.T) {
	rr := simFixture(t, "round_robin")
	if exp := rr.Explain(Requirement{Model: "sim"}); exp.RotationPredictive {
		t.Fatal("round_robin simulation must not claim to predict the live pick")
	}
	mesh := simFixture(t, "ready_mesh")
	if exp := mesh.Explain(Requirement{Model: "sim"}); exp.RotationPredictive {
		t.Fatal("ready_mesh simulation without a stable key must not claim to predict the live P2C pick")
	}
	if exp := mesh.Explain(Requirement{Model: "sim", SessionKey: "stable-key"}); !exp.RotationPredictive {
		t.Fatal("ready_mesh simulation with a session key must be rotation-predictive")
	}
	if exp := mesh.Explain(Requirement{Model: "sim", SessionKey: "stable-key"}); !exp.Deterministic {
		t.Fatal("keyed simulation must be deterministic")
	}
}
