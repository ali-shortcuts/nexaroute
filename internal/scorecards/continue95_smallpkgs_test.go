package scorecards

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestContinue95ProvenanceAndDimensionBranches(t *testing.T) {
	if Provenance("bogus").Measured() {
		t.Error("unknown provenance should not count as measured")
	}
	if ProvenanceOperatorConfig.Measured() {
		t.Error("operator config is a declaration, not a measurement")
	}
	if !ProvenanceEvaluation.Measured() || !ProvenanceProductionTelemetry.Measured() {
		t.Error("evaluation and telemetry provenances should count as measured")
	}
	if Provenance("bogus").Valid() {
		t.Error("unknown provenance should not validate")
	}
	if !ProvenanceEvaluation.Valid() {
		t.Error("evaluation provenance should validate")
	}
	if Dimension("bogus").ValidQuality() {
		t.Error("unknown dimension should not be a quality dimension")
	}
	if !DimCoding.ValidQuality() {
		t.Error("coding should be a quality dimension")
	}
	if DimLatencyMS.ValidQuality() {
		t.Error("latency is operational, not a quality dimension")
	}
}

func TestContinue95ScorecardValidateBranches(t *testing.T) {
	base := validScorecard()
	cases := []struct {
		name string
		mut  func(*Scorecard)
	}{
		{"bad provider id", func(s *Scorecard) { s.ProviderID = "bad id!" }},
		{"model too long", func(s *Scorecard) { s.Model = strings.Repeat("m", MaxStringBytes+1) }},
		{"too many notes", func(s *Scorecard) { s.Notes = make([]string, 17) }},
		{"note too long", func(s *Scorecard) { s.Notes = []string{strings.Repeat("n", MaxStringBytes+1)} }},
		{"evaluation missing evaluated_at", func(s *Scorecard) {
			s.Evaluation = &EvaluationRef{SuiteID: "coding", SuiteVersion: "1", SampleCount: 5}
		}},
	}
	for _, tc := range cases {
		sc := base
		tc.mut(&sc)
		if err := sc.Validate(); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
	if err := base.Validate(); err != nil {
		t.Errorf("valid scorecard should validate: %v", err)
	}
	// Quality on a nil-values card reports no evidence.
	if _, ok := (Scorecard{}).Quality(DimCoding); ok {
		t.Error("nil-values Quality should report ok=false")
	}
	// Clone deep-copies the evaluation reference.
	withEval := base
	withEval.Evaluation = &EvaluationRef{SuiteID: "coding", SuiteVersion: "1", SampleCount: 5, EvaluatedAt: time.Now().UTC()}
	cloned := withEval.Clone()
	if cloned.Evaluation == withEval.Evaluation {
		t.Error("Clone should copy the evaluation reference, not alias it")
	}
	if cloned.Evaluation.SuiteID != "coding" {
		t.Errorf("cloned evaluation suite = %q", cloned.Evaluation.SuiteID)
	}
}

func TestContinue95FromEvaluationAndTelemetryBranches(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if _, err := FromEvaluation(EvaluationEvidence{SuiteID: "", SuiteVersion: "", EvaluatedAt: now, Values: []EvaluationValue{{Dimension: DimCoding, Score: 0.8, SampleCount: 5}}}); err == nil {
		t.Error("missing suite id/version should fail FromEvaluation")
	}
	// FromEvaluation Validate failure: invalid deployment id.
	if _, err := FromEvaluation(EvaluationEvidence{DeploymentID: "bad id!", SuiteID: "coding", SuiteVersion: "1", EvaluatedAt: now, Values: []EvaluationValue{{Dimension: DimCoding, Score: 0.8, SampleCount: 5}}}); err == nil {
		t.Error("invalid deployment should fail FromEvaluation validation")
	}
	// FromTelemetry: missing observation time.
	if _, err := FromTelemetry(TelemetryEvidence{DeploymentID: "p1/m1", SampleCount: 5}); err == nil {
		t.Error("telemetry without observation time should fail")
	}
	// Infinite latency scores as -Inf, which put() rejects, while the
	// success rate still scores.
	sc, err := FromTelemetry(TelemetryEvidence{DeploymentID: "p1/m1", ObservedAt: now, SampleCount: 5, LatencyTargetMS: 100, LatencyP95MS: math.Inf(1), SuccessRate: 0.9})
	if err != nil {
		t.Fatalf("telemetry with infinite latency: %v", err)
	}
	if _, ok := sc.Values[DimLatencyMS]; ok {
		t.Error("infinite latency should be skipped, not scored")
	}
	if sc.Values[DimAvailability].Score != 0.9 {
		t.Errorf("availability score = %v, want 0.9", sc.Values[DimAvailability].Score)
	}
	// Negative latency score clamps to 0; success rate above 1 clamps to 1.
	sc, err = FromTelemetry(TelemetryEvidence{DeploymentID: "p1/m1", ObservedAt: now, SampleCount: 5, LatencyTargetMS: 100, LatencyP95MS: 300, SuccessRate: 1.5})
	if err != nil {
		t.Fatalf("telemetry clamps: %v", err)
	}
	if sc.Values[DimLatencyMS].Score != 0 {
		t.Errorf("negative latency score should clamp to 0, got %v", sc.Values[DimLatencyMS].Score)
	}
	if sc.Values[DimAvailability].Score != 1 {
		t.Errorf("success rate above 1 should clamp to 1, got %v", sc.Values[DimAvailability].Score)
	}
	// TTFT dimension via its target.
	sc, err = FromTelemetry(TelemetryEvidence{DeploymentID: "p1/m1", ObservedAt: now, SampleCount: 5, TTFTTargetMS: 50, TTFTMS: 25})
	if err != nil {
		t.Fatalf("telemetry ttft: %v", err)
	}
	if sc.Values[DimTTFTMS].Score != 0.5 {
		t.Errorf("ttft score = %v, want 0.5", sc.Values[DimTTFTMS].Score)
	}
	// FromTelemetry Validate failure: invalid deployment id.
	if _, err := FromTelemetry(TelemetryEvidence{DeploymentID: "bad id!", ObservedAt: now, SampleCount: 5, SuccessRate: 0.9}); err == nil {
		t.Error("invalid deployment should fail FromTelemetry validation")
	}
	// FromOperatorConfig: no declarations, and validation failure.
	if _, err := FromOperatorConfig("p1/m1", "p1", "m", "key", nil); err == nil {
		t.Error("empty declarations should fail FromOperatorConfig")
	}
	if _, err := FromOperatorConfig("bad id!", "p1", "m", "key", []OperatorDeclaration{{Dimension: DimCoding, Score: 0.5}}); err == nil {
		t.Error("invalid deployment should fail FromOperatorConfig validation")
	}
	sc, err = FromOperatorConfig("p1/m1", "p1", "m", "key", []OperatorDeclaration{{Dimension: DimCoding, Score: 0.5}})
	if err != nil || sc.Values[DimCoding].Score != 0.5 {
		t.Fatalf("operator config = %+v err=%v", sc, err)
	}
}

func Continue95ImportableCard(id string) Scorecard {
	sc := validScorecard()
	sc.DeploymentID = id
	return sc
}

func TestContinue95ImportAndRegistryBranches(t *testing.T) {
	if _, err := ImportJSON(nil, "label", 10); err == nil {
		t.Error("empty artifact should fail ImportJSON")
	}
	// Two cards out of order must come back sorted by deployment id.
	b, _ := json.Marshal(Continue95ImportableCard("p2/m2"))
	a, _ := json.Marshal(Continue95ImportableCard("p1/m1"))
	doc, _ := json.Marshal(map[string]json.RawMessage{"scorecards": json.RawMessage("[" + string(b) + "," + string(a) + "]")})
	out, err := ImportJSON(doc, "label", 10)
	if err != nil {
		t.Fatalf("ImportJSON sorted: %v", err)
	}
	if len(out) != 2 || out[0].DeploymentID != "p1/m1" || out[1].DeploymentID != "p2/m2" {
		t.Errorf("import order = %v, want sorted", out)
	}
	// Non-positive and over-max caps fall back to MaxDeployments.
	if _, err := ImportJSON(doc, "label", 0); err != nil {
		t.Errorf("zero cap should default, got %v", err)
	}
	if _, err := ImportJSON(doc, "label", MaxDeployments+100); err != nil {
		t.Errorf("over-max cap should default, got %v", err)
	}
	// Exceeding the deployment cap fails.
	doc2, _ := json.Marshal(map[string]json.RawMessage{"scorecards": json.RawMessage("[" + string(a) + "," + string(b) + "]")})
	if _, err := ImportJSON(doc2, "label", 1); err == nil {
		t.Error("artifact over the cap should fail ImportJSON")
	}
	// Registry defaults for out-of-range bounds.
	r := NewRegistry(0, 0)
	if r.maxDeployments != MaxDeployments || r.maxVersions != 4 {
		t.Errorf("registry defaults = %d/%d, want %d/4", r.maxDeployments, r.maxVersions, MaxDeployments)
	}
	r2 := NewRegistry(MaxDeployments+1, MaxHistoryPerDeploy+1)
	if r2.maxDeployments != MaxDeployments || r2.maxVersions != 4 {
		t.Errorf("registry over-cap defaults = %d/%d", r2.maxDeployments, r2.maxVersions)
	}
	// Upsert fills a zero GeneratedAt.
	sc := validScorecard()
	sc.GeneratedAt = time.Time{}
	stored, err := r.Upsert(sc)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if stored.GeneratedAt.IsZero() {
		t.Error("Upsert should fill a zero GeneratedAt")
	}
	// QualityForTask on a missing card reports no evidence.
	if _, ok := r.QualityForTask("coding", "missing/missing", DimCoding); ok {
		t.Error("QualityForTask on a missing card should report ok=false")
	}
	// Load branches: duplicate, missing generated_at, version default,
	// and exceeding the deployment cap.
	dup := validScorecard()
	if err := r.Load([]Scorecard{validScorecard(), dup}); err == nil {
		t.Error("Load with duplicates should fail")
	}
	noTime := validScorecard()
	noTime.GeneratedAt = time.Time{}
	if err := r.Load([]Scorecard{noTime}); err == nil {
		t.Error("Load without generated_at should fail")
	}
	zeroVer := validScorecard()
	zeroVer.Version = 0
	if err := r.Load([]Scorecard{zeroVer}); err != nil {
		t.Fatalf("Load with version 0: %v", err)
	}
	if got, ok := r.Get("p1/m1"); !ok || got.Version != 1 {
		t.Errorf("Load should default version to 1, got %+v ok=%v", got, ok)
	}
	small := NewRegistry(1, 4)
	if err := small.Load([]Scorecard{validScorecard(), Continue95ImportableCard("p2/m2")}); err == nil {
		t.Error("Load over the cap should fail")
	}
}
