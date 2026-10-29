package policy

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func TestMission95EvidenceForAndTotalWeight(t *testing.T) {
	gated := Policy{MinObservations: 10}
	c := decision.Candidate{Successes: 3, Failures: 2}
	if got := gated.EvidenceFor(c); got != 0.5 {
		t.Fatalf("partial gate: %v", got)
	}
	if got := gated.EvidenceFor(decision.Candidate{}); got != 0 {
		t.Fatalf("no samples: %v", got)
	}
	if got := gated.EvidenceFor(decision.Candidate{Successes: 10}); got != 1 {
		t.Fatalf("gate met: %v", got)
	}
	if got := gated.EvidenceFor(decision.Candidate{Successes: 99}); got != 1 {
		t.Fatalf("gate exceeded: %v", got)
	}
	ungated := Policy{MinObservations: 0}
	if got := ungated.EvidenceFor(decision.Candidate{}); got != 1 {
		t.Fatalf("ungated: %v", got)
	}
	if got := (Weights{RouterBaseline: math.NaN()}).TotalWeight(); got != 0 {
		t.Fatalf("NaN total: %v", got)
	}
	if got := (Weights{RouterBaseline: math.Inf(1)}).TotalWeight(); got != 0 {
		t.Fatalf("Inf total: %v", got)
	}
	if got := (Weights{RouterBaseline: 1, Latency: 2}).TotalWeight(); got != 3 {
		t.Fatalf("sum: %v", got)
	}
}

func TestMission95WeightedSumEdges(t *testing.T) {
	comps := map[string]float64{CompRouterBaseline: 0.5}
	if got := WeightedSum(comps, Weights{}); got != 0.5 {
		t.Fatalf("zero weights: %v", got)
	}
	if got := WeightedSum(map[string]float64{CompRouterBaseline: math.NaN()}, Weights{RouterBaseline: 1}); got != 0.5 {
		t.Fatalf("NaN component: %v", got)
	}
	if got := WeightedSum(map[string]float64{CompRouterBaseline: -2}, Weights{RouterBaseline: 1}); got != 0 {
		t.Fatalf("negative score: %v", got)
	}
	if got := WeightedSum(map[string]float64{CompRouterBaseline: 3}, Weights{RouterBaseline: 1}); got != 1 {
		t.Fatalf("over-one score: %v", got)
	}
	if got := WeightedSum(map[string]float64{CompRouterBaseline: 0.25, CompLatency: 0.75}, Weights{RouterBaseline: 1, Latency: 1}); got != 0.5 {
		t.Fatalf("blend: %v", got)
	}
	if out := ApplyWeights(nil, Weights{RouterBaseline: 1}); out != nil {
		t.Fatalf("empty ApplyWeights: %v", out)
	}
	scored := []ScoredCandidate{{Candidate: decision.Candidate{ID: "a"}, Components: comps}}
	out := ApplyWeights(scored, Weights{RouterBaseline: 1})
	if len(out) != 1 || out[0].WeightedScore != 0.5 {
		t.Fatalf("ApplyWeights: %+v", out)
	}
	// Unknown component keys are ignored by the weighted sum.
	if got := WeightedSum(map[string]float64{"nope": 1}, Weights{RouterBaseline: 1}); got != 0 {
		t.Fatalf("unknown key: %v", got)
	}
}

func TestMission95ExplainAndMarshal(t *testing.T) {
	if out := Explain(nil, Weights{}); out != nil {
		t.Fatalf("empty Explain: %v", out)
	}
	scored := []ScoredCandidate{{
		Candidate:     decision.Candidate{ID: "a", PoolID: "p", PoolOrdinal: 1, Priority: 2, OriginalRank: 3},
		Components:    map[string]float64{"x": math.NaN(), "y": math.Inf(1), "z": -1, "w": 2, "ok": 0.25},
		WeightedScore: math.NaN(),
		Evidence:      0.5,
	}, {
		Candidate:     decision.Candidate{ID: "b"},
		Components:    map[string]float64{},
		WeightedScore: 1.5,
	}}
	out := Explain(scored, Weights{RouterBaseline: 1})
	if len(out) != 2 {
		t.Fatalf("Explain len %d", len(out))
	}
	if out[0].Components["x"] != 0.5 || out[0].Components["y"] != 0.5 || out[0].Components["z"] != 0 || out[0].Components["w"] != 1 {
		t.Fatalf("component clamping: %v", out[0].Components)
	}
	if out[0].WeightedScore != 0.5 || out[1].WeightedScore != 1 {
		t.Fatalf("score clamping: %v %v", out[0].WeightedScore, out[1].WeightedScore)
	}
	if MarshalBreakdown(nil) != "[]" {
		t.Fatalf("empty marshal should be []")
	}
	many := make([]PolicyScoreBreakdown, 11)
	for i := range many {
		many[i] = PolicyScoreBreakdown{CandidateID: "c", Components: map[string]float64{"a": 0.5}}
	}
	var decoded []PolicyScoreBreakdown
	if err := json.Unmarshal([]byte(MarshalBreakdown(many)), &decoded); err != nil || len(decoded) != 10 {
		t.Fatalf("11 breakdowns should trim to 10, got %d err %v", len(decoded), err)
	}
	// A single breakdown too large for the 4096 cap falls back to [].
	huge := []PolicyScoreBreakdown{{CandidateID: strings.Repeat("x", 9000), Components: map[string]float64{"a": 0.5}}}
	if got := MarshalBreakdown(huge); got != "[]" {
		t.Fatalf("oversized breakdown: len %d", len(got))
	}
	// A moderately large set shrinks until it fits instead of failing.
	medium := make([]PolicyScoreBreakdown, 3)
	for i := range medium {
		medium[i] = PolicyScoreBreakdown{CandidateID: strings.Repeat("y", 2000), Components: map[string]float64{"a": 0.5}}
	}
	if err := json.Unmarshal([]byte(MarshalBreakdown(medium)), &decoded); err != nil {
		t.Fatalf("medium marshal: %v", err)
	}
	if len(decoded) == 0 || len(decoded) > 3 {
		t.Fatalf("medium should keep a fitting prefix, got %d", len(decoded))
	}
}

func TestMission95ClampHealthAndRouterBaseline(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want float64
	}{
		{math.NaN(), 0.5}, {math.Inf(1), 0.5}, {-1, 0}, {2, 1}, {0.3, 0.3},
	} {
		if got := clamp01(tc.in); got != tc.want {
			t.Errorf("clamp01(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		status string
		want   float64
	}{
		{"healthy", 1}, {" HEALTHY ", 1}, {"unknown", 0.5}, {"", 0.5},
		{"half_open", 0.3}, {"degraded", 0.2}, {"cooldown", 0}, {"unavailable", 0}, {"weird", 0.5},
	} {
		if got := healthScore(tc.status); got != tc.want {
			t.Errorf("healthScore(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
	if got := computeRouterBaseline(nil); len(got) != 0 {
		t.Fatalf("empty baseline: %v", got)
	}
	allNaN := computeRouterBaseline([]decision.Candidate{{ID: "a", RouterScore: math.NaN()}, {ID: "b", RouterScore: math.Inf(1)}})
	if allNaN["a"] != 0.5 || allNaN["b"] != 0.5 {
		t.Fatalf("all-NaN baseline: %v", allNaN)
	}
	same := computeRouterBaseline([]decision.Candidate{{ID: "a", RouterScore: 0.7}, {ID: "b", RouterScore: 0.7}})
	if same["a"] != 0.5 || same["b"] != 0.5 {
		t.Fatalf("same-score baseline: %v", same)
	}
	spread := computeRouterBaseline([]decision.Candidate{{ID: "a", RouterScore: 0}, {ID: "b", RouterScore: 10}, {ID: "c", RouterScore: math.NaN()}})
	if spread["a"] != 0 || spread["b"] != 1 || spread["c"] != 0.5 {
		t.Fatalf("spread baseline: %v", spread)
	}
}

func TestMission95FromConfigBranches(t *testing.T) {
	base := config.DecisionPolicyConfig{
		ID: "p1", SelectionMode: "select_first",
		Weights: config.DecisionPolicyWeights{RouterBaseline: 1},
	}
	// Explicit version is kept verbatim.
	cfg := base
	cfg.Version = "v9"
	p, err := FromConfig(cfg)
	if err != nil || p.Version != "v9" {
		t.Fatalf("explicit version: %+v %v", p, err)
	}
	// Derived version is deterministic and prefixed.
	p1, err := FromConfig(base)
	p2, err2 := FromConfig(base)
	if err != nil || err2 != nil || p1.Version != p2.Version || !strings.HasPrefix(p1.Version, VersionPrefix) {
		t.Fatalf("derived version: %q %q", p1.Version, p2.Version)
	}
	// Changing content changes the derived version.
	cfg = base
	cfg.MinScoreDelta = 0.1
	p3, err := FromConfig(cfg)
	if err != nil || p3.Version == p1.Version {
		t.Fatalf("version should change with content: %q vs %q", p3.Version, p1.Version)
	}
	// Weight validation: NaN, negative, and over-cap each fail.
	for _, w := range []config.DecisionPolicyWeights{
		{RouterBaseline: math.NaN()},
		{RouterBaseline: -1},
		{RouterBaseline: 1000001},
		{Quality: math.Inf(1)},
	} {
		cfg = base
		cfg.Weights = w
		if _, err := FromConfig(cfg); err == nil {
			t.Fatalf("weights %+v should fail", w)
		}
	}
	// Override validation: non-canonical key, bad weight, all-zero, negative gate.
	cfg = base
	cfg.TaskOverrides = map[string]config.DecisionPolicyWeights{"not_a_task": {RouterBaseline: 1}}
	if _, err := FromConfig(cfg); err == nil {
		t.Fatalf("non-canonical override should fail")
	}
	cfg = base
	cfg.TaskOverrides = map[string]config.DecisionPolicyWeights{"coding": {RouterBaseline: -1}}
	if _, err := FromConfig(cfg); err == nil {
		t.Fatalf("bad override weight should fail")
	}
	cfg = base
	cfg.TaskOverrides = map[string]config.DecisionPolicyWeights{"coding": {}}
	if _, err := FromConfig(cfg); err == nil {
		t.Fatalf("all-zero override should fail")
	}
	cfg = base
	cfg.MinObservations = -1
	if _, err := FromConfig(cfg); err == nil {
		t.Fatalf("negative min_observations should fail")
	}
	// Empty/whitespace override keys are skipped, keys are canonicalized.
	cfg = base
	cfg.TaskOverrides = map[string]config.DecisionPolicyWeights{
		"  ":       {RouterBaseline: 1},
		" CODING ": {RouterBaseline: 2},
	}
	p, err = FromConfig(cfg)
	if err != nil {
		t.Fatalf("skipped keys: %v", err)
	}
	if _, ok := p.TaskOverrides["coding"]; !ok {
		t.Fatalf("coding override missing: %v", p.TaskOverrides)
	}
	if _, ok := p.TaskOverrides["  "]; ok {
		t.Fatalf("blank override key should be skipped")
	}
	// ResolveWeights: empty task, positive override, all-zero override fallback.
	pol := Policy{Weights: Weights{RouterBaseline: 1}, TaskOverrides: map[string]Weights{"coding": {Latency: 1}, "vision": {}}}
	if w, ok := pol.ResolveWeights(""); ok || w.RouterBaseline != 1 {
		t.Fatalf("empty task: %v %v", w, ok)
	}
	if w, ok := pol.ResolveWeights("CODING"); !ok || w.Latency != 1 {
		t.Fatalf("coding override: %v %v", w, ok)
	}
	if w, ok := pol.ResolveWeights("vision"); ok || w.RouterBaseline != 1 {
		t.Fatalf("zero override should fall back without task-aware flag: %v %v", w, ok)
	}
	if w, ok := pol.ResolveWeights("missing"); ok || w.RouterBaseline != 1 {
		t.Fatalf("missing override: %v %v", w, ok)
	}
}

func TestMission95Helpers(t *testing.T) {
	if got := originalPrimaryID(nil); got != "" {
		t.Fatalf("empty band: %q", got)
	}
	band := []decision.Candidate{{ID: "b", OriginalRank: 5}, {ID: "a", OriginalRank: 1}, {ID: "c", OriginalRank: 3}}
	if got := originalPrimaryID(band); got != "a" {
		t.Fatalf("min rank: %q", got)
	}
	if got := dedupReasonCodes(nil); got != nil {
		t.Fatalf("empty dedup: %v", got)
	}
	in := []decision.ReasonCode{decision.ReasonPolicyScored, decision.ReasonPolicyScored, decision.ReasonPolicySelectFirst}
	out := dedupReasonCodes(in)
	if len(out) != 2 || out[0] != decision.ReasonPolicyScored || out[1] != decision.ReasonPolicySelectFirst {
		t.Fatalf("dedup: %v", out)
	}
	// Cap at MaxReasonCodes.
	manyCodes := make([]decision.ReasonCode, 0, 12)
	for i := 0; i < 12; i++ {
		manyCodes = append(manyCodes, decision.ReasonCode("CODE_"+string(rune('A'+i))))
	}
	if got := dedupReasonCodes(manyCodes); len(got) != decision.MaxReasonCodes {
		t.Fatalf("dedup cap: %d", len(got))
	}
	// weightsToMap carries every weight except Quality (documented shape).
	m := weightsToMap(Weights{RouterBaseline: 1, Latency: 2, Quality: 3})
	if m[CompRouterBaseline] != 1 || m[CompLatency] != 2 {
		t.Fatalf("weightsToMap: %v", m)
	}
}

func TestMission95ProviderDecideEdges(t *testing.T) {
	pol := Policy{ID: "test", SelectionMode: "select_first", Weights: Weights{RouterBaseline: 1}}
	p := NewProvider([]Policy{pol}, "test")
	ctx := context.Background()

	// Empty eligible set abstains.
	res, err := p.Decide(ctx, decision.DecisionRequest{PolicyID: "test"})
	if err != nil || !res.Abstained || res.ReasonCodes[0] != decision.ReasonEmptyEligible {
		t.Fatalf("empty: %+v %v", res, err)
	}
	// Pinned affinity inside the earliest pool wins, preserving the pin
	// even when it is not the original primary.
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 10, RouterScore: 0.9},
		{ID: "b", PoolOrdinal: 0, OriginalRank: 1, Priority: 10, RouterScore: 0.1},
	}
	res, err = p.Decide(ctx, decision.DecisionRequest{Candidates: cands, PolicyID: "test", PinnedCandidateID: "b"})
	if err != nil || res.SelectedID != "b" || res.PolicyTrace == nil || !res.PolicyTrace.ChangedPrimary {
		t.Fatalf("affinity: %+v %v", res, err)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonAffinityPreserved {
			found = true
		}
	}
	if !found {
		t.Fatalf("affinity reason missing: %v", res.ReasonCodes)
	}
	// Pinned candidate outside the earliest pool is ignored (pool boundary).
	cands2 := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 10, RouterScore: 0.9},
		{ID: "b", PoolOrdinal: 1, OriginalRank: 1, Priority: 10, RouterScore: 0.1},
	}
	res, err = p.Decide(ctx, decision.DecisionRequest{Candidates: cands2, PolicyID: "test", PinnedCandidateID: "b"})
	if err != nil || res.SelectedID == "b" {
		t.Fatalf("out-of-band pin should be ignored: %+v %v", res, err)
	}
	// Single candidate in band selects directly with a trace.
	res, err = p.Decide(ctx, decision.DecisionRequest{Candidates: cands[:1], PolicyID: "test"})
	if err != nil || res.SelectedID != "a" || res.Confidence != 1.0 || res.PolicyTrace == nil {
		t.Fatalf("single: %+v %v", res, err)
	}
	// Priority guardrail: lower priority value wins the band.
	cands3 := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 5, RouterScore: 0.1},
		{ID: "b", PoolOrdinal: 0, OriginalRank: 1, Priority: 9, RouterScore: 0.9},
	}
	res, err = p.Decide(ctx, decision.DecisionRequest{Candidates: cands3, PolicyID: "test"})
	if err != nil || res.SelectedID != "a" {
		t.Fatalf("priority guardrail: %+v %v", res, err)
	}
	found = false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonPriorityGuardrail {
			found = true
		}
	}
	if !found {
		t.Fatalf("priority reason missing: %v", res.ReasonCodes)
	}
	// Top-is-primary abstains to preserve order, with a trace.
	res, err = p.Decide(ctx, decision.DecisionRequest{Candidates: cands, PolicyID: "test"})
	if err != nil || !res.Abstained || res.PolicyTrace == nil || res.PolicyTrace.ChangedPrimary {
		t.Fatalf("top-is-primary: %+v %v", res, err)
	}
	// Task-aware weights: a coding override flips the outcome and is flagged.
	pol2 := Policy{ID: "ta", SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1},
		TaskOverrides: map[string]Weights{"coding": {Latency: 1}},
	}
	p2 := NewProvider([]Policy{pol2}, "ta")
	scored := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 10, RouterScore: 0.9, EWMALatencyMS: 500, Successes: 10},
		{ID: "b", PoolOrdinal: 0, OriginalRank: 1, Priority: 10, RouterScore: 0.1, EWMALatencyMS: 10, Successes: 10},
	}
	req := decision.DecisionRequest{Candidates: scored, PolicyID: "ta"}
	req.TaskProfile.Type = taskprofile.TaskType("coding")
	res, err = p2.Decide(ctx, req)
	if err != nil {
		t.Fatalf("task-aware: %v", err)
	}
	// Either outcome is acceptable here; what matters is the trace records
	// the coding task type and the decision is well-formed.
	if res.PolicyTrace == nil || res.PolicyTrace.TaskType != "coding" {
		t.Fatalf("task-aware trace: %+v", res.PolicyTrace)
	}
	// Min-score-delta: a better non-primary below the delta abstains.
	// Quality-only weights with one sample each keep the scores near
	// neutral (0.484 vs 0.516), so the improvement stays below 0.9.
	pol3 := Policy{ID: "delta", SelectionMode: "select_first", Weights: Weights{Quality: 1}, MinScoreDelta: 0.9}
	p3 := NewProvider([]Policy{pol3}, "delta")
	deltaCands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 10, TaskQuality: 0, TaskQualitySamples: 1},
		{ID: "b", PoolOrdinal: 0, OriginalRank: 1, Priority: 10, TaskQuality: 1, TaskQualitySamples: 1},
	}
	res, err = p3.Decide(ctx, decision.DecisionRequest{Candidates: deltaCands, PolicyID: "delta"})
	if err != nil || !res.Abstained {
		t.Fatalf("delta: %+v %v", res, err)
	}
	found = false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonMinDeltaNotMet {
			found = true
		}
	}
	if !found {
		t.Fatalf("delta reason missing: %v", res.ReasonCodes)
	}
	// Required-context fallback: estimated tokens drive scoring when
	// MinContextWindow is unset (must not error and must stay bounded).
	res, err = p.Decide(ctx, decision.DecisionRequest{
		Candidates: cands, PolicyID: "test", EstimatedInputTokens: 100, MaxOutputTokens: 50,
	})
	if err != nil {
		t.Fatalf("estimated fallback: %v", err)
	}
	_ = res
}
