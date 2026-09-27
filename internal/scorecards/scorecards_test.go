package scorecards

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func validValue() Value {
	return Value{
		Score:        0.8,
		Raw:          0.8,
		Provenance:   ProvenanceEvaluation,
		SampleCount:  8,
		Confidence:   0.25,
		EvaluatedAt:  time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		SuiteVersion: "1",
		Source:       "coding",
	}
}

func validScorecard() Scorecard {
	return Scorecard{
		DeploymentID: "p1/m1",
		ProviderID:   "p1",
		Model:        "model-1",
		Version:      1,
		GeneratedAt:  time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Values:       map[Dimension]Value{DimCoding: validValue()},
	}
}

func TestValidateValue_ProvenanceIsMandatory(t *testing.T) {
	v := validValue()
	v.Provenance = ""
	err := ValidateValue(DimCoding, v)
	if !errors.Is(err, ErrNoProvenance) {
		t.Fatalf("expected ErrNoProvenance, got %v", err)
	}
	if !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("error should explain provenance requirement: %v", err)
	}
}

func TestValidateValue_ProvenanceRules(t *testing.T) {
	base := validValue()
	cases := []struct {
		name    string
		mutate  func(v *Value)
		wantErr bool
	}{
		{"evaluation ok", func(v *Value) {}, false},
		{"evaluation without suite version", func(v *Value) { v.SuiteVersion = "" }, true},
		{"evaluation without samples", func(v *Value) { v.SampleCount = 0 }, true},
		{"evaluation without timestamp", func(v *Value) { v.EvaluatedAt = time.Time{} }, true},
		{"telemetry ok", func(v *Value) { v.Provenance = ProvenanceProductionTelemetry }, false},
		{"telemetry without samples", func(v *Value) { v.Provenance = ProvenanceProductionTelemetry; v.SampleCount = 0 }, true},
		{"telemetry without timestamp", func(v *Value) { v.Provenance = ProvenanceProductionTelemetry; v.EvaluatedAt = time.Time{} }, true},
		{"imported ok", func(v *Value) { v.Provenance = ProvenanceImported }, false},
		{"imported without source", func(v *Value) { v.Provenance = ProvenanceImported; v.Source = "" }, true},
		{"imported without samples", func(v *Value) { v.Provenance = ProvenanceImported; v.SampleCount = 0 }, true},
		{"operator config ok", func(v *Value) {
			v.Provenance = ProvenanceOperatorConfig
			v.SampleCount = 0
			v.EvaluatedAt = time.Time{}
			v.SuiteVersion = ""
		}, false},
		{"operator config claiming samples", func(v *Value) { v.Provenance = ProvenanceOperatorConfig }, true},
		{"operator config without source", func(v *Value) {
			v.Provenance = ProvenanceOperatorConfig
			v.SampleCount = 0
			v.EvaluatedAt = time.Time{}
			v.SuiteVersion = ""
			v.Source = ""
		}, true},
		{"unknown provenance", func(v *Value) { v.Provenance = Provenance("vibes") }, true},
		{"score above one", func(v *Value) { v.Score = 1.5 }, true},
		{"score below zero", func(v *Value) { v.Score = -0.1 }, true},
		{"nan score", func(v *Value) { v.Score = math.NaN() }, true},
		{"inf score", func(v *Value) { v.Score = math.Inf(1) }, true},
		{"nan raw", func(v *Value) { v.Raw = math.NaN() }, true},
		{"nan confidence", func(v *Value) { v.Confidence = math.NaN() }, true},
		{"negative samples", func(v *Value) { v.SampleCount = -1 }, true},
		{"too many samples", func(v *Value) { v.SampleCount = MaxSampleCount + 1 }, true},
		{"oversized source", func(v *Value) { v.Source = strings.Repeat("x", MaxStringBytes+1) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.mutate(&v)
			err := ValidateValue(DimCoding, v)
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidate_UnknownDimensionRejected(t *testing.T) {
	sc := validScorecard()
	sc.Values[Dimension("charisma")] = validValue()
	err := sc.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown scorecard dimension") {
		t.Fatalf("expected unknown dimension rejection, got %v", err)
	}
}

func TestValidate_Envelope(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(sc *Scorecard)
	}{
		{"empty deployment", func(sc *Scorecard) { sc.DeploymentID = "" }},
		{"invalid deployment id", func(sc *Scorecard) { sc.DeploymentID = "p1/m1; drop table" }},
		{"version zero", func(sc *Scorecard) { sc.Version = 0 }},
		{"too many values", func(sc *Scorecard) {
			sc.Values = map[Dimension]Value{}
			for i := 0; i < MaxValuesPerScorecard+1; i++ {
				sc.Values[Dimension("d"+string(rune('a'+i%26))+string(rune('0'+i%10)))] = validValue()
			}
		}},
		{"evaluation ref without suite", func(sc *Scorecard) {
			sc.Evaluation = &EvaluationRef{SampleCount: 5, EvaluatedAt: time.Now()}
		}},
		{"evaluation ref without samples", func(sc *Scorecard) {
			sc.Evaluation = &EvaluationRef{SuiteID: "coding", SuiteVersion: "1", EvaluatedAt: time.Now()}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := validScorecard()
			tc.mutate(&sc)
			if err := sc.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
	if err := validScorecard().Validate(); err != nil {
		t.Fatalf("valid scorecard rejected: %v", err)
	}
}

// TestNoFabrication_MissingDimensionStaysMissing is the core Phase H invariant:
// an unmeasured dimension is absent, never a default.
func TestNoFabrication_MissingDimensionStaysMissing(t *testing.T) {
	sc := validScorecard()
	if _, ok := sc.Quality(DimCoding); !ok {
		t.Fatal("expected coding dimension to be present")
	}
	for _, d := range []Dimension{DimDebugging, DimReasoning, DimToolCalling, DimStructuredOutput, DimInstructionFollow, DimLongContext, DimProtocolCompat} {
		v, ok := sc.Quality(d)
		if ok {
			t.Fatalf("dimension %s should be absent, got %+v", d, v)
		}
		if v.Score != 0 || v.Provenance != "" {
			t.Fatalf("absent dimension must be the zero value, got %+v", v)
		}
	}
	present, total := sc.Coverage()
	if present != 1 || total != len(QualityDimensions) {
		t.Fatalf("coverage = %d/%d, want 1/%d", present, total, len(QualityDimensions))
	}
}

func TestFromEvaluation_NoEvidenceNoScorecard(t *testing.T) {
	if _, err := FromEvaluation(EvaluationEvidence{DeploymentID: "p/m", SuiteID: "coding", SuiteVersion: "1", EvaluatedAt: time.Now()}); !errors.Is(err, ErrNotScoreable) {
		t.Fatalf("expected ErrNotScoreable, got %v", err)
	}
	// Samples of zero are dropped, which can leave nothing behind.
	_, err := FromEvaluation(EvaluationEvidence{
		DeploymentID: "p/m", SuiteID: "coding", SuiteVersion: "1", EvaluatedAt: time.Now(),
		Values: []EvaluationValue{{Dimension: DimCoding, Score: 0.9, SampleCount: 0}},
	})
	if !errors.Is(err, ErrNotScoreable) {
		t.Fatalf("expected ErrNotScoreable for zero-sample evidence, got %v", err)
	}
	if _, err := FromEvaluation(EvaluationEvidence{
		DeploymentID: "p/m", SuiteID: "coding", SuiteVersion: "1",
		Values: []EvaluationValue{{Dimension: DimCoding, Score: 0.9, SampleCount: 4}},
	}); err == nil {
		t.Fatal("expected missing timestamp to be rejected")
	}
}

func TestFromEvaluation_ProvenanceAndConfidence(t *testing.T) {
	sc, err := FromEvaluation(EvaluationEvidence{
		DeploymentID: "p1/m1", ProviderID: "p1", Model: "m1",
		SuiteID: "coding", SuiteVersion: "1", RunID: "run-1",
		EvaluatedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Values: []EvaluationValue{
			{Dimension: DimCoding, Score: 0.75, Raw: 0.75, SampleCount: 4},
			{Dimension: DimAvailability, Score: 1, Raw: 1, SampleCount: 4},
		},
	})
	if err != nil {
		t.Fatalf("FromEvaluation: %v", err)
	}
	coding, ok := sc.Quality(DimCoding)
	if !ok {
		t.Fatal("coding missing")
	}
	if coding.Provenance != ProvenanceEvaluation {
		t.Fatalf("provenance = %s", coding.Provenance)
	}
	if coding.SuiteVersion != "1" || coding.SampleCount != 4 || coding.Source != "coding" {
		t.Fatalf("unexpected evidence: %+v", coding)
	}
	if got := ConfidenceFromSamples(4); math.Abs(coding.Confidence-got) > 1e-9 {
		t.Fatalf("confidence = %v, want %v", coding.Confidence, got)
	}
	if sc.Evaluation == nil || sc.Evaluation.RunID != "run-1" || sc.Evaluation.SampleCount != 4 {
		t.Fatalf("evaluation ref = %+v", sc.Evaluation)
	}
	if _, ok := sc.Quality(DimReasoning); ok {
		t.Fatal("reasoning must stay absent: nothing measured it")
	}
}

func TestConfidenceFromSamples_IsBounded(t *testing.T) {
	if ConfidenceFromSamples(0) != 0 {
		t.Fatal("zero samples must have zero confidence")
	}
	if ConfidenceFromSamples(-5) != 0 {
		t.Fatal("negative samples must have zero confidence")
	}
	if ConfidenceFromSamples(FullConfidenceSamples) != 1 || ConfidenceFromSamples(1000) != 1 {
		t.Fatal("confidence must saturate at 1")
	}
	prev := -1.0
	for i := 1; i <= FullConfidenceSamples; i++ {
		c := ConfidenceFromSamples(i)
		if c <= prev || c > 1 {
			t.Fatalf("confidence must increase monotonically: %d -> %v", i, c)
		}
		prev = c
	}
}

func TestFromTelemetry_TargetsRequiredForScores(t *testing.T) {
	_, err := FromTelemetry(TelemetryEvidence{DeploymentID: "p/m", SampleCount: 10, ObservedAt: time.Now()})
	if !errors.Is(err, ErrNotScoreable) {
		t.Fatalf("telemetry without targets must not produce values, got %v", err)
	}
	sc, err := FromTelemetry(TelemetryEvidence{
		DeploymentID: "p/m", ProviderID: "p", Model: "m",
		ObservedAt: time.Now(), WindowLabel: "1h", SampleCount: 100,
		LatencyP95MS: 500, LatencyTargetMS: 1000,
		SuccessRate: 0.98, FailureRate: 0.02,
	})
	if err != nil {
		t.Fatalf("FromTelemetry: %v", err)
	}
	latency, ok := sc.Values[DimLatencyMS]
	if !ok {
		t.Fatal("latency dimension missing")
	}
	if latency.Provenance != ProvenanceProductionTelemetry || latency.SampleCount != 100 {
		t.Fatalf("unexpected latency evidence: %+v", latency)
	}
	if math.Abs(latency.Score-0.5) > 1e-9 {
		t.Fatalf("latency score = %v, want 0.5", latency.Score)
	}
	if _, ok := sc.Values[DimTTFTMS]; ok {
		t.Fatal("ttft must be absent: no target given")
	}
	if _, err := FromTelemetry(TelemetryEvidence{DeploymentID: "p/m", ObservedAt: time.Now(), LatencyTargetMS: 10, LatencyP95MS: 20}); !errors.Is(err, ErrNotScoreable) {
		t.Fatalf("telemetry without samples must be rejected, got %v", err)
	}
}

func TestFromOperatorConfig_DeclaredIsNotMeasured(t *testing.T) {
	sc, err := FromOperatorConfig("p/m", "p", "m", "scorecards.quality", []OperatorDeclaration{{Dimension: DimCoding, Score: 0.9}})
	if err != nil {
		t.Fatalf("FromOperatorConfig: %v", err)
	}
	v, ok := sc.Quality(DimCoding)
	if !ok {
		t.Fatal("declared dimension missing")
	}
	if v.Provenance != ProvenanceOperatorConfig {
		t.Fatalf("provenance = %s", v.Provenance)
	}
	if v.SampleCount != 0 || v.Confidence != 0 {
		t.Fatalf("declarations must not claim samples/confidence: %+v", v)
	}
	if _, err := FromOperatorConfig("p/m", "p", "m", "", []OperatorDeclaration{{Dimension: DimCoding, Score: 0.9}}); !errors.Is(err, ErrNotScoreable) {
		t.Fatalf("source key required, got %v", err)
	}
}

func TestRegistry_VersioningHistoryAndBounds(t *testing.T) {
	reg := NewRegistry(2, 2)
	for i := 1; i <= 3; i++ {
		sc := validScorecard()
		sc.Values[DimCoding] = Value{
			Score: float64(i) / 4, Provenance: ProvenanceEvaluation, SampleCount: 4,
			Confidence: ConfidenceFromSamples(4), EvaluatedAt: time.Now().UTC(),
			SuiteVersion: "1", Source: "coding",
		}
		stored, err := reg.Upsert(sc)
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
		if stored.Version != i {
			t.Fatalf("version = %d, want %d", stored.Version, i)
		}
	}
	if reg.Len() != 1 {
		t.Fatalf("len = %d", reg.Len())
	}
	hist := reg.History("p1/m1")
	if len(hist) != 2 {
		t.Fatalf("history = %d entries, want bounded at 2", len(hist))
	}
	if hist[0].Version != 2 || hist[1].Version != 3 {
		t.Fatalf("history must keep the newest versions: %+v", hist)
	}
	// Registry capacity: a second deployment fits, a third does not.
	if _, err := reg.Upsert(Scorecard{DeploymentID: "p1/m2", GeneratedAt: time.Now().UTC(), Version: 1, Values: map[Dimension]Value{DimCoding: validValue()}}); err != nil {
		t.Fatalf("second deployment: %v", err)
	}
	sc := Scorecard{DeploymentID: "p1/m3", GeneratedAt: time.Now().UTC(), Version: 1, Values: map[Dimension]Value{DimCoding: validValue()}}
	if _, err := reg.Upsert(sc); err == nil {
		t.Fatal("registry must reject a deployment beyond its bound")
	}
	if stats := reg.Stats(); stats.Rejected == 0 {
		t.Fatal("rejected counter must track refusals")
	}
}

func TestRegistry_RejectsInvalidAndKeepsState(t *testing.T) {
	reg := NewRegistry(4, 2)
	if _, err := reg.Upsert(validScorecard()); err != nil {
		t.Fatalf("valid upsert: %v", err)
	}
	bad := validScorecard()
	bad.Values = map[Dimension]Value{DimCoding: {Score: 0.9, Provenance: ProvenanceEvaluation, SampleCount: 4}}
	if _, err := reg.Upsert(bad); err == nil {
		t.Fatal("value without suite version must be rejected")
	}
	got, ok := reg.Get("p1/m1")
	if !ok || got.Version != 1 {
		t.Fatalf("failed upsert must not change state: %+v", got)
	}
	stats := reg.Stats()
	if stats.Upserts != 1 || stats.Rejected != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRegistry_SnapshotSortedAndCloned(t *testing.T) {
	reg := NewRegistry(8, 2)
	for _, id := range []string{"p1/m3", "p1/m1", "p1/m2"} {
		sc := validScorecard()
		sc.DeploymentID = id
		if _, err := reg.Upsert(sc); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	snap := reg.Snapshot()
	if len(snap) != 3 || snap[0].DeploymentID != "p1/m1" || snap[2].DeploymentID != "p1/m3" {
		t.Fatalf("snapshot must be sorted: %+v", snap)
	}
	// Mutating the snapshot must not corrupt the registry.
	v := snap[0].Values[DimCoding]
	v.Score = 0.01
	snap[0].Values[DimCoding] = v
	stored, _ := reg.Get("p1/m1")
	if stored.Values[DimCoding].Score == 0.01 {
		t.Fatal("registry must hand out deep copies")
	}
}

func TestRegistry_RetainResetAndLoad(t *testing.T) {
	reg := NewRegistry(8, 2)
	sc := validScorecard()
	if _, err := reg.Upsert(sc); err != nil {
		t.Fatal(err)
	}
	reg.Retain(map[string]struct{}{"other/deployment": {}})
	if reg.Len() != 0 {
		t.Fatal("retain must drop unknown deployments")
	}
	if _, ok := reg.Get("p1/m1"); ok {
		t.Fatal("dropped scorecard must not be readable")
	}
	loaded := validScorecard()
	loaded.DeploymentID = "p2/m2"
	if err := reg.Load([]Scorecard{loaded}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("len = %d", reg.Len())
	}
	// All-or-nothing load: an invalid entry rejects the whole batch.
	bad := validScorecard()
	bad.DeploymentID = "p3/m3"
	bad.Values = map[Dimension]Value{DimCoding: {Score: 2, Provenance: ProvenanceEvaluation, SampleCount: 1, SuiteVersion: "1", EvaluatedAt: time.Now()}}
	if err := reg.Load([]Scorecard{bad}); err == nil {
		t.Fatal("invalid load must fail")
	}
	if reg.Len() != 1 {
		t.Fatal("failed load must leave the registry untouched")
	}
	reg.Reset()
	if reg.Len() != 0 {
		t.Fatal("reset must clear the registry")
	}
}

func TestRegistry_QualityNeverFabricates(t *testing.T) {
	reg := NewRegistry(4, 2)
	if _, ok := reg.Quality("p1/m1", DimCoding); ok {
		t.Fatal("unknown deployment must not report a score")
	}
	if _, err := reg.Upsert(validScorecard()); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Quality("p1/m1", DimReasoning); ok {
		t.Fatal("unmeasured dimension must not report a score")
	}
	if v, ok := reg.Quality("p1/m1", DimCoding); !ok || v.Provenance != ProvenanceEvaluation {
		t.Fatalf("unexpected quality lookup: %+v ok=%v", v, ok)
	}
}

func TestRegistry_ConcurrentUpsertAndSnapshot(t *testing.T) {
	reg := NewRegistry(64, 2)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				sc := validScorecard()
				sc.DeploymentID = "p1/m" + string(rune('a'+n))
				_, _ = reg.Upsert(sc)
				_ = reg.Snapshot()
				_, _ = reg.Get(sc.DeploymentID)
			}
		}(i)
	}
	wg.Wait()
	if reg.Len() != 8 {
		t.Fatalf("len = %d, want 8", reg.Len())
	}
}

func TestImportJSON_ValidArtifact(t *testing.T) {
	doc := `{"scorecards":[{"deployment_id":"p1/m1","provider_id":"p1","model":"m1","version":1,
		"generated_at":"2026-09-26T12:00:00Z",
		"values":{"coding":{"score":0.8,"provenance":"imported","sample_count":12,"confidence":0.375,"source":"vendor-bench"}}}]}`
	list, err := ImportJSON([]byte(doc), "test.json", 16)
	if err != nil {
		t.Fatalf("ImportJSON: %v", err)
	}
	if len(list) != 1 || list[0].DeploymentID != "p1/m1" {
		t.Fatalf("unexpected import: %+v", list)
	}
	v := list[0].Values[DimCoding]
	if v.Provenance != ProvenanceImported || v.SampleCount != 12 || v.Source != "vendor-bench" {
		t.Fatalf("import must preserve provenance: %+v", v)
	}
}

func TestImportJSON_Rejections(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"empty", `{"scorecards":[]}`},
		{"unknown field", `{"scorecards":[{"deployment_id":"p/m","version":1,"generated_at":"2026-09-26T12:00:00Z","values":{"coding":{"score":0.5,"provenance":"imported","sample_count":1,"source":"s"}},"mystery":1}]}`},
		{"no provenance", `{"scorecards":[{"deployment_id":"p/m","version":1,"generated_at":"2026-09-26T12:00:00Z","values":{"coding":{"score":0.5,"sample_count":1}}}]}`},
		{"no generated_at", `{"scorecards":[{"deployment_id":"p/m","version":1,"values":{"coding":{"score":0.5,"provenance":"imported","sample_count":1,"source":"s"}}}]}`},
		{"duplicate deployment", `{"scorecards":[
			{"deployment_id":"p/m","version":1,"generated_at":"2026-09-26T12:00:00Z","values":{"coding":{"score":0.5,"provenance":"imported","sample_count":1,"source":"s"}}},
			{"deployment_id":"p/m","version":1,"generated_at":"2026-09-26T12:00:00Z","values":{"coding":{"score":0.6,"provenance":"imported","sample_count":1,"source":"s"}}}]}`},
		{"not json", `nonsense`},
		{"trailing data", `{"scorecards":[{"deployment_id":"p/m","version":1,"generated_at":"2026-09-26T12:00:00Z","values":{"coding":{"score":0.5,"provenance":"imported","sample_count":1,"source":"s"}}}]}{"more":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ImportJSON([]byte(tc.doc), "test.json", 16); err == nil {
				t.Fatal("expected import rejection")
			}
		})
	}
	if _, err := ImportJSON([]byte(strings.Repeat("a", 8<<20+1)), "big.json", 16); err == nil {
		t.Fatal("oversized artifact must be rejected")
	}
}

func TestImportJSON_AttributesMissingSource(t *testing.T) {
	doc := `{"scorecards":[{"deployment_id":"p/m","version":1,"generated_at":"2026-09-26T12:00:00Z",
		"values":{"coding":{"score":0.5,"provenance":"imported","sample_count":3}}}]}`
	list, err := ImportJSON([]byte(doc), "artifacts/bench.json", 16)
	if err != nil {
		t.Fatalf("ImportJSON: %v", err)
	}
	if got := list[0].Values[DimCoding].Source; got != "artifacts/bench.json" {
		t.Fatalf("source = %q, want the artifact label", got)
	}
}

func TestScorecardJSONRoundTrip(t *testing.T) {
	sc := validScorecard()
	sc.Evaluation = &EvaluationRef{SuiteID: "coding", SuiteVersion: "1", RunID: "r1", SampleCount: 8, EvaluatedAt: time.Now().UTC()}
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	var back Scorecard
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped scorecard invalid: %v", err)
	}
	if back.Values[DimCoding].Provenance != ProvenanceEvaluation {
		t.Fatalf("provenance lost in round trip: %+v", back.Values[DimCoding])
	}
}

func TestDimensions_Classified(t *testing.T) {
	for _, d := range AllDimensions {
		if d.Kind() == "" {
			t.Fatalf("dimension %s has no kind", d)
		}
		_ = d.Unit()
	}
	if DimCoding.Kind() != KindQuality || DimLatencyMS.Kind() != KindPerformance || DimAvailability.Kind() != KindReliability || DimInputCostPerMTok.Kind() != KindCost {
		t.Fatal("dimension kinds are wrong")
	}
	if !DimThroughputTPS.HigherIsBetter() || DimLatencyMS.HigherIsBetter() {
		t.Fatal("direction flags are wrong")
	}
	if Dimension("nope").Valid() || Dimension("nope").Kind() != "" {
		t.Fatal("unknown dimension must be invalid")
	}
}
