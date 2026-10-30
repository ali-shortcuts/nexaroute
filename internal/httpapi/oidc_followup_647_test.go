package httpapi

// Follow-up OIDC negatives for issue #647 (2026-10-30, main 7a2623a).
// Every test here pins behavior that the existing suite did not cover;
// none of them changes production behavior, weakens an assertion, or
// upgrades the live-tenant gate, which stays BLOCKED/UNVERIFIED until an
// operator-owned tenant is authorized and exercised.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// TestOIDC647NestedRoleClaimDenied pins the documented top-level-only
// role-claim limitation as fail-closed: a token whose roles live only
// under a nested object (Keycloak-style realm_access.roles) with no
// top-level claim must be denied, never mapped by traversing the object.
// This is NOT a defect fix and does NOT add nested-claim support: nested
// support would need an owner-approved behavior change, and this test
// would have to be updated deliberately as part of it. Until then the
// operator must map tenant roles to a top-level claim on the tenant
// side (see OIDC_LIVE_TENANT_ACCEPTANCE_647.md §9).
func TestOIDC647NestedRoleClaimDenied(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{
		mode: "nested-role-claim", nonce: nonce,
		challenge: location.Query().Get("code_challenge"),
		groups:    []string{"nr-admins"},
	})
	callback := finishOIDCTestLogin(t, handler, state, "nested-claim-code", nil, bindingCookie)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("nested role claim accepted: status=%d body=%s", callback.Code, callback.Body.String())
	}
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			t.Fatal("nested role claim issued an admin session")
		}
	}
}

// TestOIDC647ShortStateDeniedAndTransactionPreserved pins takeState's
// length floor: a callback carrying a short state value is denied, and
// the probe must not consume or disturb the real pending transaction,
// which must still complete afterwards.
func TestOIDC647ShortStateDeniedAndTransactionPreserved(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	probe := finishOIDCTestLogin(t, handler, "short", "short-state-code", nil, bindingCookie)
	if probe.Code != http.StatusUnauthorized {
		t.Fatalf("short state accepted: status=%d body=%s", probe.Code, probe.Body.String())
	}
	provider.setFlow(oidcTestFlow{
		mode: "valid", nonce: nonce,
		challenge: location.Query().Get("code_challenge"),
		groups:    []string{"nr-viewers"},
	})
	good := finishOIDCTestLogin(t, handler, state, "valid-after-short-state", nil, bindingCookie)
	if good.Code != http.StatusSeeOther {
		t.Fatalf("valid transaction lost after short-state probe: status=%d body=%s", good.Code, good.Body.String())
	}
}

// oidcTouchFailureBackend fails session-store reads while delegating
// everything else, modeling a security-store outage on the read path
// (write-path failures are covered by oidcWriteFailureBackend).
type oidcTouchFailureBackend struct {
	securitystore.Backend
	touchErr error
}

func (b oidcTouchFailureBackend) TouchSession(ctx context.Context, hash string, now time.Time, idleTimeout time.Duration) (securitystore.Session, bool, error) {
	return securitystore.Session{}, false, b.touchErr
}

// TestOIDC647SessionStoreReadFailureFailsClosed proves a session-store
// read outage denies protected admin requests instead of serving them
// or fabricating a session, and that the outage does not revoke the
// session: once the store recovers, the same cookie works again.
func TestOIDC647SessionStoreReadFailureFailsClosed(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	session := completeOIDCTestLogin(t, handler, provider, []string{"nr-admins"})
	orig := srv.securityDB
	srv.securityDB = oidcTouchFailureBackend{Backend: orig, touchErr: errors.New("injected session read failure")}
	denied := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/audit?limit=10", session)
	if denied.Code != http.StatusServiceUnavailable {
		t.Fatalf("session read failure status=%d, want 503 body=%s", denied.Code, denied.Body.String())
	}
	if strings.Contains(denied.Body.String(), `"events"`) {
		t.Fatalf("session read failure served audit data: %s", denied.Body.String())
	}
	srv.securityDB = orig
	recovered := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/audit?limit=10", session)
	if recovered.Code != http.StatusOK {
		t.Fatalf("session lost after store recovery: status=%d body=%s", recovered.Code, recovered.Body.String())
	}
}

// TestOIDC647DiscoveryCrossHostHTTPSEndpointsAccepted pins the exact
// discovery-pinning boundary: the discovery issuer string must match
// exactly and every endpoint must be HTTPS (or loopback HTTP), but
// endpoint hosts are NOT pinned to the issuer host. That is intentional
// and Google-compatible: Google's issuer is accounts.google.com while
// its token endpoint lives on oauth2.googleapis.com. A same-host rule
// would break real providers, so it must never be added casually.
func TestOIDC647DiscoveryCrossHostHTTPSEndpointsAccepted(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	provider.authorizationEndpointOverride = "https://auth.example.test/authorize"
	provider.tokenEndpointOverride = "https://token.example.test/token"
	srv := oidcTestGateway(t, provider)
	if srv.oidcErr != nil {
		t.Fatalf("cross-host HTTPS discovery rejected: %v", srv.oidcErr)
	}
	handler := srv.Handler()
	_, _, location, _ := beginOIDCTestLogin(t, handler, nil)
	if location.Host != "auth.example.test" {
		t.Fatalf("authorization URL host=%q, want auth.example.test (discovery endpoint not used)", location.Host)
	}
}

// TestOIDC647LoginSuccessAuditCarriesSubject strengthens the audit
// coverage from action presence (lifecycle test) to attribution: the
// login-success audit event must name the authenticated subject as its
// actor, with outcome success and the redirect status.
func TestOIDC647LoginSuccessAuditCarriesSubject(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	session := completeOIDCTestLogin(t, handler, provider, []string{"nr-admins"})
	rr := requestWithAdminCookies(handler, http.MethodGet, "/admin/api/audit?limit=100", session)
	if rr.Code != http.StatusOK {
		t.Fatalf("audit read status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Events []struct {
			Actor   string `json:"actor"`
			Action  string `json:"action"`
			Outcome string `json:"outcome"`
			Status  int    `json:"status"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, event := range body.Events {
		if event.Action == "auth.login.succeeded" {
			if event.Actor != "oidc-user-123" || event.Outcome != "success" || event.Status != http.StatusSeeOther {
				t.Fatalf("login-success audit attribution wrong: %+v", event)
			}
			return
		}
	}
	t.Fatalf("no auth.login.succeeded event among %d audit records", len(body.Events))
}
