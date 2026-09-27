package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
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

// defaultBaseURL is the official Jev endpoint used when no base_url is set.
const defaultBaseURL = "https://www.jevai.org"

// isLoopbackBaseURL reports whether raw is a plaintext loopback endpoint.
//
// config.base_url is the documented local-testing override, so it may point at a
// loopback server — but only at loopback, and only over plain HTTP. Every other
// target keeps the production transport guarantees. Notably this excludes the
// link-local metadata address (169.254.169.254) and the private ranges, which
// are the SSRF targets that matter, and excludes plaintext to any remote host,
// which would put the API key on the wire in the clear.
func isLoopbackBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// newRemoteClient builds the transport for one provider.
//
// base_url is operator-supplied configuration, so it is a trust boundary: HTTPS
// and the SSRF host guard stay in force unless the target is loopback (the
// documented local-testing override) or an HTTP client was injected by Go code,
// which is how tests address an httptest server. Loopback targets additionally
// need the loopback-permitting transport, because the production transport
// blocks loopback dials by design.
func newRemoteClient(baseURL, apiKey string, httpClient *http.Client) (*remote.Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	insecure := httpClient != nil || isLoopbackBaseURL(baseURL)
	client, err := remote.NewClient(remote.ClientConfig{
		BaseURL:          baseURL,
		APIKey:           apiKey,
		HTTPClient:       httpClient,
		AllowHTTPForTest: insecure,
	})
	if err != nil {
		return nil, err
	}
	if httpClient == nil && insecure {
		return remote.NewTestClient(baseURL, apiKey, nil)
	}
	return client, nil
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

	// Build client.
	client, err := newRemoteClient(cfg.BaseURL, apiKey, cfg.HTTPClient)
	if err != nil {
		return nil, err
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
		return defaultBaseURL
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

	// Resolve new client. The same trust boundary as construction applies: a
	// hot-reloaded base_url cannot move the provider onto a plaintext remote or
	// link-local endpoint.
	client, err := newRemoteClient(baseURL, apiKey, p.httpClient)
	if err != nil {
		return err
	}
	p.client = client
	p.apiKey = apiKey
	p.baseURL = baseURL
	p.enabled = enabled
	return nil
}

// For secret-safe string
func (p *Provider) String() string {
	p.mu.RLock()
	id, enabled, configured := p.id, p.enabled, p.apiKey != ""
	p.mu.RUnlock()
	return fmt.Sprintf("jev provider %s enabled=%t key_configured=%t", id, enabled, configured)
}

// Ensure no raw prompt leakage: BuildTaskSummary and BuildCandidateDescription already enforce metadata_only
func init() {
	// Ensure privacy mode constant
	_ = strings.ToLower("metadata_only")
}
