package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Outcome status vocabulary for recorded artifacts.
const (
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomeTimeout = "timeout"
	OutcomeRefused = "refused"
)

// ToolCall is a recorded tool invocation.
type ToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// UnitTestResult is a recorded compile/test outcome for coding suites.
type UnitTestResult struct {
	Compiled bool   `json:"compiled"`
	Passed   int    `json:"passed"`
	Failed   int    `json:"failed"`
	Log      string `json:"log,omitempty"`
}

// StreamResult is a recorded streaming outcome for protocol suites.
type StreamResult struct {
	Terminated bool   `json:"terminated"`
	Events     int    `json:"events"`
	Error      string `json:"error,omitempty"`
}

// Outcome is one recorded model behavior. It is *evidence*, not a prompt: it is
// produced outside the request path (operator harness, probe, or a previous
// evaluation) and replayed deterministically here.
type Outcome struct {
	CaseID       string          `json:"case_id"`
	Status       string          `json:"status"`
	Output       string          `json:"output,omitempty"`
	ToolCall     *ToolCall       `json:"tool_call,omitempty"`
	UnitTests    *UnitTestResult `json:"unit_tests,omitempty"`
	Stream       *StreamResult   `json:"stream,omitempty"`
	LatencyMS    int64           `json:"latency_ms,omitempty"`
	TTFTMS       int64           `json:"ttft_ms,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	PromptTokens int             `json:"prompt_tokens,omitempty"`
	ErrorType    string          `json:"error_type,omitempty"`
}

// Validate bounds one artifact.
func (o Outcome) Validate() error {
	if strings.TrimSpace(o.CaseID) == "" {
		return errors.New("artifact is missing case_id")
	}
	if len(o.CaseID) > 256 {
		return errors.New("artifact case_id exceeds safe limit")
	}
	if len(o.Output) > MaxOutputBytes {
		return fmt.Errorf("artifact %q output exceeds safe limit", o.CaseID)
	}
	switch o.Status {
	case "", OutcomeOK, OutcomeError, OutcomeTimeout, OutcomeRefused:
	default:
		return fmt.Errorf("artifact %q has unknown status %q", o.CaseID, o.Status)
	}
	if o.UnitTests != nil && len(o.UnitTests.Log) > MaxOutputBytes {
		return fmt.Errorf("artifact %q test log exceeds safe limit", o.CaseID)
	}
	if o.Stream != nil && len(o.Stream.Error) > MaxReasonBytes {
		return fmt.Errorf("artifact %q stream error exceeds safe limit", o.CaseID)
	}
	if o.ToolCall != nil && (len(o.ToolCall.Name) > 256 || len(o.ToolCall.Arguments) > MaxOutputBytes) {
		return fmt.Errorf("artifact %q tool call exceeds safe limit", o.CaseID)
	}
	if o.LatencyMS < 0 || o.TTFTMS < 0 {
		return fmt.Errorf("artifact %q has negative timing", o.CaseID)
	}
	return nil
}

// Executor produces the model behavior for a case. Phase H ships the replay
// executor (recorded artifacts, no network). A caller may supply any executor;
// the runner then only ever reads its outputs and cannot influence routing state.
type Executor interface {
	Execute(ctx context.Context, c Case) (Outcome, error)
}

// ReplayExecutor answers cases from recorded artifacts. It performs no I/O, so a
// replayed evaluation cannot touch upstream providers, credentials or health.
type ReplayExecutor struct {
	byCase map[string]Outcome
	calls  int
}

// NewReplayExecutor indexes artifacts by case id. Duplicate case ids are an
// error: silently picking one would hide operator mistakes.
func NewReplayExecutor(outcomes []Outcome) (*ReplayExecutor, error) {
	byCase := make(map[string]Outcome, len(outcomes))
	for _, o := range outcomes {
		if err := o.Validate(); err != nil {
			return nil, err
		}
		if _, dup := byCase[o.CaseID]; dup {
			return nil, fmt.Errorf("duplicate artifact for case %q", o.CaseID)
		}
		byCase[o.CaseID] = o
	}
	return &ReplayExecutor{byCase: byCase}, nil
}

// Execute returns the recorded artifact or a sentinel error.
func (r *ReplayExecutor) Execute(_ context.Context, c Case) (Outcome, error) {
	r.calls++
	o, ok := r.byCase[c.ID]
	if !ok {
		return Outcome{}, errNoArtifact
	}
	return o, nil
}

// Calls reports how many executions were served. Replay always reports its call
// count; the point is that no upstream call happened.
func (r *ReplayExecutor) Calls() int { return r.calls }

// UpstreamCalls reports network calls made by this executor: always zero.
func (r *ReplayExecutor) UpstreamCalls() int { return 0 }

var errNoArtifact = errors.New("no recorded artifact for case")

// Runner executes suites. It is stateless apart from the evaluator registry, so
// concurrent runs are safe.
type Runner struct {
	registry *Registry
	now      func() time.Time
}

// NewRunner builds a runner with the built-in deterministic evaluators.
func NewRunner() *Runner {
	return &Runner{registry: NewRegistry(), now: func() time.Time { return time.Now().UTC() }}
}

// NewRunnerWithRegistry builds a runner over a caller-supplied registry (used by
// tests to inject a judge).
func NewRunnerWithRegistry(r *Registry) *Runner {
	if r == nil {
		return NewRunner()
	}
	return &Runner{registry: r, now: func() time.Time { return time.Now().UTC() }}
}

// Evaluators exposes the registry (read-only helpers only).
func (r *Runner) Evaluators() *Registry { return r.registry }

// Request is one evaluation run request.
type Request struct {
	RunID           string               `json:"run_id,omitempty"`
	DeploymentID    string               `json:"deployment_id"`
	ProviderID      string               `json:"provider_id,omitempty"`
	Model           string               `json:"model,omitempty"`
	SuiteID         string               `json:"suite_id"`
	JudgeEnabled    bool                 `json:"judge_enabled,omitempty"`
	CaseTimeoutMS   int                  `json:"case_timeout_ms,omitempty"`
	LatencyTargetMS float64              `json:"latency_target_ms,omitempty"`
	TTFTTargetMS    float64              `json:"ttft_target_ms,omitempty"`
	Outcomes        []Outcome            `json:"artifacts,omitempty"`
	Dimension       scorecards.Dimension `json:"-"`
}

// Bounds for runs.
const (
	MaxOutcomesPerRun = 512
	MaxRunTimeoutMS   = 60_000
	// MinDecisiveSamples is the floor below which no scorecard value is produced,
	// independent of the suite's own minimum.
	MinDecisiveSamples = 1
)

// CaseResult is the bounded per-case outcome.
type CaseResult struct {
	CaseID     string          `json:"case_id"`
	Verdict    Verdict         `json:"verdict"`
	Resolution string          `json:"resolution"`
	Weight     float64         `json:"weight"`
	Verdicts   []VerdictResult `json:"verdicts,omitempty"`
	LatencyMS  int64           `json:"latency_ms,omitempty"`
	ErrorType  string          `json:"error_type,omitempty"`
}

// DimensionScore is one produced dimension value, kept in the result so callers
// can see exactly what evidence backed the scorecard.
type DimensionScore struct {
	Dimension  scorecards.Dimension `json:"dimension"`
	Score      float64              `json:"score"`
	Raw        float64              `json:"raw,omitempty"`
	Samples    int                  `json:"samples"`
	Confidence float64              `json:"confidence"`
}

// Result is the bounded, JSON-serializable outcome of one run.
type Result struct {
	RunID        string    `json:"run_id"`
	SuiteID      string    `json:"suite_id"`
	SuiteVersion string    `json:"suite_version"`
	DeploymentID string    `json:"deployment_id"`
	ProviderID   string    `json:"provider_id,omitempty"`
	Model        string    `json:"model,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	DurationMS   int64     `json:"duration_ms"`

	Cases      []CaseResult     `json:"cases"`
	Counts     map[Verdict]int  `json:"counts"`
	Score      float64          `json:"score"`
	Samples    int              `json:"samples"`
	Scoreable  bool             `json:"scoreable"`
	Reason     string           `json:"reason,omitempty"`
	Quality    *DimensionScore  `json:"quality,omitempty"`
	Extra      []DimensionScore `json:"extra_dimensions,omitempty"`
	JudgeUsed  bool             `json:"judge_used"`
	Upstream   int              `json:"upstream_calls"`
	Evaluators []string         `json:"evaluators"`
}

// Validate bounds a result read back from persistence or a client payload.
func (r Result) Validate() error {
	if r.RunID == "" || r.SuiteID == "" || r.DeploymentID == "" {
		return errors.New("evaluation result requires run_id, suite_id and deployment_id")
	}
	if len(r.Cases) > MaxOutcomesPerRun {
		return errors.New("evaluation result exceeds case bound")
	}
	if !finite01(r.Score) {
		return errors.New("evaluation score must be finite and between 0 and 1")
	}
	return nil
}

func finite01(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

// Run executes a suite against recorded artifacts.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	exec, err := NewReplayExecutor(req.Outcomes)
	if err != nil {
		return Result{}, err
	}
	return r.RunWithExecutor(ctx, req, exec)
}

// RunWithExecutor executes a suite against an arbitrary executor. Cases are
// evaluated in suite order; artifacts for unknown cases are ignored so a run
// cannot invent coverage.
func (r *Runner) RunWithExecutor(ctx context.Context, req Request, exec Executor) (Result, error) {
	suite, ok := LookupSuite(req.SuiteID)
	if !ok {
		return Result{}, fmt.Errorf("unknown evaluation suite %q", req.SuiteID)
	}
	if err := ValidateSuite(suite); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(req.DeploymentID) == "" {
		return Result{}, errors.New("evaluation requires deployment_id")
	}
	if exec == nil {
		return Result{}, errors.New("evaluation requires an executor")
	}
	if len(req.Outcomes) > MaxOutcomesPerRun {
		return Result{}, fmt.Errorf("evaluation expects at most %d artifacts", MaxOutcomesPerRun)
	}
	started := r.now()
	res := Result{
		RunID:        req.RunID,
		SuiteID:      suite.ID,
		SuiteVersion: suite.Version,
		DeploymentID: req.DeploymentID,
		ProviderID:   req.ProviderID,
		Model:        req.Model,
		StartedAt:    started,
		Counts:       map[Verdict]int{},
		Cases:        make([]CaseResult, 0, len(suite.Cases)),
		Evaluators:   r.registry.IDs(),
	}
	if res.RunID == "" {
		res.RunID = fmt.Sprintf("%s-%d", suite.ID, started.UnixNano())
	}

	caseTimeout := time.Duration(0)
	if req.CaseTimeoutMS > 0 {
		ms := req.CaseTimeoutMS
		if ms > MaxRunTimeoutMS {
			ms = MaxRunTimeoutMS
		}
		caseTimeout = time.Duration(ms) * time.Millisecond
	}

	var latencies []float64
	statusSamples, okSamples := 0, 0
	for _, c := range suite.Cases {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		caseCtx := ctx
		var cancel context.CancelFunc
		if caseTimeout > 0 {
			caseCtx, cancel = context.WithTimeout(ctx, caseTimeout)
		}
		outcome, err := exec.Execute(caseCtx, c)
		if cancel != nil {
			cancel()
		}
		cr := CaseResult{CaseID: c.ID, Weight: c.Weight}
		if err != nil {
			// Missing artifact and executor failures are recorded honestly: no
			// verdict is invented, and nothing is scored.
			cr.Verdict = VerdictMissing
			cr.Resolution = "no-evidence"
			switch {
			case errors.Is(err, errNoArtifact):
				cr.ErrorType = "missing_artifact"
			case errors.Is(err, context.DeadlineExceeded):
				cr.Verdict = VerdictError
				cr.Resolution = "timeout"
				cr.ErrorType = "case_timeout"
			default:
				cr.Verdict = VerdictError
				cr.Resolution = "executor-error"
				cr.ErrorType = "executor_error"
			}
			res.Counts[cr.Verdict]++
			res.Cases = append(res.Cases, cr)
			continue
		}
		if err := outcome.Validate(); err != nil {
			cr.Verdict = VerdictError
			cr.Resolution = "invalid-artifact"
			cr.ErrorType = "invalid_artifact"
			res.Counts[cr.Verdict]++
			res.Cases = append(res.Cases, cr)
			continue
		}
		if outcome.LatencyMS > 0 {
			latencies = append(latencies, float64(outcome.LatencyMS))
		}
		if outcome.Status != "" {
			statusSamples++
			if outcome.Status == OutcomeOK {
				okSamples++
			}
		}
		var verdicts []VerdictResult
		for _, e := range r.registry.Deterministic() {
			if !e.Supports(c.Expectation.Kind) {
				continue
			}
			if len(verdicts) >= MaxVerdictsPerCase {
				break
			}
			verdicts = append(verdicts, e.Evaluate(ctx, c, outcome))
		}
		for _, e := range r.registry.Judges() {
			if len(verdicts) >= MaxVerdictsPerCase {
				break
			}
			vr := e.Evaluate(ctx, c, outcome)
			if vr.Verdict != VerdictUnjudged {
				res.JudgeUsed = true
			}
			verdicts = append(verdicts, vr)
		}
		verdict, resolution := Resolve(verdicts, req.JudgeEnabled)
		cr.Verdict = verdict
		cr.Resolution = resolution
		cr.Verdicts = verdicts
		cr.LatencyMS = outcome.LatencyMS
		cr.ErrorType = outcome.ErrorType
		res.Counts[verdict]++
		res.Cases = append(res.Cases, cr)
	}

	// Weighted score over decisive cases only. Undecidable cases are reported,
	// never counted as failures or as successes.
	weightedTotal, weightedPassed, decisive := 0.0, 0.0, 0
	for _, cr := range res.Cases {
		if !cr.Verdict.Decisive() {
			continue
		}
		decisive++
		weightedTotal += cr.Weight
		if cr.Verdict == VerdictPass {
			weightedPassed += cr.Weight
		}
	}
	res.Samples = decisive
	if weightedTotal > 0 {
		res.Score = clamp01(weightedPassed / weightedTotal)
	}
	if decisive < suite.MinSamples || decisive < MinDecisiveSamples {
		res.Scoreable = false
		res.Reason = "insufficient_samples"
	} else {
		res.Scoreable = true
		ds := DimensionScore{
			Dimension:  suite.Dimension,
			Score:      res.Score,
			Raw:        res.Score,
			Samples:    decisive,
			Confidence: scorecards.ConfidenceFromSamples(decisive),
		}
		res.Quality = &ds
	}
	// Reliability evidence from recorded statuses.
	if statusSamples >= suite.MinSamples {
		rate := float64(okSamples) / float64(statusSamples)
		res.Extra = append(res.Extra, DimensionScore{
			Dimension:  scorecards.DimAvailability,
			Score:      clamp01(rate),
			Raw:        rate,
			Samples:    statusSamples,
			Confidence: scorecards.ConfidenceFromSamples(statusSamples),
		})
		res.Extra = append(res.Extra, DimensionScore{
			Dimension:  scorecards.DimFailureRate,
			Score:      clamp01(1 - rate),
			Raw:        1 - rate,
			Samples:    statusSamples,
			Confidence: scorecards.ConfidenceFromSamples(statusSamples),
		})
	}
	// Performance evidence needs a comparison target; without one the dimension
	// is omitted instead of scored against an invented budget.
	if len(latencies) >= suite.MinSamples {
		p95 := percentile(latencies, 0.95)
		if req.LatencyTargetMS > 0 {
			res.Extra = append(res.Extra, DimensionScore{
				Dimension:  scorecards.DimLatencyMS,
				Score:      clamp01(1 - p95/req.LatencyTargetMS),
				Raw:        p95,
				Samples:    len(latencies),
				Confidence: scorecards.ConfidenceFromSamples(len(latencies)),
			})
		}
	}
	finished := r.now()
	res.FinishedAt = finished
	res.DurationMS = finished.Sub(started).Milliseconds()
	res.Upstream = upstreamCalls(exec)
	return res, nil
}

// upstreamCalls reports how many real upstream requests an executor made.
//
// Replay reports a constant zero because it performs no I/O. A live executor
// reports its real count, so a run record always states whether evidence came
// from recorded artifacts or from a physical deployment.
func upstreamCalls(exec Executor) int {
	type upstreamCounter interface{ UpstreamCalls() int }
	if u, ok := exec.(upstreamCounter); ok {
		return u.UpstreamCalls()
	}
	return 0
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// percentile computes a nearest-rank percentile over a copy of the values, so it
// is deterministic and does not mutate the caller's slice.
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

// Scorecard converts a run into a scorecard. It returns ok=false when the run
// produced no scoreable evidence: an evaluation that measured nothing must not
// create a scorecard.
func (r Result) Scorecard() (scorecards.Scorecard, bool, error) {
	if !r.Scoreable || r.Quality == nil {
		return scorecards.Scorecard{}, false, nil
	}
	ev := scorecards.EvaluationEvidence{
		DeploymentID: r.DeploymentID,
		ProviderID:   r.ProviderID,
		Model:        r.Model,
		SuiteID:      r.SuiteID,
		SuiteVersion: r.SuiteVersion,
		RunID:        r.RunID,
		EvaluatedAt:  r.FinishedAt,
	}
	ev.Values = append(ev.Values, scorecards.EvaluationValue{
		Dimension:   r.Quality.Dimension,
		Score:       r.Quality.Score,
		Raw:         r.Quality.Raw,
		SampleCount: r.Quality.Samples,
	})
	for _, d := range r.Extra {
		ev.Values = append(ev.Values, scorecards.EvaluationValue{
			Dimension:   d.Dimension,
			Score:       d.Score,
			Raw:         d.Raw,
			SampleCount: d.Samples,
		})
	}
	sc, err := scorecards.FromEvaluation(ev)
	if err != nil {
		return scorecards.Scorecard{}, false, err
	}
	return sc, true, nil
}

// EvaluationHealth is the evaluation-plane health namespace. It is derived only
// from evaluation runs: it is never an input to routing or to the routing health
// manager.
type EvaluationHealth struct {
	DeploymentID string    `json:"deployment_id"`
	Runs         int       `json:"runs"`
	Samples      int       `json:"samples"`
	PassRate     float64   `json:"pass_rate"`
	Status       string    `json:"status"` // measured | insufficient
	LastRunAt    time.Time `json:"last_run_at"`
}

// HealthFromRuns aggregates evaluation health in deterministic order.
func HealthFromRuns(runs []Result) []EvaluationHealth {
	type acc struct {
		runs, samples int
		passed        float64
		last          time.Time
	}
	byDeployment := map[string]*acc{}
	for _, run := range runs {
		a := byDeployment[run.DeploymentID]
		if a == nil {
			a = &acc{}
			byDeployment[run.DeploymentID] = a
		}
		a.runs++
		a.samples += run.Samples
		a.passed += run.Score * float64(run.Samples)
		if run.FinishedAt.After(a.last) {
			a.last = run.FinishedAt
		}
	}
	out := make([]EvaluationHealth, 0, len(byDeployment))
	for id, a := range byDeployment {
		h := EvaluationHealth{DeploymentID: id, Runs: a.runs, Samples: a.samples, LastRunAt: a.last}
		if a.samples > 0 {
			h.PassRate = clamp01(a.passed / float64(a.samples))
		}
		if a.samples >= MinDecisiveSamples {
			h.Status = "measured"
		} else {
			h.Status = "insufficient"
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out
}
