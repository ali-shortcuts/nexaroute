package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Gap tests for the #647 live-tenant acceptance checklist, added on the
// 2026-10-30 re-audit of main. The already-merged PR #699 suite covers
// issuer pinning, state reuse/binding, nonce mismatch, PKCE S256,
// JWKS rotation/outage, unmapped/conflicting/malformed roles, session
// persistence, logout revocation, and audit failures. These tests pin
// the remaining local gaps only; none of this is live-tenant evidence.

// TestOIDCDiscoveryRejectsIssuerThatDiffersOnlyByTrailingSlash pins
// character-for-character issuer equality: a discovery document whose
// issuer differs from the configured issuer only by a trailing slash
// must still fail closed. This is stricter than rejecting an obviously
// hostile issuer, which the existing wrong-issuer test covers.
func TestOIDCDiscoveryRejectsIssuerThatDiffersOnlyByTrailingSlash(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	provider.mu.Lock()
	provider.issuerOverride = provider.server.URL + "/"
	provider.mu.Unlock()
	t.Setenv("NEXAROUTE_TEST_OIDC_SECRET", testOIDCSecret)
	srv := testGateway(t, oidcTestConfig(provider))
	if srv.oidc != nil || srv.oidcErr == nil {
		t.Fatal("issuer differing only by a trailing slash was accepted")
	}
}

// TestOIDCNonceIsBoundToItsTransaction proves the nonce is tied to the
// pending transaction that issued it, not merely "a nonce this IdP
// knows about". The first transaction is completed with the second
// transaction's (otherwise valid) nonce and must be denied; the second
// transaction is then completed normally to show denial did not depend
// on a broken fixture.
func TestOIDCNonceIsBoundToItsTransaction(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()

	stateA, _, locationA, loginA := beginOIDCTestLogin(t, handler, nil)
	bindingA := oidcFlowCookie(t, loginA)
	stateB, nonceB, locationB, loginB := beginOIDCTestLogin(t, handler, nil)
	bindingB := oidcFlowCookie(t, loginB)

	provider.setFlow(oidcTestFlow{
		mode: "valid", nonce: nonceB, challenge: locationA.Query().Get("code_challenge"),
		groups: []string{"nr-viewers"},
	})
	crossed := finishOIDCTestLogin(t, handler, stateA, "cross-transaction-nonce", nil, bindingA)
	if crossed.Code != http.StatusUnauthorized {
		t.Fatalf("nonce from another transaction accepted: status=%d body=%s", crossed.Code, crossed.Body.String())
	}
	oidcReviewAssertNoSession(t, crossed)

	provider.setFlow(oidcTestFlow{
		mode: "valid", nonce: nonceB, challenge: locationB.Query().Get("code_challenge"),
		groups: []string{"nr-viewers"},
	})
	valid := finishOIDCTestLogin(t, handler, stateB, "own-transaction-nonce", nil, bindingB)
	if valid.Code != http.StatusSeeOther {
		t.Fatalf("own-transaction nonce rejected: status=%d body=%s", valid.Code, valid.Body.String())
	}
}

// TestOIDCTokenExchangeUsesConfiguredClientRedirectAndGrant pins the
// token-endpoint half of the callback URL contract. The existing
// authorization-request tests prove the redirect is present in the
// browser redirect; this test proves the exchange also sends exactly
// the configured client ID, redirect URI, and authorization-code grant
// type. It uses a config value that differs from the request host so a
// hard-coded/default substitute cannot pass.
func TestOIDCTokenExchangeUsesConfiguredClientRedirectAndGrant(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	t.Setenv("NEXAROUTE_TEST_OIDC_SECRET", testOIDCSecret)
	const configuredRedirect = "http://127.0.0.1/admin/auth/oidc/callback"
	cfg := oidcTestConfig(provider)
	cfg.Admin.OIDC.RedirectURL = configuredRedirect
	srv := testGateway(t, cfg)
	handler := srv.Handler()

	loginReq := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/login", nil)
	loginReq.RemoteAddr = "127.0.0.1:12345"
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginReq)
	if login.Code != http.StatusFound {
		t.Fatalf("login start status=%d body=%s", login.Code, login.Body.String())
	}
	location, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := location.Query().Get("redirect_uri"); got != configuredRedirect {
		t.Fatalf("authorization redirect_uri=%q want configured %q", got, configuredRedirect)
	}
	state := location.Query().Get("state")
	nonce := location.Query().Get("nonce")
	binding := oidcFlowCookie(t, login)
	provider.setFlow(oidcTestFlow{
		mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"),
		groups: []string{"nr-viewers"},
	})
	callback := finishOIDCTestLogin(t, handler, state, "configured-redirect-code", nil, binding)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("callback status=%d body=%s", callback.Code, callback.Body.String())
	}
	clientID, redirectURI, grantType := provider.tokenRequest()
	if clientID != testOIDCClientID {
		t.Fatalf("token exchange client_id=%q want %q", clientID, testOIDCClientID)
	}
	if redirectURI != configuredRedirect {
		t.Fatalf("token exchange redirect_uri=%q want configured %q", redirectURI, configuredRedirect)
	}
	if grantType != "authorization_code" {
		t.Fatalf("token exchange grant_type=%q want authorization_code", grantType)
	}
}

// TestOIDCRoleMappingAcceptsDuplicateAndMixedMappedValues pins the
// documented "ambiguous" boundary: duplicate copies of one mapped value
// collapse to one role, and unmapped or empty values alongside exactly
// one mapped value are ignored. Only two or more DISTINCT mapped roles
// are an ambiguity that must be denied (covered by the existing suite).
func TestOIDCRoleMappingAcceptsDuplicateAndMixedMappedValues(t *testing.T) {
	cases := []struct {
		name   string
		groups []string
		role   string
	}{
		{name: "duplicate same mapped value", groups: []string{"nr-admins", "nr-admins"}, role: "admin"},
		{name: "mapped plus unmapped value", groups: []string{"nr-viewers", "unrecognized-group"}, role: "viewer"},
		{name: "empty plus mapped value", groups: []string{"", "nr-operators"}, role: "operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newInProcessOIDCProvider(t)
			srv := oidcTestGateway(t, provider)
			handler := srv.Handler()
			sessionCookie := completeOIDCTestLogin(t, handler, provider, tc.groups)
			status := requestWithAdminCookies(handler, http.MethodGet, "/admin/auth/status", sessionCookie)
			if status.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", status.Code, status.Body.String())
			}
			if !strings.Contains(status.Body.String(), `"`+tc.role+`"`) {
				t.Fatalf("mapped role %q missing from status: %s", tc.role, status.Body.String())
			}
		})
	}
}
