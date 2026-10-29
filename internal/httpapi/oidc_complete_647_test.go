package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Gap tests added by the #647 completion branch (issue #647), on top of
// the ported PR #666 tests and the PR #671 runbook. All use the local
// in-process IdP fixture only; none of this is live-tenant evidence.

// TestOIDCCompleteAudienceWithoutClientIDDenied pins the client-ID check
// independently of the audience check: a token whose aud list contains
// the configured audience but NOT the client ID must be denied, even
// though audience membership alone would pass.
func TestOIDCCompleteAudienceWithoutClientIDDenied(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{
		mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"),
		groups: []string{"nr-admins"}, aud: []string{testOIDCAudience},
	})
	callback := finishOIDCTestLogin(t, handler, state, "aud-no-client-code", nil, bindingCookie)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("audience-without-client-ID token accepted: status=%d body=%s", callback.Code, callback.Body.String())
	}
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			t.Fatalf("denied identity received an admin session: %#v", cookie)
		}
	}
}

// TestOIDCCompleteCallbackCodeLengthDenied pins Complete's code-length
// branches through the real callback: an empty code and an overlong
// (>4096-byte) code are both denied without reaching the token
// endpoint, so no exchange is attempted for a malformed code.
func TestOIDCCompleteCallbackCodeLengthDenied(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	for _, tc := range []struct {
		name string
		code string
	}{
		{name: "empty code", code: ""},
		{name: "overlong code", code: strings.Repeat("x", 4097)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, _, _, loginResponse := beginOIDCTestLogin(t, handler, nil)
			bindingCookie := oidcFlowCookie(t, loginResponse)
			callback := finishOIDCTestLogin(t, handler, state, tc.code, nil, bindingCookie)
			if callback.Code != http.StatusUnauthorized {
				t.Fatalf("malformed code accepted: status=%d body=%s", callback.Code, callback.Body.String())
			}
			if provider.verifier() != "" {
				t.Fatalf("malformed code reached the token endpoint (verifier presented)")
			}
			for _, cookie := range callback.Result().Cookies() {
				if cookie.Name == adminSessionCookie && cookie.Value != "" {
					t.Fatalf("denied callback received an admin session: %#v", cookie)
				}
			}
		})
	}
}

// TestOIDCCompleteFarFutureIatIsNotChecked documents a deliberate
// limitation, it does NOT claim iat validation: neither the gateway nor
// go-oidc's Verify checks iat, so a token with a far-future iat but a
// valid exp is accepted by the fixture flow. The runbook records this
// as an explicit limitation; do not describe iat as "validated".
func TestOIDCCompleteFarFutureIatIsNotChecked(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{
		mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"),
		groups: []string{"nr-admins"}, iatOffset: 2 * time.Hour,
	})
	callback := finishOIDCTestLogin(t, handler, state, "far-future-iat-code", nil, bindingCookie)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("far-future iat login status=%d body=%s (this test pins current non-enforcement, not a validation claim)", callback.Code, callback.Body.String())
	}
}
