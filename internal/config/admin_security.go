package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

func (c OIDCConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !absoluteSecurityURL(c.IssuerURL, true) {
		return errors.New("admin.oidc.issuer_url must be an absolute HTTPS URL (HTTP is allowed only for loopback test/development issuers)")
	}
	if len(c.ClientID) == 0 || len(c.ClientID) > 512 || strings.ContainsAny(c.ClientID, "\r\n\x00") {
		return errors.New("admin.oidc.client_id is required and must be at most 512 bytes")
	}
	if !validEnvName(c.ClientSecretEnv) {
		return errors.New("admin.oidc.client_secret_env must be a valid environment variable name")
	}
	if !absoluteSecurityURL(c.RedirectURL, true) {
		return errors.New("admin.oidc.redirect_url must be an absolute HTTPS URL (HTTP is allowed only for loopback test/development callbacks)")
	}
	if len(c.Audience) == 0 || len(c.Audience) > 512 || strings.ContainsAny(c.Audience, "\r\n\x00") {
		return errors.New("admin.oidc.audience is required and must be at most 512 bytes")
	}
	if len(c.RoleClaim) == 0 || len(c.RoleClaim) > 128 || strings.ContainsAny(c.RoleClaim, "\r\n\x00") {
		return errors.New("admin.oidc.role_claim is required and must be at most 128 bytes")
	}
	if len(c.RoleMappings) == 0 || len(c.RoleMappings) > 100 {
		return errors.New("admin.oidc.role_mappings must explicitly map between 1 and 100 verified claim values")
	}
	for claim, role := range c.RoleMappings {
		if claim == "" || strings.TrimSpace(claim) != claim || len(claim) > 256 || strings.ContainsAny(claim, "\r\n\x00") {
			return errors.New("admin.oidc.role_mappings contains an empty or invalid claim value")
		}
		switch role {
		case "viewer", "operator", "admin":
		default:
			return fmt.Errorf("admin.oidc.role_mappings value %q must be viewer, operator, or admin", role)
		}
	}
	return nil
}

func absoluteSecurityURL(raw string, allowLoopbackHTTP bool) bool {
	if raw == "" || len(raw) > maxURLBytes || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		if !allowLoopbackHTTP {
			return false
		}
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}
