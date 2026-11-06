package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// Agent D4 coverage: httpapi core/routes files. Every test asserts
// observable behavior (status, body, headers, returned values, state).
// Lines proven unreachable without a production change are reported to
// the parent instead of being forced here.

// ---------- metrics.go ----------

func TestD4MetricsProviderIncidentSortAndDecisionFallbacks(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	srv := testGateway(t, cfg)
	// Two provider states so the ProviderSnapshot sort closure runs.
	srv.hm.RecordProviderSuccess("pa")
	srv.hm.RecordProviderSuccess("pb")
	// No decision orchestrator: the latency/external fallbacks run.
	srv.decisionOrchestrator = nil
	rec := httptest.NewRecorder()
	srv.metrics(rec, httptest.NewRequest(http.MethodGet, "http://gateway/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`nexaroute_provider_incident_state{provider="pa",status="healthy"} 1`,
		`nexaroute_provider_incident_state{provider="pb",status="healthy"} 1`,
		"nexaroute_decision_latency_avg_ms 0",
		"nexaroute_decision_latency_count 0",
		`nexaroute_external_decision_latency_seconds{type="jev"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestD4ReadyNoUsableNonReadyStrategy(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{{
		ID: "ga", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream", Enabled: true, Weight: 1}},
	}}
	srv := testGateway(t, cfg)
	for _, d := range srv.rt.All() {
		srv.hm.ForceCooldown(d.ID, "d4-test", time.Hour)
	}
	rec := httptest.NewRecorder()
	srv.ready(rec, httptest.NewRequest(http.MethodGet, "http://gateway/ready", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no usable model deployments") {
		t.Fatalf("ready status=%d body=%s, want 503 no usable model deployments", rec.Code, rec.Body.String())
	}
}

// ---------- models.go ----------

func TestD4ModelsAuthDenied(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = []string{"secret-1"}
	srv := testGateway(t, cfg)
	rec := httptest.NewRecorder()
	srv.models(rec, httptest.NewRequest(http.MethodGet, "http://gateway/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("models without key status=%d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

func TestD4ModelsAutoAlreadySeen(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "ga", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream", Enabled: true, Weight: 1}},
	}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve-auto", PublicModel: "auto", RouteProfile: "rp1"},
		{ID: "ve-auto-2", PublicModel: "auto", RouteProfile: "rp1"},
	}
	srv := testGateway(t, cfg)
	rec := httptest.NewRecorder()
	srv.models(rec, httptest.NewRequest(http.MethodGet, "http://gateway/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.Count(rec.Body.String(), `"id":"auto"`); got != 1 {
		t.Fatalf("auto listed %d times, want exactly 1: %s", got, rec.Body.String())
	}
}

func TestD4ModelsEmptyModelIDSkipped(t *testing.T) {
	// New() validates the config and rejects an empty deployment
	// model, so the only way the empty-id branch can run is a router
	// built directly from such a config (router.New does not
	// validate). The models handler only needs rt/cfg, so a bare
	// server is a faithful harness for it.
	cfg := config.Config{
		Providers: []config.ProviderConfig{{
			ID: "ga", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "", Enabled: true, Weight: 1}},
		}},
	}
	hm := health.New(3, time.Minute)
	srv := &Server{cfg: cfg, rt: router.New(cfg, hm)}
	rec := httptest.NewRecorder()
	srv.models(rec, httptest.NewRequest(http.MethodGet, "http://gateway/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"ga/m1"`) {
		t.Fatalf("deployment id missing: %s", body)
	}
	if strings.Contains(body, `"id":""`) {
		t.Fatalf("empty model id leaked into listing: %s", body)
	}
}

// ---------- oidc.go ----------

func TestD4OIDCDiscoveryMetadataInvalid(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":"http://example.com/auth","token_endpoint":%q,"jwks_uri":%q}`,
			base, base+"/token", base+"/jwks")
	})
	discovery := httptest.NewServer(mux)
	t.Cleanup(discovery.Close)
	base = discovery.URL
	t.Setenv("D4_OIDC_SECRET", "test-secret")
	_, err := newOIDCAuthenticator(context.Background(), config.OIDCConfig{
		Enabled: true, IssuerURL: base, ClientID: "cid",
		ClientSecretEnv: "D4_OIDC_SECRET", RedirectURL: "https://app.example.com/callback",
		Audience: "aud", RoleClaim: "roles", RoleMappings: map[string]string{"admin": "admin"},
	})
	if err == nil || !strings.Contains(err.Error(), "discovery metadata is invalid") {
		t.Fatalf("newOIDCAuthenticator err=%v, want discovery metadata invalid", err)
	}
}

// ---------- openai.go ----------

func d4FailingUpstream(t *testing.T, delay time.Duration, status int, body string, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func TestD4OpenAIHedgeSkipsPartner(t *testing.T) {
	// Three candidates: the hedge launches against #2, so after the
	// hedge both legs fail the loop must skip #2 (already attempted as
	// the hedge leg) and try #3. With only two candidates max is
	// clamped to 2 and the loop breaks before ever reaching the
	// skipped index, which is why the two-candidate variant of this
	// scenario never executes the skip branch.
	var mu sync.Mutex
	slowCalls, fastCalls, thirdCalls := 0, 0, 0
	slow := d4FailingUpstream(t, 150*time.Millisecond, http.StatusInternalServerError, `{"error":{"message":"slow boom"}}`, &slowCalls, &mu)
	fast := d4FailingUpstream(t, 0, http.StatusInternalServerError, `{"error":{"message":"fast boom"}}`, &fastCalls, &mu)
	third := d4FailingUpstream(t, 0, http.StatusInternalServerError, `{"error":{"message":"third boom"}}`, &thirdCalls, &mu)
	t.Cleanup(slow.Close)
	t.Cleanup(fast.Close)
	t.Cleanup(third.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 10
	mkProvider := func(id string, url string, priority int) config.ProviderConfig {
		return config.ProviderConfig{ID: id, Type: "openai_compatible", BaseURL: url, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: priority,
				Capabilities: config.Capabilities{Streaming: true}}}}
	}
	cfg.Providers = []config.ProviderConfig{
		mkProvider("ga", slow.URL, 0),
		mkProvider("gb", fast.URL, 1),
		mkProvider("gc", third.URL, 2),
	}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("hedge-skip status=%d body=%s, want 503 all eligible upstream deployments failed", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if slowCalls != 1 || fastCalls != 1 || thirdCalls != 1 {
		t.Fatalf("upstream calls slow=%d fast=%d third=%d, want 1/1/1 (hedge leg once, partner skipped as loop attempt)", slowCalls, fastCalls, thirdCalls)
	}
	launched := false
	for _, ev := range srv.bus.SnapshotLimit(0) {
		if ev.Kind == "hedge_launch" {
			launched = true
		}
	}
	if !launched {
		t.Fatal("hedge never launched; skip branch was not exercised")
	}
}

func TestD4OpenAIRetryAfterTail(t *testing.T) {
	// Both candidates answer 429 with Retry-After and a body. The
	// last failure breaks out of the loop (shield terminal), so the
	// tail must surface the last Retry-After to the client.
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.RetryBackoffMS = 1
	cfg.Providers = []config.ProviderConfig{
		{ID: "ga", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
				Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "gb", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1,
				Capabilities: config.Capabilities{Streaming: true}}}},
	}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("429 tail status=%d body=%s, want 503 all eligible upstream deployments failed", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After header=%q, want %q", got, "1")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("upstream calls=%d, want 2 (both candidates attempted)", calls)
	}
}

// d4FailAfterWriter fails the Nth Write so a mid-stream write error can
// be observed at a precise emit site.
type d4FailAfterWriter struct {
	header http.Header
	writes int
	failAt int
	body   strings.Builder
}

func (w *d4FailAfterWriter) Header() http.Header { return w.header }
func (w *d4FailAfterWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, errors.New("d4 write boom")
	}
	return w.body.Write(p)
}
func (w *d4FailAfterWriter) WriteHeader(status int) {}
func (w *d4FailAfterWriter) Flush()                 {}

func TestD4AnthropicStreamUsageWriteFailure(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	// Writes: 1 role chunk, 2 final chunk, 3 usage chunk (fails).
	w := &d4FailAfterWriter{header: http.Header{}, failAt: 3}
	sinkCalled := false
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}
	err := streamAnthropicToOpenAIWithUsage(w, resp, "m", &translate.NameMap{}, func(u usage.Counts) { sinkCalled = true }, "d4-stream")
	if err == nil {
		t.Fatal("usage-chunk write failure should return an error")
	}
	if sinkCalled {
		t.Fatal("usage sink must not run after the usage chunk fails to write")
	}
	body := w.body.String()
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("final chunk missing from partial stream: %s", body)
	}
	if strings.Contains(body, `"prompt_tokens"`) {
		t.Fatalf("failed usage chunk must not appear in the stream: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("failed stream must not emit [DONE]: %s", body)
	}
}

// ---------- opencode_admin.go ----------

func TestD4AuditProviderMutationAnonymous(t *testing.T) {
	srv := testGateway(t, config.Default())
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/admin/api/providers/p1", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	srv.auditProviderMutation(req, "provider.update", "p1", []int{0, 2})
	records, err := srv.securityDB.AuditRecords(context.Background(), 5)
	if err != nil {
		t.Fatalf("AuditRecords: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("no audit records after provider mutation")
	}
	latest := records[0]
	if latest.Actor != "anonymous" {
		t.Fatalf("actor=%q, want anonymous for a request without admin identity", latest.Actor)
	}
	if latest.Target != "provider:p1 credentials:0,2" {
		t.Fatalf("target=%q, want provider:p1 credentials:0,2", latest.Target)
	}
	identified := withAdminIdentity(req, authz.Identity{Subject: "op-1"})
	srv.auditProviderMutation(identified, "provider.update", "p1", nil)
	records, err = srv.securityDB.AuditRecords(context.Background(), 5)
	if err != nil {
		t.Fatalf("AuditRecords: %v", err)
	}
	if records[0].Actor != "op-1" || records[0].Target != "provider:p1" {
		t.Fatalf("identified mutation actor=%q target=%q, want op-1 / provider:p1", records[0].Actor, records[0].Target)
	}
}

// ---------- privacy.go ----------

func TestD4PrivacyEmptyProfileID(t *testing.T) {
	cfg := config.Config{
		VirtualEndpoints: []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "pub", RouteProfile: ""}},
		RouteProfiles:    []config.RouteProfileConfig{{ID: "", CandidatePool: "pool1"}},
		CandidatePools:   []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}},
	}
	deps := []router.Deployment{{ID: "ga/m", ProviderID: "ga", Model: "m"}}
	srv := &Server{routeResolver: route.NewResolver(cfg, deps), cfg: cfg}
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
	if srv.requireNoTrainingForRequest(req, "pub") {
		t.Fatal("resolved route with an empty profile id must not require no_training")
	}
}

func TestD4PrivacyUnknownProfileID(t *testing.T) {
	// The endpoint resolves (its profile exists in the resolver's
	// config), but the server's config snapshot carries no profile
	// with that id, so the requirement lookup falls through to the
	// final false instead of assuming anything. Production reloads
	// keep resolver and config in sync, so the divergence is
	// constructed directly here to exercise the fail-closed branch.
	cfgWith := config.Config{
		VirtualEndpoints: []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "pub", RouteProfile: "rp-missing"}},
		RouteProfiles:    []config.RouteProfileConfig{{ID: "rp-missing", CandidatePool: "pool1"}},
		CandidatePools:   []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}},
	}
	cfgWithout := config.Config{
		VirtualEndpoints: []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "pub", RouteProfile: "rp-missing"}},
		RouteProfiles:    []config.RouteProfileConfig{{ID: "rp-other", CandidatePool: "pool1"}},
		CandidatePools:   []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}},
	}
	deps := []router.Deployment{{ID: "ga/m", ProviderID: "ga", Model: "m"}}
	srv := &Server{routeResolver: route.NewResolver(cfgWith, deps), cfg: cfgWithout}
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
	if srv.requireNoTrainingForRequest(req, "pub") {
		t.Fatal("an endpoint whose profile id matches nothing in the config must not require no_training")
	}
}

func TestD4PrivacyExcludedTotalResolveError(t *testing.T) {
	srv := testGateway(t, config.Default())
	disabled := false
	cfg := config.Config{
		VirtualEndpoints: []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "pub", RouteProfile: "rp1", Enabled: &disabled}},
		RouteProfiles:    []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}},
		CandidatePools:   []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}},
	}
	srv.runtimeMu.Lock()
	srv.routeResolver = route.NewResolver(cfg, srv.rt.All())
	srv.runtimeMu.Unlock()
	if got := srv.privacyExcludedTotal(router.Requirement{Model: "pub", RequireNoTraining: true}, "openai", nil); got != 0 {
		t.Fatalf("privacyExcludedTotal with a disabled endpoint=%d, want 0", got)
	}
}

func TestD4PrivacyExcludedTotalNoneExcluded(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "ga", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		DataHandling: config.DataHandlingConfig{TrainsOnData: "no"},
		Models:       []config.ModelConfig{{ID: "m", Model: "upstream", Enabled: true, Weight: 1}},
	}}
	srv := testGateway(t, cfg)
	if got := srv.privacyExcludedTotal(router.Requirement{Model: "upstream", RequireNoTraining: true}, "openai", nil); got != 0 {
		t.Fatalf("privacyExcludedTotal with an all-no-training relaxed set=%d, want 0", got)
	}
}

// ---------- request_activity.go ----------

func TestD4RequestActivityEmptyIDsAndEviction(t *testing.T) {
	srv := testGateway(t, config.Default())
	before := len(srv.bus.SnapshotLimit(0))
	req := httptest.NewRequest(http.MethodGet, "http://gateway/x", nil)
	srv.emitRequestAccepted(req, "openai", "m", false)
	srv.emitRouteResolved(req, "openai", nil, nil)
	srv.emitStreamCommitted("", "ga/m", "ga", "openai", time.Millisecond)
	srv.emitRequestFinished("", 200, time.Millisecond, "succeeded")
	if got := len(srv.bus.SnapshotLimit(0)); got != before {
		t.Fatalf("empty request ids emitted %d bus events, want 0", got-before)
	}

	s := &Server{}
	for i := 0; i <= maxAttemptSeqRequests; i++ {
		s.recordAttemptSeq(fmt.Sprintf("r-%d", i), "ga/m")
	}
	if got := len(s.attemptSeq); got != maxAttemptSeqRequests {
		t.Fatalf("attempt ledger size=%d, want %d (bounded)", got, maxAttemptSeqRequests)
	}
	if got := s.attemptsHeaderValue("r-0"); got != "" {
		t.Fatalf("evicted request still has attempts %q, want empty", got)
	}
	if got := s.attemptsHeaderValue(fmt.Sprintf("r-%d", maxAttemptSeqRequests)); got != "ga/m" {
		t.Fatalf("newest request attempts=%q, want ga/m", got)
	}
}

func TestD4AdminActivityRequests(t *testing.T) {
	srv := testGateway(t, config.Default())
	rec := httptest.NewRecorder()
	srv.adminActivityRequests(rec, httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/activity/requests", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST activity status=%d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.adminActivityRequests(rec, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/activity/requests?window=60", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET activity status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("activity body is not JSON: %v", err)
	}
	if resp["stale_after_seconds"] != float64(60) {
		t.Fatalf("stale_after_seconds=%v, want 60", resp["stale_after_seconds"])
	}
	if !strings.Contains(rec.Body.String(), `"requests":[]`) {
		t.Fatalf("empty activity feed must render an empty array, got %s", rec.Body.String())
	}
}

// ---------- route_eligibility.go ----------

func d4EligibilityServer(t *testing.T, hm *health.Manager) (*Server, config.Config, []router.Deployment) {
	t.Helper()
	cfg := config.Config{
		Routing: config.RoutingConfig{Strategy: "ready_mesh"},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "rp1", CandidatePool: "pool1"},
		},
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "pool1", Deployments: []string{"d1", "d2"}},
		},
	}
	deps := []router.Deployment{
		{ID: "d1", ProviderID: "p1", Model: "m1"},
		{ID: "d2", ProviderID: "p1", Model: "m2"},
	}
	srv := &Server{hm: hm}
	return srv, cfg, deps
}

func d4EligibilityRow(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	profiles, ok := out["profiles"].([]map[string]any)
	if !ok || len(profiles) != 1 {
		t.Fatalf("eligibility profiles=%#v, want one row", out["profiles"])
	}
	return profiles[0]
}

func TestD4RouteEligibilityExclusions(t *testing.T) {
	// Unknown pool member: the resolver was built with d1+d2 but the
	// snapshot only lists d1, so d2 is skipped without counting.
	srv, cfg, deps := d4EligibilityServer(t, health.New(3, time.Minute))
	resolver := route.NewResolver(cfg, deps)
	row := d4EligibilityRow(t, srv.routeEligibilityView(cfg, resolver, deps[:1], nil))
	if row["candidates"] != 2 {
		t.Fatalf("candidates=%v, want 2 pool members", row["candidates"])
	}
	if row["eligible_now"] != 0 {
		t.Fatalf("eligible_now=%v, want 0 (d1 health unknown under ready_mesh)", row["eligible_now"])
	}

	// Provider unavailable: three distinct-deployment failures trip
	// the provider circuit (threshold 3) into cooldown.
	gw := testGateway(t, config.Default())
	gw.hm.RecordProviderFailure("p1", "d1", "boom")
	gw.hm.RecordProviderFailure("p1", "d2", "boom")
	gw.hm.RecordProviderFailure("p1", "d3", "boom")
	if gw.hm.ProviderAvailable("p1") {
		t.Fatal("provider p1 should be cooling down after 3 failures")
	}
	row = d4EligibilityRow(t, gw.routeEligibilityView(cfg, resolver, deps, nil))
	excluded := row["excluded"].(map[string]int)
	if excluded["provider_unavailable"] != 2 {
		t.Fatalf("provider_unavailable=%d, want 2", excluded["provider_unavailable"])
	}

	// Retired deployment keeps its own bucket.
	srv2, _, _ := d4EligibilityServer(t, health.New(3, time.Minute))
	row = d4EligibilityRow(t, srv2.routeEligibilityView(cfg, resolver, deps,
		[]health.State{{Deployment: "d1", Status: health.Retired}}))
	excluded = row["excluded"].(map[string]int)
	if excluded["retired"] != 1 {
		t.Fatalf("retired=%d, want 1", excluded["retired"])
	}

	// Non-healthy (non-unknown) under a ready strategy.
	row = d4EligibilityRow(t, srv2.routeEligibilityView(cfg, resolver, deps,
		[]health.State{{Deployment: "d1", Status: health.Degraded}}))
	excluded = row["excluded"].(map[string]int)
	if excluded["not_healthy"] != 1 {
		t.Fatalf("not_healthy=%d, want 1", excluded["not_healthy"])
	}
}

// ---------- routing_helpers.go ----------

type d4BlockingReadCloser struct {
	unblock chan struct{}
	once    sync.Once
}

func (r *d4BlockingReadCloser) Read(p []byte) (int, error) {
	<-r.unblock
	return 0, io.EOF
}
func (r *d4BlockingReadCloser) Close() error {
	r.once.Do(func() { close(r.unblock) })
	return nil
}

func TestD4RoutingHelpers(t *testing.T) {
	// readResponseBody with a nil context falls back to Background and
	// returns the body.
	if got, err := readResponseBody(nil, io.NopCloser(strings.NewReader("hello")), 1024); err != nil || string(got) != "hello" {
		t.Fatalf("readResponseBody(nil ctx)=%q err=%v, want hello/nil", got, err)
	}

	// readResponseBody aborts promptly on context cancellation.
	cctx, ccancel := context.WithCancel(context.Background())
	rc := &d4BlockingReadCloser{unblock: make(chan struct{})}
	done := make(chan struct{})
	var out []byte
	var rerr error
	go func() {
		out, rerr = readResponseBody(cctx, rc, 1024)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	ccancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readResponseBody did not abort on context cancel")
	}
	if rerr != context.Canceled {
		t.Fatalf("readResponseBody err=%v, want context.Canceled", rerr)
	}
	if out != nil {
		t.Fatalf("readResponseBody body=%q, want nil on cancel", out)
	}

	// jitteredRetryBackoff: a non-positive base returns immediately,
	// and an attempt above 6 is clamped to 6.
	if got := jitteredRetryBackoff(0, 1, "d4-req"); got != 0 {
		t.Fatalf("jitteredRetryBackoff(0,1)=%v, want 0", got)
	}
	if d := jitteredRetryBackoff(time.Second, 10, "d4-req"); d < 0 || d > 5*time.Second {
		t.Fatalf("jitteredRetryBackoff(1s,10)=%v, want within [0,5s] (attempt clamped to 6)", d)
	}

	// jitteredRetryBackoff caps the doubling at 5s.
	d := jitteredRetryBackoff(3*time.Second, 1, "d4-req")
	if d < 0 || d > 5*time.Second {
		t.Fatalf("jitteredRetryBackoff(3s,1)=%v, want within [0,5s]", d)
	}
	if d2 := jitteredRetryBackoff(3*time.Second, 1, "d4-req"); d2 != d {
		t.Fatalf("jitteredRetryBackoff not deterministic: %v vs %v", d, d2)
	}

	// patchJSONModel rejects invalid JSON, and rejects a model id
	// that is not valid UTF-8.
	if _, err := patchJSONModel([]byte(`{bad json`), "m"); err == nil {
		t.Fatal("patchJSONModel accepted invalid JSON")
	}
	if _, err := patchJSONModel([]byte(`{}`), "bad\xffmodel"); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("patchJSONModel invalid-UTF-8 model err=%v, want not valid UTF-8", err)
	}

	// retryAfterDuration: an overflow-range value with no cap falls
	// back to the 30s default instead of overflowing.
	h := http.Header{}
	h.Set("Retry-After", "10000000000")
	if got := retryAfterDuration(h, 0); got != 30*time.Second {
		t.Fatalf("retryAfterDuration huge sec, max=0: %v, want 30s", got)
	}
	if got := retryAfterDuration(h, time.Hour); got != time.Hour {
		t.Fatalf("retryAfterDuration huge sec, max=1h: %v, want 1h", got)
	}

	// bodySessionKey with no session anywhere: an empty body and a
	// body with an (empty) metadata object both fall through to "".
	if got := bodySessionKey(map[string]any{}); got != "" {
		t.Fatalf("bodySessionKey empty=%q, want empty", got)
	}
	if got := bodySessionKey(map[string]any{"metadata": map[string]any{}}); got != "" {
		t.Fatalf("bodySessionKey empty-metadata=%q, want empty", got)
	}

	// clientScopeForSession: a non-virtual caller falls back to tenant.
	req := httptest.NewRequest(http.MethodGet, "http://gateway/x", nil)
	req = req.WithContext(contextWithClientIdentity(req.Context(), clientIdentity{TenantID: "tenant-9"}))
	if got := clientScopeForSession(req); got != "tenant-9" {
		t.Fatalf("clientScopeForSession=%q, want tenant-9", got)
	}

	// quotaRemainingPressure bounds: a negative remaining is clamped
	// by the input guard, and zero remaining is saturated.
	if got := quotaRemainingPressure(-1, 100); got != 0 {
		t.Fatalf("quotaRemainingPressure(-1,100)=%v, want 0", got)
	}
	if got := quotaRemainingPressure(0, 100); got != 4 {
		t.Fatalf("quotaRemainingPressure(0,100)=%v, want 4", got)
	}

	// providerLoadFromStats: token pressure wins over request pressure.
	now := time.Now().Unix()
	st := providers.ProviderStats{
		RequestResetUnix: now + 3600, TokenResetUnix: now + 3600,
		RequestLimit: 100, RemainingRequests: 100, EffectiveRemainingRequests: 100,
		TokenLimit: 100, RemainingTokens: 10, EffectiveRemainingTokens: 10,
	}
	load := providerLoadFromStats(st, now)
	if math.Abs(load.QuotaPressure-2.4) > 1e-9 {
		t.Fatalf("quotaPressure=%v, want 2.4 (token pressure)", load.QuotaPressure)
	}
	if load.QuotaExhausted {
		t.Fatal("quota must not be exhausted while tokens remain")
	}

	// prepareRequirement: an unknown provider yields an empty load.
	srv := testGateway(t, config.Default())
	prepared := srv.prepareRequirement(router.Requirement{}, httptest.NewRequest(http.MethodGet, "http://gateway/x", nil), "")
	if got := prepared.LoadForProvider("d4-no-such-provider"); got != (router.ProviderLoad{}) {
		t.Fatalf("LoadForProvider unknown=%+v, want empty", got)
	}

	// observeTaskFeedback with no evaluation plane is a silent no-op.
	(&Server{}).observeTaskFeedback("coding", "ga/m", true, time.Second)
}
