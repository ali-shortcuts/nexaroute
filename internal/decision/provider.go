package decision

import "context"

// Built-in provider IDs. External provider IDs must never collide with these;
// config validation rejects such collisions before any request executes.
const (
	BuiltinLocalID  = "local"
	BuiltinPolicyID = "policy"
)

// Capabilities describes what a DecisionProvider is allowed to do. Phase F
// providers may only select a new primary; none may rewrite the failover
// list (the orchestrator preserves the remainder of the router order).
type Capabilities struct {
	CanSelect bool
	CanRank   bool
}

// Health describes external-provider availability. It is strictly separated
// from upstream model health: a network failure must never penalize a model,
// and a model failure is recorded only for executed deployments.
type Health struct {
	// Available reports whether the provider can serve a decision now.
	Available bool
	// KeyConfigured reports whether the provider's credential resolved,
	// without revealing any secret material.
	KeyConfigured bool
	// Detail is a short, bounded, secret-free status string.
	Detail string
}

// DecisionProvider is the generic routing-decision contract owned by
// NexaRoute. Every provider — built-in or external — implements it; no
// provider-specific types may leak into the core.
type DecisionProvider interface {
	// ID returns the configured provider ID (e.g. "jev-main"), unique per
	// registry. It is operator-chosen and NOT the adapter type.
	ID() string
	// Type returns the adapter type (e.g. "jev", "local", "policy").
	Type() string
	// Capabilities declares what the provider may do.
	Capabilities() Capabilities
	// Health reports current availability without secret material.
	Health() Health
	// Decide selects a new primary from req.AllowedPrimaryIDs or abstains.
	// Implementations MUST honor ctx cancellation, MUST make at most one
	// external network call (no retries), and MUST NOT mutate req.
	Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
}
