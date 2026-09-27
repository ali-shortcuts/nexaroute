package decision

import (
	"context"
	"time"
)

// DecisionProvider is the contract for all decision intelligence.
// Phase D: only local provider is implemented. External providers (e.g. Jev)
// will be isolated adapters in Phase F. Core routing must never depend on
// a specific provider implementation.
//
// Contract:
//   - Decide MUST obey ctx.Done(). Implementations MUST use context-aware I/O
//     and return promptly after cancellation. No unbounded goroutines.
//     Future HTTP adapters must bind requests to the supplied context.
//   - Decide MUST NOT return candidates outside the eligible set. Orchestrator enforces this.
//   - Decide MUST return finite confidence in [0,1], not NaN/Inf.
//   - Decide MUST return bounded reason codes from canonical set only.
//   - Decide MUST respect Action contract: SELECT requires selected_id, RANK requires ranked_ids, ABSTAIN requires empty payload.
//   - Decide MUST NOT leak raw prompts, secrets, headers, or chain-of-thought.
type DecisionProvider interface {
	// ID returns a stable identifier for metrics and provenance. Bounded length.
	ID() string
	// Capabilities describes what the provider can do (ranking, selection, etc).
	Capabilities() Capabilities
	// Health returns current health status for admin/metrics, independent of model/provider health.
	// If Status is unavailable, orchestrator will not call Decide and fail-open with PROVIDER_UNHEALTHY.
	Health() ProviderHealth
	// Decide ranks or selects within the eligible set.
	// ctx is derived from Budget timeout and parent request context.
	// Implementations MUST honor ctx.Done() and return promptly.
	Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
}

// Capabilities describes provider features.
type Capabilities struct {
	CanRank   bool `json:"can_rank"`
	CanSelect bool `json:"can_select"`
	// Future: CanScore, CanExplain, etc.
}

// ProviderHealth is a minimal health snapshot for the provider itself
// (distinct from deployment health).
type ProviderHealth struct {
	Status    string    `json:"status"` // healthy, degraded, unavailable
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// ProviderHealth constants
const (
	HealthHealthy     = "healthy"
	HealthDegraded    = "degraded"
	HealthUnavailable = "unavailable"
)
