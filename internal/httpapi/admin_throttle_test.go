package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Admin failed-auth throttling (M3-5). The admin middleware charges a
// full token bucket (adminBucketCapacity) for an unauthenticated
// /admin/api/ attempt and a single token for an authenticated one, so
// repeated key guessing from one source address is throttled to about
// one guess per refill period while other source addresses are
// unaffected. This behavior had no test before M3-5.

func adminThrottleRequest(s *Server, ip, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.RemoteAddr = ip + ":32100"
	if key != "" {
		req.Header.Set("x-admin-key", key)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAdminFailedAuthAttemptsAreThrottledPerIP(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "test-admin-key"
	s := testGateway(t, cfg)

	// Control: a different source address with the correct key is
	// admitted before and after the throttled address is drained.
	if rr := adminThrottleRequest(s, "127.0.0.2", "test-admin-key"); rr.Code != http.StatusOK {
		t.Fatalf("control request status=%d body=%s", rr.Code, rr.Body.String())
	}

	// First failed attempt from the guessing address: a plain 401,
	// but it drains that address's bucket.
	if rr := adminThrottleRequest(s, "127.0.0.1", "wrong-key"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("first failed attempt status=%d want 401 body=%s", rr.Code, rr.Body.String())
	}
	// Second failed attempt: throttled — but F11 (issue #425) requires the
	// response to be UNIFORM with a plain failed attempt, so the
	// 401-vs-429 difference cannot be used as an auth-recency oracle
	// for this source address. The request is still dropped before any
	// authentication work happens.
	rr := adminThrottleRequest(s, "127.0.0.1", "wrong-key")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("second failed attempt status=%d want 401 (uniform with first) body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "admin authentication required") {
		t.Fatalf("throttled unauthenticated response should match a plain failed attempt: %s", rr.Body.String())
	}
	// Even the correct key from the drained address is briefly
	// throttled (the failure cost is charged before credentials are
	// rechecked on the next request).
	if rr := adminThrottleRequest(s, "127.0.0.1", "test-admin-key"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("correct key from drained address status=%d want 429 body=%s", rr.Code, rr.Body.String())
	}
	// Per-address isolation: the control address still works.
	if rr := adminThrottleRequest(s, "127.0.0.2", "test-admin-key"); rr.Code != http.StatusOK {
		t.Fatalf("control address after throttle status=%d body=%s", rr.Code, rr.Body.String())
	}
}
