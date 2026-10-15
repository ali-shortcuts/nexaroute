package config

import (
	"strings"
	"testing"
)

// Budget webhook alert config: defaults, validation, back-compat.

func TestBudgetAlertsDisabledByDefault(t *testing.T) {
	cfg := Default()
	cfg.ApplyDefaults()
	if cfg.BudgetAlerts.WebhookURL != "" || len(cfg.BudgetAlerts.Thresholds) != 0 {
		t.Fatalf("default alerts=%+v, want disabled with no thresholds", cfg.BudgetAlerts)
	}
	if cfg.BudgetAlerts.TimeoutSeconds != 5 {
		t.Fatalf("default timeout=%d, want 5", cfg.BudgetAlerts.TimeoutSeconds)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestBudgetAlertsThresholdDefaults(t *testing.T) {
	cfg := Default()
	cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
	cfg.ApplyDefaults()
	if len(cfg.BudgetAlerts.Thresholds) != 3 ||
		cfg.BudgetAlerts.Thresholds[0] != 50 ||
		cfg.BudgetAlerts.Thresholds[1] != 80 ||
		cfg.BudgetAlerts.Thresholds[2] != 100 {
		t.Fatalf("thresholds=%v, want [50 80 100]", cfg.BudgetAlerts.Thresholds)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestBudgetAlertsPerKeyURLTriggersDefaults(t *testing.T) {
	cfg := Default()
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{
		ID: "vk1", KeyHash: strings.Repeat("a", 64),
		BudgetUSD: 10, BudgetWebhookURL: "https://alerts.example.test/hook",
	}}
	cfg.ApplyDefaults()
	if len(cfg.BudgetAlerts.Thresholds) != 3 {
		t.Fatalf("thresholds=%v, want defaults when a per-key URL exists", cfg.BudgetAlerts.Thresholds)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestBudgetAlertsThresholdNormalization(t *testing.T) {
	cfg := Default()
	cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
	cfg.BudgetAlerts.Thresholds = []float64{100, 50, 50, 80}
	cfg.ApplyDefaults()
	if len(cfg.BudgetAlerts.Thresholds) != 3 ||
		cfg.BudgetAlerts.Thresholds[0] != 50 ||
		cfg.BudgetAlerts.Thresholds[1] != 80 ||
		cfg.BudgetAlerts.Thresholds[2] != 100 {
		t.Fatalf("thresholds=%v, want sorted [50 80 100]", cfg.BudgetAlerts.Thresholds)
	}
}

func TestBudgetAlertsValidation(t *testing.T) {
	valid := func() Config {
		cfg := Default()
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
		cfg.BudgetAlerts.Thresholds = []float64{50, 80, 100}
		cfg.ApplyDefaults()
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid alerts rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"plaintext http remote", func(c *Config) { c.BudgetAlerts.WebhookURL = "http://alerts.example.test/hook" }, "http only for loopback"},
		{"userinfo", func(c *Config) { c.BudgetAlerts.WebhookURL = "https://u:p@alerts.example.test/hook" }, "credentials"},
		{"query", func(c *Config) { c.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook?t=s" }, "query"},
		{"fragment", func(c *Config) { c.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook#x" }, "fragment"},
		{"bad scheme", func(c *Config) { c.BudgetAlerts.WebhookURL = "ftp://alerts.example.test/hook" }, "http or https"},
		{"zero threshold", func(c *Config) { c.BudgetAlerts.Thresholds = []float64{0} }, "(0, 100]"},
		{"over 100", func(c *Config) { c.BudgetAlerts.Thresholds = []float64{101} }, "(0, 100]"},
		{"too many", func(c *Config) { c.BudgetAlerts.Thresholds = []float64{10, 20, 30, 40, 50, 60, 70, 80, 90} }, "at most 8"},
		{"timeout low", func(c *Config) { c.BudgetAlerts.TimeoutSeconds = -1 }, "timeout_seconds"},
		{"timeout high", func(c *Config) { c.BudgetAlerts.TimeoutSeconds = 31 }, "timeout_seconds"},
		{"bad per-key url", func(c *Config) {
			c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "vk1", KeyHash: strings.Repeat("a", 64), BudgetUSD: 5, BudgetWebhookURL: "http://x.example.test/"}}
		}, "http only for loopback"},
		{"bad per-key thresholds", func(c *Config) {
			c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "vk1", KeyHash: strings.Repeat("a", 64), BudgetUSD: 5, BudgetAlertThresholds: []float64{-1}}}
		}, "(0, 100]"},
		{"bad per-key period", func(c *Config) {
			c.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "vk1", KeyHash: strings.Repeat("a", 64), BudgetUSD: 5, BudgetPeriod: "weekly"}}
		}, "budget_period"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestBudgetAlertsLoopbackHTTPAllowed(t *testing.T) {
	for _, target := range []string{
		"http://127.0.0.1:9000/hook",
		"http://localhost:9000/hook",
		"http://[::1]:9000/hook",
		"http://hook.localhost/hook",
	} {
		cfg := Default()
		cfg.BudgetAlerts.WebhookURL = target
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%q rejected: %v", target, err)
		}
	}
	// A public name that merely resolves to loopback is NOT loopback-spelled.
	cfg := Default()
	cfg.BudgetAlerts.WebhookURL = "http://public.example.test/hook"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("public http webhook accepted")
	}
}

func TestBudgetAlertsBackCompat(t *testing.T) {
	cfg := Default()
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{
		{ID: "vk1", KeyHash: strings.Repeat("b", 64), BudgetUSD: 25},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy budget config invalid: %v", err)
	}
	if cfg.ClientAuth.VirtualKeys[0].BudgetWebhookURL != "" || cfg.ClientAuth.VirtualKeys[0].BudgetPeriod != "" {
		t.Fatal("legacy keys must keep zero alert overrides")
	}
	// Monthly is the only non-default period.
	cfg.ClientAuth.VirtualKeys[0].BudgetPeriod = "monthly"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("monthly period rejected: %v", err)
	}
}
