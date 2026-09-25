// Package local implements the "local" built-in DecisionProvider: it always
// abstains, preserving the existing router order. It performs no I/O and
// never fails.
package local

import (
	"context"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// Provider is the local no-op decision provider.
type Provider struct{}

// New returns the local provider.
func New() *Provider { return &Provider{} }

// ID implements decision.DecisionProvider.
func (p *Provider) ID() string { return decision.BuiltinLocalID }

// Type implements decision.DecisionProvider.
func (p *Provider) Type() string { return decision.BuiltinLocalID }

// Capabilities implements decision.DecisionProvider.
func (p *Provider) Capabilities() decision.Capabilities {
	return decision.Capabilities{CanSelect: false, CanRank: false}
}

// Health implements decision.DecisionProvider.
func (p *Provider) Health() decision.Health {
	return decision.Health{Available: true, KeyConfigured: false, Detail: "ok"}
}

// Decide implements decision.DecisionProvider: always abstain.
func (p *Provider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	_ = ctx
	_ = req
	return decision.DecisionResult{
		Action:       decision.ActionAbstain,
		ReasonCodes:  []decision.ReasonCode{decision.ReasonLocalAbstained},
		ProviderID:   decision.BuiltinLocalID,
		ProviderType: decision.BuiltinLocalID,
	}, nil
}
