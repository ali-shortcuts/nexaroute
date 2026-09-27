package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzRunner_Artifacts asserts the invariants that matter for Phase H: arbitrary
// (attacker-shaped) artifacts never panic the runner, never escape the verdict
// vocabulary, never exceed the verdict bound and never produce a score outside
// [0,1] or a scorecard without provenance.
func FuzzRunner_Artifacts(f *testing.F) {
	f.Add("coding", `[{"case_id":"coding-bugfix","status":"ok","unit_tests":{"compiled":true,"passed":1}}]`)
	f.Add("reasoning", `[{"case_id":"reasoning-multi-step-arithmetic","status":"ok","output":"42"}]`)
	f.Add("structured_output", `[{"case_id":"structured-no-prose-wrapping","status":"ok","output":"{\"a\":1}"}]`)
	f.Add("nope", `[]`)
	f.Add("tool_calling", `[{"case_id":"tools-single-call","status":"ok","tool_call":{"name":"search_files"}}]`)

	r := NewRunner()
	f.Fuzz(func(t *testing.T, suiteID, raw string) {
		var outcomes []Outcome
		if err := json.Unmarshal([]byte(raw), &outcomes); err != nil {
			// Not every input decodes; decoding failures must simply be rejected
			// by the replay executor rather than reaching the runner.
			if _, err := NewReplayExecutor([]Outcome{{CaseID: "x", Output: raw}}); err == nil && len(raw) > MaxOutputBytes {
				t.Fatalf("oversized output accepted")
			}
			return
		}
		res, err := r.Run(context.Background(), Request{
			DeploymentID: "p1/m1", ProviderID: "p1", SuiteID: suiteID, Outcomes: outcomes,
		})
		if err != nil {
			// Unknown suites and bound violations are legitimate rejections.
			return
		}
		if len(res.Cases) > MaxCasesPerSuite {
			t.Fatalf("case count escaped the bound: %d", len(res.Cases))
		}
		for _, cr := range res.Cases {
			if !cr.Verdict.Valid() {
				t.Fatalf("non-canonical verdict %q", cr.Verdict)
			}
			if len(cr.Verdicts) > MaxVerdictsPerCase {
				t.Fatalf("verdict count escaped the bound: %d", len(cr.Verdicts))
			}
			for _, vr := range cr.Verdicts {
				if len(vr.Reason) > MaxReasonBytes {
					t.Fatalf("verdict reason exceeds bound: %d", len(vr.Reason))
				}
			}
		}
		if res.Score < 0 || res.Score > 1 {
			t.Fatalf("score out of range: %v", res.Score)
		}
		if res.Samples < 0 || res.Samples > MaxOutcomesPerRun {
			t.Fatalf("samples out of range: %d", res.Samples)
		}
		sc, ok, err := res.Scorecard()
		if err != nil {
			t.Fatalf("scorecard conversion failed: %v", err)
		}
		if !ok {
			return
		}
		if err := sc.Validate(); err != nil {
			t.Fatalf("produced scorecard is invalid: %v", err)
		}
		for d, v := range sc.Values {
			if !v.Provenance.Valid() {
				t.Fatalf("dimension %s produced without provenance", d)
			}
			if v.SampleCount < 1 {
				t.Fatalf("dimension %s produced without samples", d)
			}
		}
	})
}

// FuzzResolve_Verdicts checks precedence under adversarial verdict mixes: a judge
// can never beat a deterministic verdict and the outcome is always canonical.
func FuzzResolve_Verdicts(f *testing.F) {
	f.Add("PASS", "FAIL", true)
	f.Add("FAIL", "PASS", true)
	f.Add("ERROR", "PASS", false)
	f.Add("", "", true)
	f.Fuzz(func(t *testing.T, det, judge string, judgeEnabled bool) {
		// Only canonical verdicts are meaningful; anything else must be ignored by
		// the resolver rather than treated as a decision.
		det = strings.ToLower(strings.TrimSpace(det))
		judge = strings.ToLower(strings.TrimSpace(judge))
		if det != string(VerdictPass) && det != string(VerdictFail) && det != string(VerdictError) {
			det = ""
		}
		if judge != string(VerdictPass) && judge != string(VerdictFail) {
			judge = ""
		}
		var verdicts []VerdictResult
		if det != "" {
			verdicts = append(verdicts, VerdictResult{EvaluatorID: "unit_tests", Kind: KindDeterministic, Verdict: Verdict(det)})
		}
		if judge != "" {
			verdicts = append(verdicts, VerdictResult{EvaluatorID: "llm_judge", Kind: KindJudge, Verdict: Verdict(judge)})
		}
		v, resolution := Resolve(verdicts, judgeEnabled)
		if !v.Valid() {
			t.Fatalf("resolved verdict %q is not canonical", v)
		}
		switch resolution {
		case ResolutionDeterministic, ResolutionJudge, ResolutionNone:
		default:
			t.Fatalf("unknown resolution category %q", resolution)
		}
		if det == string(VerdictFail) && v != VerdictFail {
			t.Fatalf("deterministic failure overridden: det=%s judge=%s -> %s", det, judge, v)
		}
		if det == string(VerdictPass) && v != VerdictPass {
			t.Fatalf("deterministic pass overridden: det=%s judge=%s -> %s", det, judge, v)
		}
	})
}
