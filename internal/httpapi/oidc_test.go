package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	jose "github.com/go-jose/go-jose/v4"
)

const (
	testOIDCClientID = "nexaroute-test-client"
	testOIDCAudience = "nexaroute-control-plane"
	testOIDCSecret   = "test-only-client-secret"
	testOIDCCallback = "http://localhost/admin/auth/oidc/callback"
)

type oidcTestFlow struct {
	mode      string
	nonce     string
	challenge string
	groups    []string
}

type inProcessOIDCProvider struct {
	server                        *httptest.Server
	privateKey                    *rsa.PrivateKey
	wrongKey                      *rsa.PrivateKey
	mu                            sync.Mutex
	flow                          oidcTestFlow
	lastVerifier                  string
	issuerOverride                string
	authorizationEndpointOverride string
	tokenEndpointOverride         string
	jwksURIOverride               string
	discoveryMissingJWKS          bool
	jwksUnavailable               bool
}

var oidcTestRequestIP atomic.Uint32

func newInProcessOIDCProvider(t *testing.T) *inProcessOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &inProcessOIDCProvider{privateKey: key, wrongKey: wrongKey}
	mux := http.NewServeMux()
	provider.server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		provider.mu.Lock()
		issuer := provider.issuerOverride
		authorizationEndpoint := provider.authorizationEndpointOverride
		tokenEndpoint := provider.tokenEndpointOverride
		jwksURI := provider.jwksURIOverride
		missingJWKS := provider.discoveryMissingJWKS
		provider.mu.Unlock()
		if issuer == "" {
			issuer = provider.server.URL
		}
		if authorizationEndpoint == "" {
			authorizationEndpoint = provider.server.URL + "/authorize"
		}
		if tokenEndpoint == "" {
			tokenEndpoint = provider.server.URL + "/token"
		}
		if jwksURI == "" {
			jwksURI = provider.server.URL + "/jwks"
		}
		metadata := map[string]any{
			"issuer": issuer, "authorization_endpoint": authorizationEndpoint,
			"token_endpoint": tokenEndpoint, "jwks_uri": jwksURI,
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if missingJWKS {
			delete(metadata, "jwks_uri")
		}
		writeTestJSON(w, metadata)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		provider.mu.Lock()
		unavailable := provider.jwksUnavailable
		provider.mu.Unlock()
		if unavailable {
			http.Error(w, "signing keys unavailable", http.StatusServiceUnavailable)
			return
		}
		writeTestJSON(w, map[string]any{"keys": []any{rsaTestJWK(&provider.privateKey.PublicKey, "test-key")}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "authorization is simulated by the test", http.StatusNotImplemented)
	})
	mux.HandleFunc("/token", provider.token)
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *inProcessOIDCProvider) setFlow(flow oidcTestFlow) {
	p.mu.Lock()
	p.flow = flow
	p.lastVerifier = ""
	p.mu.Unlock()
}

func (p *inProcessOIDCProvider) verifier() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastVerifier
}

func (p *inProcessOIDCProvider) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	flow := p.flow
	p.lastVerifier = r.Form.Get("code_verifier")
	p.mu.Unlock()
	if flow.mode == "token-error" {
		writeTestJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	verifier := r.Form.Get("code_verifier")
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if verifier == "" || challenge != flow.challenge {
		writeTestJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	idToken, err := p.signedIDToken(flow)
	if err != nil {
		http.Error(w, "test signer failed", http.StatusInternalServerError)
		return
	}
	writeTestJSON(w, map[string]any{
		"access_token": "test-access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
	})
}

func (p *inProcessOIDCProvider) signedIDToken(flow oidcTestFlow) (string, error) {
	now := time.Now().UTC()
	issuer := p.server.URL
	audience := []string{testOIDCClientID, testOIDCAudience}
	nonce := flow.nonce
	expiry := now.Add(5 * time.Minute).Unix()
	signingKey := p.privateKey
	switch flow.mode {
	case "wrong-issuer":
		issuer = "https://attacker.example"
	case "wrong-audience":
		audience = []string{"another-audience"}
	case "wrong-expected-audience":
		audience = []string{testOIDCClientID}
	case "expired":
		expiry = now.Add(-5 * time.Minute).Unix()
	case "wrong-nonce":
		nonce = "not-the-browser-nonce"
	case "missing-nonce":
		nonce = ""
	case "bad-signature":
		signingKey = p.wrongKey
	}
	claims := map[string]any{
		"iss": issuer, "sub": "oidc-user-123", "aud": audience, "azp": testOIDCClientID,
		"iat": now.Unix(), "exp": expiry, "nonce": nonce, "groups": flow.groups,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	options := (&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), "test-key")
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signingKey}, options)
	if err != nil {
		return "", err
	}
	object, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return object.CompactSerialize()
}

func rsaTestJWK(key *rsa.PublicKey, kid string) map[string]string {
	return map[string]string{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func writeTestJSON(w http.ResponseWriter, value any) { writeTestJSONStatus(w, http.StatusOK, value) }

func writeTestJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func oidcTestConfig(provider *inProcessOIDCProvider) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.EmergencyAccessEnabled = false
	cfg.Admin.OIDC = config.OIDCConfig{
		Enabled: true, IssuerURL: provider.server.URL, ClientID: testOIDCClientID,
		ClientSecretEnv: "NEXAROUTE_TEST_OIDC_SECRET", RedirectURL: testOIDCCallback,
		Audience: testOIDCAudience, RoleClaim: "groups",
		RoleMappings: map[string]string{"nr-viewers": "viewer", "nr-operators": "operator", "nr-admins": "admin"},
	}
	return cfg
}

func oidcTestGateway(t *testing.T, provider *inProcessOIDCProvider) *Server {
	t.Helper()
	t.Setenv("NEXAROUTE_TEST_OIDC_SECRET", testOIDCSecret)
	cfg := oidcTestConfig(provider)
	return testGateway(t, cfg)
}

func beginOIDCTestLogin(t *testing.T, handler http.Handler, previousCookie *http.Cookie) (string, string, *url.URL, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/login", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	if previousCookie != nil {
		req.AddCookie(previousCookie)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("login start status=%d body=%s", rr.Code, rr.Body.String())
	}
	location, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	if query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" || query.Get("redirect_uri") != testOIDCCallback {
		t.Fatalf("authorization request missing required OIDC/PKCE parameters: %s", location.Redacted())
	}
	state, nonce, challenge := query.Get("state"), query.Get("nonce"), query.Get("code_challenge")
	if len(state) < 32 || len(nonce) < 32 || len(challenge) != 43 {
		t.Fatalf("weak or missing state/nonce/PKCE challenge: state=%d nonce=%d challenge=%d", len(state), len(nonce), len(challenge))
	}
	return state, nonce, location, rr
}

func oidcFlowCookie(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == adminOIDCFlowCookie {
			if !cookie.HttpOnly || cookie.Path != "/admin/" || cookie.SameSite != http.SameSiteLaxMode || cookie.Value == "" {
				t.Fatalf("unsafe OIDC flow-binding cookie: %#v", cookie)
			}
			return cookie
		}
	}
	t.Fatal("OIDC login start did not set a browser-binding cookie")
	return nil
}

func finishOIDCTestLogin(t *testing.T, handler http.Handler, state, code string, previousCookie *http.Cookie, bindingCookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	req.RemoteAddr = "127.0.0.1:12345"
	if previousCookie != nil {
		req.AddCookie(previousCookie)
	}
	for _, cookie := range bindingCookies {
		if cookie != nil {
			req.AddCookie(cookie)
		}
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func requestWithAdminCookies(handler http.Handler, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+target, nil)
	ip := oidcTestRequestIP.Add(1)%250 + 1
	req.RemoteAddr = fmt.Sprintf("127.0.0.%d:12345", ip)
	for _, cookie := range cookies {
		if cookie != nil {
			req.AddCookie(cookie)
		}
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestOIDCAuthorizationCodePKCEAndSessionLifecycle(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	var previous *http.Cookie
	var auditCanaries []string
	roles := []struct {
		group    string
		role     string
		wantKeys int
	}{
		{"nr-viewers", "viewer", http.StatusForbidden},
		{"nr-operators", "operator", http.StatusForbidden},
		{"nr-admins", "admin", http.StatusOK},
	}
	for _, tc := range roles {
		state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, previous)
		bindingCookie := oidcFlowCookie(t, loginResponse)
		challenge := location.Query().Get("code_challenge")
		code := "valid-code-" + tc.role
		provider.setFlow(oidcTestFlow{mode: "valid", nonce: nonce, challenge: challenge, groups: []string{tc.group}})
		callback := finishOIDCTestLogin(t, handler, state, code, previous, bindingCookie)
		if callback.Code != http.StatusSeeOther {
			t.Fatalf("%s callback status=%d body=%s", tc.role, callback.Code, callback.Body.String())
		}
		var sessionCookie *http.Cookie
		for _, cookie := range callback.Result().Cookies() {
			if cookie.Name == adminSessionCookie {
				sessionCookie = cookie
			}
		}
		if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.Path != "/admin/" || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Value == "" {
			t.Fatalf("unsafe or missing session cookie for %s: %#v", tc.role, sessionCookie)
		}
		flowCleared := false
		for _, cookie := range callback.Result().Cookies() {
			if cookie.Name == adminOIDCFlowCookie && cookie.MaxAge < 0 {
				flowCleared = true
			}
		}
		if !flowCleared {
			t.Fatal("OIDC flow-binding cookie was not cleared after callback")
		}
		if previous != nil && previous.Value == sessionCookie.Value {
			t.Fatal("session identifier was not rotated after privilege transition")
		}
		if got := provider.verifier(); got == "" {
			t.Fatal("authorization-code exchange did not include the PKCE verifier")
		}
		auditCanaries = append(auditCanaries, state, nonce, challenge, code, provider.verifier(), sessionCookie.Value, bindingCookie.Value, testOIDCSecret, "test-access-token")
		status := requestWithAdminCookies(handler, http.MethodGet, "/admin/auth/status", sessionCookie)
		if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"authenticated":true`) || !strings.Contains(status.Body.String(), `"`+tc.role+`"`) {
			t.Fatalf("%s status=%d body=%s", tc.role, status.Code, status.Body.String())
		}
		keys := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/client-keys", sessionCookie)
		if keys.Code != tc.wantKeys {
			t.Fatalf("%s client-key permission status=%d want=%d body=%s", tc.role, keys.Code, tc.wantKeys, keys.Body.String())
		}
		audit := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/audit", sessionCookie)
		if tc.role == "admin" && audit.Code != http.StatusOK {
			t.Fatalf("admin audit read status=%d body=%s", audit.Code, audit.Body.String())
		}
		if tc.role != "admin" && audit.Code != http.StatusForbidden {
			t.Fatalf("%s audit read status=%d, want 403", tc.role, audit.Code)
		}
		if previous != nil {
			oldRequest := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/snapshot", previous)
			if oldRequest.Code != http.StatusUnauthorized {
				t.Fatalf("rotated old session remained usable: %d %s", oldRequest.Code, oldRequest.Body.String())
			}
		}
		previous = sessionCookie
	}

	csrfResponse := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/csrf-token", previous)
	if csrfResponse.Code != http.StatusOK {
		t.Fatalf("CSRF token status=%d body=%s", csrfResponse.Code, csrfResponse.Body.String())
	}
	var csrfBody struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(csrfResponse.Body.Bytes(), &csrfBody); err != nil || len(csrfBody.Token) != 43 {
		t.Fatalf("invalid CSRF response: body=%s err=%v", csrfResponse.Body.String(), err)
	}
	var csrfCookie *http.Cookie
	for _, cookie := range csrfResponse.Result().Cookies() {
		if cookie.Name == adminCSRFCookie {
			csrfCookie = cookie
		}
	}
	if csrfCookie == nil || csrfCookie.Path != "/admin/" || !csrfCookie.HttpOnly {
		t.Fatalf("unsafe CSRF cookie: %#v", csrfCookie)
	}
	logoutReq := httptest.NewRequest(http.MethodPost, "http://localhost/admin/auth/logout", nil)
	logoutReq.RemoteAddr = "127.0.0.1:12345"
	logoutReq.Header.Set("Origin", "http://localhost")
	logoutReq.Header.Set("X-NexaRoute-CSRF", csrfBody.Token)
	logoutReq.AddCookie(previous)
	logoutReq.AddCookie(csrfCookie)
	logout := httptest.NewRecorder()
	handler.ServeHTTP(logout, logoutReq)
	if logout.Code != http.StatusOK || !strings.Contains(logout.Body.String(), `"ok":true`) {
		t.Fatalf("logout status=%d body=%s", logout.Code, logout.Body.String())
	}
	replay := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/snapshot", previous)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session still authenticated: %d %s", replay.Code, replay.Body.String())
	}
	events, err := srv.securityDB.AuditRecords(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]bool)
	for _, event := range events {
		actions[event.Action] = true
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range auditCanaries {
			if canary != "" && strings.Contains(string(encoded), canary) {
				t.Fatalf("audit event leaked credential/transaction canary %q: %s", canary, encoded)
			}
		}
	}
	for _, action := range []string{"auth.login.succeeded", "auth.logout", "admin.access.denied"} {
		if !actions[action] {
			t.Fatalf("durable audit action %q is missing from %d records", action, len(events))
		}
	}
}

func TestOIDCRejectsInvalidTokensAndTamperedResponses(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	cases := []struct{ name, mode, group string }{
		{"wrong issuer", "wrong-issuer", "nr-viewers"},
		{"wrong audience", "wrong-audience", "nr-viewers"},
		{"missing configured audience", "wrong-expected-audience", "nr-viewers"},
		{"expired token", "expired", "nr-viewers"},
		{"wrong nonce", "wrong-nonce", "nr-viewers"},
		{"missing nonce", "missing-nonce", "nr-viewers"},
		{"bad signature", "bad-signature", "nr-viewers"},
		{"PKCE challenge mismatch", "valid", "nr-viewers"},
		{"unknown role", "valid", "unrecognized-group"},
		{"token endpoint error", "token-error", "nr-viewers"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
			bindingCookie := oidcFlowCookie(t, loginResponse)
			challenge := location.Query().Get("code_challenge")
			if tc.name == "PKCE challenge mismatch" {
				challenge = "not-the-issued-challenge"
			}
			provider.setFlow(oidcTestFlow{mode: tc.mode, nonce: nonce, challenge: challenge, groups: []string{tc.group}})
			callback := finishOIDCTestLogin(t, handler, state, fmt.Sprintf("code-%d", i), nil, bindingCookie)
			if callback.Code != http.StatusUnauthorized {
				t.Fatalf("invalid identity accepted: status=%d body=%s", callback.Code, callback.Body.String())
			}
			if strings.Contains(callback.Body.String(), testOIDCSecret) || strings.Contains(callback.Body.String(), "test-access-token") {
				t.Fatal("authentication error leaked a client/access credential")
			}
		})
	}

	state, _, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{mode: "valid", nonce: location.Query().Get("nonce"), challenge: location.Query().Get("code_challenge"), groups: []string{"nr-viewers"}})
	wrongBinding := &http.Cookie{Name: adminOIDCFlowCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))}
	wrongBrowser := finishOIDCTestLogin(t, handler, state, "wrong-browser-code", nil, wrongBinding)
	if wrongBrowser.Code != http.StatusUnauthorized {
		t.Fatalf("state accepted from a different browser: %d", wrongBrowser.Code)
	}
	badState := finishOIDCTestLogin(t, handler, state+"tampered", "tampered-code", nil, bindingCookie)
	if badState.Code != http.StatusUnauthorized {
		t.Fatalf("tampered state accepted: %d", badState.Code)
	}
	good := finishOIDCTestLogin(t, handler, state, "valid-after-tamper", nil, bindingCookie)
	if good.Code != http.StatusSeeOther {
		t.Fatalf("valid state was lost after unrelated tampered callback: %d %s", good.Code, good.Body.String())
	}
	replay := finishOIDCTestLogin(t, handler, state, "valid-after-tamper", nil, bindingCookie)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed state accepted: %d", replay.Code)
	}

	errorState, _, _, errorStart := beginOIDCTestLogin(t, handler, nil)
	errorReq := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/oidc/callback?state="+url.QueryEscape(errorState)+"&error=access_denied", nil)
	errorReq.RemoteAddr = "127.0.0.1:12345"
	errorReq.AddCookie(oidcFlowCookie(t, errorStart))
	errorRR := httptest.NewRecorder()
	handler.ServeHTTP(errorRR, errorReq)
	if errorRR.Code != http.StatusUnauthorized {
		t.Fatalf("provider error response status=%d", errorRR.Code)
	}

	missingStateReq := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/oidc/callback?code=missing-state-code", nil)
	missingStateReq.RemoteAddr = "127.0.0.1:12345"
	missingStateRR := httptest.NewRecorder()
	handler.ServeHTTP(missingStateRR, missingStateReq)
	if missingStateRR.Code != http.StatusUnauthorized {
		t.Fatalf("callback without state accepted: %d", missingStateRR.Code)
	}
}

func TestOIDCProviderDiscoveryUsesFixedTrustedIssuerAndNoOpenRedirect(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	request := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/login?next=https://attacker.example", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, request)
	if rr.Code != http.StatusFound {
		t.Fatalf("login redirect status=%d body=%s", rr.Code, rr.Body.String())
	}
	location, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != strings.TrimPrefix(provider.server.URL, "http://") || location.Query().Get("redirect_uri") != testOIDCCallback {
		t.Fatalf("unexpected authorization redirect: %s", location.Redacted())
	}
}

func TestOIDCJWKSOutageRejectsCallback(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"), groups: []string{"nr-viewers"}})
	provider.mu.Lock()
	provider.jwksUnavailable = true
	provider.mu.Unlock()
	callback := finishOIDCTestLogin(t, handler, state, "jwks-outage-code", nil, bindingCookie)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("callback accepted without trusted signing keys: %d %s", callback.Code, callback.Body.String())
	}
}

func TestOIDCLoginFailureAuditStoreUnavailableFailsClosed(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	response := requestWithAdminCookies(srv.Handler(), http.MethodGet, "/admin/auth/oidc/callback")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("login denial ignored durable audit failure: %d %s", response.Code, response.Body.String())
	}
}

func TestOIDCDiscoveryIssuerMismatchFailsClosed(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	provider.mu.Lock()
	provider.issuerOverride = "https://attacker.example"
	provider.mu.Unlock()
	t.Setenv("NEXAROUTE_TEST_OIDC_SECRET", testOIDCSecret)
	cfg := oidcTestConfig(provider)
	cfg.Admin.APIKey = "emergency-key-must-not-downgrade"
	cfg.Admin.EmergencyAccessEnabled = true
	srv := testGateway(t, cfg)
	if srv.oidc != nil || srv.oidcErr == nil {
		t.Fatal("issuer-mismatched discovery metadata was accepted")
	}
	request := httptest.NewRequest(http.MethodGet, "http://localhost/admin/api/snapshot", nil)
	request.RemoteAddr = "127.0.0.7:10007"
	request.Header.Set("X-Admin-Key", "emergency-key-must-not-downgrade")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid OIDC discovery downgraded to emergency access: %d %s", response.Code, response.Body.String())
	}
}

func TestOIDCDiscoveryRejectsMalformedOrInsecureEndpoints(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*inProcessOIDCProvider)
	}{
		{name: "missing JWKS URI", setup: func(provider *inProcessOIDCProvider) { provider.discoveryMissingJWKS = true }},
		{name: "cleartext remote token endpoint", setup: func(provider *inProcessOIDCProvider) {
			provider.tokenEndpointOverride = "http://attacker.example/token"
		}},
		{name: "cleartext remote authorization endpoint", setup: func(provider *inProcessOIDCProvider) {
			provider.authorizationEndpointOverride = "http://attacker.example/authorize"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newInProcessOIDCProvider(t)
			provider.mu.Lock()
			tc.setup(provider)
			provider.mu.Unlock()
			t.Setenv("NEXAROUTE_TEST_OIDC_SECRET", testOIDCSecret)
			server := testGateway(t, oidcTestConfig(provider))
			if server.oidc != nil || server.oidcErr == nil {
				t.Fatal("malformed or insecure OIDC discovery metadata was accepted")
			}
		})
	}
}
