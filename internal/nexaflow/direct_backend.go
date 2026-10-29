package nexaflow

import (
	"context"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// DirectHealthCompatBackend adapts the in-process health manager and
// compat store directly — no HTTP, no duplication. It implements only
// the health/compat ports; compose it with other backends for a full
// Backend. A deployment absent from the manager reports status "unknown"
// (health.Manager.Get zero value) rather than an invented status, and a
// deployment absent from the compat store reports all capabilities
// "unknown" (compat's own zero-contract semantics), never "supported".
type DirectHealthCompatBackend struct {
	Health      *health.Manager
	CompatStore *compat.Store
}

func (b *DirectHealthCompatBackend) ProviderHealth(_ context.Context, deploymentID string) (ProviderHealth, error) {
	if b.Health == nil {
		return ProviderHealth{}, NewNodeError("backend_unavailable", true, "health manager is not wired")
	}
	state := b.Health.Get(deploymentID)
	return ProviderHealth{
		DeploymentID: deploymentID, Status: string(state.Status),
		Successes: state.Successes, Failures: state.Failures,
		ConsecutiveFailures: state.ConsecutiveFailures,
		LastError:           state.LastError, CooldownUntil: state.CooldownUntil,
	}, nil
}

func (b *DirectHealthCompatBackend) Compat(_ context.Context, deploymentID string) (CompatInfo, error) {
	if b.CompatStore == nil {
		return CompatInfo{}, NewNodeError("backend_unavailable", true, "compat store is not wired")
	}
	contract := b.CompatStore.Get(deploymentID)
	support := make(map[string]string, len(compat.AllCapabilities))
	for _, name := range compat.AllCapabilities {
		support[name] = contract.Capabilities.Get(name).String()
	}
	// Health for the scorecard comes from the health manager when wired;
	// otherwise "unknown" — never invented from the contract alone.
	healthStatus := "unknown"
	if b.Health != nil {
		healthStatus = string(b.Health.Get(deploymentID).Status)
	}
	return CompatInfo{
		DeploymentID: deploymentID, Support: support,
		ScorecardStatus: contract.Scorecard(healthStatus).Status,
		Protocol:        contract.Capabilities.NativeProtocol,
		ContextWindow:   contract.Capabilities.ContextWindow,
		MaxOutput:       contract.Capabilities.MaxOutput,
	}, nil
}

// DirectCostBackend adapts the in-process usage tracker and budget ledger
// directly. Snapshots are returned verbatim: costs are never repriced,
// and CostBasis/CostCompleteness/UnpricedTokens are surfaced, not hidden.
// BudgetSnapshot is a read of committed/reserved amounts only; it does
// not reserve, settle, or enforce anything.
type DirectCostBackend struct {
	Tracker *usage.Tracker
	Ledger  *usage.BudgetLedger
	Prices  map[string]usage.Price
}

func (b *DirectCostBackend) UsageSnapshot(_ context.Context) (UsageInfo, error) {
	if b.Tracker == nil {
		return UsageInfo{}, NewNodeError("backend_unavailable", true, "usage tracker is not wired")
	}
	snap := b.Tracker.Snapshot(b.Prices)
	out := UsageInfo{
		TotalPromptTokens:     snap.TotalPromptTokens,
		TotalCompletionTokens: snap.TotalCompletionTokens,
		TotalRequests:         snap.TotalRequests,
		TotalEstimatedCostUSD: snap.TotalEstimatedCostUSD,
		TotalCostBasis:        snap.TotalCostBasis,
		TotalUnpricedTokens:   snap.TotalUnpricedTokens,
	}
	for _, row := range snap.ByDeployment {
		out.ByDeployment = append(out.ByDeployment, DeploymentUsage{
			Deployment: row.Deployment, PromptTokens: row.PromptTokens,
			CompletionTokens: row.CompletionTok, Requests: row.Requests,
			EstimatedCostUSD: row.EstimatedCost, CostBasis: row.CostBasis,
			CostCompleteness: row.CostCompleteness, UnpricedTokens: row.UnpricedTokens,
		})
	}
	return out, nil
}

func (b *DirectCostBackend) BudgetSnapshot(_ context.Context, key string) (BudgetInfo, error) {
	if b.Ledger == nil {
		return BudgetInfo{}, NewNodeError("backend_unavailable", true, "budget ledger is not wired")
	}
	committed, reserved := b.Ledger.Snapshot(key, time.Now())
	return BudgetInfo{Key: key, Committed: committed, Reserved: reserved}, nil
}
