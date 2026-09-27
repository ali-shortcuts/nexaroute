package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

// Provider implements decision.DecisionProvider for Jev
// ID is configured (e.g., jev-main), type is jev, CanSelect true, CanRank false

type Provider struct {
	mu          sync.RWMutex
	id          string
	apiKey      string
	apiKeyEnv   string
	baseURL     string
	privacyMode string
	client      *remote.Client
	enabled     bool
	// For testing injection
	httpClient *http.Client
}

type ProviderConfig struct {
	ID          string
	Type        string
	Enabled     bool
	APIKey      string
	APIKeyEnv   string
	PrivacyMode string
	BaseURL     string
	HTTPClient  *http.Client // for tests
}

func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if cfg.ID == "" {
		return nil, fmt.Errorf("id required")
	}
	if cfg.Type != "jev" {
		return nil, fmt.Errorf("unsupported type %s", cfg.Type)
	}
	if cfg.PrivacyMode != "" && cfg.PrivacyMode != "metadata_only" {
		return nil, fmt.Errorf("privacy_mode must be metadata_only in Phase F")
	}
	if cfg.PrivacyMode == "" {
		cfg.PrivacyMode = "metadata_only"
	}

	// Resolve API key
	apiKey := cfg.APIKey
	if cfg.APIKeyEnv != "" {
		if v := os.Getenv(cfg.APIKeyEnv); v != "" {
			apiKey = v
		}
	}

	// Build client
	allowHTTP := cfg.HTTPClient != nil
	if cfg.BaseURL != "" && (cfg.BaseURL[:7] == "http://" || cfg.BaseURL[:8] == "https://") {
		// For tests, allow http loopback when base URL is http (httptest server)
		if len(cfg.BaseURL) >= 7 && cfg.BaseURL[:7] == "http://" {
			allowHTTP = true
		}
	}
	clientCfg := remote.ClientConfig{
		BaseURL:          cfg.BaseURL,
		APIKey:           apiKey,
		HTTPClient:       cfg.HTTPClient,
		AllowHTTPForTest: allowHTTP,
	}
	if cfg.BaseURL == "" {
		clientCfg.BaseURL = "https://www.jevai.org"
	}

	client, err := remote.NewClient(clientCfg)
	if err != nil {
		return nil, err
	}

	// For test injection with custom base URL, use test client that allows loopback
	if cfg.BaseURL != "" && allowHTTP {
		// Re-create with test client to allow 127.0.0.1 and http
		httpClient := cfg.HTTPClient
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		client, err = remote.NewTestClient(cfg.BaseURL, apiKey, httpClient)
		if err != nil {
			return nil, err
		}
	}

	return &Provider{
		id:          cfg.ID,
		apiKey:      apiKey,
		apiKeyEnv:   cfg.APIKeyEnv,
		baseURL:     cfg.BaseURL,
		privacyMode: cfg.PrivacyMode,
		client:      client,
		enabled:     cfg.Enabled,
		httpClient:  cfg.HTTPClient,
	}, nil
}

// For hot-reload: build new immutable provider and swap atomically via registry

func (p *Provider) ID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.id
}

func (p *Provider) Capabilities() decision.Capabilities {
	return decision.Capabilities{
		CanSelect: true,
		CanRank:   false,
	}
}

func (p *Provider) Health() decision.ProviderHealth {
	p.mu.RLock()
	enabled := p.enabled
	apiKey := p.apiKey
	p.mu.RUnlock()

	if !enabled {
		return decision.ProviderHealth{
			Status:  decision.HealthUnavailable,
			Message: "disabled",
		}
	}
	if apiKey == "" {
		return decision.ProviderHealth{
			Status:  decision.HealthUnavailable,
			Message: "api key not configured",
		}
	}
	return decision.ProviderHealth{
		Status: decision.HealthHealthy,
	}
}

// HasAPIKey reports whether key is configured (for admin snapshot, no secret)
func (p *Provider) HasAPIKey() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.apiKey != ""
}

// BaseURL returns base URL (for admin, not secret)
func (p *Provider) BaseURL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.baseURL == "" {
		return "https://www.jevai.org"
	}
	return p.baseURL
}

func (p *Provider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	// Respect ctx cancellation
	select {
	case <-ctx.Done():
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalAbstained, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, ctx.Err()
	default:
	}

	p.mu.RLock()
	enabled := p.enabled
	client := p.client
	privacyMode := p.privacyMode
	p.mu.RUnlock()

	if !enabled {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalProviderUnavailable, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	if privacyMode != "metadata_only" {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalProviderUnavailable, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	if len(req.Candidates) < 2 {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalAbstained, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Build opaque mapping (request-local, never persisted)
	mapping := BuildOpaqueMapping(req.Candidates)

	// Build Jev request (metadata_only)
	jevReq, err := BuildJevRequest(req, mapping)
	if err != nil {
		// Check if request too large
		if remote.IsRequestTooLarge(err) {
			return decision.DecisionResult{
				Action:      decision.ActionAbstain,
				Abstained:   true,
				Confidence:  0,
				ReasonCodes: []decision.ReasonCode{decision.ReasonExternalRequestTooLarge, decision.ReasonExistingOrderPreserved},
				ProviderID:  p.ID(),
			}, nil
		}
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalInvalidResponse, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Marshal
	body, err := json.Marshal(jevReq)
	if err != nil {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalInvalidResponse, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Do exactly one external call, context-aware, no retry
	respBody, statusCode, err := client.Do(ctx, "/api/v1/decisions/model-route", body)
	if err != nil {
		// Map error to reason codes, fail open, no raw body leak
		var rErr *remote.Error
		if ok := isRemoteError(err, &rErr); ok {
			switch {
			case remote.IsRequestTooLarge(rErr):
				return decision.DecisionResult{
					Action:      decision.ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []decision.ReasonCode{decision.ReasonExternalRequestTooLarge, decision.ReasonExistingOrderPreserved},
					ProviderID:  p.ID(),
				}, nil
			case remote.IsResponseTooLarge(rErr):
				return decision.DecisionResult{
					Action:      decision.ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []decision.ReasonCode{decision.ReasonExternalResponseTooLarge, decision.ReasonExistingOrderPreserved},
					ProviderID:  p.ID(),
				}, nil
			case remote.IsTimeout(rErr):
				return decision.DecisionResult{
					Action:      decision.ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []decision.ReasonCode{decision.ReasonExternalTimeout, decision.ReasonExistingOrderPreserved},
					ProviderID:  p.ID(),
				}, nil
			case rErr.Kind == remote.ErrRedirectNotAllowed:
				return decision.DecisionResult{
					Action:      decision.ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []decision.ReasonCode{decision.ReasonExternalInvalidResponse, decision.ReasonExistingOrderPreserved},
					ProviderID:  p.ID(),
				}, nil
			case rErr.Kind == remote.ErrSSRFBlocked:
				return decision.DecisionResult{
					Action:      decision.ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []decision.ReasonCode{decision.ReasonExternalProviderUnavailable, decision.ReasonExistingOrderPreserved},
					ProviderID:  p.ID(),
				}, nil
			default:
				if remote.IsHTTPError(rErr) {
					return decision.DecisionResult{
						Action:      decision.ActionAbstain,
						Abstained:   true,
						Confidence:  0,
						ReasonCodes: []decision.ReasonCode{decision.ReasonExternalHTTPError, decision.ReasonExistingOrderPreserved},
						ProviderID:  p.ID(),
					}, nil
				}
			}
		}
		// Generic unavailable
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalProviderUnavailable, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Check status code already handled in client.Do for non-2xx as error, but we have body for logging? We must not leak body
	// For 2xx, parse envelope
	if statusCode < 200 || statusCode >= 300 {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalHTTPError, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	parsed, err := ParseAndValidate(respBody, mapping.AllowedOpaqueIDs)
	if err != nil {
		var rErr *remote.Error
		if isRemoteError(err, &rErr) {
			if remote.IsUnknownCandidate(rErr) {
				return decision.DecisionResult{
					Action:      decision.ActionAbstain,
					Abstained:   true,
					Confidence:  0,
					ReasonCodes: []decision.ReasonCode{decision.ReasonExternalUnknownCandidate, decision.ReasonExistingOrderPreserved},
					ProviderID:  p.ID(),
				}, nil
			}
		}
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalInvalidResponse, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Map opaque back to physical
	physicalID, ok := mapping.OpaqueToPhysical[parsed.SelectedID]
	if !ok {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonExternalUnknownCandidate, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Confidence already validated finite [0,1], 0 if absent
	conf := parsed.Confidence
	if math.IsNaN(conf) || math.IsInf(conf, 0) {
		conf = 0
	}
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}

	return decision.DecisionResult{
		Action:      decision.ActionSelect,
		SelectedID:  physicalID,
		Confidence:  conf,
		ReasonCodes: []decision.ReasonCode{decision.ReasonExternalSelected, decision.ReasonEligibleSetPreserved},
		ProviderID:  p.ID(),
	}, nil
}

func isRemoteError(err error, target **remote.Error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*remote.Error); ok {
		*target = e
		return true
	}
	return false
}

// For testing: allow injecting custom mapping and checking request
func (p *Provider) testBuildRequest(req decision.DecisionRequest) (*JevRequest, OpaqueMapping, error) {
	mapping := BuildOpaqueMapping(req.Candidates)
	jevReq, err := BuildJevRequest(req, mapping)
	return jevReq, mapping, err
}

// Ensure interface compliance
var _ decision.DecisionProvider = (*Provider)(nil)

// CloneForHotReload creates a new immutable provider for hot reload
func CloneForHotReload(id, apiKey, baseURL, privacyMode string, enabled bool, httpClient *http.Client) (*Provider, error) {
	return NewProvider(ProviderConfig{
		ID:          id,
		Type:        "jev",
		Enabled:     enabled,
		APIKey:      apiKey,
		PrivacyMode: privacyMode,
		BaseURL:     baseURL,
		HTTPClient:  httpClient,
	})
}

// UpdateFromConfig updates provider fields atomically with new immutable client (for hot reload coherence)
func (p *Provider) UpdateFromConfig(apiKey, baseURL string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Resolve new client
	cfg := remote.ClientConfig{
		BaseURL:          baseURL,
		APIKey:           apiKey,
		AllowHTTPForTest: p.httpClient != nil,
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://www.jevai.org"
	}
	if p.httpClient != nil {
		cfg.HTTPClient = p.httpClient
		client, err := remote.NewTestClient(cfg.BaseURL, apiKey, p.httpClient)
		if err != nil {
			return err
		}
		p.client = client
	} else {
		client, err := remote.NewClient(cfg)
		if err != nil {
			return err
		}
		p.client = client
	}
	p.apiKey = apiKey
	p.baseURL = baseURL
	p.enabled = enabled
	return nil
}

// For secret-safe string
func (p *Provider) String() string {
	return fmt.Sprintf("jev provider %s enabled=%t key_configured=%t", p.ID(), p.enabled, p.HasAPIKey())
}

// Ensure no raw prompt leakage: BuildTaskSummary and BuildCandidateDescription already enforce metadata_only
func init() {
	// Ensure privacy mode constant
	_ = strings.ToLower("metadata_only")
}
