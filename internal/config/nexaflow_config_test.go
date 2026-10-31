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
		{"valid IPv4 loopback base URL", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.1:8080" }, false},
		{"valid IPv6 loopback base URL", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://[::1]:8080" }, false},
		{"valid localhost base URL", func(c *Config) { c.Nexaflow.GatewayBaseURL = "https://localhost:8443" }, false},
		{"valid loopback base URL with trailing slash", func(c *Config) { c.Nexaflow.GatewayBaseURL = "https://127.0.0.1:8443/" }, false},
		{"external hostname rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "https://example.com" }, true},
		{"non-loopback IP rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://8.8.8.8:8080" }, true},
		{"private IP rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://10.0.0.5:8080" }, true},
		{"other loopback spelling rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.2:8080" }, true},
		{"loopback-like external hostname rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.1.evil.com:8080" }, true},
		{"localhost subdomain rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://sub.localhost:8080" }, true},
		{"embedded credentials rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://user:secret@127.0.0.1:8080" }, true},
		{"path rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.1:8080/v1" }, true},
		{"query rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.1:8080?token=x" }, true},
		{"bare query rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.1:8080?" }, true},
		{"fragment rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.1:8080#section" }, true},
		{"port out of range rejected", func(c *Config) { c.Nexaflow.GatewayBaseURL = "http://127.0.0.1:99999" }, true},
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

// TestValidateLoopbackGatewayBaseURL is the regression table for the
// loopback-only rule itself: the three approved spellings pass, and
// every external, credential-bearing, or component-bearing form fails.
// This is the table a security reviewer can read without following
// the full config validation path.
func TestValidateLoopbackGatewayBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid IPv4 loopback", "http://127.0.0.1:8080", false},
		{"valid IPv4 loopback https", "https://127.0.0.1:8443", false},
		{"valid IPv6 loopback", "http://[::1]:8080", false},
		{"valid IPv6 loopback https", "https://[::1]:8443/", false},
		{"valid localhost", "https://localhost:8443", false},
		{"valid localhost case-insensitive", "http://LOCALHOST:8080", false},
		{"valid loopback without port", "http://127.0.0.1", false},
		{"reject https://example.com", "https://example.com", true},
		{"reject external hostname with port", "https://gateway.example:8443", true},
		{"reject a non-loopback IP", "http://8.8.8.8:8080", true},
		{"reject private IP", "http://192.168.1.10:8080", true},
		{"reject link-local IP", "http://169.254.169.254", true},
		{"reject URLs with embedded credentials", "http://user:secret@127.0.0.1:8080", true},
		{"reject credentials on external host", "https://user@127.0.0.1:8080@evil.com/", true},
		{"reject unsupported path", "http://127.0.0.1:8080/v1", true},
		{"reject unsupported query", "http://127.0.0.1:8080?x=1", true},
		{"reject unsupported fragment", "http://127.0.0.1:8080#x", true},
		{"reject non-http scheme", "ftp://127.0.0.1:8080", true},
		{"reject empty", "", true},
		{"reject hostless", "http://", true},
		{"reject mapped loopback", "http://[::ffff:127.0.0.1]:8080", true},
		{"reject zero address", "http://0.0.0.0:8080", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLoopbackGatewayBaseURL(tc.raw)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateLoopbackGatewayBaseURL(%q) accepted, want rejection", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateLoopbackGatewayBaseURL(%q) rejected: %v", tc.raw, err)
			}
		})
	}
}
