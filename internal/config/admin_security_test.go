package config

import (
	"strings"
	"testing"
)

func validOIDCConfigForTest() OIDCConfig {
	return OIDCConfig{
		Enabled: true, IssuerURL: "https://identity.example.test", ClientID: "nexaroute",
		ClientSecretEnv: "NEXAROUTE_OIDC_SECRET", RedirectURL: "https://gateway.example.test/admin/auth/oidc/callback",
		Audience: "nexaroute-control-plane", RoleClaim: "groups",
		RoleMappings: map[string]string{"nexaroute-viewers": "viewer", "nexaroute-operators": "operator", "nexaroute-admins": "admin"},
	}
}

func TestOIDCConfigValidateAcceptsSecureAndLoopbackDevelopmentURLs(t *testing.T) {
	if err := (OIDCConfig{}).Validate(); err != nil {
		t.Fatalf("disabled OIDC must not require provider configuration: %v", err)
	}
	if err := validOIDCConfigForTest().Validate(); err != nil {
		t.Fatalf("valid HTTPS configuration rejected: %v", err)
	}
	loopback := validOIDCConfigForTest()
	loopback.IssuerURL = "http://127.0.0.1:7777"
	loopback.RedirectURL = "http://localhost:8080/admin/auth/oidc/callback"
	if err := loopback.Validate(); err != nil {
		t.Fatalf("loopback-only development configuration rejected: %v", err)
	}
}

func TestOIDCConfigValidateRejectsUnsafeFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OIDCConfig)
	}{
		{"remote cleartext issuer", func(c *OIDCConfig) { c.IssuerURL = "http://identity.example.test" }},
		{"issuer user information", func(c *OIDCConfig) { c.IssuerURL = "https://user:pass@identity.example.test" }},
		{"issuer query", func(c *OIDCConfig) { c.IssuerURL = "https://identity.example.test?tenant=x" }},
		{"remote cleartext callback", func(c *OIDCConfig) { c.RedirectURL = "http://gateway.example.test/callback" }},
		{"missing client id", func(c *OIDCConfig) { c.ClientID = "" }},
		{"client id control character", func(c *OIDCConfig) { c.ClientID = "client\nforged" }},
		{"invalid secret env name", func(c *OIDCConfig) { c.ClientSecretEnv = "OIDC-SECRET" }},
		{"missing audience", func(c *OIDCConfig) { c.Audience = "" }},
		{"missing role claim", func(c *OIDCConfig) { c.RoleClaim = "" }},
		{"empty role map", func(c *OIDCConfig) { c.RoleMappings = nil }},
		{"unsupported emergency owner mapping", func(c *OIDCConfig) { c.RoleMappings = map[string]string{"break-glass": "owner"} }},
		{"whitespace claim mapping", func(c *OIDCConfig) { c.RoleMappings = map[string]string{" admin-group ": "admin"} }},
		{"control character claim mapping", func(c *OIDCConfig) { c.RoleMappings = map[string]string{"viewer\nforged": "viewer"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validOIDCConfigForTest()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("unsafe OIDC configuration was accepted")
			}
		})
	}
}

func TestOIDCConfigValidateBoundsRoleMappingCountAndValues(t *testing.T) {
	tooMany := validOIDCConfigForTest()
	tooMany.RoleMappings = make(map[string]string, 101)
	for i := 0; i < 101; i++ {
		tooMany.RoleMappings["group-"+strings.Repeat("x", i+1)] = "viewer"
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("more than 100 role mappings were accepted")
	}

	longClaim := validOIDCConfigForTest()
	longClaim.RoleMappings = map[string]string{strings.Repeat("x", 257): "viewer"}
	if err := longClaim.Validate(); err == nil {
		t.Fatal("overlong role claim value was accepted")
	}

	longClientID := validOIDCConfigForTest()
	longClientID.ClientID = strings.Repeat("x", 513)
	if err := longClientID.Validate(); err == nil {
		t.Fatal("overlong client ID was accepted")
	}
}

func TestAbsoluteSecurityURLValidation(t *testing.T) {
	cases := []struct {
		url       string
		allowHTTP bool
		want      bool
	}{
		{"https://identity.example.test/issuer", true, true},
		{"http://localhost:8080/issuer", true, true},
		{"http://127.0.0.1:8080/issuer", true, true},
		{"http://[::1]:8080/issuer", true, true},
		{"http://identity.example.test/issuer", true, false},
		{"http://localhost/issuer", false, false},
		{"ftp://identity.example.test/issuer", true, false},
		{"https://user:pass@identity.example.test/issuer", true, false},
		{"https://identity.example.test/issuer?tenant=x", true, false},
		{"https://identity.example.test/issuer#fragment", true, false},
		{"https://", true, false},
		{"https://identity.example.test/issuer\nforged", true, false},
		{strings.Repeat("x", maxURLBytes+1), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := absoluteSecurityURL(tc.url, tc.allowHTTP); got != tc.want {
				t.Fatalf("absoluteSecurityURL(%q, %t)=%v, want %v", tc.url, tc.allowHTTP, got, tc.want)
			}
		})
	}
}
