package jev

import (
	"context"
	"errors"
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

const (
	// ProviderType is the adapter type registered in the decision plane.
	ProviderType = "jev"
	// DefaultBaseURL is the official Jev service. Production calls go here
	// and only here: no base-URL override is exposed in config (SSRF-safe by
	// construction). Tests inject httptest endpoints via NewWithTransport.
	DefaultBaseURL = "https://www.jevai.org"
	// ModelRoutePath is the official model-routing endpoint. Never
	// /chat/completions for routing decisions.
	ModelRoutePath = "/api/v1/decisions/model-route"
	// PrivacyMetadataOnly is the sole external privacy mode in Phase F.
	PrivacyMetadataOnly = "metadata_only"
)

// DefaultEndpoint is the full production model-route URL.
const DefaultEndpoint = DefaultBaseURL + ModelRoutePath

// Provider is the Jev external DecisionProvider. It selects a new primary
// from the permitted band via the Jev model-route API; it never rewrites the
// failover list. A Provider is immutable after construction and safe for
// concurrent use; hot reload swaps it atomically.
type Provider struct {
	id          string
	apiKey      string // resolved snapshot; ONLY ever placed in Authorization
	privacyMode string
	endpoint    string
	client      *remote.Client
}

// New builds a production Jev provider. privacyMode must be metadata_only;
// apiKey may be empty (provider reports unavailable and assisted decisions
// fail open; the gateway itself stays usable).
func New(id, apiKey, privacyMode string) (*Provider, error) {
	return NewWithTransport(id, apiKey, privacyMode, DefaultEndpoint, nil)
}

// NewWithTransport builds a provider with an injected endpoint and transport.
// TESTS ONLY: production code must use New. The endpoint override exists so
// tests can point the adapter at httptest servers without weakening
// production validation (no config knob can change the endpoint).
func NewWithTransport(id, apiKey, privacyMode, endpoint string, transport http.RoundTripper) (*Provider, error) {
	if id == "" {
		return nil, errors.New("jev: provider ID is required")
	}
	if privacyMode != PrivacyMetadataOnly {
		return nil, errors.New("jev: only metadata_only privacy is supported")
	}
	if endpoint == "" {
		return nil, errors.New("jev: endpoint is required")
	}
	if len(apiKey) > remote.MaxAPIKeyBytes {
		return nil, errors.New("jev: api key exceeds size limit")
	}
	client := remote.NewClient()
	if transport != nil {
		client = remote.NewClientWithTransport(transport)
	}
	return &Provider{id: id, apiKey: apiKey, privacyMode: privacyMode, endpoint: endpoint, client: client}, nil
}

// ID implements decision.DecisionProvider: the configured ID (e.g. jev-main).
func (p *Provider) ID() string { return p.id }

// Type implements decision.DecisionProvider.
func (p *Provider) Type() string { return ProviderType }

// Capabilities implements decision.DecisionProvider: select-only.
func (p *Provider) Capabilities() decision.Capabilities {
	return decision.Capabilities{CanSelect: true, CanRank: false}
}

// Health implements decision.DecisionProvider. Unavailable when the key is
// unresolved; healthy when configured and callable. Never exposes secrets.
func (p *Provider) Health() decision.Health {
	if p.apiKey == "" {
		return decision.Health{Available: false, KeyConfigured: false, Detail: "missing api key"}
	}
	return decision.Health{Available: true, KeyConfigured: true, Detail: "ok"}
}

// errAbstainNotMappable signals "nothing to choose" (fewer than two allowed
// primaries). It is an abstention, not a failure.
type abstainSignal struct{}

func (abstainSignal) Error() string { return "nothing to choose between" }

func errAbstainNotMappable() error { return abstainSignal{} }

func errRequestTooLarge() error {
	return remote.NewError(remote.ClassRequestTooLarge, decision.ReasonExternalRequestTooLarge, "external decision request exceeds size limit")
}

// Decide implements decision.DecisionProvider: exactly one model-route call
// (no retries), strict response validation, opaque->physical mapping.
func (p *Provider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	base := decision.DecisionResult{ProviderID: p.id, ProviderType: ProviderType}
	if p.apiKey == "" {
		return base, remote.NewError(remote.ClassUnavailable, decision.ReasonExternalProviderUnavailable, "external decision provider unavailable")
	}
	// Only the permitted primary band is ever sent; look up full candidate
	// metadata for those IDs in band order.
	byID := make(map[string]decision.Candidate, len(req.Candidates))
	for _, c := range req.Candidates {
		if c.ID != "" {
			byID[c.ID] = c
		}
	}
	allowed := make([]decision.Candidate, 0, len(req.AllowedPrimaryIDs))
	for _, id := range req.AllowedPrimaryIDs {
		c, ok := byID[id]
		if !ok {
			// Band references outside E: refuse to consult remote
			// intelligence on an inconsistent request.
			return base, remote.NewError(remote.ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision band inconsistent")
		}
		allowed = append(allowed, c)
	}
	mapping, err := BuildMapping(allowed, req.Features)
	if err != nil {
		var abstain abstainSignal
		if errors.As(err, &abstain) {
			base.Action = decision.ActionAbstain
			base.ReasonCodes = []decision.ReasonCode{decision.ReasonExternalAbstained}
			return base, nil
		}
		return base, err
	}
	body, err := p.client.PostJSON(ctx, p.endpoint, p.apiKey, mapping.Body)
	if err != nil {
		return base, err
	}
	physical, conf, err := ParseResponse(body, mapping.OpaqueToPhysical)
	if err != nil {
		return base, err
	}
	base.Action = decision.ActionSelect
	base.SelectedID = physical
	base.Confidence = conf
	base.ReasonCodes = []decision.ReasonCode{decision.ReasonExternalSelected}
	return base, nil
}
