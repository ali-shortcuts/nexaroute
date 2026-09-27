package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

func TestBuiltinSuites_AreValidAndBounded(t *testing.T) {
	suites := builtinSuites()
	if len(suites) == 0 {
		t.Fatal("no built-in suites")
	}
	seen := map[string]struct{}{}
	for _, s := range suites {
		if err := ValidateSuite(s); err != nil {
			t.Fatalf("suite %q invalid: %v", s.ID, err)
		}
		if !s.Dimension.ValidQuality() {
			t.Fatalf("suite %q must target a quality dimension, got %q", s.ID, s.Dimension)
		}
		if _, dup := seen[s.ID]; dup {
			t.Fatalf("duplicate suite id %q", s.ID)
		}
		seen[s.ID] = struct{}{}
		if _, ok := LookupSuite(s.ID); !ok {
			t.Fatalf("suite %q is not discoverable", s.ID)
		}
	}
	if _, ok := LookupSuite("does-not-exist"); ok {
		t.Fatal("unknown suite must not resolve")
	}
	if len(Catalog()) != len(suites) {
		t.Fatal("catalog must list every suite")
	}
}

func TestValidateSuite_Rejections(t *testing.T) {
	base, _ := LookupSuite("coding")
	cases := []struct {
		name   string
		mutate func(s *Suite)
	}{
		{"no id", func(s *Suite) { s.ID = "" }},
		{"no cases", func(s *Suite) { s.Cases = nil }},
		{"unknown dimension", func(s *Suite) { s.Dimension = "charisma" }},
		{"zero min samples", func(s *Suite) { s.MinSamples = 0 }},
		{"duplicate case", func(s *Suite) { s.Cases[1].ID = s.Cases[0].ID }},
		{"bad weight", func(s *Suite) { s.Cases[0].Weight = 0 }},
		{"bad expectation", func(s *Suite) { s.Cases[0].Expectation = Expectation{Kind: ExpectJSONSchema} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Cases = append([]Case(nil), base.Cases...)
			tc.mutate(&s)
			if err := ValidateSuite(s); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestExpectation_Valid(t *testing.T) {
	if !(Expectation{Kind: ExpectExactMatch}).Valid() {
		t.Fatal("exact match expectation must be valid")
	}
	if (Expectation{Kind: "vibes"}).Valid() {
		t.Fatal("unknown expectation kind must be invalid")
	}
	if (Expectation{Kind: ExpectToolCall}).Valid() {
		t.Fatal("tool call expectation without tool name must be invalid")
	}
	if (Expectation{Kind: ExpectRegex, Pattern: "["}).Valid() != true {
		t.Fatal("pattern presence is enough at validation time; compilation is the evaluator's job")
	}
}

func codingCase(t *testing.T, id string) Case {
	t.Helper()
	suite, _ := LookupSuite("coding")
	for _, c := range suite.Cases {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("case %q not found", id)
	return Case{}
}

func TestDeterministicEvaluators(t *testing.T) {
	cases := []struct {
		name    string
		expect  Expectation
		outcome Outcome
		want    Verdict
		evalID  string
	}{
		{"exact match pass", Expectation{Kind: ExpectExactMatch, Expected: "42"}, Outcome{Status: OutcomeOK, Output: "42"}, VerdictPass, "exact_match"},
		{"exact match fail", Expectation{Kind: ExpectExactMatch, Expected: "42"}, Outcome{Status: OutcomeOK, Output: "41"}, VerdictFail, "exact_match"},
		{"exact match status error", Expectation{Kind: ExpectExactMatch, Expected: "42"}, Outcome{Status: OutcomeTimeout, Output: "42"}, VerdictFail, "exact_match"},
		{"json valid pass", Expectation{Kind: ExpectJSONValid}, Outcome{Status: OutcomeOK, Output: `{"a":1}`}, VerdictPass, "json_valid"},
		{"json valid fail", Expectation{Kind: ExpectJSONValid}, Outcome{Status: OutcomeOK, Output: `{`}, VerdictFail, "json_valid"},
		{"schema pass", Expectation{Kind: ExpectJSONSchema, Schema: `{"required":["a"],"types":{"a":"string"}}`}, Outcome{Status: OutcomeOK, Output: `{"a":"x"}`}, VerdictPass, "json_schema"},
		{"schema missing key", Expectation{Kind: ExpectJSONSchema, Schema: `{"required":["a"]}`}, Outcome{Status: OutcomeOK, Output: `{"b":1}`}, VerdictFail, "json_schema"},
		{"schema wrong type", Expectation{Kind: ExpectJSONSchema, Schema: `{"types":{"a":"string"}}`}, Outcome{Status: OutcomeOK, Output: `{"a":2}`}, VerdictFail, "json_schema"},
		{"schema bad case json", Expectation{Kind: ExpectJSONSchema, Schema: `{`}, Outcome{Status: OutcomeOK, Output: `{"a":1}`}, VerdictError, "json_schema"},
		{"tool call pass", Expectation{Kind: ExpectToolCall, ToolName: "read_file"}, Outcome{Status: OutcomeOK, ToolCall: &ToolCall{Name: "read_file"}}, VerdictPass, "expected_tool_call"},
		{"tool call wrong name", Expectation{Kind: ExpectToolCall, ToolName: "read_file"}, Outcome{Status: OutcomeOK, ToolCall: &ToolCall{Name: "rm_rf"}}, VerdictFail, "expected_tool_call"},
		{"tool call missing", Expectation{Kind: ExpectToolCall, ToolName: "read_file"}, Outcome{Status: OutcomeOK}, VerdictFail, "expected_tool_call"},
		{"regex pass", Expectation{Kind: ExpectRegex, Pattern: `^ok$`}, Outcome{Status: OutcomeOK, Output: "ok"}, VerdictPass, "regex_match"},
		{"regex fail", Expectation{Kind: ExpectRegex, Pattern: `^ok$`}, Outcome{Status: OutcomeOK, Output: "not ok"}, VerdictFail, "regex_match"},
		{"regex bad pattern", Expectation{Kind: ExpectRegex, Pattern: `[`}, Outcome{Status: OutcomeOK, Output: "ok"}, VerdictError, "regex_match"},
		{"unit tests pass", Expectation{Kind: ExpectUnitTests}, Outcome{Status: OutcomeOK, UnitTests: &UnitTestResult{Compiled: true, Passed: 3}}, VerdictPass, "unit_tests"},
		{"unit tests compile failure", Expectation{Kind: ExpectUnitTests}, Outcome{Status: OutcomeOK, UnitTests: &UnitTestResult{Compiled: false, Passed: 0}}, VerdictFail, "unit_tests"},
		{"unit tests failures", Expectation{Kind: ExpectUnitTests}, Outcome{Status: OutcomeOK, UnitTests: &UnitTestResult{Compiled: true, Passed: 2, Failed: 1}}, VerdictFail, "unit_tests"},
		{"unit tests nothing ran", Expectation{Kind: ExpectUnitTests}, Outcome{Status: OutcomeOK, UnitTests: &UnitTestResult{Compiled: true}}, VerdictFail, "unit_tests"},
		{"unit tests missing", Expectation{Kind: ExpectUnitTests}, Outcome{Status: OutcomeOK}, VerdictFail, "unit_tests"},
		{"numeric pass", Expectation{Kind: ExpectNumericTol, Expected: "3.14", Tolerance: 0.01}, Outcome{Status: OutcomeOK, Output: "3.145"}, VerdictPass, "numeric_tolerance"},
		{"numeric fail", Expectation{Kind: ExpectNumericTol, Expected: "3.14", Tolerance: 0.001}, Outcome{Status: OutcomeOK, Output: "3.2"}, VerdictFail, "numeric_tolerance"},
		{"numeric bad output", Expectation{Kind: ExpectNumericTol, Expected: "3.14", Tolerance: 0.01}, Outcome{Status: OutcomeOK, Output: "pi"}, VerdictFail, "numeric_tolerance"},
		{"numeric bad expectation", Expectation{Kind: ExpectNumericTol, Expected: "pi", Tolerance: 0.01}, Outcome{Status: OutcomeOK, Output: "3.1"}, VerdictError, "numeric_tolerance"},
		{"stream terminated", Expectation{Kind: ExpectStreamTerminat}, Outcome{Status: OutcomeOK, Stream: &StreamResult{Terminated: true}}, VerdictPass, "stream_terminated"},
		{"stream not terminated", Expectation{Kind: ExpectStreamTerminat}, Outcome{Status: OutcomeOK, Stream: &StreamResult{Terminated: false}}, VerdictFail, "stream_terminated"},
		{"stream missing", Expectation{Kind: ExpectStreamTerminat}, Outcome{Status: OutcomeOK}, VerdictFail, "stream_terminated"},
	}
	reg := NewRegistry()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Case{ID: "case-1", Weight: 1, Expectation: tc.expect}
			var got *VerdictResult
			for _, e := range reg.Deterministic() {
				if !e.Supports(tc.expect.Kind) {
					continue
				}
				if e.ID() != tc.evalID {
					continue
				}
				v := e.Evaluate(context.Background(), c, tc.outcome)
				got = &v
			}
			if got == nil {
				t.Fatalf("evaluator %q not found for kind %q", tc.evalID, tc.expect.Kind)
			}
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %s (reason %q), want %s", got.Verdict, got.Reason, tc.want)
			}
			if got.Kind != KindDeterministic {
				t.Fatalf("built-in evaluators must be deterministic, got %s", got.Kind)
			}
		})
	}
}

func TestResolve_DeterministicAlwaysWins(t *testing.T) {
	detPass := VerdictResult{EvaluatorID: "unit_tests", Kind: KindDeterministic, Verdict: VerdictPass}
	detFail := VerdictResult{EvaluatorID: "unit_tests", Kind: KindDeterministic, Verdict: VerdictFail}
	detError := VerdictResult{EvaluatorID: "json_schema", Kind: KindDeterministic, Verdict: VerdictError}
	judgePass := VerdictResult{EvaluatorID: "llm_judge", Kind: KindJudge, Verdict: VerdictPass, Reason: "looks fine"}
	judgeFail := VerdictResult{EvaluatorID: "llm_judge", Kind: KindJudge, Verdict: VerdictFail, Reason: "looks wrong"}

	cases := []struct {
		name       string
		verdicts   []VerdictResult
		judge      bool
		want       Verdict
		wantReason string
	}{
		{"deterministic pass beats judge fail", []VerdictResult{detPass, judgeFail}, true, VerdictPass, ResolutionDeterministic},
		{"deterministic fail beats judge pass", []VerdictResult{detFail, judgePass}, true, VerdictFail, ResolutionDeterministic},
		{"deterministic error beats judge pass", []VerdictResult{detError, judgePass}, true, VerdictError, ResolutionDeterministic},
		{"judge only, enabled", []VerdictResult{judgePass}, true, VerdictPass, ResolutionJudge},
		{"judge only, disabled", []VerdictResult{judgePass}, false, VerdictUnjudged, ResolutionNone},
		{"no verdicts", nil, true, VerdictUnjudged, ResolutionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Resolve(tc.verdicts, tc.judge)
			if got != tc.want || reason != tc.wantReason {
				t.Fatalf("Resolve = %s/%q, want %s/%q", got, reason, tc.want, tc.wantReason)
			}
		})
	}
}

func TestJudgeEvaluator_DefaultsToUnjudged(t *testing.T) {
	j := NewJudgeEvaluator("", nil)
	if j.Kind() != KindJudge {
		t.Fatal("judge kind")
	}
	if j.ID() != "llm_judge" {
		t.Fatalf("default id = %q", j.ID())
	}
	res := j.Evaluate(context.Background(), Case{ID: "c"}, Outcome{})
	if res.Verdict != VerdictUnjudged {
		t.Fatalf("judge without implementation must stay unjudged, got %s", res.Verdict)
	}
}

func TestRegistry_BoundsAndDuplicates(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(NewJudgeEvaluator("llm_judge", nil)); err != nil {
		t.Fatalf("register judge: %v", err)
	}
	if err := reg.Register(exactMatchEvaluator()); err == nil {
		t.Fatal("duplicate id must be rejected")
	}
	if len(reg.Judges()) != 1 {
		t.Fatal("judge must be listed separately from deterministic evaluators")
	}
	ids := reg.IDs()
	if len(ids) == 0 {
		t.Fatal("ids must not be empty")
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("ids must be sorted: %v", ids)
		}
	}
}

func codingArtifacts(t *testing.T, verdicts ...Verdict) []Outcome {
	t.Helper()
	suite, _ := LookupSuite("coding")
	out := make([]Outcome, 0, len(suite.Cases))
	for i, c := range suite.Cases {
		if i >= len(verdicts) {
			break
		}
		o := Outcome{CaseID: c.ID, Status: OutcomeOK, LatencyMS: 100, UnitTests: &UnitTestResult{Compiled: true, Passed: 1}}
		if verdicts[i] == VerdictFail {
			o.UnitTests = &UnitTestResult{Compiled: true, Passed: 0, Failed: 1}
		}
		out = append(out, o)
	}
	return out
}

func TestRunner_ScoreAndScorecard(t *testing.T) {
	r := NewRunner()
	req := Request{DeploymentID: "p1/m1", ProviderID: "p1", Model: "m1", SuiteID: "coding", Outcomes: codingArtifacts(t, VerdictPass, VerdictPass, VerdictFail)}
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Scoreable {
		t.Fatalf("run should be scoreable: %+v", res)
	}
	if res.Samples != 3 {
		t.Fatalf("samples = %d, want 3", res.Samples)
	}
	// weights 1,1,1 over three cases and one failure -> 2/3
	if diff := res.Score - 2.0/3.0; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("score = %v, want %v", res.Score, 2.0/3.0)
	}
	if res.Counts[VerdictPass] != 2 || res.Counts[VerdictFail] != 1 {
		t.Fatalf("counts = %+v", res.Counts)
	}
	if res.Upstream != 0 {
		t.Fatalf("replay must not make upstream calls, got %d", res.Upstream)
	}
	sc, ok, err := res.Scorecard()
	if err != nil || !ok {
		t.Fatalf("scorecard ok=%v err=%v", ok, err)
	}
	v, present := sc.Quality(scorecards.DimCoding)
	if !present {
		t.Fatal("coding dimension missing from scorecard")
	}
	if v.Provenance != scorecards.ProvenanceEvaluation {
		t.Fatalf("provenance = %s", v.Provenance)
	}
	if v.SampleCount != 3 || v.SuiteVersion != "1" || v.EvaluatedAt.IsZero() {
		t.Fatalf("evaluation evidence incomplete: %+v", v)
	}
	// Every value must carry provenance; no dimension is invented.
	for d, val := range sc.Values {
		if !val.Provenance.Valid() {
			t.Fatalf("dimension %s has no provenance", d)
		}
	}
	if _, ok := sc.Quality(scorecards.DimDebugging); ok {
		t.Fatal("debugging must stay absent")
	}
	// Reliability evidence comes from statuses; latency needs a target.
	if _, ok := sc.Values[scorecards.DimAvailability]; !ok {
		t.Fatal("availability evidence missing")
	}
	if _, ok := sc.Values[scorecards.DimLatencyMS]; ok {
		t.Fatal("latency must be absent without a target")
	}
	withTarget := req
	withTarget.LatencyTargetMS = 1000
	res2, err := r.Run(context.Background(), withTarget)
	if err != nil {
		t.Fatal(err)
	}
	sc2, _, err := res2.Scorecard()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sc2.Values[scorecards.DimLatencyMS]; !ok {
		t.Fatal("latency must be present once a target is configured")
	}
}

func TestRunner_InsufficientSamplesProducesNoScorecard(t *testing.T) {
	r := NewRunner()
	res, err := r.Run(context.Background(), Request{
		DeploymentID: "p1/m1", SuiteID: "coding",
		Outcomes: codingArtifacts(t, VerdictPass),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scoreable {
		t.Fatal("one sample must not be scoreable for a 3-sample suite")
	}
	if res.Reason != "insufficient_samples" {
		t.Fatalf("reason = %q", res.Reason)
	}
	sc, ok, err := res.Scorecard()
	if err != nil || ok {
		t.Fatalf("no scorecard expected: ok=%v err=%v", ok, err)
	}
	if sc.DeploymentID != "" || len(sc.Values) != 0 {
		t.Fatal("no scorecard value may be fabricated")
	}
}

func TestRunner_MissingArtifactIsRecordedNotScored(t *testing.T) {
	r := NewRunner()
	artifacts := codingArtifacts(t, VerdictPass, VerdictPass)
	res, err := r.Run(context.Background(), Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[VerdictMissing] == 0 {
		t.Fatalf("missing artifacts must be recorded: %+v", res.Counts)
	}
	if res.Samples != 2 {
		t.Fatalf("samples = %d, want 2 (missing cases are not scored)", res.Samples)
	}
	if res.Scoreable {
		t.Fatal("2 decisive samples < suite minimum 3: run must stay unscoreable")
	}
}

func TestRunner_ErrorsAreNotFailures(t *testing.T) {
	r := NewRunner()
	suite, _ := LookupSuite("coding")
	outcomes := make([]Outcome, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		outcomes = append(outcomes, Outcome{CaseID: c.ID, Status: OutcomeError})
	}
	res, err := r.Run(context.Background(), Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: outcomes})
	if err != nil {
		t.Fatal(err)
	}
	// unit_tests evaluator sees no test result -> deterministic fail (evidence says
	// the model produced nothing usable), which is a decisive failure.
	if res.Counts[VerdictFail] != len(suite.Cases) {
		t.Fatalf("counts = %+v", res.Counts)
	}
	// Reliability evidence separates upstream status from verdicts.
	var availability *DimensionScore
	for i := range res.Extra {
		if res.Extra[i].Dimension == scorecards.DimAvailability {
			availability = &res.Extra[i]
		}
	}
	if availability == nil || availability.Score != 0 {
		t.Fatalf("availability must reflect the recorded statuses: %+v", res.Extra)
	}
}

func TestRunner_JudgeCannotOverrideDeterministicVerdict(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(NewJudgeEvaluator("llm_judge", func(context.Context, Case, Outcome) (Verdict, string) {
		return VerdictPass, "judge thinks it passed"
	})); err != nil {
		t.Fatal(err)
	}
	r := NewRunnerWithRegistry(reg)
	// Three failing coding artifacts; the judge claims every one passed.
	res, err := r.Run(context.Background(), Request{
		DeploymentID: "p1/m1", SuiteID: "coding", JudgeEnabled: true,
		Outcomes: codingArtifacts(t, VerdictFail, VerdictFail, VerdictFail),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[VerdictFail] != 3 || res.Score != 0 {
		t.Fatalf("deterministic failures must stand: counts=%+v score=%v", res.Counts, res.Score)
	}
	for _, cr := range res.Cases {
		if cr.Verdict == VerdictMissing {
			// No artifact for this case: nothing was decided, so nothing is claimed.
			continue
		}
		if cr.Resolution != ResolutionDeterministic {
			t.Fatalf("resolution = %q, want deterministic", cr.Resolution)
		}
		// The judge verdict is still recorded so disagreement is observable.
		found := false
		for _, v := range cr.Verdicts {
			if v.Kind == KindJudge && v.Verdict == VerdictPass {
				found = true
			}
		}
		if !found {
			t.Fatalf("judge verdict must be recorded: %+v", cr.Verdicts)
		}
	}
}

func TestRunner_JudgeUsedOnlyWhenNoDeterministicVerdict(t *testing.T) {
	reg := &Registry{}
	if err := reg.Register(NewJudgeEvaluator("llm_judge", func(context.Context, Case, Outcome) (Verdict, string) {
		return VerdictPass, "judge says ok"
	})); err != nil {
		t.Fatal(err)
	}
	r := NewRunnerWithRegistry(reg)
	res, err := r.Run(context.Background(), Request{
		DeploymentID: "p1/m1", SuiteID: "coding", JudgeEnabled: true,
		Outcomes: codingArtifacts(t, VerdictPass, VerdictPass, VerdictPass),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.JudgeUsed {
		t.Fatal("judge must be marked used when it is the only evaluator")
	}
	if res.Counts[VerdictPass] != 3 {
		t.Fatalf("judge verdicts should carry the run: %+v", res.Counts)
	}
	// With the judge disabled the same registry yields unjudged cases and no score.
	res2, err := r.Run(context.Background(), Request{
		DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: codingArtifacts(t, VerdictPass, VerdictPass, VerdictPass),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Scoreable {
		t.Fatal("unjudged cases must not be scored")
	}
	if res2.Counts[VerdictUnjudged] != 3 {
		t.Fatalf("counts = %+v", res2.Counts)
	}
}

func TestRunner_DeterministicAcrossRepeats(t *testing.T) {
	r := NewRunner()
	req := Request{DeploymentID: "p1/m1", SuiteID: "structured_output", Outcomes: []Outcome{
		{CaseID: "structured-issue-summary", Status: OutcomeOK, Output: `{"title":"x","severity":"high"}`},
		{CaseID: "structured-tool-arguments", Status: OutcomeOK, Output: `{"path":"/tmp"}`},
		{CaseID: "structured-no-prose-wrapping", Status: OutcomeOK, Output: `{"ok":true}`},
	}}
	first, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a := stripTiming(first)
	b := stripTiming(second)
	if a != b {
		t.Fatalf("runs are not deterministic:\n%s\n%s", a, b)
	}
}

func stripTiming(res Result) string {
	res.RunID = ""
	res.StartedAt = time.Time{}
	res.FinishedAt = time.Time{}
	res.DurationMS = 0
	b, _ := json.Marshal(res)
	return string(b)
}

type blockingExecutor struct{ delay time.Duration }

func (b blockingExecutor) Execute(ctx context.Context, _ Case) (Outcome, error) {
	select {
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	case <-time.After(b.delay):
		return Outcome{CaseID: "late", Status: OutcomeOK, Output: "late"}, nil
	}
}

func TestRunner_CaseTimeoutIsHonored(t *testing.T) {
	r := NewRunner()
	start := time.Now()
	res, err := r.RunWithExecutor(context.Background(), Request{
		DeploymentID: "p1/m1", SuiteID: "coding", CaseTimeoutMS: 25,
	}, blockingExecutor{delay: 2 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("case timeout not enforced: %s", elapsed)
	}
	if res.Counts[VerdictError] == 0 {
		t.Fatalf("timeout must be recorded as an error verdict: %+v", res.Counts)
	}
	for _, cr := range res.Cases {
		if cr.ErrorType != "case_timeout" {
			t.Fatalf("error type = %q, want case_timeout", cr.ErrorType)
		}
	}
}

func TestRunner_ContextCancellation(t *testing.T) {
	r := NewRunner()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Run(ctx, Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: codingArtifacts(t, VerdictPass)}); err == nil {
		t.Fatal("cancelled context must abort the run")
	}
}

func TestRunner_InputRejections(t *testing.T) {
	r := NewRunner()
	artifacts := codingArtifacts(t, VerdictPass, VerdictPass, VerdictPass)
	cases := []struct {
		name string
		req  Request
	}{
		{"unknown suite", Request{DeploymentID: "p1/m1", SuiteID: "nope", Outcomes: artifacts}},
		{"missing deployment", Request{SuiteID: "coding", Outcomes: artifacts}},
		{"duplicate artifacts", Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: append(artifacts, artifacts[0])}},
		{"oversized artifact count", Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: make([]Outcome, MaxOutcomesPerRun+1)}},
		{"oversized output", Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: []Outcome{{CaseID: "coding-bugfix", Status: OutcomeOK, Output: strings.Repeat("x", MaxOutputBytes+1)}}}},
		{"unknown status", Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: []Outcome{{CaseID: "coding-bugfix", Status: "wobbly"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), tc.req); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if _, err := r.RunWithExecutor(context.Background(), Request{DeploymentID: "p1/m1", SuiteID: "coding"}, nil); err == nil {
		t.Fatal("nil executor must be rejected")
	}
}

type failingExecutor struct{ err error }

func (f failingExecutor) Execute(context.Context, Case) (Outcome, error) {
	return Outcome{}, f.err
}

func TestRunner_ExecutorErrorIsRecorded(t *testing.T) {
	r := NewRunner()
	res, err := r.RunWithExecutor(context.Background(), Request{DeploymentID: "p1/m1", SuiteID: "coding"},
		failingExecutor{err: errors.New("harness exploded")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Counts[VerdictError] != len(res.Cases) {
		t.Fatalf("counts = %+v", res.Counts)
	}
	if res.Scoreable {
		t.Fatal("a run with no evidence must not be scoreable")
	}
}

func TestResultValidate(t *testing.T) {
	good := Result{RunID: "r", SuiteID: "coding", DeploymentID: "p/m", Score: 0.5}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	for _, bad := range []Result{
		{RunID: "", SuiteID: "coding", DeploymentID: "p/m"},
		{RunID: "r", SuiteID: "", DeploymentID: "p/m"},
		{RunID: "r", SuiteID: "coding", DeploymentID: ""},
		{RunID: "r", SuiteID: "coding", DeploymentID: "p/m", Score: 1.5},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid result accepted: %+v", bad)
		}
	}
}

func TestStore_BoundsAndAggregates(t *testing.T) {
	store := NewStore(2)
	for i := 0; i < 3; i++ {
		res := Result{
			RunID: "run-" + string(rune('a'+i)), SuiteID: "coding", DeploymentID: "p/m",
			Score: 0.5, Samples: 3, FinishedAt: time.Now().UTC(),
			Counts: map[Verdict]int{VerdictPass: i + 1},
		}
		if err := store.Save(res); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if store.Len() != 2 {
		t.Fatalf("store len = %d, want bounded at 2", store.Len())
	}
	recent := store.Recent(10)
	if len(recent) != 2 || recent[0].RunID != "run-c" || recent[1].RunID != "run-b" {
		t.Fatalf("recent = %+v", recent)
	}
	if _, ok := store.Get("run-a"); ok {
		t.Fatal("evicted run must not be readable")
	}
	counts := store.Counts()
	if counts[VerdictPass] != 2+3 {
		t.Fatalf("counts = %+v", counts)
	}
	if suites := store.SuiteCounts(); suites["coding"] != 2 {
		t.Fatalf("suite counts = %+v", suites)
	}
	store.Retain(map[string]struct{}{"other/dep": {}})
	if store.Len() != 0 {
		t.Fatal("retain must drop runs for removed deployments")
	}
}

func TestStore_IdempotentSaveAndRejectsInvalid(t *testing.T) {
	store := NewStore(4)
	res := Result{RunID: "run-1", SuiteID: "coding", DeploymentID: "p/m", Score: 0.5, Samples: 3, FinishedAt: time.Now().UTC()}
	if err := store.Save(res); err != nil {
		t.Fatal(err)
	}
	res.Score = 0.9
	if err := store.Save(res); err != nil {
		t.Fatal(err)
	}
	if store.Len() != 1 {
		t.Fatalf("re-saving a run must replace it: len=%d", store.Len())
	}
	got, _ := store.Get("run-1")
	if got.Score != 0.9 {
		t.Fatalf("score = %v", got.Score)
	}
	if err := store.Save(Result{RunID: "", SuiteID: "coding", DeploymentID: "p/m"}); err == nil {
		t.Fatal("invalid run must be rejected")
	}
}

func TestHealthFromRuns_IsDeterministicAndSeparate(t *testing.T) {
	now := time.Now().UTC()
	runs := []Result{
		{RunID: "r1", SuiteID: "coding", DeploymentID: "p2/m2", Samples: 3, Score: 1, FinishedAt: now},
		{RunID: "r2", SuiteID: "coding", DeploymentID: "p2/m2", Samples: 3, Score: 0, FinishedAt: now.Add(time.Minute)},
		{RunID: "r3", SuiteID: "coding", DeploymentID: "p1/m1", Samples: 0, Score: 0, FinishedAt: now},
	}
	health := HealthFromRuns(runs)
	if len(health) != 2 || health[0].DeploymentID != "p1/m1" || health[1].DeploymentID != "p2/m2" {
		t.Fatalf("health must be sorted by deployment: %+v", health)
	}
	if health[0].Status != "insufficient" {
		t.Fatalf("status = %q", health[0].Status)
	}
	if health[1].Runs != 2 || health[1].Samples != 6 {
		t.Fatalf("aggregate = %+v", health[1])
	}
	if diff := health[1].PassRate - 0.5; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("pass rate = %v", health[1].PassRate)
	}
}

func TestPercentile_Deterministic(t *testing.T) {
	values := []float64{5, 1, 3, 2, 4}
	if got := percentile(values, 0.5); got != 3 {
		t.Fatalf("p50 = %v, want 3", got)
	}
	if got := percentile(values, 0.95); got != 5 {
		t.Fatalf("p95 = %v, want 5", got)
	}
	if got := percentile(values, 0); got != 1 {
		t.Fatalf("p0 = %v, want 1", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty = %v", got)
	}
	// Input slice must not be reordered by scoring.
	if values[0] != 5 {
		t.Fatalf("percentile mutated its input: %+v", values)
	}
}

func TestSuiteCaseCountsAreBounded(t *testing.T) {
	for _, s := range builtinSuites() {
		if len(s.Cases) > MaxCasesPerSuite {
			t.Fatalf("suite %s exceeds case bound", s.ID)
		}
		if s.MinSamples > len(s.Cases) {
			t.Fatalf("suite %s requires more samples than it has cases", s.ID)
		}
	}
}
