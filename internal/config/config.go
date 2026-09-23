package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen    string           `json:"listen"`
	Admin     AdminConfig      `json:"admin"`
	Routing   RoutingConfig    `json:"routing"`
	Probe     ProbeConfig      `json:"probe"`
	Providers []ProviderConfig `json:"providers"`
}

type AdminConfig struct {
	BindLocalOnly bool   `json:"bind_local_only"`
	APIKey        string `json:"api_key"`
}

type RoutingConfig struct {
	Strategy                   string  `json:"strategy"`
	FallbackOnUnknownModel     bool    `json:"fallback_on_unknown_model"`
	SessionAffinity            bool    `json:"session_affinity"`
	SessionTTLSeconds          int     `json:"session_ttl_seconds"`
	P2CWindow                  int     `json:"p2c_window"`
	MaxAttempts                int     `json:"max_attempts"`
	FailureThreshold           int     `json:"failure_threshold"`
	CooldownSeconds            int     `json:"cooldown_seconds"`
	CapabilityFailureThreshold int     `json:"capability_failure_threshold"`
	CapabilityCooldownSeconds  int     `json:"capability_cooldown_seconds"`
	RequestTimeoutMS           int     `json:"request_timeout_ms"`
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
	ForwardHeaders           []string           `json:"forward_headers,omitempty"`
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
)

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8080",
		Admin:  AdminConfig{BindLocalOnly: true},
		Routing: RoutingConfig{
			Strategy: "ready_mesh", FallbackOnUnknownModel: true, SessionAffinity: true, SessionTTLSeconds: 3600, P2CWindow: 8,
			MaxAttempts: 4, FailureThreshold: 5, CooldownSeconds: 1800,
			CapabilityFailureThreshold: 2, CapabilityCooldownSeconds: 300,
			RequestTimeoutMS: 120000, LatencyWeight: 0.015, FailureWeight: 25, CapacityWeight: 35,
			RetryBackoffMS: 150, MaxRetryAfterSeconds: 60,
		},
		Probe: ProbeConfig{Enabled: true, OnStart: true, IntervalSeconds: 120, ReadyLeaseSeconds: 300, TimeoutMS: 8000, MaxTokens: 1, Concurrency: 16, RecoveryAttempts: 5, RecoveryRetryMS: 500},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	cfg.ApplyEnvOverrides()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) ApplyEnvOverrides() {
	if v := strings.TrimSpace(os.Getenv("NEXAROUTE_LISTEN")); v != "" {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("NEXAROUTE_ADMIN_KEY"); ok {
		c.Admin.APIKey = v
	}
	if v, ok := os.LookupEnv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			c.Admin.BindLocalOnly = b
		}
	}
}

func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "ready_mesh"
	}
	if c.Routing.SessionTTLSeconds <= 0 {
		c.Routing.SessionTTLSeconds = 3600
	}
	if c.Routing.P2CWindow <= 0 {
		c.Routing.P2CWindow = 8
	}
	if c.Routing.MaxAttempts <= 0 {
		c.Routing.MaxAttempts = 4
	}
	if c.Routing.FailureThreshold <= 0 {
		c.Routing.FailureThreshold = 5
	}
	if c.Routing.CooldownSeconds <= 0 {
		c.Routing.CooldownSeconds = 1800
	}
	if c.Routing.CapabilityFailureThreshold <= 0 {
		c.Routing.CapabilityFailureThreshold = 2
	}
	if c.Routing.CapabilityCooldownSeconds <= 0 {
		c.Routing.CapabilityCooldownSeconds = 300
	}
	if c.Routing.RequestTimeoutMS <= 0 {
		c.Routing.RequestTimeoutMS = 120000
	}
	if c.Routing.LatencyWeight == 0 {
		c.Routing.LatencyWeight = 0.015
	}
	if c.Routing.FailureWeight == 0 {
		c.Routing.FailureWeight = 25
	}
	if c.Routing.CapacityWeight == 0 {
		c.Routing.CapacityWeight = 35
	}
	if c.Routing.RetryBackoffMS < 0 {
		c.Routing.RetryBackoffMS = 0
	}
	if c.Routing.MaxRetryAfterSeconds <= 0 {
		c.Routing.MaxRetryAfterSeconds = 60
	}
	if c.Probe.IntervalSeconds <= 0 {
		c.Probe.IntervalSeconds = 120
	}
	if c.Probe.ReadyLeaseSeconds <= 0 {
		c.Probe.ReadyLeaseSeconds = 300
	}
	if c.Probe.TimeoutMS <= 0 {
		c.Probe.TimeoutMS = 8000
	}
	if c.Probe.MaxTokens <= 0 {
		c.Probe.MaxTokens = 1
	}
	if c.Probe.Concurrency <= 0 {
		c.Probe.Concurrency = 16
	}
	if c.Probe.RecoveryAttempts <= 0 {
		c.Probe.RecoveryAttempts = 5
	}
	if c.Probe.RecoveryRetryMS < 0 {
		c.Probe.RecoveryRetryMS = 0
	}
	if c.Probe.RecoveryRetryMS == 0 {
		c.Probe.RecoveryRetryMS = 500
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
	if p.MaxConcurrency <= 0 {
		p.MaxConcurrency = 32
	}
	if p.StreamIdleTimeoutSeconds <= 0 {
		p.StreamIdleTimeoutSeconds = 180
	}
	if len(p.ForwardHeaders) == 0 && p.Type == "anthropic_compatible" {
		p.ForwardHeaders = []string{"anthropic-beta", "anthropic-version", "user-agent"}
	}
	for i := range p.Credentials {
		if p.Credentials[i].Name == "" {
			p.Credentials[i].Name = fmt.Sprintf("key-%d", i+1)
		}
	}
	for i := range p.Models {
		if p.Models[i].Weight <= 0 {
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
	if len(c.Providers) > maxProviders {
		return fmt.Errorf("providers exceeds safe limit %d", maxProviders)
	}
	if c.Routing.Strategy != "ready_mesh" && c.Routing.Strategy != "ready_queue" && c.Routing.Strategy != "adaptive" && c.Routing.Strategy != "adaptive_round_robin" && c.Routing.Strategy != "priority" && c.Routing.Strategy != "round_robin" && c.Routing.Strategy != "least_latency" {
		return errors.New("routing.strategy must be ready_mesh, ready_queue, adaptive, adaptive_round_robin, priority, round_robin, or least_latency")
	}
	if c.Routing.MaxAttempts <= 0 || c.Routing.MaxAttempts > maxRoutingAttempts {
		return fmt.Errorf("routing.max_attempts must be between 1 and %d", maxRoutingAttempts)
	}
	if c.Routing.SessionTTLSeconds > 30*24*60*60 {
		return errors.New("routing.session_ttl_seconds must be <= 2592000")
	}
	if c.Routing.P2CWindow > maxModelsPerProvider {
		return fmt.Errorf("routing.p2c_window must be <= %d", maxModelsPerProvider)
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
	for i, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("providers[%d].id is required", i)
		}
		if len(p.ID) > maxStringIDBytes || len(p.Name) > 1024 {
			return fmt.Errorf("provider %q id/name is too long", p.ID)
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
			if strings.TrimSpace(k) == "" || len(k) > 256 || len(v) > 8192 || strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
				return fmt.Errorf("provider %q has invalid custom header", p.ID)
			}
		}
		for _, h := range p.ForwardHeaders {
			if strings.TrimSpace(h) == "" || len(h) > 256 || strings.ContainsAny(h, "\r\n") {
				return fmt.Errorf("provider %q has invalid forward header", p.ID)
			}
		}
		for j, m := range p.Models {
			if m.ID == "" {
				return fmt.Errorf("provider %q model[%d].id is required", p.ID, j)
			}
			if len(m.ID) > maxStringIDBytes || len(m.Model) > 1024 || len(m.Aliases) > 128 {
				return fmt.Errorf("provider %q model[%d] identifiers/aliases exceed safe limits", p.ID, j)
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
