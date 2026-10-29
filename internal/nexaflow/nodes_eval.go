package nexaflow

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// nexaroute.eval.run.v1 — REPLAY ONLY. It runs the existing eval.Runner
// over caller-supplied outcomes (eval.Runner.Run builds a ReplayExecutor:
// no network, no model calls, upstream_calls is always 0). The suite must
// exist in eval.Catalog (LookupSuite inside the runner); outcomes are
// bounded by eval.MaxOutcomesPerRun (512).
//
// LIVE-MODEL EVALUATION IS NOT IMPLEMENTED HERE. The admin live path
// (httpapi adminEvaluationRunLive) is not exposed as a Go API, so there
// is no honest way for this package to run a live evaluation, and this
// node must never be presented as producing live scores. Any future live
// node needs its own design, policy, and billing approval.
// Retry semantics: replay is deterministic and side-effect free, so a
// retry is safe (MaxAttempts 2, timeout only — a replay that fails on
// validation will fail identically on retry).
func evalNodeSpecs() []*NodeSpec {
	return []*NodeSpec{{
		ID: "nexaroute.eval.run", Version: SupportedVersion, Category: "evaluation",
		Title:       "Evaluation suite replay",
		Description: "Runs an existing evaluation suite in REPLAY mode over caller-supplied outcomes using the existing evaluation plane. Live-model evaluation is NOT implemented; scores describe recorded outcomes only.",
		InputSchema: []Field{
			{Name: "suite_id", Type: "string", Required: true, Description: "Suite id from eval.Catalog."},
			{Name: "deployment_id", Type: "string", Required: true, Description: "Deployment the recorded outcomes are attributed to."},
			{Name: "outcomes", Type: "array", Required: true, Description: "Recorded outcomes (eval.Outcome objects), at most 512."},
		},
		OutputSchema: []Field{
			{Name: "score", Type: "number"},
			{Name: "counts", Type: "object"},
			{Name: "scoreable", Type: "boolean"},
			{Name: "reason", Type: "string"},
			{Name: "judge_used", Type: "boolean"},
			{Name: "upstream_calls", Type: "number", Description: "Always 0 for replay."},
			{Name: "mode", Type: "string", Description: "Always replay."},
		},
		ConfigSchema: nil,
		Permission:   authz.RunEvaluation, SideEffect: SideEffectRead,
		Retry:          RetryPolicy{MaxAttempts: 2, RetryableCodes: []string{"timeout"}},
		DefaultTimeout: 30 * time.Second, MaxTimeout: 60 * time.Second,
		MaxInputBytes: 1 << 20, MaxOutputBytes: 256 << 10,
		Run: runEvalReplay,
	}}
}

func runEvalReplay(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	suiteID, ok := getString(rc.Input, "suite_id")
	if !ok || strings.TrimSpace(suiteID) == "" {
		return nil, NewNodeError("validation_error", false, "input suite_id is required")
	}
	deploymentID, ok := getString(rc.Input, "deployment_id")
	if !ok || strings.TrimSpace(deploymentID) == "" {
		return nil, NewNodeError("validation_error", false, "input deployment_id is required")
	}
	if _, ok := eval.LookupSuite(suiteID); !ok {
		return nil, NewNodeError("validation_error", false, "unknown evaluation suite %q", suiteID)
	}
	rawOutcomes, ok := getArray(rc.Input, "outcomes")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input outcomes must be an array")
	}
	if len(rawOutcomes) > eval.MaxOutcomesPerRun {
		return nil, NewNodeError("validation_error", false, "outcomes exceed %d", eval.MaxOutcomesPerRun)
	}
	raw, err := json.Marshal(rawOutcomes)
	if err != nil {
		return nil, NewNodeError("validation_error", false, "outcomes are not JSON-encodable")
	}
	var outcomes []eval.Outcome
	if err := json.Unmarshal(raw, &outcomes); err != nil {
		return nil, NewNodeError("validation_error", false, "outcomes do not match the eval.Outcome shape")
	}
	res, err := rc.Backend.RunEvalReplay(ctx, suiteID, deploymentID, outcomes)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]any, len(res.Counts))
	for verdict, n := range res.Counts {
		counts[string(verdict)] = n
	}
	return map[string]any{
		"score": res.Score, "counts": counts, "scoreable": res.Scoreable,
		"reason": res.Reason, "judge_used": res.JudgeUsed,
		"upstream_calls": res.Upstream, "mode": "replay",
		"suite_id": res.SuiteID, "samples": res.Samples,
	}, nil
}
