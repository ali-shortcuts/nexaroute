package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"golang.org/x/oauth2"
)

// Wave 2 (issue #663): oidc.go blocks uncovered in the 2026-10-29
// baseline profile. The callback tests use the shared in-process IdP
// fixture; the two new fixture modes live in oidc_test.go.
//
// Deliberately NOT covered: newOIDCAuthenticator's discovery-Claims
// failure (oidc.go:93) and validateOIDCDiscovery's Claims failure
// (:157) — go-oidc's NewProvider already parses the same discovery
// document, so a provider that survived discovery cannot fail a
// subset re-parse; both are defensive. randomURLToken's rand.Read
// failure (:192/:209/:213) is not deterministically inducible.

func wave2OIDCValidConfig(issuer, secretEnv string) config.OIDCConfig {
	return config.OIDCConfig{
		Enabled: true, IssuerURL: issuer, ClientID: testOIDCClientID,
		ClientSecretEnv: secretEnv, RedirectURL: testOIDCCallback,
		Audience: testOIDCAudience, RoleClaim: "groups",
		RoleMappings: map[string]string{"nr-admins": "admin"},
	}
}

func TestWave2OIDCConstructorInvalidConfig(t *testing.T) {
	if _, err := newOIDCAuthenticator(context.Background(), config.OIDCConfig{Enabled: true}); err == nil {
		t.Fatal("an enabled OIDC config with no issuer/client must fail validation")
	}
}

func TestWave2OIDCConstructorSecretUnset(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	cfg := wave2OIDCValidConfig(provider.server.URL, "WAVE2_OIDC_SECRET_DEFINITELY_UNSET")
	if _, err := newOIDCAuthenticator(context.Background(), cfg); err == nil {
		t.Fatal("a valid config naming an unset secret env var must fail closed")
	}
}

func TestWave2OIDCConstructorNilContext(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	t.Setenv("WAVE2_OIDC_SECRET", testOIDCSecret)
	cfg := wave2OIDCValidConfig(provider.server.URL, "WAVE2_OIDC_SECRET")
	// A nil context falls back to context.Background and discovery
	// proceeds against the loopback test issuer.
	if _, err := newOIDCAuthenticator(nil, cfg); err != nil {
		t.Fatalf("nil-context construction failed: %v", err)
	}
}

func TestWave2OIDCConstructorHTTPSEndpoints(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	// The issuer itself is loopback HTTP, but the endpoint VALUES are
	// HTTPS: validOIDCEndpoint accepts any absolute HTTPS URL.
	provider.authorizationEndpointOverride = "https://idp.example/authorize"
	provider.tokenEndpointOverride = "https://idp.example/token"
	provider.jwksURIOverride = "https://idp.example/jwks"
	t.Setenv("WAVE2_OIDC_SECRET", testOIDCSecret)
	cfg := wave2OIDCValidConfig(provider.server.URL, "WAVE2_OIDC_SECRET")
	// Construction performs no JWKS fetch, so the unreachable HTTPS
	// endpoints do not block it; the test pins the HTTPS-accept branch.
	if _, err := newOIDCAuthenticator(context.Background(), cfg); err != nil {
		t.Fatalf("HTTPS-endpoint construction failed: %v", err)
	}
}

func TestWave2OIDCValidateDiscoveryNilProvider(t *testing.T) {
	if err := validateOIDCDiscovery(nil, "https://idp.example"); err == nil {
		t.Fatal("a nil provider must fail discovery validation")
	}
}

func TestWave2OIDCLoopbackHelpers(t *testing.T) {
	if !isLoopbackHTTP("http://localhost:8080/x") {
		t.Fatal("localhost must count as loopback HTTP")
	}
	if isLoopbackHTTP("https://example.com/x") {
		t.Fatal("an HTTPS URL must not count as loopback HTTP")
	}
	if isLoopbackHTTP("http://192.0.2.1/x") {
		t.Fatal("a non-loopback IP must not count as loopback HTTP")
	}
}

func TestWave2OIDCClaimStringValuesSingle(t *testing.T) {
	values, err := claimStringValues([]byte(`"nr-admins"`))
	if err != nil || len(values) != 1 || values[0] != "nr-admins" {
		t.Fatalf("single-string claim: values=%v err=%v", values, err)
	}
}

func TestWave2OIDCBeginInvalidBinding(t *testing.T) {
	a := &oidcAuthenticator{pending: map[string]pendingOIDCLogin{}}
	if _, _, err := a.Begin("127.0.0.1", "", "not-a-binding"); err == nil {
		t.Fatal("a malformed browser binding must be rejected before any state is issued")
	}
}

func TestWave2OIDCBeginSweepsExpiredPending(t *testing.T) {
	binding := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	a := &oidcAuthenticator{
		oauth: &oauth2.Config{ClientID: testOIDCClientID,
			Endpoint: oauth2.Endpoint{AuthURL: "http://127.0.0.1:9/authorize"}},
		pending: map[string]pendingOIDCLogin{
			"stale": {sourceIP: "127.0.0.1", expiresAt: time.Now().Add(-time.Minute)},
		},
	}
	url, state, err := a.Begin("127.0.0.1", "", binding)
	if err != nil || url == "" || state == "" {
		t.Fatalf("Begin with only an expired pending entry failed: url=%q state=%q err=%v", url, state, err)
	}
	if _, stillThere := a.pending["stale"]; stillThere {
		t.Fatal("Begin must sweep the expired pending entry")
	}
	if len(a.pending) != 1 {
		t.Fatalf("pending has %d entries, want exactly the new transaction", len(a.pending))
	}
}

func TestWave2OIDCTakeStateBadBinding(t *testing.T) {
	a := &oidcAuthenticator{pending: map[string]pendingOIDCLogin{}}
	// 32-char state passes the length gate; a 43-char binding of '!'
	// passes the length gate but fails base64url decoding.
	if _, ok := a.takeState("ssssssssssssssssssssssssssssssss", "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"); ok {
		t.Fatal("an undecodable binding must not yield a transaction")
	}
}

func TestWave2OIDCCompleteNoIDToken(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{
		mode: "no-id-token", nonce: nonce, challenge: location.Query().Get("code_challenge"),
		groups: []string{"nr-admins"},
	})
	callback := finishOIDCTestLogin(t, handler, state, "no-id-token-code", nil, bindingCookie)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("token response without an ID token accepted: status=%d body=%s", callback.Code, callback.Body.String())
	}
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			t.Fatalf("denied identity received an admin session: %#v", cookie)
		}
	}
}

func TestWave2OIDCCompleteEmptySubject(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{
		mode: "empty-sub", nonce: nonce, challenge: location.Query().Get("code_challenge"),
		groups: []string{"nr-admins"},
	})
	callback := finishOIDCTestLogin(t, handler, state, "empty-sub-code", nil, bindingCookie)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("verifiable token with an empty subject accepted: status=%d body=%s", callback.Code, callback.Body.String())
	}
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			t.Fatalf("denied identity received an admin session: %#v", cookie)
		}
	}
}
