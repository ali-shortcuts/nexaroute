// Package policy implements the "policy" built-in DecisionProvider: a
// deterministic local selector over the shared primary-selection guardrails.
// It selects the forced pin when present, otherwise the first candidate of
// the permitted primary band in existing router order. It performs no I/O,
// never fails, and shares ComputeConstraints semantics with the orchestrator,
// so external providers can never obtain weaker guardrails than policy.
package policy

import (
	"context"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// Provider is the deterministic local policy provider.
type Provider struct{}

// New returns the policy provider.
func New() *Provider { return &Provider{} }

// ID implements decision.DecisionProvider.
func (p *Provider) ID() string { return decision.BuiltinPolicyID }

// Type implements decision.DecisionProvider.
func (p *Provider) Type() string { return decision.BuiltinPolicyID }

// Capabilities implements decision.DecisionProvider.
func (p *Provider) Capabilities() decision.Capabilities {
	return decision.Capabilities{CanSelect: true, CanRank: false}
}

// Health implements decision.DecisionProvider.
func (p *Provider) Health() decision.Health {
	return decision.Health{Available: true, KeyConfigured: false, Detail: "ok"}
}

// Decide implements decision.DecisionProvider.
func (p *Provider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	_ = ctx
	base := decision.DecisionResult{
		ProviderID:   decision.BuiltinPolicyID,
		ProviderType: decision.BuiltinPolicyID,
	}
	// Forced affinity pin wins (the orchestrator normally short-circuits
	// before reaching here; this keeps direct calls consistent).
	if req.ForcedPrimaryID != "" {
		base.Action = decision.ActionSelect
		base.SelectedID = req.ForcedPrimaryID
		base.ReasonCodes = []decision.ReasonCode{decision.ReasonAffinityPreserved}
		return base, nil
	}
	if len(req.AllowedPrimaryIDs) == 0 {
		base.Action = decision.ActionAbstain
		base.ReasonCodes = []decision.ReasonCode{decision.ReasonLocalAbstained}
		return base, nil
	}
	// First allowed candidate in existing router order: deterministic, and
	// identical to the shared guardrail semantics.
	base.Action = decision.ActionSelect
	base.SelectedID = req.AllowedPrimaryIDs[0]
	base.ReasonCodes = []decision.ReasonCode{decision.ReasonPolicySelected}
	return base, nil
}
