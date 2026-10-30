package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Issue #647 follow-up, 2026-10-30: the main OIDC suite already pins
// state tampering, wrong-browser binding, replay, duplicate parameters,
// expired transactions, PKCE challenge mismatch, wrong/missing nonce,
// bad signatures, JWKS rotation/overlap, and unmapped/ambiguous/
// malformed roles. What it did not pin was the callback's
// browser-binding gate itself: a callback with no flow cookie, with a
// malformed flow cookie, or with the wrong HTTP method. Each of those
// must fail closed WITHOUT consuming the one-time state, because the
// binding check runs before takeState deletes the transaction —
// otherwise a single stray callback (or a probe) would burn a
// legitimate login in progress. These tests pin that ordering, plus
// two adjacent fail-closed branches: a provider-error callback must
// still consume its state (single-use on the error path, not only on
// the success path), and a login start whose audit write fails must
// not issue a redirect or a flow cookie.
//
// All tests use the in-process IdP fixture. They are local protocol
// coverage only and say nothing about a real organizational tenant.

func oidc647AuditCounts(t *testing.T, srv *Server) (failed, succeeded int) {
	t.Helper()
	events, err := srv.securityDB.AuditRecords(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		switch event.Action {
		case "auth.login.failed":
			failed++
		case "auth.login.succeeded":
			succeeded++
		}
	}
	return failed, succeeded
}

func oidc647AssertNoSession(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			t.Fatalf("denied callback received an admin session: %#v", cookie)
		}
	}
}

// A callback with no flow-binding cookie must be denied, must not
// issue a session, must record exactly one denial, and must not
// consume the state: the legitimate browser completes afterwards
// with the binding cookie issued at login start.
func TestOIDC647CallbackWithoutFlowCookieDeniedAndStatePreserved(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	denied := finishOIDCTestLogin(t, handler, state, "no-cookie-code", nil)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("callback without flow cookie accepted: %d %s", denied.Code, denied.Body.String())
	}
	oidc647AssertNoSession(t, denied)
	if failed, _ := oidc647AuditCounts(t, srv); failed != 1 {
		t.Fatalf("denied callback produced %d auth.login.failed records, want 1", failed)
	}
	provider.setFlow(oidcTestFlow{mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"), groups: []string{"nr-viewers"}})
	good := finishOIDCTestLogin(t, handler, state, "after-no-cookie-code", nil, bindingCookie)
	if good.Code != http.StatusSeeOther {
		t.Fatalf("preserved state did not complete after no-cookie denial: %d %s", good.Code, good.Body.String())
	}
	if _, succeeded := oidc647AuditCounts(t, srv); succeeded != 1 {
		t.Fatalf("completed login produced %d auth.login.succeeded records, want 1", succeeded)
	}
}

// A callback with a present but malformed flow cookie (too short to
// be a 32-byte binding) must be denied the same way, without
// consuming the state.
func TestOIDC647CallbackMalformedFlowCookieDeniedAndStatePreserved(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	malformed := &http.Cookie{Name: adminOIDCFlowCookie, Value: "not-a-binding"}
	denied := finishOIDCTestLogin(t, handler, state, "malformed-cookie-code", nil, malformed)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("callback with malformed flow cookie accepted: %d %s", denied.Code, denied.Body.String())
	}
	oidc647AssertNoSession(t, denied)
	if failed, _ := oidc647AuditCounts(t, srv); failed != 1 {
		t.Fatalf("denied callback produced %d auth.login.failed records, want 1", failed)
	}
	provider.setFlow(oidcTestFlow{mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"), groups: []string{"nr-viewers"}})
	good := finishOIDCTestLogin(t, handler, state, "after-malformed-code", nil, bindingCookie)
	if good.Code != http.StatusSeeOther {
		t.Fatalf("preserved state did not complete after malformed-cookie denial: %d %s", good.Code, good.Body.String())
	}
}

// The callback accepts GET only, and the method check precedes
// takeState: a POST callback must be rejected with 405, must not
// record a login denial (the method guard runs before any audit),
// and must not consume the state.
func TestOIDC647CallbackWrongMethodDoesNotConsumeState(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	post := httptest.NewRequest(http.MethodPost, "http://localhost/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=post-code", nil)
	post.RemoteAddr = "127.0.0.1:12345"
	post.AddCookie(bindingCookie)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, post)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST callback status=%d, want 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("POST callback Allow=%q, want GET", allow)
	}
	oidc647AssertNoSession(t, rr)
	if failed, _ := oidc647AuditCounts(t, srv); failed != 0 {
		t.Fatalf("method rejection produced %d auth.login.failed records, want 0", failed)
	}
	provider.setFlow(oidcTestFlow{mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"), groups: []string{"nr-viewers"}})
	good := finishOIDCTestLogin(t, handler, state, "after-post-code", nil, bindingCookie)
	if good.Code != http.StatusSeeOther {
		t.Fatalf("preserved state did not complete after POST rejection: %d %s", good.Code, good.Body.String())
	}
}

// A provider-error callback takes the state (single-use) before the
// error check, so replaying the same state afterwards must fail —
// and must record a second denial, not silently reuse the error path.
func TestOIDC647ProviderErrorCallbackConsumesState(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, _, _, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	errorReq := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&error=access_denied", nil)
	errorReq.RemoteAddr = "127.0.0.1:12345"
	errorReq.AddCookie(bindingCookie)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, errorReq)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("provider-error callback status=%d, want 401", rr.Code)
	}
	oidc647AssertNoSession(t, rr)
	if failed, _ := oidc647AuditCounts(t, srv); failed != 1 {
		t.Fatalf("provider-error callback produced %d auth.login.failed records, want 1", failed)
	}
	replay := finishOIDCTestLogin(t, handler, state, "replay-after-error-code", nil, bindingCookie)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("state replayed after provider error: %d", replay.Code)
	}
	if failed, _ := oidc647AuditCounts(t, srv); failed != 2 {
		t.Fatalf("replay after provider error produced %d auth.login.failed records, want 2", failed)
	}
}

// Audit-failure backend for the login-start test: only AppendAudit
// fails; session and audit reads still work, so the test isolates
// authLogin's recordAudit branch (503, no redirect, no flow cookie)
// rather than the store-unavailable branch.
type oidc647AuditFailureBackend struct {
	securitystore.Backend
}

func (b oidc647AuditFailureBackend) AppendAudit(ctx context.Context, event securitystore.AuditEvent) error {
	return errors.New("injected audit failure")
}

func TestOIDC647LoginStartAuditFailureFailsClosed(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	original := srv.securityDB
	srv.securityDB = oidc647AuditFailureBackend{Backend: original}
	req := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/login", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("login start with audit failure status=%d, want 503", rr.Code)
	}
	if rr.Header().Get("Location") != "" {
		t.Fatalf("login start with audit failure redirected to %q", rr.Header().Get("Location"))
	}
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == adminOIDCFlowCookie && cookie.Value != "" {
			t.Fatalf("login start with audit failure issued a flow cookie: %#v", cookie)
		}
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			t.Fatalf("login start with audit failure issued a session: %#v", cookie)
		}
	}
}
