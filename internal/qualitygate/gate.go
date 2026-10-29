// Package qualitygate compares a candidate model/deployment against a
// recorded baseline using only evidence the evaluation plane already
// produces (internal/eval results). It adds no new evaluator, no new
// provider path and no routing decision.
//
// Design rules, mirroring the evaluation plane:
//
//   - Replay by default. Summarize and Compare read recorded eval.Results;
//     they perform no I/O and make no upstream call. A live candidate is
//     only evidence that was already produced through the opt-in live
//     evaluation path (evaluation.live_enabled, one explicit deployment),
//     and CheckLive enforces request and cost ceilings on top of it.
//   - Insufficient evidence is a verdict of its own. A suite that is
//     missing, or whose run was not scoreable, yields no quality score
//     and makes the whole report "insufficient_evidence" — it is never
//     counted as a failure, and a regression in another suite does not
//     promote it to a failure either, because the comparison is
//     incomplete. Regressions that were measurable are still listed.
//   - No invented numbers. Task success comes from a scoreable run only.
//     Latency and TTFT come from recorded per-case timings only. Cost is
//     reported only when the operator supplies an actually observed
//     cost; it is never estimated from token counts or prices.
//   - No prompt or response content. A report carries identifiers,
//     counts, timings, costs and deltas. eval.Result itself carries no
//     output text, and this package never reads Outcome artifacts, so
//     there is nothing to leak.
//   - No routing change, ever. The report states RoutingAction "none"
//     explicitly. Acting on a report (changing a route, a weight or a
//     deployment) is an operator decision outside this package, which
//     deliberately imports neither the router nor the decision plane.
package qualitygate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// GateSuiteSetVersion versions the default suite set as a whole, so a
// report always says which suites were required, not just which ran.
const GateSuiteSetVersion = "quality-gate/v1"

// DefaultSuites is the gate's required suite set: the three reused v1
// suites for tool calls, structured JSON and protocol compatibility,
// plus the three new v1 suites for multi-turn conversations, streaming
// and cancellation.
var DefaultSuites = []string{
	"tool_calling",
	"structured_output",
	"protocol_compat",
	"multi_turn",
	"streaming",
	"cancellation",
}

// Bounds for gate inputs and reports.
const (
	MaxGateInputBytes = 8 << 20
	MaxGateRuns       = 64
	MaxGateSuites     = 32
)

const (
	FormatInput    = "quality-gate-input/v1"
	FormatSnapshot = "quality-gate-snapshot/v1"
	FormatReport   = "quality-gate-report/v1"
)

// Overall verdicts. They are deliberately not pass/fail: "insufficient"
// is not a failure, and "regression" is a report for an operator, not
// an automatic routing action.
const (
	OverallNoRegression         = "no_regression"
	OverallRegression           = "regression"
	OverallInsufficientEvidence = "insufficient_evidence"
)

// Metric statuses.
const (
	StatusRegression   = "regression"
	StatusImprovement  = "improvement"
	StatusUnchanged    = "unchanged"
	StatusInsufficient = "insufficient"
)

// Metrics compared by the gate.
const (
	MetricTaskSuccess = "task_success"
	MetricErrorRate   = "error_rate"
	MetricLatencyP95  = "latency_p95_ms"
	MetricTTFTP95     = "ttft_p95_ms"
	MetricCost        = "cost_usd"
)

// Provenance records exactly what was compared. Every field is an
// identifier or a timestamp; none of it is prompt or response content.
type Provenance struct {
	GatewayCommit   string    `json:"gateway_commit"`
	Model           string    `json:"model"`
	DeploymentID    string    `json:"deployment_id"`
	ProviderID      string    `json:"provider_id,omitempty"`
	RouteConfigID   string    `json:"route_config_id,omitempty"`
	RouteConfigHash string    `json:"route_config_hash,omitempty"`
	Mode            string    `json:"mode"`
	GeneratedAt     time.Time `json:"generated_at"`
}

// Validate checks provenance without inventing a missing field.
func (p Provenance) Validate() error {
	if strings.TrimSpace(p.GatewayCommit) == "" || len(p.GatewayCommit) > 256 {
		return errors.New("quality gate provenance requires gateway_commit")
	}
	if strings.ContainsAny(p.GatewayCommit, " \t\r\n") {
		return errors.New("quality gate gateway_commit must not contain whitespace")
	}
	if strings.TrimSpace(p.Model) == "" || len(p.Model) > 256 {
		return errors.New("quality gate provenance requires model")
	}
	if strings.TrimSpace(p.DeploymentID) == "" || len(p.DeploymentID) > 256 {
		return errors.New("quality gate provenance requires deployment_id")
	}
	if len(p.ProviderID) > 256 || len(p.RouteConfigID) > 256 {
		return errors.New("quality gate provenance identifier exceeds safe limit")
	}
	if p.RouteConfigHash != "" {
		if len(p.RouteConfigHash) != 64 {
			return errors.New("quality gate route_config_hash must be a 64-character hex digest when present")
		}
		for _, r := range p.RouteConfigHash {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return errors.New("quality gate route_config_hash must be hex")
			}
		}
	}
	switch p.Mode {
	case "replay", "live":
	default:
		return fmt.Errorf("quality gate mode must be replay or live, got %q", p.Mode)
	}
	if p.GeneratedAt.IsZero() {
		return errors.New("quality gate provenance requires generated_at")
	}
	return nil
}

// Input is one side of a comparison: recorded eval runs plus provenance
// plus, optionally, an actually observed total cost.
type Input struct {
	Format          string        `json:"format"`
	Provenance      Provenance    `json:"provenance"`
	Runs            []eval.Result `json:"runs"`
	ObservedCostUSD *float64      `json:"observed_cost_usd,omitempty"`
}

// Validate checks an input without scoring anything.
func (in Input) Validate() error {
	if in.Format != FormatInput {
		return fmt.Errorf("quality gate input format must be %q", FormatInput)
	}
	if err := in.Provenance.Validate(); err != nil {
		return err
	}
	if len(in.Runs) == 0 || len(in.Runs) > MaxGateRuns {
		return fmt.Errorf("quality gate input requires 1 to %d runs", MaxGateRuns)
	}
	if in.ObservedCostUSD != nil {
		c := *in.ObservedCostUSD
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
			return errors.New("quality gate observed cost must be a finite, non-negative number")
		}
	}
	seen := map[string]struct{}{}
	for _, r := range in.Runs {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("quality gate run %q: %w", r.RunID, err)
		}
		if _, dup := seen[r.SuiteID]; dup {
			return fmt.Errorf("quality gate input has duplicate runs for suite %q", r.SuiteID)
		}
		seen[r.SuiteID] = struct{}{}
		// A recorded run that claims a built-in suite version the catalog
		// does not have is stale evidence, not a baseline.
		if suite, ok := eval.LookupSuite(r.SuiteID); ok && suite.Version != r.SuiteVersion {
			return fmt.Errorf("quality gate run for suite %q has version %q, catalog has %q", r.SuiteID, r.SuiteVersion, suite.Version)
		}
	}
	return nil
}

// SuiteSummary is the aggregate view of one eval run. It carries counts
// and timings only; it never carries output text.
type SuiteSummary struct {
	SuiteID           string  `json:"suite_id"`
	SuiteVersion      string  `json:"suite_version"`
	Dimension         string  `json:"dimension,omitempty"`
	Scoreable         bool    `json:"scoreable"`
	Score             float64 `json:"score,omitempty"`
	ScoreObserved     bool    `json:"score_observed"`
	Samples           int     `json:"samples"`
	Cases             int     `json:"cases"`
	Pass              int     `json:"pass"`
	Fail              int     `json:"fail"`
	Error             int     `json:"error"`
	Missing           int     `json:"missing"`
	LatencyP50MS      float64 `json:"latency_p50_ms,omitempty"`
	LatencyP95MS      float64 `json:"latency_p95_ms,omitempty"`
	LatencySamples    int     `json:"latency_samples"`
	LatencyObserved   bool    `json:"latency_observed"`
	TTFTP50MS         float64 `json:"ttft_p50_ms,omitempty"`
	TTFTP95MS         float64 `json:"ttft_p95_ms,omitempty"`
	TTFTSamples       int     `json:"ttft_samples"`
	TTFTObserved      bool    `json:"ttft_observed"`
	ErrorRate         float64 `json:"error_rate,omitempty"`
	ErrorRateObserved bool    `json:"error_rate_observed"`
}

// Summary is the comparable view of one side of a comparison.
type Summary struct {
	Provenance      Provenance     `json:"provenance"`
	Suites          []SuiteSummary `json:"suites"`
	UpstreamCalls   int            `json:"upstream_calls"`
	ObservedCostUSD *float64       `json:"observed_cost_usd,omitempty"`
}

// Summarize converts recorded runs into a comparable summary. It makes
// no network call and reads no artifact output.
func Summarize(in Input) (Summary, error) {
	if err := in.Validate(); err != nil {
		return Summary{}, err
	}
	out := Summary{Provenance: in.Provenance, ObservedCostUSD: in.ObservedCostUSD}
	for _, r := range in.Runs {
		s := SuiteSummary{
			SuiteID:      r.SuiteID,
			SuiteVersion: r.SuiteVersion,
			Scoreable:    r.Scoreable,
			Samples:      r.Samples,
			Cases:        len(r.Cases),
			Pass:         r.Counts[eval.VerdictPass],
			Fail:         r.Counts[eval.VerdictFail],
			Error:        r.Counts[eval.VerdictError],
			Missing:      r.Counts[eval.VerdictMissing],
		}
		if suite, ok := eval.LookupSuite(r.SuiteID); ok {
			s.Dimension = string(suite.Dimension)
		}
		if r.Scoreable {
			s.Score = r.Score
			s.ScoreObserved = true
		}
		if len(r.Cases) > 0 {
			s.ErrorRate = float64(s.Error) / float64(len(r.Cases))
			s.ErrorRateObserved = true
		}
		var latencies, ttfts []float64
		for _, c := range r.Cases {
			if c.LatencyMS > 0 {
				latencies = append(latencies, float64(c.LatencyMS))
			}
			if c.TTFTMS > 0 {
				ttfts = append(ttfts, float64(c.TTFTMS))
			}
		}
		if len(latencies) > 0 {
			s.LatencyP50MS = percentile(latencies, 0.50)
			s.LatencyP95MS = percentile(latencies, 0.95)
			s.LatencySamples = len(latencies)
			s.LatencyObserved = true
		}
		if len(ttfts) > 0 {
			s.TTFTP50MS = percentile(ttfts, 0.50)
			s.TTFTP95MS = percentile(ttfts, 0.95)
			s.TTFTSamples = len(ttfts)
			s.TTFTObserved = true
		}
		out.Suites = append(out.Suites, s)
		out.UpstreamCalls += r.Upstream
	}
	sort.Slice(out.Suites, func(i, j int) bool { return out.Suites[i].SuiteID < out.Suites[j].SuiteID })
	return out, nil
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	if p <= 0 {
		return cp[0]
	}
	if p >= 1 {
		return cp[len(cp)-1]
	}
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// Thresholds configures what counts as a regression. All values are
// operator configuration, not measurements, and are recorded in the
// report so a verdict can always be re-derived.
type Thresholds struct {
	SuccessDrop    float64  `json:"success_drop"`
	ErrorRateRise  float64  `json:"error_rate_rise"`
	LatencyRisePct float64  `json:"latency_rise_pct"`
	TTFTRisePct    float64  `json:"ttft_rise_pct"`
	CostRisePct    float64  `json:"cost_rise_pct"`
	RequiredSuites []string `json:"required_suites"`
}

// DefaultThresholds returns the gate defaults: a 5-point absolute drop
// in task success or rise in error rate, or a 20% relative rise in
// latency, TTFT or cost, counts as a regression.
func DefaultThresholds() Thresholds {
	return Thresholds{
		SuccessDrop:    0.05,
		ErrorRateRise:  0.05,
		LatencyRisePct: 0.20,
		TTFTRisePct:    0.20,
		CostRisePct:    0.20,
		RequiredSuites: append([]string(nil), DefaultSuites...),
	}
}

// Validate bounds thresholds.
func (t Thresholds) Validate() error {
	for name, v := range map[string]float64{
		"success_drop": t.SuccessDrop, "error_rate_rise": t.ErrorRateRise,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("quality gate threshold %s must be between 0 and 1", name)
		}
	}
	for name, v := range map[string]float64{
		"latency_rise_pct": t.LatencyRisePct, "ttft_rise_pct": t.TTFTRisePct, "cost_rise_pct": t.CostRisePct,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 5 {
			return fmt.Errorf("quality gate threshold %s must be between 0 and 5", name)
		}
	}
	if len(t.RequiredSuites) == 0 || len(t.RequiredSuites) > MaxGateSuites {
		return fmt.Errorf("quality gate requires 1 to %d required suites", MaxGateSuites)
	}
	seen := map[string]struct{}{}
	for _, s := range t.RequiredSuites {
		if strings.TrimSpace(s) == "" {
			return errors.New("quality gate required suite id must not be empty")
		}
		if _, dup := seen[s]; dup {
			return fmt.Errorf("quality gate required suite %q is duplicated", s)
		}
		seen[s] = struct{}{}
	}
	return nil
}

// MetricComparison is one compared metric. Baseline and Candidate are
// nil exactly when the metric was not observed on that side; a nil
// value is reported as insufficient, never as zero.
type MetricComparison struct {
	Metric    string   `json:"metric"`
	Direction string   `json:"direction"`
	Baseline  *float64 `json:"baseline,omitempty"`
	Candidate *float64 `json:"candidate,omitempty"`
	Delta     *float64 `json:"delta,omitempty"`
	DeltaPct  *float64 `json:"delta_pct,omitempty"`
	Status    string   `json:"status"`
}

func fptr(v float64) *float64 { return &v }

// SuiteComparison is the per-suite comparison.
type SuiteComparison struct {
	SuiteID               string             `json:"suite_id"`
	SuiteVersionBaseline  string             `json:"suite_version_baseline,omitempty"`
	SuiteVersionCandidate string             `json:"suite_version_candidate,omitempty"`
	ScoreableBaseline     bool               `json:"scoreable_baseline"`
	ScoreableCandidate    bool               `json:"scoreable_candidate"`
	Metrics               []MetricComparison `json:"metrics"`
	Status                string             `json:"status"`
}

// Report is the full gate output. It contains no prompt or response
// content and it changes no routing: RoutingChanged is always false.
type Report struct {
	Format         string            `json:"format"`
	GeneratedAt    time.Time         `json:"generated_at"`
	GateSuiteSet   string            `json:"gate_suite_set"`
	Baseline       Provenance        `json:"baseline"`
	Candidate      Provenance        `json:"candidate"`
	Thresholds     Thresholds        `json:"thresholds"`
	Suites         []SuiteComparison `json:"suites"`
	Cost           MetricComparison  `json:"cost"`
	Overall        string            `json:"overall"`
	Regressions    []string          `json:"regressions,omitempty"`
	Improvements   []string          `json:"improvements,omitempty"`
	RoutingAction  string            `json:"routing_action"`
	RoutingChanged bool              `json:"routing_changed"`
	Notes          []string          `json:"notes,omitempty"`
}

func compareMetric(metric, direction string, baseline, candidate *float64, absThreshold, pctThreshold float64) MetricComparison {
	mc := MetricComparison{Metric: metric, Direction: direction, Baseline: baseline, Candidate: candidate}
	if baseline == nil || candidate == nil {
		mc.Status = StatusInsufficient
		return mc
	}
	delta := *candidate - *baseline
	mc.Delta = fptr(delta)
	if *baseline != 0 {
		mc.DeltaPct = fptr(delta / math.Abs(*baseline))
	}
	switch direction {
	case "higher_better":
		switch {
		case delta < -absThreshold:
			mc.Status = StatusRegression
		case delta > absThreshold:
			mc.Status = StatusImprovement
		default:
			mc.Status = StatusUnchanged
		}
	case "lower_better":
		// Absolute thresholds apply to ratio metrics (error rate);
		// relative thresholds apply to latency, TTFT and cost.
		if metric == MetricErrorRate {
			switch {
			case delta > absThreshold:
				mc.Status = StatusRegression
			case delta < -absThreshold:
				mc.Status = StatusImprovement
			default:
				mc.Status = StatusUnchanged
			}
			return mc
		}
		// Relative comparison; a zero baseline can only regress when the
		// candidate is positive, and improves when it stays zero.
		if *baseline == 0 {
			switch {
			case *candidate > 0:
				mc.Status = StatusRegression
			default:
				mc.Status = StatusUnchanged
			}
			return mc
		}
		rel := delta / math.Abs(*baseline)
		switch {
		case rel > pctThreshold:
			mc.Status = StatusRegression
		case rel < -pctThreshold:
			mc.Status = StatusImprovement
		default:
			mc.Status = StatusUnchanged
		}
	}
	return mc
}

// Compare builds a report from two summaries. It never scores an
// untested model: a required suite missing on either side, or not
// scoreable on either side, makes the overall verdict insufficient.
func Compare(baseline, candidate Summary, thresholds Thresholds) (Report, error) {
	if err := thresholds.Validate(); err != nil {
		return Report{}, err
	}
	if err := baseline.Provenance.Validate(); err != nil {
		return Report{}, fmt.Errorf("baseline provenance: %w", err)
	}
	if err := candidate.Provenance.Validate(); err != nil {
		return Report{}, fmt.Errorf("candidate provenance: %w", err)
	}
	bySuite := func(s Summary) map[string]SuiteSummary {
		m := make(map[string]SuiteSummary, len(s.Suites))
		for _, suite := range s.Suites {
			m[suite.SuiteID] = suite
		}
		return m
	}
	baseBySuite, candBySuite := bySuite(baseline), bySuite(candidate)
	rep := Report{
		Format:         FormatReport,
		GeneratedAt:    time.Now().UTC(),
		GateSuiteSet:   GateSuiteSetVersion,
		Baseline:       baseline.Provenance,
		Candidate:      candidate.Provenance,
		Thresholds:     thresholds,
		RoutingAction:  "none",
		RoutingChanged: false,
	}
	insufficient := false
	anyRegression := false
	for _, suiteID := range thresholds.RequiredSuites {
		base, baseOK := baseBySuite[suiteID]
		cand, candOK := candBySuite[suiteID]
		sc := SuiteComparison{SuiteID: suiteID}
		if baseOK {
			sc.SuiteVersionBaseline = base.SuiteVersion
			sc.ScoreableBaseline = base.Scoreable
		}
		if candOK {
			sc.SuiteVersionCandidate = cand.SuiteVersion
			sc.ScoreableCandidate = cand.Scoreable
		}
		var baseScore, candScore, baseErr, candErr, baseLat, candLat, baseTTFT, candTTFT *float64
		if baseOK && base.ScoreObserved {
			baseScore = fptr(base.Score)
		}
		if candOK && cand.ScoreObserved {
			candScore = fptr(cand.Score)
		}
		if baseOK && base.ErrorRateObserved {
			baseErr = fptr(base.ErrorRate)
		}
		if candOK && cand.ErrorRateObserved {
			candErr = fptr(cand.ErrorRate)
		}
		if baseOK && base.LatencyObserved {
			baseLat = fptr(base.LatencyP95MS)
		}
		if candOK && cand.LatencyObserved {
			candLat = fptr(cand.LatencyP95MS)
		}
		if baseOK && base.TTFTObserved {
			baseTTFT = fptr(base.TTFTP95MS)
		}
		if candOK && cand.TTFTObserved {
			candTTFT = fptr(cand.TTFTP95MS)
		}
		sc.Metrics = []MetricComparison{
			compareMetric(MetricTaskSuccess, "higher_better", baseScore, candScore, thresholds.SuccessDrop, 0),
			compareMetric(MetricErrorRate, "lower_better", baseErr, candErr, thresholds.ErrorRateRise, 0),
			compareMetric(MetricLatencyP95, "lower_better", baseLat, candLat, 0, thresholds.LatencyRisePct),
			compareMetric(MetricTTFTP95, "lower_better", baseTTFT, candTTFT, 0, thresholds.TTFTRisePct),
		}
		// Suite status: missing, version-mismatched or unscoreable on
		// either side is insufficient evidence, distinct from a
		// measured regression. Comparing two different suite versions
		// would compare two different tests.
		switch {
		case !baseOK || !candOK || !base.Scoreable || !cand.Scoreable || base.SuiteVersion != cand.SuiteVersion:
			sc.Status = StatusInsufficient
			insufficient = true
			rep.Notes = append(rep.Notes, fmt.Sprintf("suite %s: insufficient evidence for a conclusive comparison", suiteID))
		default:
			sc.Status = StatusUnchanged
			for _, m := range sc.Metrics {
				if m.Status == StatusRegression {
					sc.Status = StatusRegression
					anyRegression = true
					rep.Regressions = append(rep.Regressions, suiteID+":"+m.Metric)
				}
				if m.Status == StatusImprovement {
					rep.Improvements = append(rep.Improvements, suiteID+":"+m.Metric)
				}
			}
			// A regression in a measurable metric of an otherwise
			// insufficient suite is still surfaced below, but it does
			// not change the suite status: the suite was not fully
			// comparable. Surface it in the regressions list only when
			// the suite itself was comparable, which is the case here.
		}
		// Measurable regressions in an insufficient suite are still
		// worth listing for the operator, without changing the verdict.
		if sc.Status == StatusInsufficient {
			for _, m := range sc.Metrics {
				if m.Status == StatusRegression {
					rep.Regressions = append(rep.Regressions, suiteID+":"+m.Metric+" (partial evidence)")
				}
				if m.Status == StatusImprovement {
					rep.Improvements = append(rep.Improvements, suiteID+":"+m.Metric+" (partial evidence)")
				}
			}
		}
		rep.Suites = append(rep.Suites, sc)
	}
	rep.Cost = compareMetric(MetricCost, "lower_better", baseline.ObservedCostUSD, candidate.ObservedCostUSD, 0, thresholds.CostRisePct)
	switch rep.Cost.Status {
	case StatusRegression:
		anyRegression = true
		rep.Regressions = append(rep.Regressions, MetricCost)
	case StatusImprovement:
		rep.Improvements = append(rep.Improvements, MetricCost)
	case StatusInsufficient:
		// Cost is optional evidence: an unobserved cost is not, by
		// itself, insufficient evidence for the quality comparison.
		// It is recorded as insufficient on the cost metric only.
		rep.Notes = append(rep.Notes, "cost: unobserved on at least one side; cost is not compared")
	}
	switch {
	case insufficient:
		rep.Overall = OverallInsufficientEvidence
	case anyRegression:
		rep.Overall = OverallRegression
	default:
		rep.Overall = OverallNoRegression
	}
	return rep, nil
}

// LiveLimits are the hard ceilings for a live candidate. They are
// checked in addition to, never instead of, the live evaluation
// plane's own opt-in and bounds.
type LiveLimits struct {
	MaxRequests int      `json:"max_requests"`
	MaxCostUSD  *float64 `json:"max_cost_usd,omitempty"`
}

// CheckLive enforces live ceilings on a summary. A replay summary
// must report zero upstream calls; a live summary must stay within
// its request ceiling, and within its cost ceiling when a cost was
// actually observed.
func CheckLive(summary Summary, limits LiveLimits) error {
	if summary.Provenance.Mode == "replay" {
		if summary.UpstreamCalls != 0 {
			return errors.New("quality gate replay summary reports upstream calls")
		}
		return nil
	}
	if summary.Provenance.Mode != "live" {
		return fmt.Errorf("quality gate unknown mode %q", summary.Provenance.Mode)
	}
	if limits.MaxRequests <= 0 {
		return errors.New("quality gate live evaluation requires a positive max_requests limit")
	}
	if summary.UpstreamCalls > limits.MaxRequests {
		return fmt.Errorf("quality gate live request limit exceeded: %d upstream calls over limit %d", summary.UpstreamCalls, limits.MaxRequests)
	}
	if limits.MaxCostUSD != nil && summary.ObservedCostUSD != nil && *summary.ObservedCostUSD > *limits.MaxCostUSD {
		return fmt.Errorf("quality gate live cost limit exceeded: observed %.4f USD over limit %.4f USD", *summary.ObservedCostUSD, *limits.MaxCostUSD)
	}
	return nil
}

// RequireLiveAcknowledgement refuses a live candidate unless the
// operator explicitly acknowledged that live evaluation may spend
// provider quota. Replay candidates never need it.
func RequireLiveAcknowledgement(mode string, acknowledged bool) error {
	if mode == "live" && !acknowledged {
		return errors.New("quality gate refuses a live candidate without explicit acknowledgement of provider cost")
	}
	return nil
}

// Snapshot is a recorded summary that can serve as a future baseline.
type Snapshot struct {
	Format  string  `json:"format"`
	Summary Summary `json:"summary"`
}

// MarshalSnapshot serializes a summary as a baseline snapshot.
func MarshalSnapshot(summary Summary) ([]byte, error) {
	return json.MarshalIndent(Snapshot{Format: FormatSnapshot, Summary: summary}, "", "  ")
}

// ParseInput parses a gate input file with unknown fields rejected.
func ParseInput(data []byte) (Input, error) {
	if len(data) == 0 || len(data) > MaxGateInputBytes {
		return Input{}, fmt.Errorf("quality gate input must be 1 to %d bytes", MaxGateInputBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var in Input
	if err := dec.Decode(&in); err != nil {
		return Input{}, fmt.Errorf("invalid quality gate input: %w", err)
	}
	if err := in.Validate(); err != nil {
		return Input{}, err
	}
	return in, nil
}

// ParseSnapshot parses a baseline snapshot file.
func ParseSnapshot(data []byte) (Summary, error) {
	if len(data) == 0 || len(data) > MaxGateInputBytes {
		return Summary{}, fmt.Errorf("quality gate snapshot must be 1 to %d bytes", MaxGateInputBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var snap Snapshot
	if err := dec.Decode(&snap); err != nil {
		return Summary{}, fmt.Errorf("invalid quality gate snapshot: %w", err)
	}
	if snap.Format != FormatSnapshot {
		return Summary{}, fmt.Errorf("quality gate snapshot format must be %q", FormatSnapshot)
	}
	if err := snap.Summary.Provenance.Validate(); err != nil {
		return Summary{}, fmt.Errorf("snapshot provenance: %w", err)
	}
	if len(snap.Summary.Suites) == 0 || len(snap.Summary.Suites) > MaxGateSuites {
		return Summary{}, fmt.Errorf("quality gate snapshot requires 1 to %d suites", MaxGateSuites)
	}
	return snap.Summary, nil
}

// ParseBaseline accepts either a snapshot or a gate input file and
// returns its summary, so a recorded run can be used as a baseline
// directly or after being summarized once.
func ParseBaseline(data []byte) (Summary, error) {
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Summary{}, fmt.Errorf("invalid quality gate baseline: %w", err)
	}
	switch probe.Format {
	case FormatSnapshot:
		return ParseSnapshot(data)
	case FormatInput:
		in, err := ParseInput(data)
		if err != nil {
			return Summary{}, err
		}
		return Summarize(in)
	default:
		return Summary{}, fmt.Errorf("quality gate baseline format must be %q or %q", FormatSnapshot, FormatInput)
	}
}
