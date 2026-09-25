package decision

import (
	"context"
	"time"
)

// LocalProvider is the Phase D deterministic pass-through provider.
// It preserves the exact order of the eligible set and exercises the
// DecisionProvider contract without changing routing semantics.
type LocalProvider struct{}

func (p *LocalProvider) ID() string { return "local" }

func (p *LocalProvider) Capabilities() Capabilities {
	return Capabilities{CanRank: true, CanSelect: false}
}

func (p *LocalProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy, CheckedAt: time.Now()}
}

func (p *LocalProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	// Respect context cancellation (timeout) — MUST obey ctx.Done() per contract
	select {
	case <-ctx.Done():
		return DecisionResult{
			Action:      ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []ReasonCode{ReasonTimeout, ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, ctx.Err()
	default:
	}

	// Pass-through: return ABSTAIN to signal "preserve existing order"
	return DecisionResult{
		Action:      ActionAbstain,
		Abstained:   true,
		Confidence:  1.0,
		ReasonCodes: []ReasonCode{ReasonExistingOrderPreserved, ReasonLocalPassThrough},
		ProviderID:  p.ID(),
	}, nil
}

// Ensure interface
var _ DecisionProvider = (*LocalProvider)(nil)
