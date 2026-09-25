package decision

import (
	"context"
	"time"
)

// DecisionProvider is the contract for all decision intelligence.
// Phase D: only local provider is implemented. External providers (e.g. Jev)
// will be isolated adapters in Phase F. Core routing must never depend on
// a specific provider implementation.
type DecisionProvider interface {
	// ID returns a stable identifier for metrics and provenance.
	ID() string
	// Capabilities describes what the provider can do (ranking, selection, etc).
	Capabilities() Capabilities
	// Health returns current health status for admin/metrics.
	Health() ProviderHealth
	// Decide ranks or selects within the eligible set. It MUST NOT return
	// candidates outside the eligible set. The orchestrator enforces this.
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
