package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// liveRequestError carries an HTTP status so the admin handler can answer with
// 409 (feature gated off) vs 400 (malformed payload) distinctly.
type liveRequestError struct {
	status int
	msg    string
}

func (e *liveRequestError) Error() string { return e.msg }

// liveExecutorFor validates the live-mode payload and builds the executor
// bound to the requested deployment's own provider adapter. The deployment
// must exist (checked by the caller); this resolves only its configured
// provider — no router candidate selection is involved anywhere.
func (s *Server) liveExecutorFor(cfg config.EvaluationConfig, dep deploymentView, suite eval.Suite, in evaluationRunRequest) (eval.Executor, error) {
	if !cfg.LiveEnabled {
		return nil, &liveRequestError{status: http.StatusConflict, msg: "live evaluation is disabled (set evaluation.live_enabled)"}
	}
	if len(in.Artifacts) > 0 {
		return nil, &liveRequestError{status: http.StatusBadRequest, msg: "artifacts are replay evidence; live mode sends inputs to the selected deployment"}
	}
	if len(in.Inputs) == 0 {
		return nil, &liveRequestError{status: http.StatusBadRequest, msg: "inputs are required: live mode sends them to the explicitly selected deployment"}
	}
	if len(in.Inputs) > cfg.MaxArtifacts {
		return nil, &liveRequestError{status: http.StatusBadRequest, msg: "too many live inputs for one run"}
	}
	caseIDs := make(map[string]struct{}, len(suite.Cases))
	for _, c := range suite.Cases {
		caseIDs[c.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(in.Inputs))
	for _, input := range in.Inputs {
		if err := input.Validate(); err != nil {
			return nil, &liveRequestError{status: http.StatusBadRequest, msg: err.Error()}
		}
		if _, dup := seen[input.CaseID]; dup {
			return nil, &liveRequestError{status: http.StatusBadRequest, msg: "duplicate live input for case " + input.CaseID}
		}
		seen[input.CaseID] = struct{}{}
		if _, known := caseIDs[input.CaseID]; !known {
			return nil, &liveRequestError{status: http.StatusBadRequest, msg: "live input references unknown suite case: " + input.CaseID}
		}
	}
	s.runtimeMu.RLock()
	adapter, ok := s.reg.Get(dep.ProviderID)
	s.runtimeMu.RUnlock()
	if !ok {
		return nil, &liveRequestError{status: http.StatusConflict, msg: "deployment provider adapter is unavailable (provider disabled?)"}
	}
	exec, err := NewLiveEvaluationExecutor(adapter, dep.Model, in.Inputs)
	if err != nil {
		return nil, &liveRequestError{status: http.StatusBadRequest, msg: err.Error()}
	}
	return exec, nil
}

// LiveEvaluationExecutor is the Phase H live-mode executor. It evaluates one
// explicitly selected physical deployment by sending each case's declared
// input to that deployment through the existing provider adapter — one
// bounded, non-streaming completion per case with a declared input — and
// turning the response into an artifact the deterministic evaluators judge.
//
// Isolation, by construction (strict tests prove every line):
//   - No router selection: the executor never builds a router.Requirement,
//     never calls Router.Candidates/Eligible/Route and never sees a second
//     deployment. The admin handler resolves the deployment and its provider
//     adapter beforehand; this executor only ever holds that one adapter.
//   - No DecisionProviders: the request path (candidatesForRequirement →
//     applyDecisionPlane → orchestrator) is never entered, so the local,
//     policy, Jev and hybrid-chain providers are never consulted and their
//     health state cannot change.
//   - No production health: health.Manager.RecordSuccess/RecordFailure and
//     provider-incident recording live only in the request-path handlers and
//     the probe engine. The executor calls neither, so model and provider
//     health snapshots are untouched regardless of the upstream result.
//   - No production cache: the response cache is written only by the
//     request-path handlers (cacheStoreResponse); evaluation never reaches it.
//   - No session affinity: the router session table is written only by
//     ObserveSession from the request path; evaluation never goes there.
//   - No routing state: the router's round-robin cursor, weights, priorities
//     and candidate ordering are never read or written.
//   - One upstream client stack: the traffic uses the production
//     providers.Adapter/transport (credentials, TLS, timeouts); nothing here
//     creates an http.Client.
//
// Privacy: prompts and model outputs never enter run records, events, metrics
// or health state. Case results carry verdicts and bounded error categories
// only; upstream error bodies are dropped after status classification.
type LiveEvaluationExecutor struct {
	completer providers.Completer
	adapterID string
	model     string
	inputs    map[string]eval.Input
	calls     int
}

// NewLiveEvaluationExecutor builds a live executor bound to exactly one
// provider adapter (the provider of the explicitly selected deployment) and
// that deployment's upstream model id.
func NewLiveEvaluationExecutor(adapter providers.Adapter, model string, inputs []eval.Input) (*LiveEvaluationExecutor, error) {
	if adapter == nil {
		return nil, errors.New("live evaluation requires a provider adapter")
	}
	completer, ok := adapter.(providers.Completer)
	if !ok {
		return nil, fmt.Errorf("provider adapter %q cannot make bounded completion calls", adapter.ID())
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("live evaluation requires the deployment model")
	}
	byCase := make(map[string]eval.Input, len(inputs))
	for _, in := range inputs {
		if err := in.Validate(); err != nil {
			return nil, err
		}
		if _, dup := byCase[in.CaseID]; dup {
			return nil, fmt.Errorf("duplicate live input for case %q", in.CaseID)
		}
		byCase[in.CaseID] = in
	}
	return &LiveEvaluationExecutor{
		completer: completer,
		adapterID: adapter.ID(),
		model:     model,
		inputs:    byCase,
	}, nil
}

// Execute performs one real upstream completion for the case and converts it
// into an artifact. Cases without a declared input are missing evidence, not
// failures. HTTP failures are honest evidence (the model deployment answered
// with an error status). Transport/context failures surface as executor
// errors; the runner records the case as error/timeout without inventing
// evidence.
func (l *LiveEvaluationExecutor) Execute(ctx context.Context, c eval.Case) (eval.Outcome, error) {
	in, ok := l.inputs[c.ID]
	if !ok {
		return eval.Outcome{}, fmt.Errorf("%w: case %q has no live input", eval.ErrNoEvidence, c.ID)
	}
	l.calls++
	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = eval.DefaultLiveMaxTokens
	}
	comp, err := l.completer.Complete(ctx, providers.CompletionRequest{
		Model: l.model, System: in.System, Prompt: in.Prompt, MaxTokens: maxTokens,
	})
	if comp.StatusCode > 0 && (comp.StatusCode < 200 || comp.StatusCode >= 300) {
		// The deployment answered with a non-2xx status: that is real evidence
		// about its behavior. The verdict evaluators see a non-ok status and
		// the availability/failure_rate dimensions see the failure. The
		// upstream error body is dropped here — it is diagnostic text that
		// may echo the request and never enters any record.
		return eval.Outcome{
			CaseID:    c.ID,
			Status:    eval.OutcomeError,
			ErrorType: "http_" + strconv.Itoa(comp.StatusCode),
			LatencyMS: comp.Latency.Milliseconds(),
		}, nil
	}
	if err != nil {
		return eval.Outcome{}, err
	}
	status := eval.OutcomeOK
	if comp.Refusal {
		status = eval.OutcomeRefused
	}
	out := eval.Outcome{
		CaseID:       c.ID,
		Status:       status,
		Output:       comp.Text,
		LatencyMS:    comp.Latency.Milliseconds(),
		PromptTokens: comp.InputTokens,
		OutputTokens: comp.OutputTokens,
	}
	if len(comp.ToolCalls) > 0 {
		out.ToolCall = &eval.ToolCall{Name: comp.ToolCalls[0].Name, Arguments: comp.ToolCalls[0].Arguments}
	}
	return out, nil
}

// Calls reports how many live completions were executed: one per case with a
// declared input.
func (l *LiveEvaluationExecutor) Calls() int { return l.calls }

// UpstreamCalls satisfies the runner's upstream reporting. Live mode is the
// only mode where this is non-zero, and it is exactly the number of real model
// requests made to the explicitly selected deployment.
func (l *LiveEvaluationExecutor) UpstreamCalls() int { return l.calls }
