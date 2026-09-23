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
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen     string                 `json:"listen"`
	Admin      AdminConfig            `json:"admin"`
	Logging    LoggingConfig          `json:"logging"`
	Routing    RoutingConfig          `json:"routing"`
	Probe      ProbeConfig            `json:"probe"`
	ClientAuth ClientAuthConfig       `json:"client_auth"`
	Pricing    map[string]PriceConfig `json:"pricing,omitempty"`
	Guardrails GuardrailsConfig       `json:"guardrails"`
	Providers  []ProviderConfig       `json:"providers"`
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

// ClientKey is a virtual client credential for the /v1/* data plane.
type ClientKey struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

type ClientAuthConfig struct {
	Required bool        `json:"required"`
	Keys     []ClientKey `json:"keys"`
}

// PriceConfig is USD per 1M tokens for cost estimation.
type PriceConfig struct {
	InputPerM  float64 `json:"input_per_m"`
	OutputPerM float64 `json:"output_per_m"`
}

type GuardrailsConfig struct {
	MaxPromptChars     int      `json:"max_prompt_chars"`
	BlockedPatterns    []string `json:"blocked_patterns"`
	BlockEmptyMessages bool     `json:"block_empty_messages"`
}

type RoutingConfig struct {
	Strategy                   string  `json:"strategy"`
	FallbackOnUnknownModel     bool    `json:"fallback_on_unknown_model"`
	SessionAffinity            bool    `json:"session_affinity"`
	SessionTTLSeconds          int     `json:"session_ttl_seconds"`
	P2CWindow                  int     `json:"p2c_window"`
	MaxAttempts                int     `json:"max_attempts"`
	MaxInflightRequests        int     `json:"max_inflight_requests"`
	FailureThreshold           int     `json:"failure_threshold"`
	CooldownSeconds            int     `json:"cooldown_seconds"`
	CapabilityFailureThreshold int     `json:"capability_failure_threshold"`
	CapabilityCooldownSeconds  int     `json:"capability_cooldown_seconds"`
	RequestTimeoutMS           int     `json:"request_timeout_ms"`
	AttemptTimeoutMS           int     `json:"attempt_timeout_ms"`
	LatencyWeight              float64 `json:"latency_weight"`
	FailureWeight              float64 `json:"failure_weight"`
	CapacityWeight             float64 `json:"capacity_weight"`
	RetryBackoffMS             int     `json:"retry_backoff_ms"`
	MaxRetryAfterSeconds       int     `json:"max_retry_after_seconds"`
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
	Type                     string             `json:"type"` // openai_compatible | anthropic_compatible
	BaseURL                  string             `json:"base_url"`
	APIKey                   string             `json:"api_key,omitempty"`
	APIKeyEnv                string             `json:"api_key_env,omitempty"`
	Credentials              []CredentialConfig `json:"credentials,omitempty"`
	AuthMode                 string             `json:"auth_mode,omitempty"` // bearer | x-api-key | none
	Headers                  map[string]string  `json:"headers,omitempty"`
	ForwardHeaders           []string           `json:"forward_headers"`
	ProxyURL                 string             `json:"proxy_url,omitempty"`
	ChatPath                 string             `json:"chat_path,omitempty"`
	MessagesPath             string             `json:"messages_path,omitempty"`
	ModelsPath               string             `json:"models_path,omitempty"`
	CountTokensPath          string             `json:"count_tokens_path,omitempty"`
	MaxConcurrency           int                `json:"max_concurrency,omitempty"`
	StreamIdleTimeoutSeconds int                `json:"stream_idle_timeout_seconds,omitempty"`
	Enabled                  bool               `json:"enabled"`
	Models                   []ModelConfig      `json:"models"`
}

type ModelConfig struct {
	ID           string       `json:"id"`
	Model        string       `json:"model"`
	Aliases      []string     `json:"aliases,omitempty"`
	Enabled      bool         `json:"enabled"`
	Priority     int          `json:"priority"`
	Weight       float64      `json:"weight"`
	Capabilities Capabilities `json:"capabilities"`
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
			CapabilityFailureThreshold: 2, CapabilityCooldownSeconds: 300,
			RequestTimeoutMS: 120000, AttemptTimeoutMS: 0, LatencyWeight: 0.015, FailureWeight: 25, CapacityWeight: 35,
			RetryBackoffMS: 150, MaxRetryAfterSeconds: 60,
		},
		Probe: ProbeConfig{Enabled: true, OnStart: true, IntervalSeconds: 120, ReadyLeaseSeconds: 300, TimeoutMS: 8000, MaxTokens: 1, Concurrency: 16, RecoveryAttempts: 5, RecoveryRetryMS: 500},
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
		c.Admin.APIKey = v
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
	if c.Routing.CapabilityFailureThreshold == 0 {
		c.Routing.CapabilityFailureThreshold = 2
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
}

func (p *ProviderConfig) ApplyDefaults() {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.AuthMode == "" {
		if p.Type == "anthropic_compatible" {
			p.AuthMode = "x-api-key"
		} else {
			p.AuthMode = "bearer"
		}
	}
	if p.ChatPath == "" {
		p.ChatPath = "/v1/chat/completions"
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
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
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
	if err := validateClientAuth(c.ClientAuth); err != nil {
		return err
	}
	if err := validatePricing(c.Pricing); err != nil {
		return err
	}
	if err := validateGuardrails(c.Guardrails); err != nil {
		return err
	}
	if c.Routing.Strategy != "ready_mesh" && c.Routing.Strategy != "ready_queue" && c.Routing.Strategy != "adaptive" && c.Routing.Strategy != "adaptive_round_robin" && c.Routing.Strategy != "priority" && c.Routing.Strategy != "round_robin" && c.Routing.Strategy != "least_latency" {
		return errors.New("routing.strategy must be ready_mesh, ready_queue, adaptive, adaptive_round_robin, priority, round_robin, or least_latency")
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
	if c.Routing.CapabilityCooldownSeconds < 1 || c.Routing.CapabilityCooldownSeconds > 7*24*60*60 {
		return errors.New("routing.capability_cooldown_seconds must be between 1 and 604800")
	}
	if c.Routing.RequestTimeoutMS < 100 || c.Routing.RequestTimeoutMS > 30*60*1000 {
		return errors.New("routing.request_timeout_ms must be between 100 and 1800000")
	}
	if c.Routing.AttemptTimeoutMS < 0 || c.Routing.AttemptTimeoutMS > 30*60*1000 {
		return errors.New("routing.attempt_timeout_ms must be between 0 and 1800000")
	}
	if c.Routing.RetryBackoffMS < 0 || c.Routing.RetryBackoffMS > 60000 {
		return errors.New("routing.retry_backoff_ms must be between 0 and 60000")
	}
	if c.Routing.MaxRetryAfterSeconds < 1 || c.Routing.MaxRetryAfterSeconds > 86400 {
		return errors.New("routing.max_retry_after_seconds must be between 1 and 86400")
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
		if p.Type != "openai_compatible" && p.Type != "anthropic_compatible" {
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
		if p.AuthMode != "" && p.AuthMode != "bearer" && p.AuthMode != "x-api-key" && p.AuthMode != "none" {
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
			"chat_path": p.ChatPath, "messages_path": p.MessagesPath, "models_path": p.ModelsPath, "count_tokens_path": p.CountTokensPath,
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

// AttemptTimeout bounds a single provider attempt (time until the full
// non-streaming exchange completes) so one hung provider cannot consume the
// entire route budget and starve failover. 0 disables the per-attempt bound
// and keeps the historical whole-request deadline behaviour.
func (c Config) AttemptTimeout() time.Duration {
	return time.Duration(c.Routing.AttemptTimeoutMS) * time.Millisecond
}

func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.Routing.RequestTimeoutMS) * time.Millisecond
}
func (c Config) Cooldown() time.Duration {
	return time.Duration(c.Routing.CooldownSeconds) * time.Second
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

const (
	maxClientKeys        = 256
	maxClientKeyBytes    = 256
	maxClientKeyName     = 128
	maxPricingEntries    = 4096
	maxPricingKeyBytes   = 256
	maxBlockedPatterns   = 64
	maxBlockedPatternLen = 512
)

func validateClientAuth(a ClientAuthConfig) error {
	if len(a.Keys) > maxClientKeys {
		return fmt.Errorf("client_auth.keys exceeds safe limit %d", maxClientKeys)
	}
	seenID := map[string]struct{}{}
	seenKey := map[string]struct{}{}
	enabled := 0
	for i, k := range a.Keys {
		if k.Key == "" {
			return fmt.Errorf("client_auth.keys[%d].key is required", i)
		}
		if len(k.Key) > maxClientKeyBytes {
			return fmt.Errorf("client_auth.keys[%d].key exceeds %d bytes", i, maxClientKeyBytes)
		}
		if len(k.Name) > maxClientKeyName {
			return fmt.Errorf("client_auth.keys[%d].name exceeds %d bytes", i, maxClientKeyName)
		}
		if k.ID != "" {
			if _, dup := seenID[k.ID]; dup {
				return fmt.Errorf("client_auth.keys[%d].id %q is duplicated", i, k.ID)
			}
			seenID[k.ID] = struct{}{}
		}
		if _, dup := seenKey[k.Key]; dup {
			return fmt.Errorf("client_auth.keys[%d].key value is duplicated", i)
		}
		seenKey[k.Key] = struct{}{}
		if k.Enabled {
			enabled++
		}
	}
	if a.Required && enabled == 0 {
		return errors.New("client_auth.required=true needs at least one enabled client key")
	}
	return nil
}

func validatePricing(p map[string]PriceConfig) error {
	if len(p) > maxPricingEntries {
		return fmt.Errorf("pricing exceeds safe limit %d entries", maxPricingEntries)
	}
	for k, v := range p {
		if len(k) > maxPricingKeyBytes {
			return fmt.Errorf("pricing key exceeds %d bytes", maxPricingKeyBytes)
		}
		if v.InputPerM < 0 || v.OutputPerM < 0 {
			return fmt.Errorf("pricing[%q] rates cannot be negative", k)
		}
		if v.InputPerM > 1e6 || v.OutputPerM > 1e6 {
			return fmt.Errorf("pricing[%q] rates exceed the safe bound 1000000 USD per 1M tokens", k)
		}
	}
	return nil
}

func validateGuardrails(g GuardrailsConfig) error {
	if g.MaxPromptChars < 0 || g.MaxPromptChars > 64<<20 {
		return errors.New("guardrails.max_prompt_chars must be between 0 and 67108864")
	}
	if len(g.BlockedPatterns) > maxBlockedPatterns {
		return fmt.Errorf("guardrails.blocked_patterns exceeds safe limit %d", maxBlockedPatterns)
	}
	for i, p := range g.BlockedPatterns {
		if p == "" {
			return fmt.Errorf("guardrails.blocked_patterns[%d] is empty", i)
		}
		if len(p) > maxBlockedPatternLen {
			return fmt.Errorf("guardrails.blocked_patterns[%d] exceeds %d bytes", i, maxBlockedPatternLen)
		}
		if _, err := regexp.Compile("(?i)" + p); err != nil {
			return fmt.Errorf("guardrails.blocked_patterns[%d] is not a valid regex: %w", i, err)
		}
	}
	return nil
}
