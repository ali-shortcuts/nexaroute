package config

import (
	"strings"
	"testing"
)

// Security-relevant validation evidence (M3-4D). The header-injection
// test in config_test.go covers a CRLF header VALUE; these tests pin
// the name side of the same boundary and the secret-safety of
// validation errors.

// Header NAMES are validated with validHeaderName for both static
// provider headers and ForwardHeaders entries: names containing
// whitespace or CRLF (which could split or forge headers at the
// upstream) must be rejected, while valid names keep validating.
func TestValidateRejectsInvalidHeaderNames(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.Providers = []ProviderConfig{{
			ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true,
		}}
		return cfg
	}
	cases := []struct {
		name    string
		mutate  func(*ProviderConfig)
		wantErr string
	}{
		{"static header name with space", func(p *ProviderConfig) {
			p.Headers = map[string]string{"X Bad": "v"}
		}, "invalid custom header"},
		{"static header name with CRLF", func(p *ProviderConfig) {
			p.Headers = map[string]string{"X-Test\r\nInjected": "v"}
		}, "invalid custom header"},
		{"forward header entry padded with spaces", func(p *ProviderConfig) {
			p.ForwardHeaders = []string{" X-Trace "}
		}, "invalid forward header"},
		{"forward header entry with internal space", func(p *ProviderConfig) {
			p.ForwardHeaders = []string{"X Trace"}
		}, "invalid forward header"},
		{"forward header entry with CRLF", func(p *ProviderConfig) {
			p.ForwardHeaders = []string{"X-Trace\r\nInjected"}
		}, "invalid forward header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg.Providers[0])
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q validation error, got %v", tc.wantErr, err)
			}
		})
	}

	// Preservation: valid names in both places validate cleanly.
	cfg := base()
	cfg.Providers[0].Headers = map[string]string{"X-Custom-Header": "v"}
	cfg.Providers[0].ForwardHeaders = []string{"X-Trace-Id", "anthropic-beta"}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid header names should pass validation, got %v", err)
	}
}

// A base_url carrying embedded credentials is rejected, and the
// validation error must never echo the credential itself (errors are
// logged and surfaced to operators): the canary password appears in
// the config but not in the error.
func TestValidateEndpointErrorNeverContainsEmbeddedSecret(t *testing.T) {
	const canary = "canary-password-123"
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible",
		BaseURL: "https://user:" + canary + "@example.com/v1",
		Enabled: true,
	}}
	cfg.ApplyDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected embedded-credentials rejection")
	}
	if !strings.Contains(err.Error(), "embedded credentials") {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("validation error leaks the embedded secret: %v", err)
	}
}
