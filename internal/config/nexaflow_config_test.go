package config

import "testing"

// NexaFlow opt-in wiring (issue #765): the section is inert when
// disabled and strictly validated when enabled, so a bad store choice
// fails config validation instead of half-wiring the gateway.
func TestValidateNexaflow(t *testing.T) {
	enabled := func(store, path string) Config {
		cfg := Default()
		cfg.Nexaflow.Enabled = true
		cfg.Nexaflow.StoreBackend = store
		cfg.Nexaflow.StorePath = path
		return cfg
	}
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"empty store means file default, path required", enabled("", ""), true},
		{"empty store with path means file default", enabled("", "nexaflow.json"), false},
		{"memory store", enabled("memory", ""), false},
		{"file store with path", enabled("file", "nexaflow.json"), false},
		{"file store without path", enabled("file", ""), true},
		{"postgres store is not offered", enabled("postgres", ""), true},
		{"unknown store", enabled("bogus", ""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateNexaflow()
			if tc.wantErr && err == nil {
				t.Fatalf("accepted, want rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
	// Disabled: the section is inert even with a nonsense store value.
	cfg := Default()
	cfg.Nexaflow.StoreBackend = "bogus"
	if err := cfg.validateNexaflow(); err != nil {
		t.Fatalf("disabled nexaflow section rejected: %v", err)
	}
	// Full Validate passes for the default (disabled) config.
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate: %v", err)
	}
}

// TestValidateNexaflowGatewayFields covers the combined wiring's
// gateway-side shapes on top of the store table above.
func TestValidateNexaflowGatewayFields(t *testing.T) {
	enabled := func() Config {
		cfg := Default()
		cfg.Nexaflow.Enabled = true
		cfg.Nexaflow.StoreBackend = "file"
		cfg.Nexaflow.StorePath = "nexaflow.json"
		return cfg
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid https base URL", func(c *Config) { c.Nexaflow.GatewayBaseURL = "https://127.0.0.1:8443" }, false},
		{"non-http base URL rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "ftp://example.com" }, true},
		{"hostless base URL rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://" }, true},
		{"valid token ref", func(c *Config) { c.Nexaflow.GatewayTokenRef = "NEXAFLOW_TOKEN" }, false},
		{"invalid token ref rejected", func(c *Config) { c.Nexaflow.GatewayTokenRef = "bad name!" }, true},
		{"valid worker id", func(c *Config) { c.Nexaflow.WorkerID = "gateway-2" }, false},
		{"control-char worker id rejected", func(c *Config) { c.Nexaflow.WorkerID = "bad\x00id" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enabled()
			tc.mutate(&cfg)
			err := cfg.validateNexaflow()
			if tc.wantErr && err == nil {
				t.Fatal("accepted, want rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
	// Disabled: gateway nonsense is inert, like store nonsense.
	cfg := Default()
	cfg.Nexaflow.GatewayBaseURL = "ftp://example.com"
	cfg.Nexaflow.GatewayTokenRef = "bad name!"
	if err := cfg.validateNexaflow(); err != nil {
		t.Fatalf("disabled nexaflow section rejected: %v", err)
	}
}
