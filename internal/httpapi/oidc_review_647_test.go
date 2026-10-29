package httpapi

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Independent security review for issue #647. These tests pin
// behaviors the main OIDC suite and PR #666 do not cover: authorized
// party (azp) validation, unsigned tokens, duplicate callback
// parameters, expired pending transactions, clock-claim handling,
// and session/audit survival across a gateway restart. They run
// against the same in-process IdP fixture; a local mock is evidence
// about the gateway's checks only, never about a real tenant.

// oidcReviewGateway mirrors testGateway but keeps the caller-chosen
// config directory, so a test can close one server and open another
// on the same security store (a restart on the same host).
func oidcReviewGateway(t *testing.T, cfg config.Config, configDir string) *Server {
	t.Helper()
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	if router.IsReadyStrategy(cfg.Routing.Strategy) {
		for _, d := range rt.All() {
			hm.RecordSuccess(d.ID, time.Millisecond)
		}
	}
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	srv := New(cfg, configDir+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
	if err := srv.SecurityStoreError(); err != nil {
		t.Fatalf("create test security store: %v", err)
	}
	t.Cleanup(func() { _ = srv.CloseSecurityStore() })
	return srv
}

func oidcReviewAssertNoSession(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			t.Fatalf("denied login received an admin session: %#v", cookie)
		}
	}
}

func oidcReviewSessionCookie(t *testing.T, rr *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == adminSessionCookie && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("callback did not issue an admin session cookie")
	return nil
}

// go-oidc checks audience membership but not the authorized party,
// so a token minted for another client that merely lists this client
// in aud would otherwise pass. The gateway now rejects azp != client
// ID, while still accepting tokens without azp (single-audience
// tokens need not carry it).
func TestOIDCReviewAzpValidation(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	cases := []struct {
		name string
		flow func(nonce, challenge string) oidcTestFlow
		want int
	}{
		{
			name: "authorized party is another client",
			flow: func(nonce, challenge string) oidcTestFlow {
				return oidcTestFlow{mode: "valid", nonce: nonce, challenge: challenge, groups: []string{"nr-viewers"}, azp: "some-other-client"}
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "authorized party absent",
			flow: func(nonce, challenge string) oidcTestFlow {
				return oidcTestFlow{mode: "valid", nonce: nonce, challenge: challenge, groups: []string{"nr-viewers"}, omitAzp: true}
			},
			want: http.StatusSeeOther,
		},
		{
			name: "authorized party is this client",
			flow: func(nonce, challenge string) oidcTestFlow {
				return oidcTestFlow{mode: "valid", nonce: nonce, challenge: challenge, groups: []string{"nr-viewers"}}
			},
			want: http.StatusSeeOther,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
			bindingCookie := oidcFlowCookie(t, loginResponse)
			provider.setFlow(tc.flow(nonce, location.Query().Get("code_challenge")))
			callback := finishOIDCTestLogin(t, handler, state, "azp-code", nil, bindingCookie)
			if callback.Code != tc.want {
				t.Fatalf("azp case status=%d want=%d body=%s", callback.Code, tc.want, callback.Body.String())
			}
			if tc.want == http.StatusUnauthorized {
				oidcReviewAssertNoSession(t, callback)
			}
		})
	}
}

// An alg=none token carries no signature at all. The verifier only
// trusts RS256 (SupportedSigningAlgs is unset, so go-oidc defaults
// to RS256) and requires exactly one signature; the unsigned token
// must fail closed.
func TestOIDCReviewUnsignedTokenDenied(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"), groups: []string{"nr-viewers"}, unsigned: true})
	callback := finishOIDCTestLogin(t, handler, state, "unsigned-code", nil, bindingCookie)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned token accepted: status=%d body=%s", callback.Code, callback.Body.String())
	}
	oidcReviewAssertNoSession(t, callback)
}

// The callback handler requires exactly one state value and exactly
// one code value; duplicated parameters must not be tolerated by
// taking the first (or last) one.
func TestOIDCReviewDuplicateCallbackParametersDenied(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()

	state, _, _, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	dupState := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&state="+url.QueryEscape(state)+"&code=dup-state-code", nil)
	dupState.RemoteAddr = "127.0.0.1:12345"
	dupState.AddCookie(bindingCookie)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, dupState)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("duplicated state accepted: %d", rr.Code)
	}

	state2, _, _, loginResponse2 := beginOIDCTestLogin(t, handler, nil)
	bindingCookie2 := oidcFlowCookie(t, loginResponse2)
	dupCode := httptest.NewRequest(http.MethodGet, "http://localhost/admin/auth/oidc/callback?state="+url.QueryEscape(state2)+"&code=dup-code&code=dup-code", nil)
	dupCode.RemoteAddr = "127.0.0.1:12345"
	dupCode.AddCookie(bindingCookie2)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, dupCode)
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("duplicated code accepted: %d", rr2.Code)
	}
	oidcReviewAssertNoSession(t, rr2)
}

// A pending login transaction lives 10 minutes and is single-use.
// Expiring it server-side must make the callback fail, even with the
// correct state and browser-binding cookie.
func TestOIDCReviewExpiredPendingTransactionDenied(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	state, _, _, loginResponse := beginOIDCTestLogin(t, handler, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	srv.oidc.mu.Lock()
	for key, tx := range srv.oidc.pending {
		tx.expiresAt = time.Now().Add(-time.Minute)
		srv.oidc.pending[key] = tx
	}
	srv.oidc.mu.Unlock()
	callback := finishOIDCTestLogin(t, handler, state, "expired-pending-code", nil, bindingCookie)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("expired pending transaction accepted: %d", callback.Code)
	}
	oidcReviewAssertNoSession(t, callback)
}

// Clock behavior, pinned against go-oidc v3.15.0 (verified in the
// module source, not assumed): expiry is enforced with zero leeway
// and no custom clock; nbf, when present, is enforced with a 5
// minute leeway; iat is not independently checked. The gateway adds
// no leeway of its own.
func TestOIDCReviewClockClaimsPinned(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	srv := oidcTestGateway(t, provider)
	handler := srv.Handler()
	now := time.Now()
	cases := []struct {
		name string
		flow func(nonce, challenge string) oidcTestFlow
		want int
	}{
		{
			name: "issued-at in the near future is not a rejection reason",
			flow: func(nonce, challenge string) oidcTestFlow {
				return oidcTestFlow{mode: "valid", nonce: nonce, challenge: challenge, groups: []string{"nr-viewers"}, iatOffset: 2 * time.Minute}
			},
			want: http.StatusSeeOther,
		},
		{
			name: "not-before within the library leeway is accepted",
			flow: func(nonce, challenge string) oidcTestFlow {
				return oidcTestFlow{mode: "valid", nonce: nonce, challenge: challenge, groups: []string{"nr-viewers"}, nbfUnix: now.Add(2 * time.Minute).Unix()}
			},
			want: http.StatusSeeOther,
		},
		{
			name: "not-before beyond the library leeway is rejected",
			flow: func(nonce, challenge string) oidcTestFlow {
				return oidcTestFlow{mode: "valid", nonce: nonce, challenge: challenge, groups: []string{"nr-viewers"}, nbfUnix: now.Add(10 * time.Minute).Unix()}
			},
			want: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler, nil)
			bindingCookie := oidcFlowCookie(t, loginResponse)
			provider.setFlow(tc.flow(nonce, location.Query().Get("code_challenge")))
			callback := finishOIDCTestLogin(t, handler, state, "clock-code", nil, bindingCookie)
			if callback.Code != tc.want {
				t.Fatalf("clock case status=%d want=%d body=%s", callback.Code, tc.want, callback.Body.String())
			}
			if tc.want == http.StatusUnauthorized {
				oidcReviewAssertNoSession(t, callback)
			}
		})
	}
}

// Restart on the same host: durable sessions and audit records must
// survive (bbolt store file), while process-local pending login
// transactions must not. This is the documented split: sessions and
// audit are durable, pending logins need the initiating instance.
func TestOIDCReviewSessionSurvivesGatewayRestart(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_TEST_OIDC_SECRET", testOIDCSecret)
	cfg := oidcTestConfig(provider)

	srv1 := oidcReviewGateway(t, cfg, dir)
	handler1 := srv1.Handler()
	state, nonce, location, loginResponse := beginOIDCTestLogin(t, handler1, nil)
	bindingCookie := oidcFlowCookie(t, loginResponse)
	provider.setFlow(oidcTestFlow{mode: "valid", nonce: nonce, challenge: location.Query().Get("code_challenge"), groups: []string{"nr-admins"}})
	callback := finishOIDCTestLogin(t, handler1, state, "restart-code", nil, bindingCookie)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("pre-restart callback status=%d body=%s", callback.Code, callback.Body.String())
	}
	sessionCookie := oidcReviewSessionCookie(t, callback)
	status := requestWithAdminCookies(handler1, http.MethodGet, "/admin/auth/status", sessionCookie)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"authenticated":true`) {
		t.Fatalf("pre-restart status=%d body=%s", status.Code, status.Body.String())
	}
	// A second login is started but not completed: its pending
	// transaction must not survive the restart.
	pendingState, _, _, pendingLogin := beginOIDCTestLogin(t, handler1, nil)
	pendingBinding := oidcFlowCookie(t, pendingLogin)

	if err := srv1.CloseSecurityStore(); err != nil {
		t.Fatal(err)
	}
	srv2 := oidcReviewGateway(t, cfg, dir)
	handler2 := srv2.Handler()
	restarted := requestWithAdminCookies(handler2, http.MethodGet, "/admin/auth/status", sessionCookie)
	if restarted.Code != http.StatusOK || !strings.Contains(restarted.Body.String(), `"authenticated":true`) {
		t.Fatalf("session did not survive restart: status=%d body=%s", restarted.Code, restarted.Body.String())
	}
	events, err := srv2.securityDB.AuditRecords(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	succeeded := false
	for _, event := range events {
		if event.Action == "auth.login.succeeded" {
			succeeded = true
		}
	}
	if !succeeded {
		t.Fatal("audit record for the pre-restart login did not survive restart")
	}
	stale := finishOIDCTestLogin(t, handler2, pendingState, "restart-pending-code", nil, pendingBinding)
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("pending login survived restart: %d", stale.Code)
	}
	oidcReviewAssertNoSession(t, stale)
}
