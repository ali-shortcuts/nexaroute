package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave4MiddlewareRequest(s *Server, method, path, remoteAddr, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = remoteAddr
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

// An unauthenticated source pays the full bucket (90) on its first
// admin attempt; the second attempt is throttled, and the throttle
// answer is deliberately the same 401 as a plain auth failure so the
// status cannot be used as an auth-recency oracle.
func TestWave4MiddlewareUnauthenticatedThrottle(t *testing.T) {
	s := wave4AdminGateway(t)
	first := wave4MiddlewareRequest(s, http.MethodGet, "/admin/api/snapshot", "203.0.113.7:1234", "")
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("first status=%d body=%s, want 401", first.Code, first.Body.String())
	}
	second := wave4MiddlewareRequest(s, http.MethodGet, "/admin/api/snapshot", "203.0.113.7:1234", "")
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("throttled status=%d body=%s, want 401", second.Code, second.Body.String())
	}
}

// Authenticated loopback requests cost 1 token against a 90-token
// bucket while refill (1.5/s) cannot keep up with the request rate, so
// a sustained burst is eventually refused with 429.
func TestWave4MiddlewareAuthenticatedThrottle(t *testing.T) {
	s := wave4AdminGateway(t)
	first := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s, want 200", first.Code, first.Body.String())
	}
	throttled := false
	for i := 0; i < 200 && !throttled; i++ {
		rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot", "")
		if rr.Code == http.StatusTooManyRequests {
			throttled = true
		} else if rr.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s, want 200 or 429", i, rr.Code, rr.Body.String())
		}
	}
	if !throttled {
		t.Fatal("no 429 within 200 loopback admin requests; bucket never drained")
	}
}

// With admin client certificates required, an authenticated loopback
// request without a verified chain is refused before the handler runs.
func TestWave4MiddlewareAdminClientCertRequired(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.TLS.RequireClientCertAdmin = true
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "verified client certificate") {
		t.Fatalf("body=%s, want the certificate message", rr.Body.String())
	}
}

// A draining gateway refuses new data-plane work with a Retry-After
// hint instead of admitting it.
func TestWave4MiddlewareDraining(t *testing.T) {
	s := wave4AdminGateway(t)
	s.draining.Store(true)
	rr := wave4MiddlewareRequest(s, http.MethodPost, "/v1/chat/completions", "127.0.0.1:1234", `{"model":"a","messages":[]}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining status=%d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After = %q, want 5", rr.Header().Get("Retry-After"))
	}
}

// Saturating the data-plane admission counter refuses further work
// with 503 and a Retry-After hint.
func TestWave4MiddlewareOverload(t *testing.T) {
	s := wave4AdminGateway(t)
	s.inflight.Store(128)
	rr := wave4MiddlewareRequest(s, http.MethodPost, "/v1/chat/completions", "127.0.0.1:1234", `{"model":"a","messages":[]}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("overload status=%d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", rr.Header().Get("Retry-After"))
	}
}
