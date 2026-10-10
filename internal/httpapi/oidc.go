package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	maxPendingOIDCLogins = 4096
	oidcLoginLifetime    = 10 * time.Minute
)

type pendingOIDCLogin struct {
	nonce               string
	verifier            string
	previousSessionHash string
	browserBindingHash  string
	expiresAt           time.Time
}

type oidcAuthenticator struct {
	provider   *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	oauth      *oauth2.Config
	issuer     string
	audience   string
	roleClaim  string
	roleValues map[string]authz.Role
	policyHash string

	mu      sync.Mutex
	pending map[string]pendingOIDCLogin // SHA-256(state) -> one-time transaction
}

func newOIDCAuthenticator(ctx context.Context, cfg config.OIDCConfig) (*oidcAuthenticator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	secret := strings.TrimSpace(os.Getenv(cfg.ClientSecretEnv))
	if secret == "" {
		return nil, fmt.Errorf("OIDC client secret environment variable %s is unset", cfg.ClientSecretEnv)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	discoveryCtx = oidc.ClientContext(discoveryCtx, &http.Client{Timeout: 10 * time.Second})
	provider, err := oidc.NewProvider(discoveryCtx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC provider discovery failed")
	}
	if err := validateOIDCDiscovery(provider, cfg.IssuerURL); err != nil {
		return nil, fmt.Errorf("OIDC provider discovery metadata is invalid: %w", err)
	}
	roles := make(map[string]authz.Role, len(cfg.RoleMappings))
	for claimValue, roleValue := range cfg.RoleMappings {
		roles[claimValue] = authz.Role(roleValue)
	}
	secretDigest := sha256.Sum256([]byte(secret))
	policyData, _ := json.Marshal(struct {
		Version      int               `json:"version"`
		Issuer       string            `json:"issuer"`
		ClientID     string            `json:"client_id"`
		Audience     string            `json:"audience"`
		RoleClaim    string            `json:"role_claim"`
		RoleMappings map[string]string `json:"role_mappings"`
		SecretHash   string            `json:"secret_hash"`
	}{1, cfg.IssuerURL, cfg.ClientID, cfg.Audience, cfg.RoleClaim, cfg.RoleMappings, hex.EncodeToString(secretDigest[:])})
	policyDigest := sha256.Sum256(policyData)
	client := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: secret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
	}
	return &oidcAuthenticator{
		provider:   provider,
		verifier:   provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth:      client,
		issuer:     cfg.IssuerURL,
		audience:   cfg.Audience,
		roleClaim:  cfg.RoleClaim,
		roleValues: roles,
		policyHash: hex.EncodeToString(policyDigest[:]),
		pending:    make(map[string]pendingOIDCLogin),
	}, nil
}

func validateOIDCDiscovery(provider *oidc.Provider, issuer string) error {
	if provider == nil {
		return errors.New("provider is missing")
	}
	endpoint := provider.Endpoint()
	if !validOIDCEndpoint(endpoint.AuthURL, issuer) || !validOIDCEndpoint(endpoint.TokenURL, issuer) {
		return errors.New("authorization or token endpoint must be an absolute HTTPS URL (HTTP loopback is allowed only for loopback development issuers)")
	}
	var metadata struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return errors.New("discovery claims are malformed")
	}
	if metadata.Issuer != issuer || !validOIDCEndpoint(metadata.JWKS, issuer) {
		return errors.New("discovery issuer or JWKS endpoint is invalid")
	}
	return nil
}

func validOIDCEndpoint(raw, issuer string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && isLoopbackHTTP(issuer) && isLoopbackHTTP(raw)
}

func isLoopbackHTTP(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashState(state string) string {
	digest := sha256.Sum256([]byte(state))
	return hex.EncodeToString(digest[:])
}

func (a *oidcAuthenticator) Begin(previousSessionHash, browserBinding string) (string, string, error) {
	bindingBytes, err := base64.RawURLEncoding.DecodeString(browserBinding)
	if err != nil || len(bindingBytes) != 32 || len(browserBinding) != 43 {
		return "", "", errors.New("invalid OIDC browser binding")
	}
	state, err := randomURLToken(32)
	if err != nil {
		return "", "", err
	}
	nonce, err := randomURLToken(32)
	if err != nil {
		return "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	now := time.Now()
	a.mu.Lock()
	for key, value := range a.pending {
		if !now.Before(value.expiresAt) {
			delete(a.pending, key)
		}
	}
	if len(a.pending) >= maxPendingOIDCLogins {
		a.mu.Unlock()
		return "", "", errors.New("too many pending OIDC login transactions")
	}
	a.pending[hashState(state)] = pendingOIDCLogin{
		nonce: nonce, verifier: verifier, previousSessionHash: previousSessionHash,
		browserBindingHash: hashState(browserBinding), expiresAt: now.Add(oidcLoginLifetime),
	}
	a.mu.Unlock()
	url := a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	return url, state, nil
}

func (a *oidcAuthenticator) takeState(state, browserBinding string) (pendingOIDCLogin, bool) {
	if len(state) < 32 || len(state) > 256 || len(browserBinding) != 43 {
		return pendingOIDCLogin{}, false
	}
	decodedBinding, err := base64.RawURLEncoding.DecodeString(browserBinding)
	if err != nil || len(decodedBinding) != 32 {
		return pendingOIDCLogin{}, false
	}
	key := hashState(state)
	a.mu.Lock()
	transaction, ok := a.pending[key]
	if !ok {
		a.mu.Unlock()
		return pendingOIDCLogin{}, false
	}
	if !time.Now().Before(transaction.expiresAt) {
		delete(a.pending, key)
		a.mu.Unlock()
		return pendingOIDCLogin{}, false
	}
	bindingHash := hashState(browserBinding)
	if subtle.ConstantTimeCompare([]byte(transaction.browserBindingHash), []byte(bindingHash)) != 1 {
		a.mu.Unlock()
		return pendingOIDCLogin{}, false
	}
	delete(a.pending, key)
	a.mu.Unlock()
	return transaction, true
}

func (a *oidcAuthenticator) Complete(ctx context.Context, transaction pendingOIDCLogin, code string) (authz.Identity, string, error) {
	if code == "" || len(code) > 4096 {
		return authz.Identity{}, "", errors.New("invalid authorization code")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	token, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(transaction.verifier))
	if err != nil {
		return authz.Identity{}, "", errors.New("OIDC token exchange failed")
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" || len(rawIDToken) > 64<<10 {
		return authz.Identity{}, "", errors.New("OIDC response did not contain a valid ID token")
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return authz.Identity{}, "", errors.New("OIDC ID token verification failed")
	}
	if !containsAudience(idToken.Audience, a.audience) {
		return authz.Identity{}, "", errors.New("OIDC ID token audience is invalid")
	}
	if idToken.Issuer != a.issuer || idToken.Subject == "" || len(idToken.Subject) > 256 || strings.ContainsAny(idToken.Subject, "\r\n\x00") {
		return authz.Identity{}, "", errors.New("OIDC ID token identity claims are invalid")
	}
	if idToken.Nonce == "" || len(idToken.Nonce) != len(transaction.nonce) || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(transaction.nonce)) != 1 {
		return authz.Identity{}, "", errors.New("OIDC nonce validation failed")
	}
	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return authz.Identity{}, "", errors.New("OIDC ID token claims are malformed")
	}
	values, err := claimStringValues(claims[a.roleClaim])
	if err != nil {
		return authz.Identity{}, "", errors.New("OIDC role claim has an invalid type")
	}
	roles := make(map[authz.Role]struct{})
	for _, value := range values {
		if role, allowed := a.roleValues[value]; allowed {
			roles[role] = struct{}{}
		}
	}
	if len(roles) == 0 {
		return authz.Identity{}, "", errors.New("OIDC identity has no allowlisted role")
	}
	if len(roles) != 1 {
		return authz.Identity{}, "", errors.New("OIDC identity has conflicting allowlisted roles")
	}
	var role authz.Role
	for value := range roles {
		role = value
	}
	return authz.Identity{Subject: idToken.Subject, Issuer: idToken.Issuer, Roles: []authz.Role{role}}, transaction.previousSessionHash, nil
}

func containsAudience(audiences []string, expected string) bool {
	for _, audience := range audiences {
		if audience == expected {
			return true
		}
	}
	return false
}

func claimStringValues(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	return nil, errors.New("role claim must be a string or an array of strings")
}
