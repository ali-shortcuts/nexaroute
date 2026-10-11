package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoverageBehaviorCredentialResolutionAndDurations(t *testing.T) {
	t.Setenv("NEXA_TEST_PRIMARY", "env-primary")
	p := ProviderConfig{APIKey: "inline", APIKeyEnv: "NEXA_TEST_PRIMARY", Credentials: []CredentialConfig{
		{Name: "dup", APIKey: "env-primary", Enabled: true},
		{Name: "second", APIKey: "second", Enabled: true},
		{Name: "disabled", APIKey: "disabled", Enabled: false},
	}}
	got := p.ResolvedCredentials()
	want := []string{"env-primary", "second"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("resolved credentials = %#v, want %#v", got, want)
	}
	cfg := Default()
	cfg.Routing.RequestTimeoutMS = 125
	cfg.Routing.CooldownSeconds = 7
	cfg.Routing.ProviderFailureWindowSeconds = 8
	cfg.Routing.ProviderCooldownSeconds = 9
	cfg.Routing.HedgingDelayMS = 10
	cfg.Cache.TTLSeconds = 11
	cfg.Probe.IntervalSeconds = 12
	cfg.Probe.ReadyLeaseSeconds = 13
	cfg.Probe.TimeoutMS = 14
	cfg.Probe.RecoveryRetryMS = 15
	cfg.Routing.RetryBackoffMS = 16
	checks := []struct{ got, want time.Duration }{
		{cfg.RequestTimeout(), 125 * time.Millisecond}, {cfg.Cooldown(), 7 * time.Second},
		{cfg.ProviderFailureWindow(), 8 * time.Second}, {cfg.ProviderCooldown(), 9 * time.Second},
		{cfg.HedgingDelay(), 10 * time.Millisecond}, {cfg.CacheTTL(), 11 * time.Second},
		{cfg.ProbeInterval(), 12 * time.Second}, {cfg.ProbeReadyLease(), 13 * time.Second},
		{cfg.ProbeTimeout(), 14 * time.Millisecond}, {cfg.ProbeRecoveryRetry(), 15 * time.Millisecond},
		{cfg.RetryBackoff(), 16 * time.Millisecond},
	}
	for i, c := range checks {
		if c.got != c.want {
			t.Fatalf("duration %d = %s, want %s", i, c.got, c.want)
		}
	}
}

func TestCoverageBehaviorLoadBaseFailuresAndProviderIndex(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadBase(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing config should fail")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(bad); err == nil {
		t.Fatal("malformed config should fail")
	}
	large := filepath.Join(dir, "large.json")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", maxConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(large); err == nil || !strings.Contains(err.Error(), "safe limit") {
		t.Fatalf("large config error = %v", err)
	}
	cfg := Default()
	cfg.Providers = []ProviderConfig{{ID: "p1"}, {ID: "p2"}}
	if cfg.ProviderIndex("p2") != 1 || cfg.ProviderIndex("missing") != -1 {
		t.Fatalf("provider index semantics failed")
	}
	if err := ValidateProviderConfig(ProviderConfig{Type: "openai_compatible", BaseURL: "https://example.test", Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true}}}); err != nil {
		t.Fatalf("valid provider rejected: %v", err)
	}
}

func TestCoverageBehaviorSaveAtomicPreservesEnvironmentOwnedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	base := Default()
	base.Listen = "127.0.0.1:9000"
	base.Admin.APIKey = "durable-admin"
	base.Logging.File = "durable.log"
	if err := SaveAtomic(path, base); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_LISTEN", "127.0.0.1:9999")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "runtime-admin")
	t.Setenv("NEXAROUTE_LOG_FILE", "runtime.log")
	mutated := base
	mutated.Listen, mutated.Admin.APIKey, mutated.Logging.File = "0.0.0.0:1", "new-admin", "new.log"
	if err := SaveAtomic(path, mutated); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBase(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Listen != base.Listen || got.Admin.APIKey != base.Admin.APIKey || got.Logging.File != base.Logging.File {
		t.Fatalf("environment-owned fields changed: %#v", got)
	}
	var step DecisionChainStep
	for _, raw := range []string{`[]`, `1`, `{bad}`} {
		if err := json.Unmarshal([]byte(raw), &step); err == nil {
			t.Fatalf("malformed step %s accepted", raw)
		}
	}
}

func TestCoverageBehaviorConfigValidationMatrix(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"missing listen", func(c *Config) { c.Listen = "" }},
		{"bad listen", func(c *Config) { c.Listen = "not a host port" }},
		{"bad port", func(c *Config) { c.Listen = "127.0.0.1:70000" }},
		{"logging size low", func(c *Config) { c.Logging.MaxSizeMB = 0 }},
		{"logging backups high", func(c *Config) { c.Logging.MaxBackups = 21 }},
		{"logging mode", func(c *Config) { c.Logging.AccessMode = "invalid" }},
		{"logging sample", func(c *Config) { c.Logging.SuccessSampleEvery = 0 }},
		{"logging slow negative", func(c *Config) { c.Logging.SlowRequestMS = -1 }},
		{"client base url", func(c *Config) { c.ClientBaseURL = "file:///etc/passwd" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.ApplyDefaults()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
	var enabled, disabled bool
	virtual := VirtualEndpointConfig{ID: "v", Enabled: &enabled}
	decision := DecisionProviderConfig{ID: "d", Enabled: &disabled}
	if virtual.IsEnabled() || decision.IsEnabled() {
		t.Fatal("explicit enabled flags have wrong semantics")
	}
	step := DecisionChainStep{Provider: "jev", TimeoutMS: 10}
	encoded, err := json.Marshal(step)
	if err != nil || string(encoded) != `{"provider":"jev","timeout_ms":10}` {
		t.Fatalf("step marshal=%s err=%v", encoded, err)
	}
}
