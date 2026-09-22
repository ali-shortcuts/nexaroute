package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
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
	Strategy               string  `json:"strategy"`
	FallbackOnUnknownModel bool    `json:"fallback_on_unknown_model"`
	MaxAttempts            int     `json:"max_attempts"`
	FailureThreshold       int     `json:"failure_threshold"`
	CooldownSeconds        int     `json:"cooldown_seconds"`
	RequestTimeoutMS       int     `json:"request_timeout_ms"`
	LatencyWeight          float64 `json:"latency_weight"`
	FailureWeight          float64 `json:"failure_weight"`
	RetryBackoffMS         int     `json:"retry_backoff_ms"`
	MaxRetryAfterSeconds   int     `json:"max_retry_after_seconds"`
}

type ProbeConfig struct {
	Enabled         bool `json:"enabled"`
	OnStart         bool `json:"on_start"`
	IntervalSeconds int  `json:"interval_seconds"`
	TimeoutMS       int  `json:"timeout_ms"`
	MaxTokens       int  `json:"max_tokens"`
	Concurrency     int  `json:"concurrency"`
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

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8080",
		Admin:  AdminConfig{BindLocalOnly: true},
		Routing: RoutingConfig{
			Strategy: "adaptive_round_robin", FallbackOnUnknownModel: true, MaxAttempts: 4, FailureThreshold: 5, CooldownSeconds: 3600,
			RequestTimeoutMS: 120000, LatencyWeight: 0.015, FailureWeight: 25,
			RetryBackoffMS: 150, MaxRetryAfterSeconds: 60,
		},
		Probe: ProbeConfig{Enabled: true, OnStart: true, IntervalSeconds: 120, TimeoutMS: 8000, MaxTokens: 1, Concurrency: 16},
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
	// NEXAROUTE_* is the preferred namespace. ULG_* remains supported for
	// backward compatibility with existing v0.3 deployments.
	if v := strings.TrimSpace(os.Getenv("NEXAROUTE_LISTEN")); v != "" {
		c.Listen = v
	} else if v := strings.TrimSpace(os.Getenv("ULG_LISTEN")); v != "" {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("NEXAROUTE_ADMIN_KEY"); ok {
		c.Admin.APIKey = v
	} else if v, ok := os.LookupEnv("ULG_ADMIN_KEY"); ok {
		c.Admin.APIKey = v
	}
	if v, ok := os.LookupEnv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			c.Admin.BindLocalOnly = b
		}
	} else if v, ok := os.LookupEnv("ULG_ADMIN_BIND_LOCAL_ONLY"); ok {
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
		c.Routing.Strategy = "adaptive_round_robin"
	}
	if c.Routing.MaxAttempts <= 0 {
		c.Routing.MaxAttempts = 4
	}
	if c.Routing.FailureThreshold <= 0 {
		c.Routing.FailureThreshold = 5
	}
	if c.Routing.CooldownSeconds <= 0 {
		c.Routing.CooldownSeconds = 3600
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
	if c.Routing.RetryBackoffMS < 0 {
		c.Routing.RetryBackoffMS = 0
	}
	if c.Routing.MaxRetryAfterSeconds <= 0 {
		c.Routing.MaxRetryAfterSeconds = 60
	}
	if c.Probe.IntervalSeconds <= 0 {
		c.Probe.IntervalSeconds = 120
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
	if c.Routing.Strategy != "adaptive" && c.Routing.Strategy != "adaptive_round_robin" && c.Routing.Strategy != "priority" && c.Routing.Strategy != "round_robin" && c.Routing.Strategy != "least_latency" {
		return errors.New("routing.strategy must be adaptive, adaptive_round_robin, priority, round_robin, or least_latency")
	}
	if c.Routing.MaxAttempts <= 0 {
		return errors.New("routing.max_attempts must be > 0")
	}
	if c.Routing.FailureThreshold <= 0 {
		return errors.New("routing.failure_threshold must be > 0")
	}
	if c.Routing.CooldownSeconds < 1 {
		return errors.New("routing.cooldown_seconds must be > 0")
	}
	if c.Routing.RequestTimeoutMS < 100 {
		return errors.New("routing.request_timeout_ms must be >= 100")
	}
	seenP := map[string]bool{}
	seenD := map[string]bool{}
	for i, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("providers[%d].id is required", i)
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
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q has invalid base_url", p.ID)
		}
		if p.ProxyURL != "" {
			u, err := url.Parse(p.ProxyURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("provider %q has invalid proxy_url", p.ID)
			}
		}
		if p.AuthMode != "" && p.AuthMode != "bearer" && p.AuthMode != "x-api-key" && p.AuthMode != "none" {
			return fmt.Errorf("provider %q has unsupported auth_mode %q", p.ID, p.AuthMode)
		}
		if p.MaxConcurrency < 0 {
			return fmt.Errorf("provider %q max_concurrency must be >= 0", p.ID)
		}
		if p.StreamIdleTimeoutSeconds < 0 {
			return fmt.Errorf("provider %q stream_idle_timeout_seconds must be >= 0", p.ID)
		}
		for j, m := range p.Models {
			if m.ID == "" {
				return fmt.Errorf("provider %q model[%d].id is required", p.ID, j)
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
func (c Config) ProbeTimeout() time.Duration {
	return time.Duration(c.Probe.TimeoutMS) * time.Millisecond
}
func (c Config) RetryBackoff() time.Duration {
	return time.Duration(c.Routing.RetryBackoffMS) * time.Millisecond
}

func SaveAtomic(path string, c Config) error {
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o600)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
func LoadBackup(path string) (Config, error) { return Load(path + ".bak") }
func (c Config) ProviderIndex(id string) int {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return i
		}
	}
	return -1
}
