package config

import (
	"strings"
	"testing"
)

// Issue #427 (audit F6): config validation must reject a provider proxy_url
// that points at a built-in blocked address or an operator-denied host/CIDR,
// not merely check that the URL parses. Before the fix only the egress
// transport construction (and not even it) stood between a metadata-address
// proxy and production traffic.
func TestValidateRejectsProxyEgressViolations(t *testing.T) {
	cases := []struct {
		name   string
		proxy  string
		egress EgressConfig
	}{
		{"metadata literal", "http://169.254.169.254:8080/", EgressConfig{}},
		{"loopback literal", "http://127.0.0.1:8080/", EgressConfig{}},
		{"6to4 embedded metadata", "http://[2002:a9fe:a9fe::]:8080/", EgressConfig{}},
		{"operator denied CIDR", "http://8.8.8.8:8080/", EgressConfig{DenyCIDRs: []string{"8.8.8.0/24"}}},
		{"operator denied host", "http://proxy.evil.example:8080/", EgressConfig{DenyHosts: []string{"proxy.evil.example"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			cfg.Egress = c.egress
			p := ProviderConfig{
				ID: "p", Name: "P", Type: "openai_compatible",
				BaseURL: "https://example.com", ProxyURL: c.proxy,
			}
			p.ApplyDefaults()
			cfg.Providers = []ProviderConfig{p}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "proxy_url") {
				t.Fatalf("proxy %q should fail validation with a proxy_url error, got %v", c.proxy, err)
			}
		})
	}

	t.Run("loopback proxy accepted for loopback provider", func(t *testing.T) {
		cfg := Default()
		p := ProviderConfig{
			ID: "p", Name: "P", Type: "openai_compatible",
			BaseURL: "http://127.0.0.1:9999", ProxyURL: "http://127.0.0.1:8888",
		}
		p.ApplyDefaults()
		cfg.Providers = []ProviderConfig{p}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("loopback proxy for a local provider should validate, got %v", err)
		}
	})

	t.Run("public proxy accepted", func(t *testing.T) {
		cfg := Default()
		p := ProviderConfig{
			ID: "p", Name: "P", Type: "openai_compatible",
			BaseURL: "https://example.com", ProxyURL: "http://8.8.8.8:8080/",
		}
		p.ApplyDefaults()
		cfg.Providers = []ProviderConfig{p}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("public proxy should validate, got %v", err)
		}
	})
}
