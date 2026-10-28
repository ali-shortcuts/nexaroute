package nexaflow

import (
	"context"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Cost nodes are read-only views over the existing usage tracker and
// budget ledger. They never reprice anything: the totals returned are
// the tracker's own snapshot, including CostBasis, CostCompleteness, and
// UnpricedTokens verbatim. A figure whose basis is "unknown" is not a
// cost at all, and this node says so instead of presenting a number
// as exact.
//
// BUDGET CHECK IS NOT ENFORCEMENT. There is no hard-budget enforcement
// in NexaRoute on main: a hard-budget design proposal is pending owner
// review and is NOT implemented. nexaroute.cost.budget_check only reads
// committed/reserved amounts and compares them against an
// operator-configured threshold, reporting over_threshold. It does not
// reserve, settle, block, or reject anything, and a workflow that needs
// a hard stop must implement that stop in the runtime, not by reading
// this node's output as a guarantee.
// Retry semantics: read-only, retryable on timeout. Cancellation:
// context propagation only.
func costNodeSpecs() []*NodeSpec {
	return []*NodeSpec{
		{
			ID: "nexaroute.cost.usage", Version: SupportedVersion, Category: "cost",
			Title:       "Cost and usage inspection",
			Description: "Usage snapshot from the existing usage tracker, with cost basis, completeness, and unpriced tokens surfaced verbatim. Never repriced. Read-only.",
			InputSchema: nil,
			OutputSchema: []Field{
				{Name: "total_prompt_tokens", Type: "number"},
				{Name: "total_completion_tokens", Type: "number"},
				{Name: "total_requests", Type: "number"},
				{Name: "total_estimated_cost_usd", Type: "number"},
				{Name: "total_cost_basis", Type: "string", Description: "recorded|estimated|mixed|unknown; unknown means the figure is not a cost."},
				{Name: "total_unpriced_tokens", Type: "number"},
				{Name: "by_deployment", Type: "array"},
			},
			ConfigSchema: nil,
			Permission:   authz.ReadUsage, SideEffect: SideEffectRead,
			Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout", "backend_error"}},
			DefaultTimeout: 10 * time.Second, MaxTimeout: 30 * time.Second,
			MaxInputBytes: 1024, MaxOutputBytes: 256 << 10,
			Run: runCostUsage,
		},
		{
			ID: "nexaroute.cost.budget_check", Version: SupportedVersion, Category: "cost",
			Title:       "Budget threshold check (report only)",
			Description: "Compares a budget ledger snapshot for one key against an operator threshold and reports over_threshold. This is NOT enforcement and NOT a reservation: nothing is blocked, reserved, or settled by this node.",
			InputSchema: []Field{
				{Name: "key", Type: "string", Required: true, Description: "Budget ledger key."},
				{Name: "threshold_usd", Type: "number", Required: true, Description: "Operator-configured threshold in USD; must be >= 0."},
			},
			OutputSchema: []Field{
				{Name: "key", Type: "string"},
				{Name: "committed", Type: "number"},
				{Name: "reserved", Type: "number"},
				{Name: "total", Type: "number", Description: "committed + reserved."},
				{Name: "remaining", Type: "number", Description: "threshold - total; may be negative."},
				{Name: "over_threshold", Type: "boolean", Description: "Report only. Does not block anything."},
			},
			ConfigSchema: nil,
			Permission:   authz.ReadUsage, SideEffect: SideEffectRead,
			Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout", "backend_error"}},
			DefaultTimeout: 10 * time.Second, MaxTimeout: 30 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 4096,
			Run: runBudgetCheck,
		},
	}
}

func runCostUsage(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	snap, err := rc.Backend.UsageSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	byDeploy := make([]any, 0, len(snap.ByDeployment))
	for _, row := range snap.ByDeployment {
		byDeploy = append(byDeploy, map[string]any{
			"deployment": row.Deployment, "prompt_tokens": row.PromptTokens,
			"completion_tokens": row.CompletionTokens, "requests": row.Requests,
			"estimated_cost_usd": row.EstimatedCostUSD, "cost_basis": row.CostBasis,
			"cost_completeness": row.CostCompleteness, "unpriced_tokens": row.UnpricedTokens,
		})
	}
	return map[string]any{
		"total_prompt_tokens":      snap.TotalPromptTokens,
		"total_completion_tokens":  snap.TotalCompletionTokens,
		"total_requests":           snap.TotalRequests,
		"total_estimated_cost_usd": snap.TotalEstimatedCostUSD,
		"total_cost_basis":         snap.TotalCostBasis,
		"total_unpriced_tokens":    snap.TotalUnpricedTokens,
		"by_deployment":            byDeploy,
	}, nil
}

func runBudgetCheck(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	key, ok := getString(rc.Input, "key")
	if !ok || strings.TrimSpace(key) == "" {
		return nil, NewNodeError("validation_error", false, "input key is required")
	}
	threshold, ok := getNumber(rc.Input, "threshold_usd")
	if !ok || !isFinite(threshold) || threshold < 0 {
		return nil, NewNodeError("validation_error", false, "threshold_usd is required and must be >= 0")
	}
	info, err := rc.Backend.BudgetSnapshot(ctx, key)
	if err != nil {
		return nil, err
	}
	total := info.Committed + info.Reserved
	return map[string]any{
		"key": info.Key, "committed": info.Committed, "reserved": info.Reserved,
		"total": total, "remaining": threshold - total,
		"over_threshold": total > threshold,
	}, nil
}
