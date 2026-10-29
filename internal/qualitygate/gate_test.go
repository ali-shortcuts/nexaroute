package qualitygate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

func prov(commit, model, deployment, mode string) Provenance {
	return Provenance{
		GatewayCommit: commit,
		Model:         model,
		DeploymentID:  deployment,
		ProviderID:    "prov-1",
		Mode:          mode,
		GeneratedAt:   time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
}

func f(v float64) *float64 { return &v }

// runResult builds a validated eval.Result for one suite: pass passing
// cases, fail failing cases, err error cases, each with the given
// latency/TTFT samples (one per case, in order).
func runResult(t *testing.T, suiteID string, pass, fail, err int, latencies, ttfts []int64, upstream int) eval.Result {
	t.Helper()
	var cases []eval.CaseResult
	add := func(n int, v eval.Verdict) {
		for i := 0; i < n; i++ {
			// Timings are assigned by global case index, not by verdict
			// group, so a mixed pass/fail run keeps its intended order.
			idx := len(cases)
			cr := eval.CaseResult{CaseID: suiteID + "-case", Verdict: v, Resolution: "deterministic", Weight: 1}
			if idx < len(latencies) {
				cr.LatencyMS = latencies[idx]
			}
			if idx < len(ttfts) {
				cr.TTFTMS = ttfts[idx]
			}
			cases = append(cases, cr)
		}
	}
	add(pass, eval.VerdictPass)
	add(fail, eval.VerdictFail)
	add(err, eval.VerdictError)
	counts := map[eval.Verdict]int{}
	for _, c := range cases {
		counts[c.Verdict]++
	}
	decisive := pass + fail
	score := 0.0
	if decisive > 0 {
		score = float64(pass) / float64(decisive)
	}
	// Every built-in gate suite has MinSamples 3; use 3 decisive cases
	// as the scoreable baseline shape unless the caller wants otherwise.
	scoreable := decisive >= 3
	r := eval.Result{
		RunID:        suiteID + "-run",
		SuiteID:      suiteID,
		SuiteVersion: "1",
		DeploymentID: "dep-1",
		ProviderID:   "prov-1",
		Model:        "model-1",
		StartedAt:    time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
		FinishedAt:   time.Date(2026, 10, 27, 12, 0, 1, 0, time.UTC),
		Cases:        cases,
		Counts:       counts,
		Score:        score,
		Samples:      decisive,
		Scoreable:    scoreable,
		Upstream:     upstream,
		Evaluators:   []string{"exact_match"},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("test result for %s invalid: %v", suiteID, err)
	}
	return r
}

func allSuitesInput(t *testing.T, pass, fail int, cost *float64) Input {
	t.Helper()
	runs := make([]eval.Result, 0, len(DefaultSuites))
	for _, s := range DefaultSuites {
		runs = append(runs, runResult(t, s, pass, fail, 0,
			[]int64{100, 110, 120, 130}, []int64{20, 25, 30, 35}, 0))
	}
	in := Input{Format: FormatInput, Provenance: prov("e6c3cdf3", "model-1", "dep-1", "replay"), Runs: runs, ObservedCostUSD: cost}
	if err := in.Validate(); err != nil {
		t.Fatalf("test input invalid: %v", err)
	}
	return in
}

func summarize(t *testing.T, in Input) Summary {
	t.Helper()
	s, err := Summarize(in)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	return s
}

func TestSummarize_GateSuites(t *testing.T) {
	s := summarize(t, allSuitesInput(t, 3, 1, f(2.50)))
	if len(s.Suites) != len(DefaultSuites) {
		t.Fatalf("suites = %d, want %d", len(s.Suites), len(DefaultSuites))
	}
	for _, suite := range s.Suites {
		if !suite.ScoreObserved || suite.Score != 0.75 {
			t.Fatalf("suite %s score = %v observed=%v, want 0.75 observed", suite.SuiteID, suite.Score, suite.ScoreObserved)
		}
		if !suite.LatencyObserved || suite.LatencyP95MS != 130 {
			t.Fatalf("suite %s latency p95 = %v, want 130", suite.SuiteID, suite.LatencyP95MS)
		}
		if !suite.TTFTObserved || suite.TTFTP95MS != 35 {
			t.Fatalf("suite %s ttft p95 = %v, want 35", suite.SuiteID, suite.TTFTP95MS)
		}
		if suite.ErrorRate != 0 || !suite.ErrorRateObserved {
			t.Fatalf("suite %s error rate = %v, want 0 observed", suite.SuiteID, suite.ErrorRate)
		}
	}
	if s.ObservedCostUSD == nil || *s.ObservedCostUSD != 2.50 {
		t.Fatalf("observed cost = %v, want 2.50", s.ObservedCostUSD)
	}
}

func TestCompare_NoRegression(t *testing.T) {
	rep, err := Compare(summarize(t, allSuitesInput(t, 4, 0, f(1.0))), summarize(t, allSuitesInput(t, 4, 0, f(1.0))), DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overall != OverallNoRegression {
		t.Fatalf("overall = %s, want %s", rep.Overall, OverallNoRegression)
	}
	if rep.RoutingChanged || rep.RoutingAction != "none" {
		t.Fatalf("report must never change routing: action=%q changed=%v", rep.RoutingAction, rep.RoutingChanged)
	}
}

func TestCompare_RegressionOnSuccessAndLatency(t *testing.T) {
	base := summarize(t, allSuitesInput(t, 4, 0, f(1.0)))
	// Candidate: one suite drops from 1.0 to 0.5 success and latency
	// rises well over 20%; the rest are unchanged.
	candIn := allSuitesInput(t, 4, 0, f(1.0))
	for i, r := range candIn.Runs {
		if r.SuiteID == "tool_calling" {
			candIn.Runs[i] = runResult(t, "tool_calling", 2, 2, 0,
				[]int64{200, 210, 220, 230}, []int64{20, 25, 30, 35}, 0)
		}
	}
	cand := summarize(t, candIn)
	rep, err := Compare(base, cand, DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overall != OverallRegression {
		t.Fatalf("overall = %s, want %s", rep.Overall, OverallRegression)
	}
	found := false
	for _, r := range rep.Regressions {
		if strings.HasPrefix(r, "tool_calling:task_success") {
			found = true
		}
	}
	if !found {
		t.Fatalf("regressions %v must include tool_calling task_success", rep.Regressions)
	}
}

func TestCompare_InsufficientIsDistinctFromFailure(t *testing.T) {
	base := summarize(t, allSuitesInput(t, 4, 0, nil))
	// Candidate missing one required suite entirely, and with a real
	// regression in another suite: the verdict must still be
	// insufficient, not regression, because the comparison is incomplete.
	candIn := allSuitesInput(t, 4, 0, nil)
	var kept []eval.Result
	for _, r := range candIn.Runs {
		if r.SuiteID == "cancellation" {
			continue
		}
		if r.SuiteID == "tool_calling" {
			r = runResult(t, "tool_calling", 1, 3, 0,
				[]int64{100, 110, 120, 130}, []int64{20, 25, 30, 35}, 0)
		}
		kept = append(kept, r)
	}
	candIn.Runs = kept
	rep, err := Compare(base, summarize(t, candIn), DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overall != OverallInsufficientEvidence {
		t.Fatalf("overall = %s, want %s (missing suite must not be a failure)", rep.Overall, OverallInsufficientEvidence)
	}
	// No quality score is assigned to the missing suite.
	for _, s := range rep.Suites {
		if s.SuiteID == "cancellation" && s.Status != StatusInsufficient {
			t.Fatalf("cancellation status = %s, want insufficient", s.Status)
		}
	}
}

func TestCompare_UnscoreableIsInsufficientNotFailure(t *testing.T) {
	base := summarize(t, allSuitesInput(t, 4, 0, nil))
	candIn := allSuitesInput(t, 4, 0, nil)
	for i, r := range candIn.Runs {
		if r.SuiteID == "streaming" {
			// 1 decisive case: below the suite minimum, so unscoreable.
			candIn.Runs[i] = runResult(t, "streaming", 1, 0, 0,
				[]int64{100}, []int64{20}, 0)
			// runResult marks decisive<3 unscoreable, matching MinSamples 3.
			candIn.Runs[i].Scoreable = false
			candIn.Runs[i].Score = 0
		}
	}
	rep, err := Compare(base, summarize(t, candIn), DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overall != OverallInsufficientEvidence {
		t.Fatalf("overall = %s, want insufficient for an unscoreable suite", rep.Overall)
	}
}

func TestCompare_VersionMismatchIsInsufficient(t *testing.T) {
	base := summarize(t, allSuitesInput(t, 4, 0, nil))
	// The input validator rejects a stale built-in version, so a
	// version-mismatched candidate cannot reach Compare as an input.
	candIn := allSuitesInput(t, 4, 0, nil)
	for i, r := range candIn.Runs {
		if r.SuiteID == "streaming" {
			candIn.Runs[i].SuiteVersion = "2"
		}
	}
	if _, err := Summarize(candIn); err == nil {
		t.Fatal("input with a stale built-in suite version must be rejected")
	}
	// Compare itself must also treat differing versions as insufficient,
	// for summaries that arrive from a snapshot rather than an input.
	// Copy the suite slice first: a struct copy shares the backing
	// array, and mutating it would change the baseline too.
	candS := base
	candS.Suites = append([]SuiteSummary(nil), base.Suites...)
	for i := range candS.Suites {
		if candS.Suites[i].SuiteID == "streaming" {
			candS.Suites[i].SuiteVersion = "2"
		}
	}
	rep, err := Compare(base, candS, DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overall != OverallInsufficientEvidence {
		t.Fatalf("overall = %s, want insufficient for a version-mismatched suite", rep.Overall)
	}
}

func TestCompare_CostUnobservedIsNotInsufficientOverall(t *testing.T) {
	base := summarize(t, allSuitesInput(t, 4, 0, nil))
	cand := summarize(t, allSuitesInput(t, 4, 0, f(3.0)))
	rep, err := Compare(base, cand, DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Overall != OverallNoRegression {
		t.Fatalf("overall = %s, want no_regression when cost alone is unobserved", rep.Overall)
	}
	if rep.Cost.Status != StatusInsufficient {
		t.Fatalf("cost status = %s, want insufficient", rep.Cost.Status)
	}
	// Observed cost rising over 20% is a regression.
	rep2, err := Compare(summarize(t, allSuitesInput(t, 4, 0, f(1.0))), summarize(t, allSuitesInput(t, 4, 0, f(1.5))), DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Overall != OverallRegression {
		t.Fatalf("overall = %s, want regression for a 50%% cost rise", rep2.Overall)
	}
}

func TestCheckLive(t *testing.T) {
	// Replay must report zero upstream calls.
	replay := summarize(t, allSuitesInput(t, 4, 0, nil))
	if err := CheckLive(replay, LiveLimits{MaxRequests: 10}); err != nil {
		t.Fatalf("replay CheckLive: %v", err)
	}
	// Live within limits passes.
	liveIn := allSuitesInput(t, 4, 0, f(0.5))
	liveIn.Provenance.Mode = "live"
	for i := range liveIn.Runs {
		liveIn.Runs[i].Upstream = 1
	}
	live := summarize(t, liveIn)
	if err := CheckLive(live, LiveLimits{MaxRequests: 10, MaxCostUSD: f(1.0)}); err != nil {
		t.Fatalf("live within limits: %v", err)
	}
	if err := CheckLive(live, LiveLimits{MaxRequests: 2}); err == nil {
		t.Fatal("live over request limit must be rejected")
	}
	if err := CheckLive(live, LiveLimits{MaxRequests: 10, MaxCostUSD: f(0.25)}); err == nil {
		t.Fatal("live over cost limit must be rejected")
	}
	if err := RequireLiveAcknowledgement("live", false); err == nil {
		t.Fatal("live without acknowledgement must be refused")
	}
	if err := RequireLiveAcknowledgement("replay", false); err != nil {
		t.Fatalf("replay must not need acknowledgement: %v", err)
	}
}

func TestParseBaseline_SnapshotAndInput(t *testing.T) {
	in := allSuitesInput(t, 4, 0, f(1.0))
	s := summarize(t, in)
	data, err := MarshalSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseBaseline(data)
	if err != nil {
		t.Fatalf("snapshot baseline: %v", err)
	}
	if len(got.Suites) != len(DefaultSuites) {
		t.Fatalf("snapshot suites = %d", len(got.Suites))
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := ParseBaseline(raw)
	if err != nil {
		t.Fatalf("input baseline: %v", err)
	}
	if got2.Provenance.GatewayCommit != "e6c3cdf3" {
		t.Fatalf("input baseline commit = %q", got2.Provenance.GatewayCommit)
	}
	if _, err := ParseInput([]byte(`{"format":"quality-gate-input/v1","provenance":{},"runs":[],"surprise":true}`)); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
}

func TestReport_ContainsNoPromptOrResponseContent(t *testing.T) {
	// Run the real deterministic runner over artifacts whose output
	// and conversation turns contain sentinels, then build a report
	// from the run. The report must contain neither sentinel.
	runner := eval.NewRunner()
	sentinelOutput := "SECRET-OUTPUT-9f3a"
	sentinelTurn := "SECRET-TURN-7c1e"
	outcomes := []eval.Outcome{
		{CaseID: "multi-turn-context-recall", Status: eval.OutcomeOK, Output: sentinelOutput, Turns: []string{"first turn", sentinelTurn, "NEEDLE-A"}},
		{CaseID: "multi-turn-followup-correction", Status: eval.OutcomeOK, Turns: []string{"try again", "corrected"}},
		{CaseID: "multi-turn-tool-result-followup", Status: eval.OutcomeOK, Turns: []string{"tool result", "done"}},
	}
	res, err := runner.Run(context.Background(), eval.Request{
		DeploymentID: "dep-1", ProviderID: "prov-1", Model: "model-1",
		SuiteID: "multi_turn", Outcomes: outcomes,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fill the remaining gate suites with passing runs so the report
	// is comparable, then check the whole report for leakage.
	runs := []eval.Result{res}
	for _, s := range DefaultSuites {
		if s == "multi_turn" {
			continue
		}
		runs = append(runs, runResult(t, s, 3, 0, 0, []int64{100, 110, 120}, []int64{20, 25, 30}, 0))
	}
	in := Input{Format: FormatInput, Provenance: prov("e6c3cdf3", "model-1", "dep-1", "replay"), Runs: runs}
	rep, err := Compare(summarize(t, in), summarize(t, in), DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), sentinelOutput) || strings.Contains(string(data), sentinelTurn) {
		t.Fatalf("report leaks prompt/response content: %s", data)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), sentinelOutput) || strings.Contains(string(raw), sentinelTurn) {
		t.Fatalf("eval result itself leaks content: %s", raw)
	}
}

func TestThresholds_Validate(t *testing.T) {
	th := DefaultThresholds()
	if err := th.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	th.SuccessDrop = 2
	if err := th.Validate(); err == nil {
		t.Fatal("success drop over 1 must be rejected")
	}
	th = DefaultThresholds()
	th.RequiredSuites = []string{"tool_calling", "tool_calling"}
	if err := th.Validate(); err == nil {
		t.Fatal("duplicate required suites must be rejected")
	}
}
