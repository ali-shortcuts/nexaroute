package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type VirtualEndpointConfig struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	Enabled      *bool    `json:"enabled,omitempty"`
	PublicModel  string   `json:"public_model"`
	RouteProfile string   `json:"route_profile"`
	Protocols    []string `json:"protocols,omitempty"`
}

func (v VirtualEndpointConfig) IsEnabled() bool {
	if v.Enabled == nil {
		return true
	}
	return *v.Enabled
}

type RouteProfileConfig struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	CandidatePool string `json:"candidate_pool"`
	FallbackChain string `json:"fallback_chain,omitempty"`
	Strategy      string `json:"strategy,omitempty"`
}

type CandidatePoolConfig struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Mode        string   `json:"mode,omitempty"` // explicit or all
	Deployments []string `json:"deployments,omitempty"`
}

type FallbackChainConfig struct {
	ID    string   `json:"id"`
	Name  string   `json:"name,omitempty"`
	Pools []string `json:"pools"`
}

type DecisionConfig struct {
	Mode      string `json:"mode,omitempty"`       // off | local
	Provider  string `json:"provider,omitempty"`   // local (Phase D only)
	TimeoutMS int    `json:"timeout_ms,omitempty"` // bounded, default 10ms
}

type Config struct {
	Listen           string                  `json:"listen"`
	Admin            AdminConfig             `json:"admin"`
	Logging          LoggingConfig           `json:"logging"`
	Routing          RoutingConfig           `json:"routing"`
	Probe            ProbeConfig             `json:"probe"`
	Cache            CacheConfig             `json:"cache"`
	ClientAuth       ClientAuthConfig        `json:"client_auth"`
	Decision         DecisionConfig          `json:"decision,omitempty"`
	Providers        []ProviderConfig        `json:"providers"`
	VirtualEndpoints []VirtualEndpointConfig `json:"virtual_endpoints,omitempty"`
	RouteProfiles    []RouteProfileConfig    `json:"route_profiles,omitempty"`
	CandidatePools   []CandidatePoolConfig   `json:"candidate_pools,omitempty"`
	FallbackChains   []FallbackChainConfig   `json:"fallback_chains,omitempty"`
}

type LoggingConfig struct {
	File                     string `json:"file"`
	MaxSizeMB                int    `json:"max_size_mb"`
	MaxBackups               int    `json:"max_backups"`
	AccessMode               string `json:"access_mode"` // off | errors | sampled | all
	SuccessSampleEvery       int    `json:"success_sample_every"`
	SlowRequestMS            int    `json:"slow_request_ms"`
	ConsoleMaxLinesPerMinute int    `json:"console_max_lines_per_minute"`
}

type AdminConfig struct {
	BindLocalOnly bool   `json:"bind_local_only"`
	APIKey        string `json:"api_key"`
}

type RoutingConfig struct {
	Strategy                     string  `json:"strategy"`
	FallbackOnUnknownModel       bool    `json:"fallback_on_unknown_model"`
	SessionAffinity              bool    `json:"session_affinity"`
	SessionTTLSeconds            int     `json:"session_ttl_seconds"`
	P2CWindow                    int     `json:"p2c_window"`
	MaxAttempts                  int     `json:"max_attempts"`
	MaxInflightRequests          int     `json:"max_inflight_requests"`
	FailureThreshold             int     `json:"failure_threshold"`
	CooldownSeconds              int     `json:"cooldown_seconds"`
	ProviderFailureThreshold     int     `json:"provider_failure_threshold"`
	ProviderFailureWindowSeconds int     `json:"provider_failure_window_seconds"`
	ProviderCooldownSeconds      int     `json:"provider_cooldown_seconds"`
	CapabilityFailureThreshold   int     `json:"capability_failure_threshold"`
	CapabilityCooldownSeconds    int     `json:"capability_cooldown_seconds"`
	RequestTimeoutMS             int     `json:"request_timeout_ms"`
	LatencyWeight                float64 `json:"latency_weight"`
	FailureWeight                float64 `json:"failure_weight"`
	CapacityWeight               float64 `json:"capacity_weight"`
	RetryBackoffMS               int     `json:"retry_backoff_ms"`
	MaxRetryAfterSeconds         int     `json:"max_retry_after_seconds"`
	HedgingEnabled               bool    `json:"hedging_enabled"`
	HedgingDelayMS               int     `json:"hedging_delay_ms"`
	// MaxRepairAttempts bounds deterministic bounded-repair retries per
	// candidate (capability rejections only). 1-2 recommended.
	MaxRepairAttempts int `json:"max_repair_attempts"`
	// SanitizeEnabled enables proactive pre-dispatch parameter
	// sanitization against the cached capability contract.
	SanitizeEnabled bool `json:"sanitize_enabled"`
	// PublicModel is the legacy single-endpoint public model name from
	// PR #13 (Beta 0.6.1). It is deprecated in favor of virtual_endpoints
	// but preserved for backward compatibility. When set and no virtual
	// endpoints are defined, it is auto-migrated to a default virtual
	// endpoint with an all-eligible pool.
	PublicModel string `json:"public_model,omitempty"`
}

// CacheConfig gates the exact-match response cache. It is deliberately
// opt-in: a shared gateway serving mixed traffic must not memorize responses
// unless the operator asked for it.
type CacheConfig struct {
	Enabled      bool `json:"enabled"`
	TTLSeconds   int  `json:"ttl_seconds"`
	MaxEntries   int  `json:"max_entries"`
	MaxBodyBytes int  `json:"max_body_bytes"`
}

// ClientAuthConfig enables optional per-key data-plane authentication with a
// per-key request-per-minute ceiling. Disabled by default so local single-user
// deployments keep working without configuration.
type ClientAuthConfig struct {
	Enabled bool     `json:"enabled"`
	Keys    []string `json:"keys"`
	RPM     int      `json:"rpm"`
}

type ProbeConfig struct {
	Enabled           bool `json:"enabled"`
	OnStart           bool `json:"on_start"`
	IntervalSeconds   int  `json:"interval_seconds"`
	ReadyLeaseSeconds int  `json:"ready_lease_seconds"`
	TimeoutMS         int  `json:"timeout_ms"`
	MaxTokens         int  `json:"max_tokens"`
	Concurrency       int  `json:"concurrency"`
	RecoveryAttempts  int  `json:"recovery_attempts"`
	RecoveryRetryMS   int  `json:"recovery_retry_ms"`
	// CapabilityProbes enables Level B compatibility probing (bounded,
	// cached per deployment) in addition to Level A availability probes.
	CapabilityProbes bool `json:"capability_probes"`
}

type CredentialConfig struct {
	Name      string `json:"name,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	Enabled   bool   `json:"enabled"`
}

func (c CredentialConfig) Resolved() string {
	if c.APIKeyEnv != "" {
		if v := os.Getenv(c.APIKeyEnv); v != "" {
			return v
		}
	}
	return c.APIKey
}

type ProviderConfig struct {
	ID                       string             `json:"id"`
	Name                     string             `json:"name"`
	Type                     string             `json:"type"`              // openai_compatible | anthropic_compatible | gemini | openai_responses
	Dialect                  string             `json:"dialect,omitempty"` // optional dialect override (see compat.Dialects)
	BaseURL                  string             `json:"base_url"`
	APIKey                   string             `json:"api_key,omitempty"`
	APIKeyEnv                string             `json:"api_key_env,omitempty"`
	Credentials              []CredentialConfig `json:"credentials,omitempty"`
	AuthMode                 string             `json:"auth_mode,omitempty"` // bearer | x-api-key | x-goog-api-key | none
	Headers                  map[string]string  `json:"headers,omitempty"`
	ForwardHeaders           []string           `json:"forward_headers"`
	ProxyURL                 string             `json:"proxy_url,omitempty"`
	ChatPath                 string             `json:"chat_path,omitempty"`
	MessagesPath             string             `json:"messages_path,omitempty"`
	ModelsPath               string             `json:"models_path,omitempty"`
	CountTokensPath          string             `json:"count_tokens_path,omitempty"`
	ResponsesPath            string             `json:"responses_path,omitempty"`
	MaxConcurrency           int                `json:"max_concurrency,omitempty"`
	StreamIdleTimeoutSeconds int                `json:"stream_idle_timeout_seconds,omitempty"`
	Enabled                  bool               `json:"enabled"`
	Models                   []ModelConfig      `json:"models"`
}

type ModelConfig struct {
	ID                string       `json:"id"`
	Model             string       `json:"model"`
	Aliases           []string     `json:"aliases,omitempty"`
	Enabled           bool         `json:"enabled"`
	Priority          int          `json:"priority"`
	Weight            float64      `json:"weight"`
	ContextWindow     int          `json:"context_window,omitempty"`
	InputCostPerMTok  float64      `json:"input_cost_per_mtok,omitempty"`
	OutputCostPerMTok float64      `json:"output_cost_per_mtok,omitempty"`
	Capabilities      Capabilities `json:"capabilities"`
}

type Capabilities struct {
	Streaming bool `json:"streaming"`
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

const (
	maxProviders              = 512
	maxModelsPerProvider      = 4096
	maxCredentialsPerProvider = 256
	maxProviderHeaders        = 128
	maxProviderConcurrency    = 4096
	maxProbeConcurrency       = 1024
	maxRoutingAttempts        = 64
	maxProbeTokens            = 64
	maxStringIDBytes          = 256
	maxURLBytes               = 4096
	maxTotalDeployments       = 20000
	maxTotalAliases           = 100000
	maxConfigBytes            = 16 << 20
	// Phase B limits
	maxVirtualEndpoints    = 256
	maxRouteProfiles       = 256
	maxCandidatePools      = 256
	maxFallbackChains      = 256
	maxPoolDeployments     = 4096
	maxFallbackPools       = 32
	maxProtocols           = 16
	maxPublicModelBytes    = 128
	maxPoolIDBytes         = 256
	maxVirtualEndpointName = 256
)

func validLocalID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '-', '_', '.':
			continue
		default:
			return false
		}
	}
	return true
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', 0x27, '*', '+', '-', '.', '^', '_', 0x60, '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' || c >= 0x20 && c != 0x7f {
			continue
		}
		return false
	}
	return true
}

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8080",
		Admin:  AdminConfig{BindLocalOnly: true},
		Logging: LoggingConfig{
			File: "auto", MaxSizeMB: 32, MaxBackups: 3,
			AccessMode: "sampled", SuccessSampleEvery: 1000, SlowRequestMS: 5000,
			ConsoleMaxLinesPerMinute: 30,
		},
		Routing: RoutingConfig{
			Strategy: "ready_mesh", FallbackOnUnknownModel: true, SessionAffinity: true, SessionTTLSeconds: 3600, P2CWindow: 8,
			MaxAttempts: 4, MaxInflightRequests: 128, FailureThreshold: 5, CooldownSeconds: 1800,
			ProviderFailureThreshold: 3, ProviderFailureWindowSeconds: 20, ProviderCooldownSeconds: 30,
			CapabilityFailureThreshold: 2, CapabilityCooldownSeconds: 300,
			RequestTimeoutMS: 120000, LatencyWeight: 0.015, FailureWeight: 25, CapacityWeight: 35,
			RetryBackoffMS: 150, MaxRetryAfterSeconds: 60,
			MaxRepairAttempts: 1, SanitizeEnabled: true,
			HedgingEnabled: false, HedgingDelayMS: 1500,
		},
		Probe:      ProbeConfig{Enabled: true, OnStart: true, IntervalSeconds: 120, ReadyLeaseSeconds: 300, TimeoutMS: 8000, MaxTokens: 1, Concurrency: 16, RecoveryAttempts: 5, RecoveryRetryMS: 500, CapabilityProbes: true},
		Cache:      CacheConfig{Enabled: false, TTLSeconds: 300, MaxEntries: 256, MaxBodyBytes: 1 << 20},
		ClientAuth: ClientAuthConfig{Enabled: false, RPM: 0},
		Decision:   DecisionConfig{Mode: "off", Provider: "local", TimeoutMS: 10},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return cfg, err
	}
	if len(b) > maxConfigBytes {
		return cfg, fmt.Errorf("config exceeds safe limit %d bytes", maxConfigBytes)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if err := cfg.ApplyEnvOverrides(); err != nil {
		return cfg, err
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) ApplyEnvOverrides() error {
	if v := strings.TrimSpace(os.Getenv("NEXAROUTE_LISTEN")); v != "" {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("NEXAROUTE_ADMIN_KEY"); ok {
		// An empty value (e.g. Environment=NEXAROUTE_ADMIN_KEY= in a systemd
		// unit) must not silently disable key auth over a file-provided key.
		c.Admin.APIKey = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY"); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY must be a valid boolean: %w", err)
		}
		c.Admin.BindLocalOnly = b
	}
	if v, ok := os.LookupEnv("NEXAROUTE_LOG_FILE"); ok {
		c.Logging.File = strings.TrimSpace(v)
		if c.Logging.File == "" {
			c.Logging.File = "off"
		}
	}
	return nil
}

func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Logging.File == "" {
		c.Logging.File = "auto"
	}
	if c.Logging.MaxSizeMB == 0 {
		c.Logging.MaxSizeMB = 32
	}
	if c.Logging.AccessMode == "" {
		c.Logging.AccessMode = "sampled"
	}
	if c.Logging.SuccessSampleEvery == 0 {
		c.Logging.SuccessSampleEvery = 1000
	}
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "ready_mesh"
	}
	if c.Routing.SessionTTLSeconds == 0 {
		c.Routing.SessionTTLSeconds = 3600
	}
	if c.Routing.P2CWindow == 0 {
		c.Routing.P2CWindow = 8
	}
	if c.Routing.MaxAttempts == 0 {
		c.Routing.MaxAttempts = 4
	}
	if c.Routing.MaxInflightRequests == 0 {
		c.Routing.MaxInflightRequests = 128
	}
	if c.Routing.FailureThreshold == 0 {
		c.Routing.FailureThreshold = 5
	}
	if c.Routing.CooldownSeconds == 0 {
		c.Routing.CooldownSeconds = 1800
	}
	if c.Routing.ProviderFailureThreshold == 0 {
		c.Routing.ProviderFailureThreshold = 3
	}
	if c.Routing.ProviderFailureWindowSeconds == 0 {
		c.Routing.ProviderFailureWindowSeconds = 20
	}
	if c.Routing.ProviderCooldownSeconds == 0 {
		c.Routing.ProviderCooldownSeconds = 30
	}
	if c.Routing.CapabilityFailureThreshold == 0 {
		c.Routing.CapabilityFailureThreshold = 2
	}
	if c.Routing.HedgingDelayMS == 0 {
		c.Routing.HedgingDelayMS = 1500
	}
	if c.Cache.TTLSeconds == 0 {
		c.Cache.TTLSeconds = 300
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = 256
	}
	if c.Cache.MaxBodyBytes == 0 {
		c.Cache.MaxBodyBytes = 1 << 20
	}
	if c.Routing.CapabilityCooldownSeconds == 0 {
		c.Routing.CapabilityCooldownSeconds = 300
	}
	if c.Routing.RequestTimeoutMS == 0 {
		c.Routing.RequestTimeoutMS = 120000
	}
	if c.Routing.MaxRetryAfterSeconds == 0 {
		c.Routing.MaxRetryAfterSeconds = 60
	}
	if c.Probe.IntervalSeconds == 0 {
		c.Probe.IntervalSeconds = 120
	}
	if c.Probe.ReadyLeaseSeconds == 0 {
		c.Probe.ReadyLeaseSeconds = 300
	}
	if c.Probe.TimeoutMS == 0 {
		c.Probe.TimeoutMS = 8000
	}
	if c.Probe.MaxTokens == 0 {
		c.Probe.MaxTokens = 1
	}
	if c.Probe.Concurrency == 0 {
		c.Probe.Concurrency = 16
	}
	if c.Probe.RecoveryAttempts == 0 {
		c.Probe.RecoveryAttempts = 5
	}
	for i := range c.Providers {
		c.Providers[i].ApplyDefaults()
	}
	// Normalize legacy public_model.
	c.Routing.PublicModel = strings.TrimSpace(c.Routing.PublicModel)
	// Normalize virtual endpoints, route profiles, pools, fallback chains.
	for i := range c.VirtualEndpoints {
		ve := &c.VirtualEndpoints[i]
		ve.ID = strings.TrimSpace(ve.ID)
		ve.Name = strings.TrimSpace(ve.Name)
		ve.PublicModel = strings.TrimSpace(ve.PublicModel)
		ve.RouteProfile = strings.TrimSpace(ve.RouteProfile)
		for j := range ve.Protocols {
			ve.Protocols[j] = strings.TrimSpace(ve.Protocols[j])
		}
		if ve.Enabled == nil {
			t := true
			ve.Enabled = &t
		}
	}
	for i := range c.RouteProfiles {
		rp := &c.RouteProfiles[i]
		rp.ID = strings.TrimSpace(rp.ID)
		rp.Name = strings.TrimSpace(rp.Name)
		rp.CandidatePool = strings.TrimSpace(rp.CandidatePool)
		rp.FallbackChain = strings.TrimSpace(rp.FallbackChain)
		rp.Strategy = strings.TrimSpace(strings.ToLower(rp.Strategy))
		// Phase B: only empty (inherit) is functional. Normalize "inherit" to empty for storage.
		if rp.Strategy == "inherit" {
			rp.Strategy = ""
		}
	}
	for i := range c.CandidatePools {
		cp := &c.CandidatePools[i]
		cp.ID = strings.TrimSpace(cp.ID)
		cp.Name = strings.TrimSpace(cp.Name)
		cp.Mode = strings.TrimSpace(strings.ToLower(cp.Mode))
		if cp.Mode == "" {
			cp.Mode = "explicit"
		}
		for j := range cp.Deployments {
			cp.Deployments[j] = strings.TrimSpace(cp.Deployments[j])
		}
	}
	for i := range c.FallbackChains {
		fc := &c.FallbackChains[i]
		fc.ID = strings.TrimSpace(fc.ID)
		fc.Name = strings.TrimSpace(fc.Name)
		for j := range fc.Pools {
			fc.Pools[j] = strings.TrimSpace(fc.Pools[j])
		}
	}
	// Decision defaults (Phase D)
	c.Decision.Mode = strings.TrimSpace(strings.ToLower(c.Decision.Mode))
	c.Decision.Provider = strings.TrimSpace(strings.ToLower(c.Decision.Provider))
	if c.Decision.Mode == "" {
		c.Decision.Mode = "off"
	}
	if c.Decision.Provider == "" {
		c.Decision.Provider = "local"
	}
	if c.Decision.TimeoutMS == 0 {
		c.Decision.TimeoutMS = 10
	}

	// Backward compatibility: legacy public_model → default virtual endpoint.
	if c.Routing.PublicModel != "" && len(c.VirtualEndpoints) == 0 {
		t := true
		// Only synthesize if legacy model is valid; validation will catch invalid.
		if len(c.CandidatePools) == 0 {
			c.CandidatePools = []CandidatePoolConfig{{ID: "default", Name: "Default (all eligible)", Mode: "all"}}
		}
		if len(c.RouteProfiles) == 0 {
			poolID := c.CandidatePools[0].ID
			if poolID == "" {
				poolID = "default"
			}
			c.RouteProfiles = []RouteProfileConfig{{ID: "default", Name: "Default", CandidatePool: poolID}}
		}
		profileID := c.RouteProfiles[0].ID
		if profileID == "" {
			profileID = "default"
		}
		c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "default", Name: "Default", Enabled: &t, PublicModel: c.Routing.PublicModel, RouteProfile: profileID}}
	}
}

func (p *ProviderConfig) ApplyDefaults() {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.AuthMode == "" {
		switch p.Type {
		case "anthropic_compatible":
			p.AuthMode = "x-api-key"
		case "gemini":
			p.AuthMode = "x-goog-api-key"
		default:
			p.AuthMode = "bearer"
		}
	}
	if p.ChatPath == "" {
		p.ChatPath = "/v1/chat/completions"
	}
	if p.ResponsesPath == "" {
		p.ResponsesPath = "/v1/responses"
	}
	if p.MessagesPath == "" {
		p.MessagesPath = "/v1/messages"
	}
	if p.ModelsPath == "" {
		p.ModelsPath = "/v1/models"
	}
	if p.CountTokensPath == "" {
		p.CountTokensPath = "/v1/messages/count_tokens"
	}
	if p.MaxConcurrency == 0 {
		p.MaxConcurrency = 32
	}
	if p.StreamIdleTimeoutSeconds == 0 {
		p.StreamIdleTimeoutSeconds = 180
	}
	if p.ForwardHeaders == nil && p.Type == "anthropic_compatible" {
		p.ForwardHeaders = []string{"anthropic-beta", "anthropic-version", "user-agent"}
	}
	for i := range p.Credentials {
		if p.Credentials[i].Name == "" {
			p.Credentials[i].Name = fmt.Sprintf("key-%d", i+1)
		}
	}
	for i := range p.Models {
		if p.Models[i].Weight == 0 {
			p.Models[i].Weight = 1
		}
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen is required")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	if pn, perr := strconv.Atoi(port); perr != nil || pn < 1 || pn > 65535 {
		return fmt.Errorf("listen port %q must be numeric between 1 and 65535", port)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	if net.ParseIP(strings.Trim(host, "[]")) == nil && host != "localhost" {
		if net.ParseIP(host) == nil && !strings.Contains(host, ":") && strings.ContainsAny(host, "/\\ ") {
			return fmt.Errorf("listen host %q is not a valid address", host)
		}
	}
	if len(c.Logging.File) > maxURLBytes {
		return errors.New("logging.file is too long")
	}
	if c.Logging.MaxSizeMB < 1 || c.Logging.MaxSizeMB > 1024 {
		return errors.New("logging.max_size_mb must be between 1 and 1024")
	}
	if c.Logging.MaxBackups < 0 || c.Logging.MaxBackups > 20 {
		return errors.New("logging.max_backups must be between 0 and 20")
	}
	switch c.Logging.AccessMode {
	case "off", "errors", "sampled", "all":
	default:
		return errors.New("logging.access_mode must be off, errors, sampled, or all")
	}
	if c.Logging.SuccessSampleEvery < 1 || c.Logging.SuccessSampleEvery > 1_000_000 {
		return errors.New("logging.success_sample_every must be between 1 and 1000000")
	}
	if c.Logging.SlowRequestMS < 0 || c.Logging.SlowRequestMS > 60*60*1000 {
		return errors.New("logging.slow_request_ms must be between 0 and 3600000")
	}
	if c.Logging.ConsoleMaxLinesPerMinute < 0 || c.Logging.ConsoleMaxLinesPerMinute > 10000 {
		return errors.New("logging.console_max_lines_per_minute must be between 0 and 10000")
	}
	if len(c.Providers) > maxProviders {
		return fmt.Errorf("providers exceeds safe limit %d", maxProviders)
	}
	if c.Routing.Strategy != "ready_mesh" && c.Routing.Strategy != "ready_queue" && c.Routing.Strategy != "cost_aware" && c.Routing.Strategy != "adaptive" && c.Routing.Strategy != "adaptive_round_robin" && c.Routing.Strategy != "priority" && c.Routing.Strategy != "round_robin" && c.Routing.Strategy != "least_latency" {
		return errors.New("routing.strategy must be ready_mesh, ready_queue, cost_aware, adaptive, adaptive_round_robin, priority, round_robin, or least_latency")
	}
	if c.Routing.MaxAttempts <= 0 || c.Routing.MaxAttempts > maxRoutingAttempts {
		return fmt.Errorf("routing.max_attempts must be between 1 and %d", maxRoutingAttempts)
	}
	if c.Routing.MaxInflightRequests < 1 || c.Routing.MaxInflightRequests > 10000 {
		return errors.New("routing.max_inflight_requests must be between 1 and 10000")
	}
	if c.Routing.SessionTTLSeconds < 1 || c.Routing.SessionTTLSeconds > 30*24*60*60 {
		return errors.New("routing.session_ttl_seconds must be between 1 and 2592000")
	}
	if c.Routing.P2CWindow < 1 || c.Routing.P2CWindow > maxModelsPerProvider {
		return fmt.Errorf("routing.p2c_window must be between 1 and %d", maxModelsPerProvider)
	}
	if c.Routing.FailureThreshold <= 0 || c.Routing.FailureThreshold > 1000 {
		return errors.New("routing.failure_threshold must be between 1 and 1000")
	}
	if c.Routing.CapabilityFailureThreshold <= 0 || c.Routing.CapabilityFailureThreshold > 1000 {
		return errors.New("routing.capability_failure_threshold must be between 1 and 1000")
	}
	if c.Routing.CooldownSeconds < 1 || c.Routing.CooldownSeconds > 7*24*60*60 {
		return errors.New("routing.cooldown_seconds must be between 1 and 604800")
	}
	if c.Routing.ProviderFailureThreshold < 2 || c.Routing.ProviderFailureThreshold > 100 {
		return errors.New("routing.provider_failure_threshold must be between 2 and 100")
	}
	if c.Routing.ProviderFailureWindowSeconds < 1 || c.Routing.ProviderFailureWindowSeconds > 3600 {
		return errors.New("routing.provider_failure_window_seconds must be between 1 and 3600")
	}
	if c.Routing.ProviderCooldownSeconds < 1 || c.Routing.ProviderCooldownSeconds > 24*60*60 {
		return errors.New("routing.provider_cooldown_seconds must be between 1 and 86400")
	}
	if c.Routing.CapabilityCooldownSeconds < 1 || c.Routing.CapabilityCooldownSeconds > 7*24*60*60 {
		return errors.New("routing.capability_cooldown_seconds must be between 1 and 604800")
	}
	if c.Routing.RequestTimeoutMS < 100 || c.Routing.RequestTimeoutMS > 30*60*1000 {
		return errors.New("routing.request_timeout_ms must be between 100 and 1800000")
	}
	if c.Routing.RetryBackoffMS < 0 || c.Routing.RetryBackoffMS > 60000 {
		return errors.New("routing.retry_backoff_ms must be between 0 and 60000")
	}
	if c.Routing.MaxRetryAfterSeconds < 1 || c.Routing.MaxRetryAfterSeconds > 86400 {
		return errors.New("routing.max_retry_after_seconds must be between 1 and 86400")
	}
	if c.Routing.HedgingDelayMS < 50 || c.Routing.HedgingDelayMS > 600000 {
		return errors.New("routing.hedging_delay_ms must be between 50 and 600000")
	}
	if c.Cache.TTLSeconds < 1 || c.Cache.TTLSeconds > 30*24*60*60 {
		return errors.New("cache.ttl_seconds must be between 1 and 2592000")
	}
	if c.Cache.MaxEntries < 1 || c.Cache.MaxEntries > 65536 {
		return errors.New("cache.max_entries must be between 1 and 65536")
	}
	if c.Cache.MaxBodyBytes < 1024 || c.Cache.MaxBodyBytes > 64<<20 {
		return errors.New("cache.max_body_bytes must be between 1024 and 67108864")
	}
	if c.ClientAuth.Enabled {
		if len(c.ClientAuth.Keys) == 0 {
			return errors.New("client_auth.keys must not be empty when client_auth is enabled")
		}
		if len(c.ClientAuth.Keys) > 1024 {
			return errors.New("client_auth.keys exceeds safe limit 1024")
		}
		for _, k := range c.ClientAuth.Keys {
			if len(k) < 8 || len(k) > 512 {
				return errors.New("client_auth.keys entries must be between 8 and 512 bytes")
			}
		}
		if c.ClientAuth.RPM < 0 || c.ClientAuth.RPM > 1000000 {
			return errors.New("client_auth.rpm must be between 0 and 1000000")
		}
	}
	for name, v := range map[string]float64{
		"routing.latency_weight":  c.Routing.LatencyWeight,
		"routing.failure_weight":  c.Routing.FailureWeight,
		"routing.capacity_weight": c.Routing.CapacityWeight,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1_000_000 {
			return fmt.Errorf("%s must be finite and between 0 and 1000000", name)
		}
	}
	if c.Probe.IntervalSeconds < 1 || c.Probe.IntervalSeconds > 86400 {
		return errors.New("probe.interval_seconds must be between 1 and 86400")
	}
	if c.Probe.ReadyLeaseSeconds < 1 || c.Probe.ReadyLeaseSeconds > 7*24*60*60 {
		return errors.New("probe.ready_lease_seconds must be between 1 and 604800")
	}
	if c.Probe.TimeoutMS < 100 || c.Probe.TimeoutMS > 300000 {
		return errors.New("probe.timeout_ms must be between 100 and 300000")
	}
	if c.Probe.MaxTokens < 1 || c.Probe.MaxTokens > maxProbeTokens {
		return fmt.Errorf("probe.max_tokens must be between 1 and %d", maxProbeTokens)
	}
	if c.Probe.Concurrency < 1 || c.Probe.Concurrency > maxProbeConcurrency {
		return fmt.Errorf("probe.concurrency must be between 1 and %d", maxProbeConcurrency)
	}
	if c.Probe.RecoveryAttempts < 1 || c.Probe.RecoveryAttempts > 100 {
		return errors.New("probe.recovery_attempts must be between 1 and 100")
	}
	if c.Probe.RecoveryRetryMS < 0 || c.Probe.RecoveryRetryMS > 60000 {
		return errors.New("probe.recovery_retry_ms must be between 0 and 60000")
	}
	// Legacy public_model validation (PR #13 compatibility)
	if c.Routing.PublicModel != "" {
		if len(c.Routing.PublicModel) > maxPublicModelBytes || strings.ContainsAny(c.Routing.PublicModel, " \t\r\n\"'`$\\") {
			return errors.New("routing.public_model must be a simple model name of at most 128 bytes")
		}
		if c.Routing.PublicModel == "auto" || c.Routing.PublicModel == "claude-auto" {
			return errors.New("routing.public_model must not be auto or claude-auto")
		}
	}
	seenP := map[string]bool{}
	seenD := map[string]bool{}
	totalModels := 0
	totalAliases := 0
	for i, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("providers[%d].id is required", i)
		}
		if len(p.ID) > maxStringIDBytes || len(p.Name) > 1024 {
			return fmt.Errorf("provider %q id/name is too long", p.ID)
		}
		if !validLocalID(p.ID) {
			return fmt.Errorf("provider id %q may contain only letters, digits, dot, underscore, and hyphen", p.ID)
		}
		if seenP[p.ID] {
			return fmt.Errorf("duplicate provider id %q", p.ID)
		}
		seenP[p.ID] = true
		if p.Type != "openai_compatible" && p.Type != "anthropic_compatible" && p.Type != "gemini" && p.Type != "openai_responses" {
			return fmt.Errorf("provider %q has unsupported type %q", p.ID, p.Type)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("provider %q base_url is required", p.ID)
		}
		if len(p.BaseURL) > maxURLBytes || len(p.ProxyURL) > maxURLBytes {
			return fmt.Errorf("provider %q URL is too long", p.ID)
		}
		if len(p.Models) > maxModelsPerProvider {
			return fmt.Errorf("provider %q models exceeds safe limit %d", p.ID, maxModelsPerProvider)
		}
		totalModels += len(p.Models)
		if totalModels > maxTotalDeployments {
			return fmt.Errorf("total deployments exceeds safe limit %d", maxTotalDeployments)
		}
		if len(p.Credentials) > maxCredentialsPerProvider {
			return fmt.Errorf("provider %q credentials exceeds safe limit %d", p.ID, maxCredentialsPerProvider)
		}
		if len(p.Headers) > maxProviderHeaders || len(p.ForwardHeaders) > maxProviderHeaders {
			return fmt.Errorf("provider %q headers exceeds safe limit %d", p.ID, maxProviderHeaders)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q has invalid base_url", p.ID)
		}
		if p.ProxyURL != "" {
			u, err := url.Parse(p.ProxyURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("provider %q has invalid proxy_url", p.ID)
			}
		}
		if p.AuthMode != "" && p.AuthMode != "bearer" && p.AuthMode != "x-api-key" && p.AuthMode != "x-goog-api-key" && p.AuthMode != "none" {
			return fmt.Errorf("provider %q has unsupported auth_mode %q", p.ID, p.AuthMode)
		}
		if p.MaxConcurrency < 1 || p.MaxConcurrency > maxProviderConcurrency {
			return fmt.Errorf("provider %q max_concurrency must be between 1 and %d", p.ID, maxProviderConcurrency)
		}
		if p.StreamIdleTimeoutSeconds < 1 || p.StreamIdleTimeoutSeconds > 24*60*60 {
			return fmt.Errorf("provider %q stream_idle_timeout_seconds must be between 1 and 86400", p.ID)
		}
		for k, v := range p.Headers {
			if len(k) > 256 || len(v) > 8192 || !validHeaderName(k) || !validHeaderValue(v) {
				return fmt.Errorf("provider %q has invalid custom header", p.ID)
			}
		}
		for _, h := range p.ForwardHeaders {
			if len(h) > 256 || !validHeaderName(h) {
				return fmt.Errorf("provider %q has invalid forward header", p.ID)
			}
		}
		for name, path := range map[string]string{
			"chat_path": p.ChatPath, "messages_path": p.MessagesPath, "models_path": p.ModelsPath,
			"count_tokens_path": p.CountTokensPath, "responses_path": p.ResponsesPath,
		} {
			if len(path) > maxURLBytes {
				return fmt.Errorf("provider %q %s is too long", p.ID, name)
			}
			if _, err := url.Parse(path); err != nil {
				return fmt.Errorf("provider %q has invalid %s", p.ID, name)
			}
		}
		for j, m := range p.Models {
			if m.ID == "" {
				return fmt.Errorf("provider %q model[%d].id is required", p.ID, j)
			}
			if len(m.ID) > maxStringIDBytes || len(m.Model) > 1024 || len(m.Aliases) > 128 {
				return fmt.Errorf("provider %q model[%d] identifiers/aliases exceed safe limits", p.ID, j)
			}
			if !validLocalID(m.ID) {
				return fmt.Errorf("provider %q model[%d].id may contain only letters, digits, dot, underscore, and hyphen", p.ID, j)
			}
			totalAliases += len(m.Aliases)
			if totalAliases > maxTotalAliases {
				return fmt.Errorf("total model aliases exceeds safe limit %d", maxTotalAliases)
			}
			for _, alias := range m.Aliases {
				if len(alias) > maxStringIDBytes {
					return fmt.Errorf("deployment %q alias is too long", p.ID+"/"+m.ID)
				}
			}
			if math.IsNaN(m.Weight) || math.IsInf(m.Weight, 0) || m.Weight <= 0 || m.Weight > 1_000_000 {
				return fmt.Errorf("deployment %q weight must be finite and between 0 and 1000000", p.ID+"/"+m.ID)
			}
			if m.ContextWindow < 0 || m.ContextWindow > 100_000_000 {
				return fmt.Errorf("deployment %q context_window must be between 0 and 100000000", p.ID+"/"+m.ID)
			}
			for name, cost := range map[string]float64{
				"input_cost_per_mtok": m.InputCostPerMTok, "output_cost_per_mtok": m.OutputCostPerMTok,
			} {
				if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 || cost > 100000 {
					return fmt.Errorf("deployment %q %s must be finite and between 0 and 100000", p.ID+"/"+m.ID, name)
				}
			}
			key := p.ID + "/" + m.ID
			if seenD[key] {
				return fmt.Errorf("duplicate deployment %q", key)
			}
			seenD[key] = true
			if m.Model == "" {
				return fmt.Errorf("deployment %q model is required", key)
			}
		}
	}
	// Phase D: Decision
	switch strings.ToLower(strings.TrimSpace(c.Decision.Mode)) {
	case "", "off", "local":
	default:
		return errors.New("decision.mode must be off or local")
	}
	switch strings.ToLower(strings.TrimSpace(c.Decision.Provider)) {
	case "", "local":
	default:
		return errors.New("decision.provider must be local in Phase D")
	}
	if c.Decision.TimeoutMS < 1 || c.Decision.TimeoutMS > 5000 {
		return errors.New("decision.timeout_ms must be between 1 and 5000")
	}

	// Phase B: Virtual Endpoints, Route Profiles, Candidate Pools, Fallback Chains
	if len(c.VirtualEndpoints) > maxVirtualEndpoints {
		return fmt.Errorf("virtual_endpoints exceeds safe limit %d", maxVirtualEndpoints)
	}
	if len(c.RouteProfiles) > maxRouteProfiles {
		return fmt.Errorf("route_profiles exceeds safe limit %d", maxRouteProfiles)
	}
	if len(c.CandidatePools) > maxCandidatePools {
		return fmt.Errorf("candidate_pools exceeds safe limit %d", maxCandidatePools)
	}
	if len(c.FallbackChains) > maxFallbackChains {
		return fmt.Errorf("fallback_chains exceeds safe limit %d", maxFallbackChains)
	}
	// Collect physical model identifiers for collision detection
	physicalIDs := map[string]struct{}{}
	for _, p := range c.Providers {
		for _, m := range p.Models {
			if !m.Enabled {
				continue
			}
			physicalIDs[m.ID] = struct{}{}
			physicalIDs[m.Model] = struct{}{}
			physicalIDs[p.ID+"/"+m.ID] = struct{}{}
			for _, a := range m.Aliases {
				if a != "" {
					physicalIDs[a] = struct{}{}
				}
			}
		}
	}
	physicalIDs["auto"] = struct{}{}
	physicalIDs["claude-auto"] = struct{}{}

	// Candidate pools
	poolIDs := map[string]struct{}{}
	for i, cp := range c.CandidatePools {
		if cp.ID == "" {
			return fmt.Errorf("candidate_pools[%d].id is required", i)
		}
		if len(cp.ID) > maxPoolIDBytes || !validLocalID(cp.ID) {
			return fmt.Errorf("candidate pool id %q invalid", cp.ID)
		}
		if _, dup := poolIDs[cp.ID]; dup {
			return fmt.Errorf("duplicate candidate pool id %q", cp.ID)
		}
		poolIDs[cp.ID] = struct{}{}
		if len(cp.Name) > maxVirtualEndpointName {
			return fmt.Errorf("candidate pool %q name too long", cp.ID)
		}
		if cp.Mode != "explicit" && cp.Mode != "all" {
			return fmt.Errorf("candidate pool %q mode must be explicit or all", cp.ID)
		}
		if len(cp.Deployments) > maxPoolDeployments {
			return fmt.Errorf("candidate pool %q deployments exceeds limit %d", cp.ID, maxPoolDeployments)
		}
		for j, d := range cp.Deployments {
			if d == "" {
				return fmt.Errorf("candidate pool %q deployment[%d] is empty", cp.ID, j)
			}
			if len(d) > maxStringIDBytes {
				return fmt.Errorf("candidate pool %q deployment %q too long", cp.ID, d)
			}
			if strings.ContainsAny(d, " \t\r\n\"'`$\\") {
				return fmt.Errorf("candidate pool %q deployment %q contains invalid characters", cp.ID, d)
			}
		}
	}

	// Fallback chains
	chainIDs := map[string]struct{}{}
	for i, fc := range c.FallbackChains {
		if fc.ID == "" {
			return fmt.Errorf("fallback_chains[%d].id is required", i)
		}
		if len(fc.ID) > maxPoolIDBytes || !validLocalID(fc.ID) {
			return fmt.Errorf("fallback chain id %q invalid", fc.ID)
		}
		if _, dup := chainIDs[fc.ID]; dup {
			return fmt.Errorf("duplicate fallback chain id %q", fc.ID)
		}
		chainIDs[fc.ID] = struct{}{}
		if len(fc.Name) > maxVirtualEndpointName {
			return fmt.Errorf("fallback chain %q name too long", fc.ID)
		}
		if len(fc.Pools) == 0 {
			return fmt.Errorf("fallback chain %q must have at least one pool", fc.ID)
		}
		if len(fc.Pools) > maxFallbackPools {
			return fmt.Errorf("fallback chain %q exceeds pool limit %d", fc.ID, maxFallbackPools)
		}
		seenPool := map[string]struct{}{}
		for j, pid := range fc.Pools {
			if pid == "" {
				return fmt.Errorf("fallback chain %q pool[%d] is empty", fc.ID, j)
			}
			if _, ok := poolIDs[pid]; !ok {
				return fmt.Errorf("fallback chain %q references unknown pool %q", fc.ID, pid)
			}
			if _, dup := seenPool[pid]; dup {
				return fmt.Errorf("fallback chain %q contains duplicate pool %q", fc.ID, pid)
			}
			seenPool[pid] = struct{}{}
		}
	}

	// Route profiles
	profileIDs := map[string]struct{}{}
	for i, rp := range c.RouteProfiles {
		if rp.ID == "" {
			return fmt.Errorf("route_profiles[%d].id is required", i)
		}
		if len(rp.ID) > maxPoolIDBytes || !validLocalID(rp.ID) {
			return fmt.Errorf("route profile id %q invalid", rp.ID)
		}
		if _, dup := profileIDs[rp.ID]; dup {
			return fmt.Errorf("duplicate route profile id %q", rp.ID)
		}
		profileIDs[rp.ID] = struct{}{}
		if len(rp.Name) > maxVirtualEndpointName {
			return fmt.Errorf("route profile %q name too long", rp.ID)
		}
		if rp.CandidatePool == "" {
			return fmt.Errorf("route profile %q candidate_pool is required", rp.ID)
		}
		if _, ok := poolIDs[rp.CandidatePool]; !ok {
			return fmt.Errorf("route profile %q references unknown candidate pool %q", rp.ID, rp.CandidatePool)
		}
		if rp.FallbackChain != "" {
			if _, ok := chainIDs[rp.FallbackChain]; !ok {
				return fmt.Errorf("route profile %q references unknown fallback chain %q", rp.ID, rp.FallbackChain)
			}
		}
		// Phase B: Route Profiles inherit the global routing strategy.
		// Per-profile strategy override is deferred to Phase E. Only empty
		// or "inherit" are accepted to avoid a misleading configurable field.
		if rp.Strategy != "" && rp.Strategy != "inherit" {
			return fmt.Errorf("route profile %q strategy must be empty or \"inherit\" in Phase B (got %q); per-profile routing strategies are deferred to Phase E", rp.ID, rp.Strategy)
		}
	}

	// Virtual endpoints
	veIDs := map[string]struct{}{}
	vePublicModels := map[string]struct{}{}
	for i, ve := range c.VirtualEndpoints {
		if ve.ID == "" {
			return fmt.Errorf("virtual_endpoints[%d].id is required", i)
		}
		if len(ve.ID) > maxPoolIDBytes || !validLocalID(ve.ID) {
			return fmt.Errorf("virtual endpoint id %q invalid", ve.ID)
		}
		if _, dup := veIDs[ve.ID]; dup {
			return fmt.Errorf("duplicate virtual endpoint id %q", ve.ID)
		}
		veIDs[ve.ID] = struct{}{}
		if len(ve.Name) > maxVirtualEndpointName {
			return fmt.Errorf("virtual endpoint %q name too long", ve.ID)
		}
		if ve.PublicModel == "" {
			return fmt.Errorf("virtual endpoint %q public_model is required", ve.ID)
		}
		if len(ve.PublicModel) > maxPublicModelBytes {
			return fmt.Errorf("virtual endpoint %q public_model too long", ve.ID)
		}
		if strings.ContainsAny(ve.PublicModel, " \t\r\n\"'`$\\") {
			return fmt.Errorf("virtual endpoint %q public_model contains invalid characters", ve.ID)
		}
		if ve.PublicModel == "auto" || ve.PublicModel == "claude-auto" {
			return fmt.Errorf("virtual endpoint %q public_model must not be auto or claude-auto", ve.ID)
		}
		if _, dup := vePublicModels[ve.PublicModel]; dup {
			return fmt.Errorf("duplicate virtual endpoint public_model %q", ve.PublicModel)
		}
		vePublicModels[ve.PublicModel] = struct{}{}
		// Collision with physical models/aliases
		if _, collides := physicalIDs[ve.PublicModel]; collides {
			return fmt.Errorf("virtual endpoint %q public_model %q collides with existing physical model/alias", ve.ID, ve.PublicModel)
		}
		if ve.RouteProfile == "" {
			return fmt.Errorf("virtual endpoint %q route_profile is required", ve.ID)
		}
		if _, ok := profileIDs[ve.RouteProfile]; !ok {
			return fmt.Errorf("virtual endpoint %q references unknown route profile %q", ve.ID, ve.RouteProfile)
		}
		if len(ve.Protocols) > maxProtocols {
			return fmt.Errorf("virtual endpoint %q protocols exceeds limit %d", ve.ID, maxProtocols)
		}
		for _, proto := range ve.Protocols {
			if proto == "" {
				return fmt.Errorf("virtual endpoint %q has empty protocol", ve.ID)
			}
			if len(proto) > 64 {
				return fmt.Errorf("virtual endpoint %q protocol %q too long", ve.ID, proto)
			}
			// Allow letters, digits, hyphen, underscore
			for _, ch := range proto {
				if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
					continue
				}
				return fmt.Errorf("virtual endpoint %q protocol %q invalid", ve.ID, proto)
			}
		}
	}
	return nil
}

func ValidateProviderConfig(p ProviderConfig) error {
	p.ApplyDefaults()
	if p.ID == "" {
		p.ID = "_provider_check"
	}
	cfg := Default()
	cfg.Providers = []ProviderConfig{p}
	return cfg.Validate()
}

func (p ProviderConfig) ResolvedAPIKey() string {
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v
		}
	}
	return p.APIKey
}

func (p ProviderConfig) ResolvedCredentials() []string {
	out := []string{}
	seen := map[string]bool{}
	if k := p.ResolvedAPIKey(); k != "" {
		out = append(out, k)
		seen[k] = true
	}
	for _, c := range p.Credentials {
		if !c.Enabled {
			continue
		}
		if k := c.Resolved(); k != "" && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	return out
}

func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.Routing.RequestTimeoutMS) * time.Millisecond
}
func (c Config) Cooldown() time.Duration {
	return time.Duration(c.Routing.CooldownSeconds) * time.Second
}
func (c Config) ProviderFailureWindow() time.Duration {
	return time.Duration(c.Routing.ProviderFailureWindowSeconds) * time.Second
}
func (c Config) ProviderCooldown() time.Duration {
	return time.Duration(c.Routing.ProviderCooldownSeconds) * time.Second
}
func (c Config) HedgingDelay() time.Duration {
	return time.Duration(c.Routing.HedgingDelayMS) * time.Millisecond
}
func (c Config) CacheTTL() time.Duration {
	return time.Duration(c.Cache.TTLSeconds) * time.Second
}
func (c Config) ProbeInterval() time.Duration {
	return time.Duration(c.Probe.IntervalSeconds) * time.Second
}
func (c Config) ProbeReadyLease() time.Duration {
	return time.Duration(c.Probe.ReadyLeaseSeconds) * time.Second
}
func (c Config) ProbeTimeout() time.Duration {
	return time.Duration(c.Probe.TimeoutMS) * time.Millisecond
}
func (c Config) ProbeRecoveryRetry() time.Duration {
	return time.Duration(c.Probe.RecoveryRetryMS) * time.Millisecond
}
func (c Config) RetryBackoff() time.Duration {
	return time.Duration(c.Routing.RetryBackoffMS) * time.Millisecond
}

func RemoveStaleBackup(path string) error {
	err := os.Remove(path + ".bak")
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("remove stale config backup: %w", err)
}

func SaveAtomic(path string, c Config) error {
	if err := RemoveStaleBackup(path); err != nil {
		return err
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxConfigBytes {
		return fmt.Errorf("serialized config exceeds safe limit %d bytes", maxConfigBytes)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	// Best-effort directory sync makes the rename durable on filesystems that support it.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (c Config) ProviderIndex(id string) int {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return i
		}
	}
	return -1
}
