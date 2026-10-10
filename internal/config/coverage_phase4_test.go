package config

import (
	"math"
	"strings"
	"testing"
)

func TestCoveragePhase4ValidateRejectsUnsafeRuntimeLimits(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"strategy", func(c *Config) { c.Routing.Strategy = "unknown" }, "routing.strategy"},
		{"attempts", func(c *Config) { c.Routing.MaxAttempts = 0 }, "routing.max_attempts"},
		{"inflight", func(c *Config) { c.Routing.MaxInflightRequests = 0 }, "routing.max_inflight_requests"},
		{"session ttl", func(c *Config) { c.Routing.SessionTTLSeconds = 0 }, "routing.session_ttl_seconds"},
		{"failure threshold", func(c *Config) { c.Routing.FailureThreshold = 0 }, "routing.failure_threshold"},
		{"request timeout", func(c *Config) { c.Routing.RequestTimeoutMS = 1 }, "routing.request_timeout_ms"},
		{"retry after", func(c *Config) { c.Routing.MaxRetryAfterSeconds = 0 }, "routing.max_retry_after_seconds"},
		{"cache ttl", func(c *Config) { c.Cache.TTLSeconds = 0 }, "cache.ttl_seconds"},
		{"cache body", func(c *Config) { c.Cache.MaxBodyBytes = 1 }, "cache.max_body_bytes"},
		{"guardrail mode", func(c *Config) { c.Guardrails.Mode = "unsafe" }, "guardrails.mode"},
		{"probe timeout", func(c *Config) { c.Probe.TimeoutMS = 1 }, "probe.timeout_ms"},
		{"routing NaN", func(c *Config) { c.Routing.LatencyWeight = math.NaN() }, "routing.latency_weight"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestCoveragePhase4ValidateClientAuthAndControlPlane(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"enabled without credentials", func(c *Config) { c.ClientAuth.Enabled = true }, "client_auth.keys"},
		{"short key", func(c *Config) { c.ClientAuth.Enabled = true; c.ClientAuth.Keys = []string{"short"} }, "keys entries"},
		{"bad tenant id", func(c *Config) { c.ClientAuth.Tenants = []TenantConfig{{ID: "bad id"}} }, "tenant id"},
		{"duplicate tenant", func(c *Config) { c.ClientAuth.Tenants = []TenantConfig{{ID: "t"}, {ID: "t"}} }, "duplicate client_auth.tenant"},
		{"bad virtual key hash", func(c *Config) { c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "vk", KeyHash: "bad"}} }, "key_hash"},
		{"bad virtual key role", func(c *Config) {
			c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "vk", KeyHash: strings.Repeat("a", 64), Role: "root"}}
		}, "unsupported role"},
		{"bad control env", func(c *Config) { c.ControlPlane.Enabled = true; c.ControlPlane.PostgresDSNEnv = "bad-name" }, "control_plane"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error=%v, want substring %q", err, tc.want)
			}
		})
	}
}
