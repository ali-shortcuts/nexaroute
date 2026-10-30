package qualitygate

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

func Continue95Prov(mode string) Provenance {
	return Provenance{
		GatewayCommit: "abc123",
		Model:         "m1",
		DeploymentID:  "dep-1",
		Mode:          mode,
		GeneratedAt:   time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
}

func Continue95Suite(id, ver string, scoreable bool, score, errRate, lat, ttft float64) SuiteSummary {
	return SuiteSummary{
		SuiteID: id, SuiteVersion: ver, Scoreable: scoreable,
		Score: score, ScoreObserved: true,
		ErrorRate: errRate, ErrorRateObserved: true,
		LatencyP95MS: lat, LatencyObserved: true,
		TTFTP95MS: ttft, TTFTObserved: true,
	}
}

func TestContinue95ProvenanceBranches(t *testing.T) {
	long := strings.Repeat("x", 257)
	cases := []struct {
		name string
		p    Provenance
	}{
		{"empty commit", func() Provenance { p := Continue95Prov("replay"); p.GatewayCommit = ""; return p }()},
		{"long commit", func() Provenance { p := Continue95Prov("replay"); p.GatewayCommit = long; return p }()},
		{"whitespace commit", func() Provenance { p := Continue95Prov("replay"); p.GatewayCommit = "a b"; return p }()},
		{"empty model", func() Provenance { p := Continue95Prov("replay"); p.Model = ""; return p }()},
		{"empty deployment", func() Provenance { p := Continue95Prov("replay"); p.DeploymentID = ""; return p }()},
		{"long provider", func() Provenance { p := Continue95Prov("replay"); p.ProviderID = long; return p }()},
		{"long route config", func() Provenance { p := Continue95Prov("replay"); p.RouteConfigID = long; return p }()},
		{"short hash", func() Provenance { p := Continue95Prov("replay"); p.RouteConfigHash = "abc"; return p }()},
		{"nonhex hash", func() Provenance {
			p := Continue95Prov("replay")
			p.RouteConfigHash = strings.Repeat("g", 64)
			return p
		}()},
		{"bad mode", Continue95Prov("bogus")},
		{"zero time", func() Provenance { p := Continue95Prov("replay"); p.GeneratedAt = time.Time{}; return p }()},
	}
	for _, tc := range cases {
		if err := tc.p.Validate(); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
	ok := Continue95Prov("live")
	ok.RouteConfigHash = strings.Repeat("A", 64)
	if err := ok.Validate(); err != nil {
		t.Errorf("uppercase hex hash should validate: %v", err)
	}
}

func TestContinue95ThresholdsBranches(t *testing.T) {
	base := DefaultThresholds()
	bad := []struct {
		name string
		mut  func(*Thresholds)
	}{
		{"nan success", func(th *Thresholds) { th.SuccessDrop = math.NaN() }},
		{"inf error", func(th *Thresholds) { th.ErrorRateRise = math.Inf(1) }},
		{"neg success", func(th *Thresholds) { th.SuccessDrop = -0.1 }},
		{"over success", func(th *Thresholds) { th.SuccessDrop = 1.5 }},
		{"nan latency", func(th *Thresholds) { th.LatencyRisePct = math.NaN() }},
		{"over latency", func(th *Thresholds) { th.LatencyRisePct = 5.5 }},
		{"neg ttft", func(th *Thresholds) { th.TTFTRisePct = -0.1 }},
		{"empty suites", func(th *Thresholds) { th.RequiredSuites = nil }},
		{"too many suites", func(th *Thresholds) {
			th.RequiredSuites = make([]string, MaxGateSuites+1)
			for i := range th.RequiredSuites {
				th.RequiredSuites[i] = "s"
			}
		}},
		{"empty suite id", func(th *Thresholds) { th.RequiredSuites = []string{" "} }},
		{"dup suite", func(th *Thresholds) { th.RequiredSuites = []string{"a", "a"} }},
	}
	for _, tc := range bad {
		th := base
		tc.mut(&th)
		if err := th.Validate(); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
	if err := base.Validate(); err != nil {
		t.Errorf("default thresholds should validate: %v", err)
	}
}

func TestContinue95InputBranches(t *testing.T) {
	validRun := runResult(t, "custom-suite", 3, 0, 0, []int64{10, 20, 30}, []int64{5, 6, 7}, 0)
	good := Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{validRun}}
	if _, err := Summarize(good); err != nil {
		t.Fatalf("valid custom input should summarize: %v", err)
	}
	nan := math.NaN()
	inf := math.Inf(1)
	neg := -1.0
	many := make([]eval.Result, 0, MaxGateRuns+1)
	for i := 0; i <= MaxGateRuns; i++ {
		r := validRun
		r.SuiteID = "suite-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		r.RunID = r.SuiteID + "-run"
		many = append(many, r)
	}
	badRun := validRun
	badRun.RunID = ""
	versionRun := runResult(t, "tool_calling", 3, 0, 0, []int64{10, 20, 30}, []int64{5, 6, 7}, 0)
	versionRun.SuiteVersion = "99"
	cases := []struct {
		name string
		in   Input
	}{
		{"bad format", Input{Format: "nope", Provenance: Continue95Prov("replay"), Runs: []eval.Result{validRun}}},
		{"no runs", Input{Format: FormatInput, Provenance: Continue95Prov("replay")}},
		{"too many runs", Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: many}},
		{"nan cost", Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{validRun}, ObservedCostUSD: &nan}},
		{"inf cost", Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{validRun}, ObservedCostUSD: &inf}},
		{"neg cost", Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{validRun}, ObservedCostUSD: &neg}},
		{"bad run", Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{badRun}}},
		{"dup suite", Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{validRun, validRun}}},
		{"version mismatch", Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{versionRun}}},
	}
	for _, tc := range cases {
		if err := tc.in.Validate(); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestContinue95PercentileEdges(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty percentile = %v, want 0", got)
	}
	if got := percentile([]float64{30, 10, 20}, 0); got != 10 {
		t.Errorf("p<=0 percentile = %v, want 10", got)
	}
	if got := percentile([]float64{30, 10, 20}, 1); got != 30 {
		t.Errorf("p>=1 percentile = %v, want 30", got)
	}
	if got := percentile([]float64{10, 20, 30, 40}, 0.5); got != 20 {
		t.Errorf("median percentile = %v, want 20", got)
	}
}

func TestContinue95CompareZeroBaselinePartialAndCost(t *testing.T) {
	th := Thresholds{SuccessDrop: 0.05, ErrorRateRise: 0.05, LatencyRisePct: 0.20, TTFTRisePct: 0.20, CostRisePct: 0.20, RequiredSuites: []string{"s1"}}
	// Zero-baseline latency: candidate positive regresses; TTFT stays zero so unchanged.
	base := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 0, 0)}}
	cand := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 10, 0)}}
	rep, err := Compare(base, cand, th)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if rep.Overall != OverallRegression {
		t.Errorf("zero-baseline overall = %q, want regression", rep.Overall)
	}
	found := false
	for _, r := range rep.Regressions {
		if r == "s1:latency_p95_ms" {
			found = true
		}
	}
	if !found {
		t.Errorf("regressions %v missing latency regression", rep.Regressions)
	}
	// Relative improvement: latency halves, error rate drops.
	base2 := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.5, 0.20, 100, 100)}}
	cand2 := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.05, 50, 50)}}
	rep2, err := Compare(base2, cand2, th)
	if err != nil {
		t.Fatalf("Compare improvement: %v", err)
	}
	if rep2.Overall != OverallNoRegression || len(rep2.Improvements) == 0 {
		t.Errorf("improvement overall = %q improvements=%v", rep2.Overall, rep2.Improvements)
	}
	// Partial evidence: candidate unscoreable but score measurably drops.
	cand3 := Continue95Suite("s1", "1", false, 0.5, 0.01, 10, 10)
	rep3, err := Compare(base, Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{cand3}}, th)
	if err != nil {
		t.Fatalf("Compare partial: %v", err)
	}
	if rep3.Overall != OverallInsufficientEvidence {
		t.Errorf("partial overall = %q, want insufficient", rep3.Overall)
	}
	partial := false
	for _, r := range rep3.Regressions {
		if strings.Contains(r, "(partial evidence)") {
			partial = true
		}
	}
	if !partial {
		t.Errorf("partial regressions %v missing partial marker", rep3.Regressions)
	}
	// Cost regression and cost-insufficient note.
	costBase := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 10, 10)}, ObservedCostUSD: f(10)}
	costCand := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 10, 10)}, ObservedCostUSD: f(20)}
	rep4, err := Compare(costBase, costCand, th)
	if err != nil {
		t.Fatalf("Compare cost: %v", err)
	}
	if rep4.Cost.Status != StatusRegression {
		t.Errorf("cost status = %q, want regression", rep4.Cost.Status)
	}
	rep5, err := Compare(costBase, Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 10, 10)}}, th)
	if err != nil {
		t.Fatalf("Compare cost missing: %v", err)
	}
	if rep5.Cost.Status != StatusInsufficient || len(rep5.Notes) == 0 {
		t.Errorf("cost missing status = %q notes=%v", rep5.Cost.Status, rep5.Notes)
	}
	// Compare validation errors.
	if _, err := Compare(base, cand, Thresholds{}); err == nil {
		t.Error("empty thresholds should fail Compare")
	}
	badProv := base
	badProv.Provenance = Continue95Prov("bogus")
	if _, err := Compare(badProv, cand, th); err == nil {
		t.Error("bad baseline provenance should fail Compare")
	}
	badCand := cand
	badCand.Provenance = Continue95Prov("bogus")
	if _, err := Compare(base, badCand, th); err == nil {
		t.Error("bad candidate provenance should fail Compare")
	}
}

func TestContinue95CheckLiveAndAck(t *testing.T) {
	replayBad := Summary{Provenance: Continue95Prov("replay"), UpstreamCalls: 3}
	if err := CheckLive(replayBad, LiveLimits{}); err == nil {
		t.Error("replay with upstream calls should fail")
	}
	if err := CheckLive(Summary{Provenance: Continue95Prov("replay")}, LiveLimits{}); err != nil {
		t.Errorf("clean replay should pass: %v", err)
	}
	unknown := Summary{Provenance: Continue95Prov("bogus")}
	if err := CheckLive(unknown, LiveLimits{}); err == nil {
		t.Error("unknown mode should fail")
	}
	live := Summary{Provenance: Continue95Prov("live"), UpstreamCalls: 5}
	if err := CheckLive(live, LiveLimits{MaxRequests: 0}); err == nil {
		t.Error("zero max_requests should fail")
	}
	if err := CheckLive(live, LiveLimits{MaxRequests: 2}); err == nil {
		t.Error("over-limit live should fail")
	}
	costly := Summary{Provenance: Continue95Prov("live"), UpstreamCalls: 5, ObservedCostUSD: f(9)}
	if err := CheckLive(costly, LiveLimits{MaxRequests: 10, MaxCostUSD: f(5)}); err == nil {
		t.Error("over-cost live should fail")
	}
	if err := CheckLive(costly, LiveLimits{MaxRequests: 10, MaxCostUSD: f(10)}); err != nil {
		t.Errorf("within-limit live should pass: %v", err)
	}
	if err := RequireLiveAcknowledgement("live", false); err == nil {
		t.Error("unacknowledged live should fail")
	}
	if err := RequireLiveAcknowledgement("live", true); err != nil {
		t.Errorf("acknowledged live should pass: %v", err)
	}
	if err := RequireLiveAcknowledgement("replay", false); err != nil {
		t.Errorf("replay needs no ack: %v", err)
	}
}

func TestContinue95ParseBranches(t *testing.T) {
	if _, err := ParseInput(nil); err == nil {
		t.Error("empty ParseInput should fail")
	}
	if _, err := ParseInput([]byte(`{"format": "x", "unknown": 1}`)); err == nil {
		t.Error("unknown-field ParseInput should fail")
	}
	if _, err := ParseInput(make([]byte, MaxGateInputBytes+1)); err == nil {
		t.Error("oversize ParseInput should fail")
	}
	if _, err := ParseSnapshot(nil); err == nil {
		t.Error("empty ParseSnapshot should fail")
	}
	if _, err := ParseSnapshot([]byte(`{"format": "wrong"}`)); err == nil {
		t.Error("wrong-format ParseSnapshot should fail")
	}
	snap, err := MarshalSnapshot(Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0, 10, 10)}})
	if err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}
	parsed, err := ParseSnapshot(snap)
	if err != nil {
		t.Fatalf("ParseSnapshot round-trip: %v", err)
	}
	if len(parsed.Suites) != 1 || parsed.Suites[0].SuiteID != "s1" {
		t.Errorf("parsed snapshot suites = %v", parsed.Suites)
	}
	if _, err := ParseBaseline([]byte(`not json`)); err == nil {
		t.Error("invalid ParseBaseline should fail")
	}
	if _, err := ParseBaseline([]byte(`{"format": "other"}`)); err == nil {
		t.Error("unknown-format ParseBaseline should fail")
	}
	if _, err := ParseBaseline(snap); err != nil {
		t.Errorf("snapshot ParseBaseline should pass: %v", err)
	}
	// Snapshot with no suites fails the suite-count check.
	if _, err := ParseSnapshot([]byte(`{"format": "quality-gate-snapshot/v1", "summary": {"provenance": {"gateway_commit": "abc", "model": "m", "deployment_id": "d", "mode": "replay", "generated_at": "2026-10-27T12:00:00Z"}, "suites": []}}`)); err == nil {
		t.Error("empty-suite snapshot should fail")
	}
	// Input-format baseline goes through Summarize.
	in := Input{Format: FormatInput, Provenance: Continue95Prov("replay"), Runs: []eval.Result{runResult(t, "custom-suite", 3, 0, 0, []int64{10, 20, 30}, []int64{5, 6, 7}, 0)}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	if _, err := ParseBaseline(data); err != nil {
		t.Errorf("input ParseBaseline should pass: %v", err)
	}
	if _, err := ParseInput(data); err != nil {
		t.Errorf("ParseInput round-trip should pass: %v", err)
	}
}

func TestContinue95RemainingBranches(t *testing.T) {
	th := Thresholds{SuccessDrop: 0.05, ErrorRateRise: 0.05, LatencyRisePct: 0.20, TTFTRisePct: 0.20, CostRisePct: 0.20, RequiredSuites: []string{"s1"}}
	base := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 10, 10)}}
	// Error-rate regression (absolute threshold on a ratio metric).
	errUp := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.30, 10, 10)}}
	rep, err := Compare(base, errUp, th)
	if err != nil {
		t.Fatalf("Compare error regression: %v", err)
	}
	if rep.Overall != OverallRegression {
		t.Errorf("error-rate overall = %q, want regression", rep.Overall)
	}
	// Cost improvement.
	costBase := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 10, 10)}, ObservedCostUSD: f(20)}
	costDown := Summary{Provenance: Continue95Prov("replay"), Suites: []SuiteSummary{Continue95Suite("s1", "1", true, 0.9, 0.01, 10, 10)}, ObservedCostUSD: f(10)}
	rep2, err := Compare(costBase, costDown, th)
	if err != nil {
		t.Fatalf("Compare cost improvement: %v", err)
	}
	if rep2.Cost.Status != StatusImprovement {
		t.Errorf("cost status = %q, want improvement", rep2.Cost.Status)
	}
	// Input.Validate provenance failure path.
	badProvIn := Input{Format: FormatInput, Provenance: Continue95Prov("bogus"), Runs: []eval.Result{runResult(t, "custom-suite", 3, 0, 0, []int64{10, 20, 30}, []int64{5, 6, 7}, 0)}}
	if err := badProvIn.Validate(); err == nil {
		t.Error("bad-provenance input should fail Validate")
	}
	// ParseInput that decodes but fails Validate.
	if _, err := ParseInput([]byte(`{"format": "quality-gate-input/v1", "provenance": {"gateway_commit": "abc", "model": "m", "deployment_id": "d", "mode": "replay", "generated_at": "2026-10-27T12:00:00Z"}, "runs": []}`)); err == nil {
		t.Error("no-run ParseInput should fail")
	}
	// ParseSnapshot decode failure and provenance failure.
	if _, err := ParseSnapshot([]byte(`{"format": "quality-gate-snapshot/v1",`)); err == nil {
		t.Error("truncated ParseSnapshot should fail")
	}
	if _, err := ParseSnapshot([]byte(`{"format": "quality-gate-snapshot/v1", "summary": {"provenance": {"gateway_commit": "", "model": "m", "deployment_id": "d", "mode": "replay", "generated_at": "2026-10-27T12:00:00Z"}, "suites": [{"suite_id": "s1"}]}}`)); err == nil {
		t.Error("bad-provenance ParseSnapshot should fail")
	}
	// ParseBaseline input branch that fails ParseInput.
	if _, err := ParseBaseline([]byte(`{"format": "quality-gate-input/v1", "provenance": {"gateway_commit": "abc", "model": "m", "deployment_id": "d", "mode": "replay", "generated_at": "2026-10-27T12:00:00Z"}, "runs": []}`)); err == nil {
		t.Error("no-run ParseBaseline should fail")
	}
}
