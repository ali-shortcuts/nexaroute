package httpapi

// Wave-2 coverage for the gateway server surface (issue #663,
// continuing PR #730). Targets the merged-tree profile's uncovered
// blocks in internal/httpapi/server.go that are reachable without a
// concurrent config swap or fault injection. Skipped as unreachable
// (see the commit message): the nil-resolver candidates path (needs a
// torn snapshot pair), the ResolveByID miss (same-list invariant),
// the audit-completion write failure (needs a counting fake backend
// plus a full admin operation), the CSRF rand.Read failure, and the
// security-path empty branch (the helper never returns empty+nil).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestWave2ServerNilResolverVE(t *testing.T) {
	var s Server
	_, isVirtual, err := s.resolveVirtualEndpoint("coding")
	if err != nil || isVirtual {
		t.Fatalf("nil resolver: isVirtual=%v err=%v want false,nil", isVirtual, err)
	}
}

func TestWave2ServerAdminHostLoopbackIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.2:8080/", nil)
	req.Host = "127.0.0.2:8080"
	if !adminHostAllowed(req) {
		t.Fatal("127.0.0.2 must count as a loopback admin host")
	}
	req.Host = "example.com"
	if adminHostAllowed(req) {
		t.Fatal("example.com must not count as a loopback admin host")
	}
}

func TestWave2ServerAdminBucketIdleReset(t *testing.T) {
	s := testGateway(t, config.Default())
	s.adminBuckets = map[string]*adminBucket{
		"9.9.9.9": {tokens: 0, last: time.Now().Add(-20 * time.Minute)},
	}
	if !s.adminAllow("9.9.9.9", 1.0) {
		t.Fatal("idle-reset bucket must allow the request")
	}
	if got := s.adminBuckets["9.9.9.9"].tokens; got != adminBucketCapacity-1.0 {
		t.Fatalf("tokens=%v want %v (fresh bucket minus cost)", got, adminBucketCapacity-1.0)
	}
}

func TestWave2ServerJevProviderFailureSkipped(t *testing.T) {
	cfg := config.Default()
	cfg.DecisionProviders = []config.DecisionProviderConfig{{Type: "jev"}}
	s := testGateway(t, cfg)
	if s == nil {
		t.Fatal("gateway must still build when a decision provider fails")
	}
	for _, id := range s.decisionRegistry.List() {
		if id == "" {
			t.Fatal("failed jev provider must not be registered")
		}
	}
}

func TestWave2ServerConvertDecisionPoliciesSkipsInvalid(t *testing.T) {
	out := convertDecisionPolicies([]config.DecisionPolicyConfig{
		{ID: ""},
		{ID: "p1", SelectionMode: "select_first", MinScoreDelta: 0.5,
			Weights: config.DecisionPolicyWeights{RouterBaseline: 1.0}},
	})
	if len(out) != 1 {
		t.Fatalf("policies=%d want 1 (invalid skipped, valid kept)", len(out))
	}
}

func TestWave2ServerChangedDeploymentIDsSkipsDisabled(t *testing.T) {
	oldCfg := config.Config{Providers: []config.ProviderConfig{{
		ID: "pA", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Enabled: true}},
	}}}
	newCfg := config.Config{Providers: []config.ProviderConfig{
		{ID: "pA", Enabled: true, Models: []config.ModelConfig{{ID: "m", Enabled: false}}},
		{ID: "pB", Enabled: false, Models: []config.ModelConfig{{ID: "m", Enabled: true}}},
	}}
	changed := changedDeploymentIDs(oldCfg, newCfg)
	if _, ok := changed["pA/m"]; ok {
		t.Fatal("disabled model must not appear in changed deployments")
	}
	if _, ok := changed["pB/m"]; ok {
		t.Fatal("disabled provider must not appear in changed deployments")
	}
}

func TestWave2ServerApplyRolloutCanaryAbsent(t *testing.T) {
	cfg := config.Default()
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp", CanaryDeploymentID: "zz/m", CanaryPercent: 100}}
	resolved := &route.ResolvedRoute{RouteProfileID: "rp"}
	in := []router.Scored{
		{Deployment: router.Deployment{ID: "pA/m"}},
		{Deployment: router.Deployment{ID: "pB/m"}},
	}
	out := applyRollout(cfg, resolved, router.Requirement{SessionKey: "k"}, in)
	if len(out) != 2 || out[0].Deployment.ID != "pA/m" || out[1].Deployment.ID != "pB/m" {
		t.Fatalf("absent canary must leave order unchanged, got %v", out)
	}
}

func TestWave2ServerCurrentRouteCandidateMissingAdapter(t *testing.T) {
	cfgA := config.Default()
	cfgA.Routing.Strategy = "priority"
	cfgA.Providers = []config.ProviderConfig{{
		ID: "pX", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "physical-X", Enabled: true}},
	}}
	hm := health.New(cfgA.Routing.FailureThreshold, cfgA.Cooldown())
	rt := router.New(cfgA, hm)
	reg, err := providers.NewRegistry(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{rt: rt, reg: reg}
	if _, _, ok := s.currentRouteCandidate("pX/m", router.Requirement{Model: "physical-X"}); ok {
		t.Fatal("candidate with no registered adapter must be unavailable")
	}
}

func TestWave2ServerMiddlewarePanicRecovered(t *testing.T) {
	s := testGateway(t, config.Default())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(errors.New("boom")) })
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(`{}`))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.middleware(next).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "internal gateway error") {
		t.Fatalf("status=%d body=%s want 500 internal gateway error", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKind(s, "internal_panic") {
		t.Fatal("internal_panic event missing")
	}
}

func TestWave2ServerMiddlewareAbortHandlerRepanics(t *testing.T) {
	s := testGateway(t, config.Default())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(`{}`))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("recovered=%v want http.ErrAbortHandler", recovered)
		}
	}()
	s.middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("ErrAbortHandler must propagate out of the middleware")
}

func TestWave2ServerAdminSecurityStoreUnavailable(t *testing.T) {
	s := testGateway(t, config.Default())
	s.securityDB = nil
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/models", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "admin security store unavailable") {
		t.Fatalf("status=%d body=%s want 503 admin security store unavailable", rr.Code, rr.Body.String())
	}
}

func TestWave2ServerTrustedProxyUnparsablePeer(t *testing.T) {
	cfg := config.Default()
	cfg.TLS.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	req.RemoteAddr = "garbage"
	if got := s.withTrustedProxyContext(req); got != req {
		t.Fatal("unparsable peer address must leave the request untouched")
	}
	if v := req.Context().Value(trustedProxyContextKey{}); v != nil {
		t.Fatalf("unparsable peer must not be marked trusted, got %v", v)
	}
}

func TestWave2ServerSameOriginRejected(t *testing.T) {
	mk := func(origin string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/admin/api/x", nil)
		req.Host = "example.com"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return req
	}
	for _, tc := range []struct {
		name   string
		origin string
		want   bool
	}{
		{"unparsable", "://bad", false},
		{"userinfo", "http://user@example.com/", false},
		{"bad scheme", "ftp://example.com/", false},
		{"host mismatch", "https://other.example/", false},
		{"no origin", "", true},
		{"matching", "http://example.com/", true},
	} {
		if got := sameOrigin(mk(tc.origin)); got != tc.want {
			t.Fatalf("%s: sameOrigin=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestWave2ServerAdminCSRFTokenFromCookie(t *testing.T) {
	s := testGateway(t, config.Default())
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/csrf", nil)
	req.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: "pinned-token"})
	rr := httptest.NewRecorder()
	s.adminCSRFToken(rr, req)
	if !strings.Contains(rr.Body.String(), "pinned-token") {
		t.Fatalf("body=%s want pinned-token echoed", rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Set-Cookie"), "pinned-token") {
		t.Fatalf("Set-Cookie=%s want pinned-token", rr.Header().Get("Set-Cookie"))
	}
}

func TestWave2ServerGeminiVEProtocolMatch(t *testing.T) {
	s, _, _ := wave2GeminiVirtualServer(t, "", "", nil, []string{"gemini"})
	rr := geminiPost(t, s, "http://gateway/v1beta/models/nexa-private:generateContent", wave2GeminiBody(), "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (protocol match) body=%s", rr.Code, rr.Body.String())
	}
}
