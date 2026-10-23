package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestValidateGoldenCorpus pins the exact observable behaviour of
// Config.Validate across a corpus of single-defect and multi-defect
// configurations: which error (if any) is returned, verbatim. It exists so
// that structural refactors of Validate (for example splitting it into
// per-section helpers) must prove they preserved both the error strings
// and the precedence order in which coexisting defects are reported.
// Regenerate the golden file with UPDATE_VALIDATE_GOLDEN=1.
func TestValidateGoldenCorpus(t *testing.T) {
	mutations := map[string]func(*Config){
		"valid-default": func(c *Config) {},
		// Admin plane.
		"admin-session-ttl-low":     func(c *Config) { c.Admin.SessionTTLSeconds = 10 },
		"admin-idle-exceeds-ttl":    func(c *Config) { c.Admin.IdleTimeoutSeconds = c.Admin.SessionTTLSeconds + 1 },
		"admin-audit-retention":     func(c *Config) { c.Admin.AuditRetentionDays = 0 },
		"admin-audit-max-events":    func(c *Config) { c.Admin.AuditMaxEvents = 1 },
		"admin-store-path-newline":  func(c *Config) { c.Admin.SecurityStorePath = "bad\npath" },
		"admin-store-backend-bogus": func(c *Config) { c.Admin.SecurityStoreBackend = "sqlite" },
		"admin-store-url-on-bbolt":  func(c *Config) { c.Admin.SecurityStoreURL = "redis://x" },
		"admin-store-url-missing":   func(c *Config) { c.Admin.SecurityStoreBackend = "redis" },
		// Listen + logging.
		"listen-empty":        func(c *Config) { c.Listen = "" },
		"listen-bad-port":     func(c *Config) { c.Listen = "127.0.0.1:99999" },
		"listen-no-port":      func(c *Config) { c.Listen = "localhost" },
		"logging-max-size":    func(c *Config) { c.Logging.MaxSizeMB = 0 },
		"logging-max-backups": func(c *Config) { c.Logging.MaxBackups = 99 },
		"logging-access-mode": func(c *Config) { c.Logging.AccessMode = "verbose" },
		"logging-sample":      func(c *Config) { c.Logging.SuccessSampleEvery = 0 },
		// Routing + cache.
		"routing-strategy":        func(c *Config) { c.Routing.Strategy = "random" },
		"routing-max-attempts":    func(c *Config) { c.Routing.MaxAttempts = 0 },
		"routing-inflight":        func(c *Config) { c.Routing.MaxInflightRequests = 0 },
		"routing-failure-thresh":  func(c *Config) { c.Routing.FailureThreshold = 0 },
		"routing-timeout-low":     func(c *Config) { c.Routing.RequestTimeoutMS = 10 },
		"routing-hedging-low":     func(c *Config) { c.Routing.HedgingDelayMS = 1 },
		"cache-ttl":               func(c *Config) { c.Cache.TTLSeconds = 0 },
		"cache-max-entries":       func(c *Config) { c.Cache.MaxEntries = 0 },
		"cache-body-too-small":    func(c *Config) { c.Cache.MaxBodyBytes = 10 },
		"cache-semantic-no-dep":   func(c *Config) { c.Cache.Semantic.Enabled = true },
		"cache-semantic-bad-form": func(c *Config) { c.Cache.Semantic.Enabled = true; c.Cache.Semantic.EmbeddingDeployment = "noslash" },
		// MCP + guardrails.
		"mcp-enabled-no-servers": func(c *Config) { c.MCP.Enabled = true },
		"mcp-rpm-negative": func(c *Config) {
			c.MCP.Enabled = true
			c.MCP.Servers = []MCPServerConfig{{ID: "srv", URL: "https://example.invalid/mcp"}}
			c.MCP.RPM = -1
		},
		"guardrails-mode": func(c *Config) { c.Guardrails.Mode = "strict" },
		"guardrails-body": func(c *Config) { c.Guardrails.MaxBodyBytes = 1 },
		// Telemetry + control plane.
		"telemetry-otlp-env":      func(c *Config) { c.Telemetry.OTLPEndpointEnv = "9BAD" },
		"telemetry-service-long":  func(c *Config) { c.Telemetry.ServiceName = strings.Repeat("x", 200) },
		"controlplane-sync-alone": func(c *Config) { c.ControlPlane.BreakerSync = true },
		"controlplane-backend": func(c *Config) {
			c.ControlPlane.Enabled = true
			c.ControlPlane.Backend = "etcd"
		},
		// Budget alerts.
		"budget-webhook-bad": func(c *Config) { c.BudgetAlerts.WebhookURL = "ftp://example.invalid/hook" },
		// Cross-section precedence: the earlier section's error must win.
		"prec-admin-over-listen": func(c *Config) { c.Admin.SessionTTLSeconds = 1; c.Listen = "" },
		"prec-listen-over-routing": func(c *Config) {
			c.Listen = ""
			c.Routing.Strategy = "random"
		},
		"prec-routing-over-mcp": func(c *Config) {
			c.Routing.Strategy = "random"
			c.MCP.Enabled = true
		},
		"prec-mcp-over-guardrails": func(c *Config) {
			c.MCP.Enabled = true
			c.Guardrails.Mode = "strict"
		},
		"prec-guardrails-over-telemetry": func(c *Config) {
			c.Guardrails.Mode = "strict"
			c.Telemetry.OTLPEndpointEnv = "9BAD"
		},
		"prec-cache-over-mcp": func(c *Config) {
			c.Cache.TTLSeconds = 0
			c.MCP.Enabled = true
		},
		"prec-admin-over-budget": func(c *Config) {
			c.Admin.AuditRetentionDays = 0
			c.BudgetAlerts.WebhookURL = "ftp://example.invalid/hook"
		},
		"prec-valid-plus-late-defect": func(c *Config) { c.BudgetAlerts.WebhookURL = "ftp://example.invalid/hook" },
	}

	names := make([]string, 0, len(mutations))
	for name := range mutations {
		names = append(names, name)
	}
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		cfg := Default()
		mutations[name](&cfg)
		errStr := "<nil>"
		if err := cfg.Validate(); err != nil {
			errStr = err.Error()
		}
		lines = append(lines, name+"\t"+errStr)
	}
	got := strings.Join(lines, "\n") + "\n"

	goldenPath := filepath.Join("testdata", "validate_golden.tsv")
	if os.Getenv("UPDATE_VALIDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	if got != string(want) {
		t.Fatalf("Validate behaviour drifted from golden corpus.\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}
