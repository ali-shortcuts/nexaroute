package eval

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// mission95BaseResult is a minimal result that passes Result.Validate.
func mission95BaseResult() Result {
	return Result{RunID: "r1", SuiteID: "coding", SuiteVersion: "1", DeploymentID: "dep-a", Score: 0.5}
}

func mission95MustValid(t *testing.T, r Result) {
	t.Helper()
	if err := r.Validate(); err != nil {
		t.Fatalf("base result should validate: %v", err)
	}
}

// --- fnEvaluator / Verdict edges -------------------------------------------

func TestMission95FnEvaluatorEdges(t *testing.T) {
	ctx := context.Background()
	// A nil supports predicate accepts every expectation kind.
	any := &fnEvaluator{id: "any", kind: KindDeterministic, fn: func(Case, Outcome) (Verdict, string) {
		return VerdictPass, "ok"
	}}
	if !any.Supports("whatever") {
		t.Fatal("nil supports predicate must accept any kind")
	}
	// An unknown verdict is coerced to error, never trusted.
	bogus := &fnEvaluator{id: "bogus", kind: KindDeterministic, fn: func(Case, Outcome) (Verdict, string) {
		return Verdict("bogus"), "nonsense"
	}}
	res := bogus.Evaluate(ctx, Case{}, Outcome{})
	if res.Verdict != VerdictError || res.Reason != "evaluator returned an unknown verdict" {
		t.Fatalf("unknown verdict -> %+v want error/unknown-verdict reason", res)
	}
	if Verdict("bogus").Valid() {
		t.Fatal("unknown verdict must not be Valid")
	}
	// Reasons are bounded at MaxReasonBytes.
	long := &fnEvaluator{id: "long", kind: KindDeterministic, fn: func(Case, Outcome) (Verdict, string) {
		return VerdictPass, strings.Repeat("r", MaxReasonBytes+100)
	}}
	res = long.Evaluate(ctx, Case{}, Outcome{})
	if len(res.Reason) != MaxReasonBytes {
		t.Fatalf("reason len=%d want %d", len(res.Reason), MaxReasonBytes)
	}
}

func TestMission95SchemaWrongTypeAndNonFiniteNumeric(t *testing.T) {
	ctx := context.Background()
	schema := jsonSchemaEvaluator()
	c := Case{Expectation: Expectation{Kind: ExpectJSONSchema, Schema: `{"required":["n"],"types":{"n":"number"}}`}}
	res := schema.Evaluate(ctx, c, Outcome{Output: `{"n":"not-a-number"}`})
	if res.Verdict != VerdictFail || !strings.Contains(res.Reason, "wrong type") {
		t.Fatalf("wrong-type schema -> %+v want fail/wrong type", res)
	}
	// A present key with a matching type still passes.
	res = schema.Evaluate(ctx, c, Outcome{Output: `{"n": 7}`})
	if res.Verdict != VerdictPass {
		t.Fatalf("matching-type schema -> %+v want pass", res)
	}

	num := numericToleranceEvaluator()
	nc := Case{Expectation: Expectation{Kind: ExpectNumericTol, Expected: "3.14", Tolerance: 0.01}}
	for _, out := range []string{"NaN", "+Inf", "-Inf"} {
		res := num.Evaluate(ctx, nc, Outcome{Output: out})
		if res.Verdict != VerdictFail || res.Reason != "output is not finite" {
			t.Fatalf("numeric %q -> %+v want fail/not finite", out, res)
		}
	}

	stream := streamTerminatedEvaluator()
	res = stream.Evaluate(ctx, Case{}, Outcome{Stream: &StreamResult{Terminated: false, Error: "boom"}})
	if res.Verdict != VerdictFail || res.Reason != "stream ended with error" {
		t.Fatalf("errored stream -> %+v want fail/stream ended with error", res)
	}
}

// --- Registry rejection paths ----------------------------------------------

type mission95KindEvaluator struct {
	id   string
	kind EvaluatorKind
}

func (e mission95KindEvaluator) ID() string           { return e.id }
func (e mission95KindEvaluator) Kind() EvaluatorKind  { return e.kind }
func (e mission95KindEvaluator) Supports(string) bool { return true }
func (e mission95KindEvaluator) Evaluate(context.Context, Case, Outcome) VerdictResult {
	return VerdictResult{EvaluatorID: e.id, Kind: e.kind, Verdict: VerdictPass}
}

func TestMission95RegistryRejections(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("nil evaluator must be rejected")
	}
	if err := r.Register(mission95KindEvaluator{id: "", kind: KindDeterministic}); err == nil {
		t.Fatal("empty evaluator id must be rejected")
	}
	if err := r.Register(mission95KindEvaluator{id: "exact_match", kind: KindDeterministic}); err == nil {
		t.Fatal("duplicate evaluator id must be rejected")
	}
	if err := r.Register(mission95KindEvaluator{id: "weird-one", kind: EvaluatorKind("weird")}); err == nil {
		t.Fatal("unknown evaluator kind must be rejected")
	}
	// Fill the registry to the bound, then one more must fail.
	full := &Registry{}
	for i := 0; i < MaxEvaluators; i++ {
		if err := full.Register(mission95KindEvaluator{id: fmt.Sprintf("ev-%02d", i), kind: KindDeterministic}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	if err := full.Register(mission95KindEvaluator{id: "one-too-many", kind: KindDeterministic}); err == nil {
		t.Fatal("registering beyond MaxEvaluators must be rejected")
	}
}

// --- Outcome.Validate bounds ------------------------------------------------

func TestMission95OutcomeValidateBounds(t *testing.T) {
	cases := []struct {
		name string
		out  Outcome
		want string
	}{
		{"case id too long", Outcome{CaseID: strings.Repeat("c", 257)}, "case_id exceeds"},
		{"unit log too long", Outcome{CaseID: "c", UnitTests: &UnitTestResult{Log: strings.Repeat("l", MaxOutputBytes+1)}}, "test log exceeds"},
		{"stream error too long", Outcome{CaseID: "c", Stream: &StreamResult{Error: strings.Repeat("e", MaxReasonBytes+1)}}, "stream error exceeds"},
		{"tool name too long", Outcome{CaseID: "c", ToolCall: &ToolCall{Name: strings.Repeat("n", 257)}}, "tool call exceeds"},
		{"tool args too long", Outcome{CaseID: "c", ToolCall: &ToolCall{Name: "t", Arguments: strings.Repeat("a", MaxOutputBytes+1)}}, "tool call exceeds"},
		{"negative timing", Outcome{CaseID: "c", LatencyMS: -1}, "negative timing"},
		{"negative ttft", Outcome{CaseID: "c", TTFTMS: -5}, "negative timing"},
	}
	for _, tc := range cases {
		err := tc.out.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v want containing %q", tc.name, err, tc.want)
		}
	}
	if err := (Outcome{CaseID: "c", Status: OutcomeOK, Output: "ok"}).Validate(); err != nil {
		t.Fatalf("valid outcome rejected: %v", err)
	}
}

// --- Result.Validate consistency --------------------------------------------

func TestMission95ResultValidateBounds(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Result)
		want   string
	}{
		{"run id too long", func(r *Result) { r.RunID = strings.Repeat("r", 257) }, "exceeds safe limit"},
		{"model too long", func(r *Result) { r.Model = strings.Repeat("m", 1025) }, "text field exceeds"},
		{"reason too long", func(r *Result) { r.Reason = strings.Repeat("x", MaxReasonBytes+1) }, "text field exceeds"},
		{"extra too many", func(r *Result) {
			for i := 0; i < 65; i++ {
				r.Extra = append(r.Extra, DimensionScore{})
			}
		}, "collection bound"},
		{"evaluators too many", func(r *Result) {
			for i := 0; i < MaxEvaluators+1; i++ {
				r.Evaluators = append(r.Evaluators, fmt.Sprintf("e%d", i))
			}
		}, "collection bound"},
		{"negative counters", func(r *Result) { r.DurationMS = -1 }, "negative counters"},
		{"case without id", func(r *Result) {
			r.Cases = []CaseResult{{Verdict: VerdictPass}}
		}, "case result exceeds"},
		{"case invalid verdict", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: Verdict("bogus"), Weight: 1}}
		}, "case result is invalid"},
		{"case negative latency", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1, LatencyMS: -1}}
		}, "case result is invalid"},
		{"case NaN weight", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: math.NaN()}}
		}, "case result is invalid"},
		{"too many verdicts", func(r *Result) {
			vr := make([]VerdictResult, 0, 9)
			for i := 0; i < 9; i++ {
				vr = append(vr, VerdictResult{EvaluatorID: "e", Kind: KindDeterministic, Verdict: VerdictPass})
			}
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1, Verdicts: vr}}
		}, "verdict bound"},
		{"verdict without evaluator id", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1,
				Verdicts: []VerdictResult{{Kind: KindDeterministic, Verdict: VerdictPass}}}}
		}, "verdict result exceeds"},
		{"verdict unknown kind", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1,
				Verdicts: []VerdictResult{{EvaluatorID: "e", Kind: EvaluatorKind("weird"), Verdict: VerdictPass}}}}
		}, "unknown kind"},
		{"counts negative", func(r *Result) { r.Counts = map[Verdict]int{VerdictPass: -1} }, "counts are invalid"},
		{"counts unknown verdict", func(r *Result) { r.Counts = map[Verdict]int{Verdict("bogus"): 1} }, "counts are invalid"},
		{"counts inconsistent", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1}}
			r.Counts = map[Verdict]int{VerdictFail: 1}
			r.Samples = 1
		}, "counts are inconsistent"},
		{"counts incomplete", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1}}
			r.Counts = map[Verdict]int{}
			r.Samples = 1
		}, "counts are incomplete"},
		{"samples inconsistent", func(r *Result) {
			r.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1}}
			r.Counts = map[Verdict]int{VerdictPass: 1}
			r.Samples = 0
		}, "sample count is inconsistent"},
		{"evaluator id empty", func(r *Result) { r.Evaluators = []string{""} }, "evaluator id exceeds"},
		{"quality invalid", func(r *Result) { r.Quality = &DimensionScore{Score: 2} }, "dimension score is invalid"},
		{"extra invalid", func(r *Result) { r.Extra = []DimensionScore{{Score: -0.5}} }, "dimension score is invalid"},
	}
	for _, tc := range cases {
		r := mission95BaseResult()
		tc.mutate(&r)
		err := r.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v want containing %q", tc.name, err, tc.want)
		}
	}
	mission95MustValid(t, mission95BaseResult())
	// A consistent run with case detail validates.
	good := mission95BaseResult()
	good.Cases = []CaseResult{{CaseID: "c1", Verdict: VerdictPass, Weight: 1}}
	good.Counts = map[Verdict]int{VerdictPass: 1}
	good.Samples = 1
	mission95MustValid(t, good)
}

// --- Runner surface ----------------------------------------------------------

func TestMission95RunnerRegistrySurface(t *testing.T) {
	if got := NewRunnerWithRegistry(nil).Evaluators(); got == nil {
		t.Fatal("nil registry must fall back to the built-in registry")
	}
	ids := NewRunner().Evaluators().IDs()
	found := false
	for _, id := range ids {
		if id == "exact_match" {
			found = true
		}
	}
	if !found {
		t.Fatalf("built-in registry ids=%v want exact_match present", ids)
	}
}

func TestMission95RunWithSuiteRejections(t *testing.T) {
	r := NewRunner()
	exec, err := NewReplayExecutor(nil)
	if err != nil {
		t.Fatal(err)
	}
	validSuite := Suite{ID: "s1", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 1,
		Cases: []Case{{ID: "c1", Weight: 1, Expectation: Expectation{Kind: ExpectExactMatch, Expected: "ok"}}}}
	ctx := context.Background()
	if _, err := r.RunWithSuite(ctx, Request{DeploymentID: "d"}, Suite{}, exec); err == nil {
		t.Fatal("empty suite id must be rejected")
	}
	if _, err := r.RunWithSuite(ctx, Request{DeploymentID: "d", SuiteID: "other"}, validSuite, exec); err == nil {
		t.Fatal("suite id mismatch must be rejected")
	}
	badSuite := validSuite
	badSuite.Cases = nil
	if _, err := r.RunWithSuite(ctx, Request{DeploymentID: "d"}, badSuite, exec); err == nil {
		t.Fatal("invalid suite must be rejected")
	}
	over := make([]Outcome, MaxOutcomesPerRun+1)
	if _, err := r.RunWithSuite(ctx, Request{DeploymentID: "d", Outcomes: over}, validSuite, exec); err == nil {
		t.Fatal("outcome count above the bound must be rejected")
	}
}

type mission95DeadlineExecutor struct {
	deadline time.Time
	hasDead  bool
	outcome  Outcome
}

func (e *mission95DeadlineExecutor) Execute(ctx context.Context, c Case) (Outcome, error) {
	e.deadline, e.hasDead = ctx.Deadline()
	return e.outcome, nil
}

func TestMission95CaseTimeoutIsCapped(t *testing.T) {
	suite := Suite{ID: "s1", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 1,
		Cases: []Case{{ID: "c1", Weight: 1, Expectation: Expectation{Kind: ExpectExactMatch, Expected: "ok"}}}}
	exec := &mission95DeadlineExecutor{outcome: Outcome{CaseID: "c1", Status: OutcomeOK, Output: "ok"}}
	start := time.Now()
	_, err := NewRunner().RunWithSuite(context.Background(),
		Request{DeploymentID: "d", CaseTimeoutMS: 10 * MaxRunTimeoutMS}, suite, exec)
	if err != nil {
		t.Fatalf("capped-timeout run failed: %v", err)
	}
	if !exec.hasDead {
		t.Fatal("case context must carry a deadline when a timeout is set")
	}
	// The cap is MaxRunTimeoutMS (60s): the deadline must sit near
	// start+cap, not near start+10*cap.
	if d := exec.deadline.Sub(start); d < 55*time.Second || d > 65*time.Second {
		t.Fatalf("case deadline offset=%v want ~60s (capped from 600s)", d)
	}
}

type mission95InvalidExecutor struct{}

func (mission95InvalidExecutor) Execute(context.Context, Case) (Outcome, error) {
	return Outcome{CaseID: ""}, nil
}

func TestMission95InvalidArtifactRecorded(t *testing.T) {
	suite := Suite{ID: "s1", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 1,
		Cases: []Case{{ID: "c1", Weight: 1, Expectation: Expectation{Kind: ExpectExactMatch, Expected: "ok"}}}}
	res, err := NewRunner().RunWithSuite(context.Background(), Request{DeploymentID: "d"}, suite, mission95InvalidExecutor{})
	if err != nil {
		t.Fatalf("invalid artifact must be recorded, not fail the run: %v", err)
	}
	if len(res.Cases) != 1 {
		t.Fatalf("cases=%d want 1", len(res.Cases))
	}
	cr := res.Cases[0]
	if cr.Verdict != VerdictError || cr.Resolution != "invalid-artifact" || cr.ErrorType != "invalid_artifact" {
		t.Fatalf("invalid artifact case=%+v want error/invalid-artifact/invalid_artifact", cr)
	}
}

func TestMission95VerdictCapStopsJudges(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("cap-det-%d", i)
		if err := reg.Register(&fnEvaluator{id: id, kind: KindDeterministic,
			supports: func(string) bool { return true },
			fn:       func(Case, Outcome) (Verdict, string) { return VerdictPass, "cap" }}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Register(NewJudgeEvaluator("cap-judge", func(context.Context, Case, Outcome) (Verdict, string) {
		return VerdictPass, "judge"
	})); err != nil {
		t.Fatal(err)
	}
	suite := Suite{ID: "s1", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 1,
		Cases: []Case{{ID: "c1", Weight: 1, Expectation: Expectation{Kind: ExpectExactMatch, Expected: "ok"}}}}
	exec, err := NewReplayExecutor([]Outcome{{CaseID: "c1", Status: OutcomeOK, Output: "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewRunnerWithRegistry(reg).RunWithSuite(context.Background(),
		Request{DeploymentID: "d", JudgeEnabled: true}, suite, exec)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cases) != 1 || len(res.Cases[0].Verdicts) != MaxVerdictsPerCase {
		t.Fatalf("verdicts=%d want capped at %d", len(res.Cases[0].Verdicts), MaxVerdictsPerCase)
	}
	if res.JudgeUsed {
		t.Fatal("judge must not run once the verdict cap is reached")
	}
	if res.Cases[0].Verdict != VerdictPass {
		t.Fatalf("verdict=%s want pass", res.Cases[0].Verdict)
	}
}

func TestMission95Clamp01AndPercentile(t *testing.T) {
	if got := clamp01(math.NaN()); got != 0 {
		t.Fatalf("clamp01(NaN)=%v want 0", got)
	}
	if got := clamp01(math.Inf(1)); got != 0 {
		t.Fatalf("clamp01(+Inf)=%v want 0", got)
	}
	if got := clamp01(-0.5); got != 0 {
		t.Fatalf("clamp01(-0.5)=%v want 0", got)
	}
	if got := clamp01(1.5); got != 1 {
		t.Fatalf("clamp01(1.5)=%v want 1", got)
	}
	if got := clamp01(0.25); got != 0.25 {
		t.Fatalf("clamp01(0.25)=%v want 0.25", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("percentile(empty)=%v want 0", got)
	}
	if got := percentile([]float64{3, 1, 2}, 0); got != 1 {
		t.Fatalf("percentile(p<=0)=%v want 1", got)
	}
	if got := percentile([]float64{3, 1, 2}, 1); got != 3 {
		t.Fatalf("percentile(p>=1)=%v want 3", got)
	}
	// Note: the idx<0 / idx>=len defensive clamps in percentile are only
	// reachable via NaN, whose float-to-int conversion is
	// implementation-defined (amd64 wraps to the max clamp, arm64 to the
	// min clamp), so no portable assertion is made for them here.
}

// --- Store surface ------------------------------------------------------------

func TestMission95StoreSurface(t *testing.T) {
	s := NewStore(0) // clamped to 64
	for i := 0; i < 70; i++ {
		if err := s.Save(Result{RunID: fmt.Sprintf("r%02d", i), SuiteID: "coding", DeploymentID: "dep-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Len(); got != 64 {
		t.Fatalf("Len=%d want clamped bound 64", got)
	}
	s2 := NewStore(MaxStoredRuns + 100)
	for i := 0; i < 70; i++ {
		if err := s2.Save(Result{RunID: fmt.Sprintf("r%02d", i), SuiteID: "coding", DeploymentID: "dep-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s2.Len(); got != 64 {
		t.Fatalf("oversized maxRuns Len=%d want clamped 64", got)
	}

	s3 := NewStore(16)
	for _, id := range []string{"a", "b", "c"} {
		if err := s3.Save(Result{RunID: id, SuiteID: "coding", DeploymentID: "dep-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	all := s3.All()
	if len(all) != 3 || all[0].RunID != "a" || all[2].RunID != "c" {
		t.Fatalf("All order=%v want [a b c] oldest first", []string{all[0].RunID, all[1].RunID, all[2].RunID})
	}
	// Retain keeps listed deployments and drops the rest.
	s3.Retain(map[string]struct{}{"dep-a": {}, "dep-c": {}})
	if got := s3.Len(); got != 2 {
		t.Fatalf("after Retain Len=%d want 2", got)
	}
	if _, ok := s3.Get("b"); ok {
		t.Fatal("dropped run must not be retrievable")
	}
	if _, ok := s3.Get("a"); !ok {
		t.Fatal("kept run must remain retrievable")
	}

	keys := SortedVerdictKeys()
	sorted := append([]Verdict(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i := range keys {
		if keys[i] != sorted[i] {
			t.Fatalf("SortedVerdictKeys=%v not in canonical sorted order", keys)
		}
	}
	if len(keys) != len(AllVerdicts) {
		t.Fatalf("SortedVerdictKeys len=%d want %d", len(keys), len(AllVerdicts))
	}
	// Mutating the returned slice must not affect the next call.
	keys[0] = Verdict("mutated")
	if fresh := SortedVerdictKeys(); fresh[0] == Verdict("mutated") {
		t.Fatal("SortedVerdictKeys must return a fresh copy")
	}
}

// --- Suite catalog / lookup / validation --------------------------------------

func TestMission95SuiteCatalogAndLookup(t *testing.T) {
	op := Suite{ID: "operator-custom", Version: "2", Dimension: scorecards.DimCoding, MinSamples: 1,
		Cases: []Case{{ID: "c1", Weight: 1, Expectation: Expectation{Kind: ExpectExactMatch, Expected: "ok"}}}}
	entries := CatalogWith([]Suite{op})
	found := false
	for _, e := range entries {
		if e.ID == "operator-custom" {
			found = true
			if e.Source != "operator" || e.Cases != 1 || e.Version != "2" {
				t.Fatalf("operator entry=%+v want source=operator cases=1 version=2", e)
			}
		}
	}
	if !found {
		t.Fatal("operator suite missing from catalog")
	}
	if s, ok := LookupWith("operator-custom", []Suite{op}); !ok || s.ID != "operator-custom" {
		t.Fatalf("LookupWith operator suite ok=%v suite=%+v", ok, s)
	}
	// An operator id colliding with a built-in never shadows it.
	if _, ok := LookupWith("coding", []Suite{{ID: "coding", Version: "9"}}); ok {
		t.Fatal("colliding operator id must not resolve")
	}
	if _, ok := LookupWith("coding", nil); !ok {
		t.Fatal("built-in lookup must succeed")
	}
	if _, ok := LookupWith("no-such-suite", nil); ok {
		t.Fatal("unknown suite must not resolve")
	}

	base := Suite{ID: "s1", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 1,
		Cases: []Case{{ID: "c1", Weight: 1, Expectation: Expectation{Kind: ExpectExactMatch, Expected: "ok"}}}}
	bad := base
	bad.MinSamples = 0
	if err := ValidateSuite(bad); err == nil {
		t.Fatal("MinSamples 0 must be rejected")
	}
	bad = base
	bad.Cases = []Case{{ID: "c1", Weight: 1, Expectation: Expectation{Kind: "vibes"}}}
	if err := ValidateSuite(bad); err == nil {
		t.Fatal("invalid expectation kind must be rejected")
	}
}

// --- Operator suite packs ------------------------------------------------------

func mission95ValidSuiteJSON(id string) string {
	return fmt.Sprintf(`{"id":%q,"version":"1","dimension":"coding","MinSamples":1,"MaxCases":128,`+
		`"cases":[{"id":"c1","weight":1,"expectation":{"kind":"exact_match","expected":"ok"}}]}`, id)
}

func TestMission95ParseSuiteEdges(t *testing.T) {
	if _, err := ParseSuite(nil); err == nil {
		t.Fatal("empty pack must be rejected")
	}
	if _, err := ParseSuite(make([]byte, MaxSuitePackBytes+1)); err == nil {
		t.Fatal("oversized pack must be rejected")
	}
	if _, err := ParseSuites(make([]byte, MaxSuitePackBytes+1)); err == nil {
		t.Fatal("oversized multi-pack must be rejected")
	}
	if _, err := ParseSuites([]byte("   \n\t ")); err == nil {
		t.Fatal("whitespace-only pack must be rejected")
	}
	if _, err := ParseSuites([]byte("42")); err == nil {
		t.Fatal("non-object/array pack must be rejected")
	}
	if _, err := ParseSuites([]byte(`[{"id":`)); err == nil {
		t.Fatal("malformed array pack must be rejected")
	}
	list, err := ParseSuites([]byte(`[` + mission95ValidSuiteJSON("zeta") + `,` + mission95ValidSuiteJSON("alpha") + `]`))
	if err != nil {
		t.Fatalf("valid array pack rejected: %v", err)
	}
	if len(list) != 2 || list[0].ID != "alpha" || list[1].ID != "zeta" {
		t.Fatalf("array pack order=%v want sorted [alpha zeta]", list)
	}
	if _, err := ParseSuites([]byte(`[]`)); err == nil {
		t.Fatal("empty array pack must be rejected")
	}
	if _, err := ParseSuites([]byte(`[` + mission95ValidSuiteJSON("dup") + `,` + mission95ValidSuiteJSON("dup") + `]`)); err == nil {
		t.Fatal("duplicate suite ids in a pack must be rejected")
	}
	invalid := mission95ValidSuiteJSON("bad")
	invalid = strings.Replace(invalid, `"version":"1"`, `"version":""`, 1)
	if _, err := ParseSuites([]byte(`[` + invalid + `]`)); err == nil {
		t.Fatal("invalid suite inside a pack must be rejected")
	}
	// 33 suites exceed the pack bound.
	var many strings.Builder
	many.WriteString("[")
	for i := 0; i < MaxOperatorSuites+1; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(mission95ValidSuiteJSON(fmt.Sprintf("s%02d", i)))
	}
	many.WriteString("]")
	if _, err := ParseSuites([]byte(many.String())); err == nil {
		t.Fatal("pack above MaxOperatorSuites must be rejected")
	}
}

func TestMission95ParseSuitesEnvelopeEdges(t *testing.T) {
	good := `{"suites":[` + mission95ValidSuiteJSON("env-one") + `]}`
	list, err := ParseSuites([]byte(good))
	if err != nil || len(list) != 1 || list[0].ID != "env-one" {
		t.Fatalf("valid envelope -> list=%v err=%v", list, err)
	}
	if _, err := ParseSuites([]byte(`{"suites":[` + mission95ValidSuiteJSON("env-one") + `],"extra":1}`)); err == nil {
		t.Fatal("envelope with extra keys must be rejected")
	}
	if _, err := ParseSuites([]byte(`{"suites":"not-an-array"}`)); err == nil {
		t.Fatal("non-array suites payload must be rejected")
	}
	if _, err := ParseSuites([]byte(`{"suites":[]}`)); err == nil {
		t.Fatal("empty envelope suites must be rejected")
	}
}

func TestMission95LoadSuitesFromPathEdges(t *testing.T) {
	list, err := LoadSuitesFromPath("   ")
	if err != nil || list != nil {
		t.Fatalf("blank path -> list=%v err=%v want nil,nil", list, err)
	}
	if _, err := LoadSuitesFromPath(strings.Repeat("p", MaxSuitesPathBytes+1)); err == nil {
		t.Fatal("oversized path must be rejected")
	}
	if _, err := LoadSuitesFromPath(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("missing path must be rejected")
	}
	// A directory with no packs, plus non-json and subdirectory entries.
	dir := t.TempDir()
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("empty directory must be rejected")
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a pack"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(mission95ValidSuiteJSON("zeta")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(mission95ValidSuiteJSON("alpha")), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err = LoadSuitesFromPath(dir)
	if err != nil {
		t.Fatalf("directory load failed: %v", err)
	}
	if len(list) != 2 || list[0].ID != "alpha" || list[1].ID != "zeta" {
		t.Fatalf("directory load order=%v want [alpha zeta] (txt + subdir skipped)", list)
	}
	// A malformed pack inside a directory aborts the whole load.
	if err := os.WriteFile(filepath.Join(dir, "c.json"), []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("malformed pack in a directory must abort the load")
	}
	if err := os.Remove(filepath.Join(dir, "c.json")); err != nil {
		t.Fatal(err)
	}
	// A file pack that is itself malformed is rejected with its path context.
	badFile := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badFile, []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuitesFromPath(badFile); err == nil {
		t.Fatal("malformed file pack must be rejected")
	}
	if err := os.Remove(badFile); err != nil {
		t.Fatal(err)
	}
	// An oversized pack file inside a directory is rejected before reading.
	big := make([]byte, MaxSuitePackBytes+1)
	for i := range big {
		big[i] = ' '
	}
	if err := os.WriteFile(filepath.Join(dir, "big.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("oversized pack file must be rejected")
	}
	if err := os.Remove(filepath.Join(dir, "big.json")); err != nil {
		t.Fatal(err)
	}
	// Duplicates across files and totals above the bound are rejected.
	if err := os.WriteFile(filepath.Join(dir, "d.json"), []byte(mission95ValidSuiteJSON("alpha")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("duplicate suite across files must be rejected")
	}
	if err := os.Remove(filepath.Join(dir, "d.json")); err != nil {
		t.Fatal(err)
	}
	manyDir := t.TempDir()
	for f := 0; f < 2; f++ {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < 20; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(mission95ValidSuiteJSON(fmt.Sprintf("m%02d-%02d", f, i)))
		}
		b.WriteString("]")
		if err := os.WriteFile(filepath.Join(manyDir, fmt.Sprintf("p%d.json", f)), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadSuitesFromPath(manyDir); err == nil {
		t.Fatal("total above MaxOperatorSuites across files must be rejected")
	}
}

func TestMission95SchemaMissingTypeKeySkipped(t *testing.T) {
	schema := jsonSchemaEvaluator()
	c := Case{Expectation: Expectation{Kind: ExpectJSONSchema, Schema: `{"required":["n"],"types":{"ghost":"string","n":"number"}}`}}
	res := schema.Evaluate(context.Background(), c, Outcome{Output: `{"n": 7}`})
	if res.Verdict != VerdictPass {
		t.Fatalf("absent non-required type key -> %+v want pass", res)
	}
}

func TestMission95FnEvaluatorSkipVerdict(t *testing.T) {
	skip := &fnEvaluator{id: "skip", kind: KindDeterministic, fn: func(Case, Outcome) (Verdict, string) {
		return VerdictSkip, "not applicable"
	}}
	res := skip.Evaluate(context.Background(), Case{}, Outcome{})
	if res.Verdict != VerdictSkip {
		t.Fatalf("skip verdict -> %+v want skip", res)
	}
}

func TestMission95ScorecardFromEvaluationError(t *testing.T) {
	r := mission95BaseResult()
	r.Scoreable = true
	r.Quality = &DimensionScore{Dimension: "", Score: 0.5, Samples: 10}
	// FinishedAt stays zero, so the evidence timestamp check fails.
	_, ok, err := r.Scorecard()
	if err == nil || ok || !strings.Contains(err.Error(), "evaluation timestamp is required") {
		t.Fatalf("zero-timestamp scorecard ok=%v err=%v", ok, err)
	}
}

func TestMission95ValidateSuiteCaseBounds(t *testing.T) {
	cases := make([]Case, MaxCasesPerSuite+1)
	for i := range cases {
		cases[i] = Case{ID: fmt.Sprintf("c-%d", i)}
	}
	if err := ValidateSuite(Suite{ID: "big", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 1, Cases: cases}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized suite err=%v want exceeds", err)
	}
	if err := ValidateSuite(Suite{ID: "noid", Version: "1", Dimension: scorecards.DimCoding, MinSamples: 1, Cases: []Case{{}}}); err == nil || !strings.Contains(err.Error(), "without id") {
		t.Fatalf("caseless-id suite err=%v want without id", err)
	}
}
