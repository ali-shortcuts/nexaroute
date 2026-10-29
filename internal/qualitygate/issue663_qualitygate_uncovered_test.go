package qualitygate

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

func TestIssue663CoreProvenanceValidate(t *testing.T) {
	long := strings.Repeat("a", 257)
	valid := prov("abc123", "model-1", "dep-1", "replay")
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid provenance rejected: %v", err)
	}
	upper := valid
	upper.RouteConfigHash = strings.Repeat("A", 64)
	if err := upper.Validate(); err != nil {
		t.Fatalf("uppercase hex hash must be accepted: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Provenance)
	}{
		{"empty commit", func(p *Provenance) { p.GatewayCommit = "" }},
		{"long commit", func(p *Provenance) { p.GatewayCommit = long }},
		{"whitespace commit", func(p *Provenance) { p.GatewayCommit = "ab c" }},
		{"empty model", func(p *Provenance) { p.Model = "" }},
		{"long model", func(p *Provenance) { p.Model = long }},
		{"empty deployment", func(p *Provenance) { p.DeploymentID = "" }},
		{"long deployment", func(p *Provenance) { p.DeploymentID = long }},
		{"long provider", func(p *Provenance) { p.ProviderID = long }},
		{"long route config", func(p *Provenance) { p.RouteConfigID = long }},
		{"short hash", func(p *Provenance) { p.RouteConfigHash = "abc" }},
		{"non-hex hash", func(p *Provenance) { p.RouteConfigHash = strings.Repeat("g", 64) }},
		{"bad mode", func(p *Provenance) { p.Mode = "shadow" }},
		{"zero time", func(p *Provenance) { p.GeneratedAt = time.Time{} }},
	}
	for _, tc := range cases {
		p := valid
		tc.mut(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: provenance must be rejected", tc.name)
		}
	}
}

func TestIssue663CoreInputValidate(t *testing.T) {
	valid := allSuitesInput(t, 3, 1, f(2.50))
	bad := valid
	bad.Format = "other/v1"
	if err := bad.Validate(); err == nil {
		t.Error("wrong input format must be rejected")
	}
	bad = valid
	bad.Provenance.Model = ""
	if err := bad.Validate(); err == nil {
		t.Error("input with invalid provenance must be rejected")
	}
	bad = valid
	bad.Runs = nil
	if err := bad.Validate(); err == nil {
		t.Error("input with zero runs must be rejected")
	}
	many := valid
	many.Runs = nil
	for i := 0; i < 65; i++ {
		many.Runs = append(many.Runs, runResult(t, "custom-"+strconv.Itoa(i), 3, 0, 0, nil, nil, 0))
	}
	if err := many.Validate(); err == nil {
		t.Error("input with 65 runs must be rejected")
	}
	bad = valid
	bad.ObservedCostUSD = f(-1)
	if err := bad.Validate(); err == nil {
		t.Error("negative observed cost must be rejected")
	}
	bad = valid
	nan := math.NaN()
	bad.ObservedCostUSD = &nan
	if err := bad.Validate(); err == nil {
		t.Error("NaN observed cost must be rejected")
	}
	bad = valid
	bad.Runs = append(append([]eval.Result{}, valid.Runs[:1]...), valid.Runs[0])
	if err := bad.Validate(); err == nil {
		t.Error("duplicate suite runs must be rejected")
	}
	bad = valid
	stale := valid.Runs[0]
	stale.SuiteVersion = "2"
	bad.Runs = []eval.Result{stale}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("stale suite version must be rejected, got %v", err)
	}
	bad = valid
	broken := valid.Runs[0]
	broken.RunID = ""
	bad.Runs = []eval.Result{broken}
	if err := bad.Validate(); err == nil {
		t.Error("a run failing eval validation must be rejected")
	}
}

func TestIssue663CorePercentile(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile of empty = %v, want 0", got)
	}
	if got := percentile([]float64{5, 10}, 0); got != 5 {
		t.Errorf("percentile p<=0 = %v, want 5", got)
	}
	if got := percentile([]float64{5, 10}, -1); got != 5 {
		t.Errorf("percentile negative p = %v, want 5", got)
	}
	if got := percentile([]float64{5, 10}, 1); got != 10 {
		t.Errorf("percentile p>=1 = %v, want 10", got)
	}
	if got := percentile([]float64{5, 10}, 2); got != 10 {
		t.Errorf("percentile p>1 = %v, want 10", got)
	}
	if got := percentile([]float64{100, 110, 120, 130}, 0.5); got != 110 {
		t.Errorf("percentile p50 = %v, want 110", got)
	}
	if got := percentile([]float64{100, 110, 120, 130}, 0.95); got != 130 {
		t.Errorf("percentile p95 = %v, want 130", got)
	}
}

func TestIssue663CoreThresholdsValidate(t *testing.T) {
	valid := DefaultThresholds()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default thresholds rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Thresholds)
	}{
		{"success drop below zero", func(th *Thresholds) { th.SuccessDrop = -0.1 }},
		{"success drop above one", func(th *Thresholds) { th.SuccessDrop = 1.5 }},
		{"success drop NaN", func(th *Thresholds) { th.SuccessDrop = math.NaN() }},
		{"error rise above one", func(th *Thresholds) { th.ErrorRateRise = 2 }},
		{"latency rise below zero", func(th *Thresholds) { th.LatencyRisePct = -0.1 }},
		{"latency rise above five", func(th *Thresholds) { th.LatencyRisePct = 6 }},
		{"ttft rise NaN", func(th *Thresholds) { th.TTFTRisePct = math.NaN() }},
		{"cost rise above five", func(th *Thresholds) { th.CostRisePct = 6 }},
		{"no required suites", func(th *Thresholds) { th.RequiredSuites = nil }},
		{"empty suite id", func(th *Thresholds) { th.RequiredSuites = []string{""} }},
		{"duplicate suite", func(th *Thresholds) { th.RequiredSuites = []string{"a", "a"} }},
	}
	for _, tc := range cases {
		th := valid
		tc.mut(&th)
		if err := th.Validate(); err == nil {
			t.Errorf("%s: thresholds must be rejected", tc.name)
		}
	}
	tooMany := valid
	tooMany.RequiredSuites = nil
	for i := 0; i < MaxGateSuites+1; i++ {
		tooMany.RequiredSuites = append(tooMany.RequiredSuites, "s"+strconv.Itoa(i))
	}
	if err := tooMany.Validate(); err == nil {
		t.Error("more than MaxGateSuites required suites must be rejected")
	}
}

func TestIssue663CoreCompareMetric(t *testing.T) {
	if got := compareMetric(MetricTaskSuccess, "higher_better", nil, f(0.9), 0.05, 0); got.Status != StatusInsufficient {
		t.Errorf("nil baseline status = %q, want insufficient", got.Status)
	}
	if got := compareMetric(MetricTaskSuccess, "higher_better", f(0.9), nil, 0.05, 0); got.Status != StatusInsufficient {
		t.Errorf("nil candidate status = %q, want insufficient", got.Status)
	}
	if got := compareMetric(MetricTaskSuccess, "higher_better", f(0.5), f(0.45), 0.05, 0); got.Status != StatusUnchanged {
		t.Errorf("delta exactly at threshold must be unchanged, got %q", got.Status)
	}
	if got := compareMetric(MetricTaskSuccess, "higher_better", f(0.9), f(0.96), 0.05, 0); got.Status != StatusImprovement {
		t.Errorf("success rise above threshold must improve, got %q", got.Status)
	}
	if got := compareMetric(MetricErrorRate, "lower_better", f(0.1), f(0.2), 0.05, 0); got.Status != StatusRegression {
		t.Errorf("error-rate rise must regress, got %q", got.Status)
	}
	if got := compareMetric(MetricErrorRate, "lower_better", f(0.2), f(0.1), 0.05, 0); got.Status != StatusImprovement {
		t.Errorf("error-rate fall must improve, got %q", got.Status)
	}
	if got := compareMetric(MetricLatencyP95, "lower_better", f(0), f(10), 0, 0.2); got.Status != StatusRegression {
		t.Errorf("latency from zero baseline must regress, got %q", got.Status)
	}
	if got := compareMetric(MetricLatencyP95, "lower_better", f(0), f(0), 0, 0.2); got.Status != StatusUnchanged {
		t.Errorf("latency staying at zero must be unchanged, got %q", got.Status)
	}
	if got := compareMetric(MetricLatencyP95, "lower_better", f(100), f(130), 0, 0.2); got.Status != StatusRegression {
		t.Errorf("latency rise over pct must regress, got %q", got.Status)
	}
	if got := compareMetric(MetricLatencyP95, "lower_better", f(100), f(70), 0, 0.2); got.Status != StatusImprovement {
		t.Errorf("latency fall over pct must improve, got %q", got.Status)
	}
	if got := compareMetric(MetricLatencyP95, "lower_better", f(100), f(110), 0, 0.2); got.Status != StatusUnchanged || got.DeltaPct == nil || *got.DeltaPct != 0.1 {
		t.Errorf("latency delta pct wrong: %+v", got)
	}
	if got := compareMetric(MetricCost, "sideways", f(1), f(2), 0, 0.2); got.Status != "" {
		t.Errorf("unknown direction status = %q, want empty", got.Status)
	}
}

func TestIssue663CoreCheckLiveBranches(t *testing.T) {
	replay := summarize(t, allSuitesInput(t, 3, 1, nil))
	if err := CheckLive(replay, LiveLimits{MaxRequests: 10}); err != nil {
		t.Fatalf("replay with zero upstream calls must pass: %v", err)
	}
	replay.UpstreamCalls = 3
	if err := CheckLive(replay, LiveLimits{MaxRequests: 10}); err == nil {
		t.Error("replay reporting upstream calls must fail")
	}
	live := summarize(t, allSuitesInput(t, 3, 1, f(1.0)))
	live.Provenance.Mode = "live"
	live.UpstreamCalls = 2
	if err := CheckLive(live, LiveLimits{MaxRequests: 0}); err == nil {
		t.Error("live evaluation needs a positive max_requests limit")
	}
	if err := CheckLive(live, LiveLimits{MaxRequests: 1}); err == nil {
		t.Error("live evaluation over the request limit must fail")
	}
	if err := CheckLive(live, LiveLimits{MaxRequests: 10, MaxCostUSD: f(0.5)}); err == nil {
		t.Error("live evaluation over the cost limit must fail")
	}
	if err := CheckLive(live, LiveLimits{MaxRequests: 10, MaxCostUSD: f(2.0)}); err != nil {
		t.Fatalf("live evaluation within limits must pass: %v", err)
	}
	live.Provenance.Mode = "shadow"
	if err := CheckLive(live, LiveLimits{MaxRequests: 10}); err == nil {
		t.Error("unknown summary mode must fail")
	}
	if err := RequireLiveAcknowledgement("live", false); err == nil {
		t.Error("live without acknowledgement must be refused")
	}
	if err := RequireLiveAcknowledgement("live", true); err != nil {
		t.Fatalf("acknowledged live must pass: %v", err)
	}
	if err := RequireLiveAcknowledgement("replay", false); err != nil {
		t.Fatalf("replay must never need acknowledgement: %v", err)
	}
}

func TestIssue663CoreParseSnapshotAndBaseline(t *testing.T) {
	summary := summarize(t, allSuitesInput(t, 3, 1, f(2.50)))
	raw, err := MarshalSnapshot(summary)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSnapshot(raw)
	if err != nil || len(parsed.Suites) != len(DefaultSuites) {
		t.Fatalf("valid snapshot parse = %d suites, %v", len(parsed.Suites), err)
	}
	if _, err := ParseSnapshot(nil); err == nil {
		t.Error("empty snapshot must be rejected")
	}
	if _, err := ParseSnapshot(make([]byte, MaxGateInputBytes+1)); err == nil {
		t.Error("oversize snapshot must be rejected")
	}
	if _, err := ParseSnapshot([]byte(`{bad`)); err == nil {
		t.Error("malformed snapshot JSON must be rejected")
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	snap.Format = FormatInput
	wrongFormat, _ := json.Marshal(snap)
	if _, err := ParseSnapshot(wrongFormat); err == nil {
		t.Error("snapshot with the wrong format must be rejected")
	}
	snap.Format = FormatSnapshot
	snap.Summary.Provenance.Model = ""
	badProv, _ := json.Marshal(snap)
	if _, err := ParseSnapshot(badProv); err == nil {
		t.Error("snapshot with invalid provenance must be rejected")
	}
	snap.Summary.Provenance = prov("abc", "model-1", "dep-1", "replay")
	snap.Summary.Suites = nil
	noSuites, _ := json.Marshal(snap)
	if _, err := ParseSnapshot(noSuites); err == nil {
		t.Error("snapshot with zero suites must be rejected")
	}
	snap.Summary.Suites = make([]SuiteSummary, MaxGateSuites+1)
	for i := range snap.Summary.Suites {
		snap.Summary.Suites[i] = SuiteSummary{SuiteID: "s" + strconv.Itoa(i)}
	}
	tooMany, _ := json.Marshal(snap)
	if _, err := ParseSnapshot(tooMany); err == nil {
		t.Error("snapshot with too many suites must be rejected")
	}

	if _, err := ParseBaseline([]byte(`{bad`)); err == nil {
		t.Error("baseline with invalid JSON must be rejected")
	}
	if _, err := ParseBaseline([]byte(`{"format":"other/v1"}`)); err == nil {
		t.Error("baseline with an unknown format must be rejected")
	}
	fromSnap, err := ParseBaseline(raw)
	if err != nil || len(fromSnap.Suites) != len(DefaultSuites) {
		t.Fatalf("baseline from snapshot = %d suites, %v", len(fromSnap.Suites), err)
	}
	inBytes, err := json.Marshal(allSuitesInput(t, 3, 1, f(2.50)))
	if err != nil {
		t.Fatal(err)
	}
	fromInput, err := ParseBaseline(inBytes)
	if err != nil || len(fromInput.Suites) != len(DefaultSuites) {
		t.Fatalf("baseline from input = %d suites, %v", len(fromInput.Suites), err)
	}
	badInput := allSuitesInput(t, 3, 1, nil)
	badInput.Format = FormatInput
	badInput.Runs = nil
	badBytes, _ := json.Marshal(badInput)
	if _, err := ParseBaseline(badBytes); err == nil {
		t.Error("baseline from an invalid input must be rejected")
	}
	if _, err := ParseInput(nil); err == nil {
		t.Error("empty input must be rejected")
	}
	if _, err := ParseInput([]byte(`{"format":"quality-gate-input/v1","unknown_field":1}`)); err == nil {
		t.Error("input with unknown fields must be rejected")
	}
}
