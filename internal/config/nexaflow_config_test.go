package config

import (
	"strings"
	"testing"
)

func nexaflowBaseConfig() Config {
	cfg := Default()
	cfg.Nexaflow = NexaflowConfig{
		Enabled:        true,
		StorePath:      "nexaflow/nexaflow-store",
		GatewayBaseURL: "http://127.0.0.1:8080",
		WorkerID:       "gateway-1",
	}
	return cfg
}

func TestValidateNexaflow(t *testing.T) {
	if err := nexaflowBaseConfig().Validate(); err != nil {
		t.Fatalf("valid nexaflow config rejected: %v", err)
	}

	// Disabled means the section is ignored: a bad path must not block a
	// gateway that never opens a workflow store.
	disabled := Default()
	disabled.Nexaflow = NexaflowConfig{Enabled: false, StorePath: "bad\npath"}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled nexaflow config should ignore its fields: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"newline store path", func(c *Config) { c.Nexaflow.StorePath = "bad\npath" }, "nexaflow.store_path"},
		{"oversized store path", func(c *Config) { c.Nexaflow.StorePath = strings.Repeat("a", 4097) }, "nexaflow.store_path"},
		{"newline worker id", func(c *Config) { c.Nexaflow.WorkerID = "worker\x00id" }, "nexaflow.worker_id"},
		{"newline gateway url", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://x\r\n" }, "nexaflow.gateway_base_url"},
		{"non-http gateway url", func(c *Config) { c.Nexaflow.GatewayBaseURL = "ftp://example.invalid" }, "nexaflow.gateway_base_url"},
		{"gateway url without host", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://" }, "nexaflow.gateway_base_url"},
		{"invalid token ref", func(c *Config) { c.Nexaflow.GatewayTokenRef = "1BAD-NAME" }, "nexaflow.gateway_token_ref"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := nexaflowBaseConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
