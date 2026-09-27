// Package evallive implements the Phase H *live* physical-deployment evaluation
// executor.
//
// Phase H measures models. Two execution modes exist and they are deliberately
// separate:
//
//   - Replay (internal/eval.ReplayExecutor) — offline. It replays recorded
//     artifacts and performs no I/O at all.
//   - Live (this package) — online. It targets exactly one explicitly selected
//     physical deployment and makes one real upstream model request per case.
//
// The live executor is a drop-in eval.Executor: the runner that consumes it is
// unchanged, so grading, verdict resolution, scorecard creation and every bound
// that already governs replay also govern live evaluation.
//
// What the live executor guarantees:
//
//   - No routing. The deployment is already selected by the evaluation request.
//     There is no LocalProvider, no PolicyProvider, no Jev provider, no hybrid
//     DecisionProvider chain, no candidate ordering and no fallback.
//   - No production state. The upstream call goes through an evaluation-isolated
//     clone of the provider adapter (see providers.EvaluationTwin), so
//     credentials, cooldowns, quota accounting, concurrency gauges, model
//     health, session affinity and the response cache are never touched — in
//     either direction. A live evaluation cannot improve and cannot degrade
//     production health.
//   - No second client architecture. The request is built and dispatched by the
//     provider adapter itself, using the same path resolution, auth
//     application, header handling and transport as the data plane.
//   - No invented evidence. A case without a prompt produces a "missing" result,
//     never a synthetic score. Timeouts, HTTP failures and empty completions are
//     recorded as the evidence they are and graded honestly.
//
// The package imports neither the router, nor the health manager, nor the
// decision plane, so it structurally cannot reach routing state.
package evallive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// Bounds for live evaluation. They are constants, not configuration: a live
// evaluation sends real prompts to a real model, so its blast radius must be
// cheaper to reason about than to tune.
const (
	// MaxPromptsPerRun bounds how many live upstream calls one run may make.
	MaxPromptsPerRun = eval.MaxOutcomesPerRun
	// MaxPromptBytes bounds one live prompt.
	MaxPromptBytes = 64 << 10
	// MaxCaseIDBytes bounds one case identifier.
	MaxCaseIDBytes = 256
	// MaxOutputTokens bounds the completion length a live evaluation may request.
	MaxOutputTokens = 4096
	// DefaultOutputTokens is used when the operator does not set one.
	DefaultOutputTokens = 512
)

// Deployment is the minimal, read-only projection of one physical deployment.
// It carries only the fields required to address a model on an upstream — no
// health, no score, no affinity, nothing the routing plane owns.
type Deployment struct {
	ID           string
	ProviderID   string
	Model        string
	ProviderType string
}

// Validate rejects a deployment that could not be addressed upstream.
func (d Deployment) Validate() error {
	if strings.TrimSpace(d.ID) == "" {
		return errors.New("live evaluation requires deployment_id")
	}
	if strings.TrimSpace(d.ProviderID) == "" {
		return errors.New("live evaluation deployment is missing provider_id")
	}
	if strings.TrimSpace(d.Model) == "" {
		return errors.New("live evaluation deployment is missing model")
	}
	if len(d.ID) > MaxCaseIDBytes {
		return errors.New("live evaluation deployment id exceeds safe limit")
	}
	return nil
}

// Prompt binds one live input to one suite case. The prompt is the evaluation's
// input only: it is never persisted in a run record, scorecard, event, metric or
// admin payload.
type Prompt struct {
	CaseID string `json:"case_id"`
	Prompt string `json:"prompt"`
}

// Validate bounds one prompt.
func (p Prompt) Validate() error {
	if strings.TrimSpace(p.CaseID) == "" {
		return errors.New("live prompt requires case_id")
	}
	if len(p.CaseID) > MaxCaseIDBytes {
		return errors.New("live prompt case_id exceeds safe limit")
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return fmt.Errorf("live prompt for case %q is empty", p.CaseID)
	}
	if len(p.Prompt) > MaxPromptBytes {
		return fmt.Errorf("live prompt for case %q exceeds safe limit", p.CaseID)
	}
	return nil
}

// MissingPromptError is returned when a suite case has no live prompt. It is a
// distinct sentinel so the runner records "missing" instead of inventing
// evidence or silently skipping the case.
type MissingPromptError struct {
	CaseID string
}

func (e *MissingPromptError) Error() string {
	return fmt.Sprintf("no live prompt for case %q", e.CaseID)
}

// ErrNoAdapter is returned when a deployment's provider has no usable adapter.
var ErrNoAdapter = errors.New("live evaluation deployment has no provider adapter")

// LiveEvaluationExecutor executes suite cases against one explicitly selected
// physical deployment.
//
// It is safe for concurrent use, but the runner executes cases sequentially, so
// in practice each run performs one upstream request per case, in suite order.
type LiveEvaluationExecutor struct {
	dep             Deployment
	adapter         providers.Adapter
	prompts         map[string]string
	maxOutputTokens int
	now             func() time.Time

	calls    atomic.Int64
	upstream atomic.Int64
	failures atomic.Int64
	missing  atomic.Int64
}

// NewLiveEvaluationExecutor builds a live executor for exactly one deployment.
//
// The adapter must be an evaluation-isolated clone (providers.EvaluationTwin);
// passing a production adapter is rejected rather than silently accepted, so a
// live evaluation can never mutate routing state by construction.
func NewLiveEvaluationExecutor(dep Deployment, adapter providers.Adapter, prompts []Prompt, maxOutputTokens int) (*LiveEvaluationExecutor, error) {
	if err := dep.Validate(); err != nil {
		return nil, err
	}
	if adapter == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoAdapter, dep.ID)
	}
	if providers.LiveCapable(adapter) == nil {
		return nil, errors.New("live evaluation refused: adapter is not evaluation-isolated")
	}
	if len(prompts) == 0 {
		return nil, errors.New("live evaluation requires at least one prompt")
	}
	if len(prompts) > MaxPromptsPerRun {
		return nil, fmt.Errorf("live evaluation accepts at most %d prompts", MaxPromptsPerRun)
	}
	byCase := make(map[string]string, len(prompts))
	for _, p := range prompts {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, dup := byCase[p.CaseID]; dup {
			return nil, fmt.Errorf("duplicate live prompt for case %q", p.CaseID)
		}
		byCase[p.CaseID] = p.Prompt
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = DefaultOutputTokens
	}
	if maxOutputTokens > MaxOutputTokens {
		maxOutputTokens = MaxOutputTokens
	}
	return &LiveEvaluationExecutor{
		dep:             dep,
		adapter:         adapter,
		prompts:         byCase,
		maxOutputTokens: maxOutputTokens,
		now:             func() time.Time { return time.Now().UTC() },
	}, nil
}

// Deployment returns the single physical deployment this executor targets.
func (e *LiveEvaluationExecutor) Deployment() Deployment { return e.dep }

// Execute performs exactly one real upstream request for one case and converts
// the result into evaluation evidence.
//
// Routing is never consulted: the deployment was fixed when the executor was
// built. A missing prompt, a transport failure, an HTTP failure, a timeout and
// an empty completion are all recorded as evidence and graded by the same
// deterministic evaluators that grade replayed artifacts.
func (e *LiveEvaluationExecutor) Execute(ctx context.Context, c eval.Case) (eval.Outcome, error) {
	if e == nil {
		return eval.Outcome{}, errors.New("live evaluation executor is not configured")
	}
	e.calls.Add(1)
	prompt, ok := e.prompts[c.ID]
	if !ok || strings.TrimSpace(prompt) == "" {
		e.missing.Add(1)
		return eval.Outcome{}, &MissingPromptError{CaseID: c.ID}
	}
	started := e.now()
	res, err := providers.LiveComplete(ctx, e.adapter, e.dep.Model, prompt, e.maxOutputTokens)
	if err != nil {
		// The executor itself could not run. Nothing was measured, so nothing is
		// scored: an executor failure must never become a synthetic verdict.
		return eval.Outcome{}, err
	}
	if res.UpstreamAttempt {
		e.upstream.Add(1)
	}
	elapsed := e.now().Sub(started).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	out := eval.Outcome{
		CaseID:       c.ID,
		LatencyMS:    elapsed,
		PromptTokens: res.PromptTokens,
		OutputTokens: res.OutputTokens,
	}
	switch res.ErrorType {
	case providers.LiveErrNone:
		out.Status = eval.OutcomeOK
		out.Output = res.Output
	case providers.LiveErrTimeout:
		e.failures.Add(1)
		out.Status = eval.OutcomeTimeout
		out.ErrorType = "upstream_timeout"
	case providers.LiveErrHTTP:
		e.failures.Add(1)
		out.Status = eval.OutcomeError
		out.ErrorType = httpErrorType(res.StatusCode)
	case providers.LiveErrEmpty:
		e.failures.Add(1)
		out.Status = eval.OutcomeError
		out.ErrorType = "empty_completion"
	case providers.LiveErrTransport:
		e.failures.Add(1)
		out.Status = eval.OutcomeError
		out.ErrorType = "upstream_transport"
	default:
		e.failures.Add(1)
		out.Status = eval.OutcomeError
		out.ErrorType = "upstream_unknown"
	}
	return out, nil
}

// Calls reports how many cases the executor was asked to evaluate.
func (e *LiveEvaluationExecutor) Calls() int { return int(e.calls.Load()) }

// UpstreamCalls reports how many real upstream requests the executor issued.
// One live evaluation case is exactly one upstream call — never a retry, a
// fallback or a second candidate.
func (e *LiveEvaluationExecutor) UpstreamCalls() int { return int(e.upstream.Load()) }

// Failures reports how many upstream calls ended in a bounded failure
// (timeout, HTTP error, empty completion, transport error).
func (e *LiveEvaluationExecutor) Failures() int { return int(e.failures.Load()) }

// Missing reports how many cases had no live prompt.
func (e *LiveEvaluationExecutor) Missing() int { return int(e.missing.Load()) }

// PromptCaseIDs returns the case ids with a live prompt, in deterministic order.
func (e *LiveEvaluationExecutor) PromptCaseIDs() []string {
	out := make([]string, 0, len(e.prompts))
	for id := range e.prompts {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// MaxOutputTokensForRun returns the completion budget in force for this run.
func (e *LiveEvaluationExecutor) MaxOutputTokensForRun() int { return e.maxOutputTokens }

func httpErrorType(status int) string {
	if status > 0 {
		return "http_" + itoa(status)
	}
	return "http_error"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
