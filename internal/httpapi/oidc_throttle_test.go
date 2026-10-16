package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

// F3 (issue #425): unauthenticated OIDC login starts were unthrottled —
// one source IP could fill the global 4096-entry pending-login table and
// lock out all admin OIDC logins. The fix rate-limits login starts per
// source IP on the shared admin token bucket AND caps pending logins per
// IP inside the authenticator. This test drives the HTTP surface: an
// attacking IP must hit 429 while a different IP still gets its 302.
func TestOIDCLoginStartThrottledPerIP(t *testing.T) {
	provider := newInProcessOIDCProvider(t)
	s := oidcTestGateway(t, provider)
	h := s.Handler()

	loginStart := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/auth/login", nil)
		req.RemoteAddr = ip + ":44444"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Drain the attacker IP: each login start costs oidcLoginStartCost
	// tokens from a bucket of adminBucketCapacity, so after
	// capacity/cost starts the address must be throttled.
	throttled := false
	for i := 0; i < 30; i++ {
		if rr := loginStart("10.9.9.9"); rr.Code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("attacker IP was never throttled: 30 login starts from one address all succeeded")
	}

	// A different source address is unaffected and still starts a login.
	if rr := loginStart("10.9.9.10"); rr.Code != http.StatusFound {
		t.Fatalf("clean IP login start status=%d want 302 body=%s", rr.Code, rr.Body.String())
	}

	// The attacker address stays throttled.
	if rr := loginStart("10.9.9.9"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("attacker IP status=%d want 429 body=%s", rr.Code, rr.Body.String())
	}
}

// F3 (issue #425), authenticator level: pending logins are bounded per
// source IP so one address can never consume the whole
// maxPendingOIDCLogins table even if it slips past the token bucket.
func TestOIDCBeginBoundedPerIP(t *testing.T) {
	a := &oidcAuthenticator{
		oauth:   &oauth2.Config{ClientID: "test", Endpoint: oauth2.Endpoint{AuthURL: "https://idp.example/auth"}},
		pending: map[string]pendingOIDCLogin{},
	}
	binding := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 43 chars, 32 decoded bytes

	filled := 0
	for i := 0; i < maxPendingOIDCLoginsPerIP+5; i++ {
		if _, _, err := a.Begin("10.9.9.9", "", binding); err != nil {
			break
		}
		filled++
	}
	if filled != maxPendingOIDCLoginsPerIP {
		t.Fatalf("attacker IP filled %d pending logins, want exactly the per-IP cap %d", filled, maxPendingOIDCLoginsPerIP)
	}
	// A different address is unaffected by the attacker's full per-IP quota.
	if _, _, err := a.Begin("10.9.9.10", "", binding); err != nil {
		t.Fatalf("clean IP login start rejected: %v", err)
	}
}
