package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// Wave-3 coverage tests: handlers, metrics, models, admin event stream,
// video force-fail, and count_tokens guards. All helpers are wave3-prefixed
// so they never collide with existing test helpers.

func wave3Request(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func wave3AdminRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:29371"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func wave3JSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("status=%d invalid JSON: %v; body=%s", rr.Code, err, rr.Body.String())
	}
	return out
}

func wave3BusHasKind(s *Server, kind string) bool {
	for _, e := range s.bus.Snapshot() {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func wave3BusHasKindDeployment(s *Server, kind, deployment string) bool {
	for _, e := range s.bus.Snapshot() {
		if e.Kind == kind && e.Deployment == deployment {
			return true
		}
	}
	return false
}

// wave3FailWriter fails writes after the first n successful writes and
// signals each write on w.wrote, so tests can synchronize without sleeps.
type wave3FailWriter struct {
	header  http.Header
	body    strings.Builder
	failAt  int
	writes  int
	wrote   chan struct{}
	flushed chan struct{}
}

func newWave3FailWriter(failAt int) *wave3FailWriter {
	return &wave3FailWriter{
		header:  http.Header{},
		failAt:  failAt,
		wrote:   make(chan struct{}, 64),
		flushed: make(chan struct{}, 64),
	}
}

func (w *wave3FailWriter) Header() http.Header { return w.header }

func (w *wave3FailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAt {
		return 0, errWave3WriteFail
	}
	w.body.Write(p)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *wave3FailWriter) WriteHeader(int) {}

func (w *wave3FailWriter) Flush() {
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

type wave3Error string

func (e wave3Error) Error() string { return string(e) }

const errWave3WriteFail wave3Error = "wave3 injected write failure"

func TestWave3ModelsMethodAndDedup(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "upstream-model", Aliases: []string{"client-model", "upstream-model"},
			Enabled: true, Weight: 1,
		}},
	}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{
		ID: "ve-one", PublicModel: "ve-model", RouteProfile: "rp",
		Enabled: func() *bool { b := true; return &b }(),
	}}
	cfg.VirtualEndpoints = append(cfg.VirtualEndpoints, config.VirtualEndpointConfig{
		ID: "ve-two", PublicModel: "ve-disabled", RouteProfile: "rp",
		Enabled: func() *bool { b := false; return &b }(),
	})
	cfg.RouteProfiles = []config.RouteProfileConfig{{
		ID:            "rp",
		CandidatePool: "pool",
	}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
	s := testGateway(t, cfg)

	if rr := wave3Request(s, "POST", "/v1/models", ""); rr.Code != 405 {
		t.Fatalf("POST /v1/models status=%d want 405 body=%s", rr.Code, rr.Body.String())
	}
	rr := wave3Request(s, "GET", "/v1/models", "")
	if rr.Code != 200 {
		t.Fatalf("GET /v1/models status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := wave3JSON(t, rr)
	data, _ := out["data"].([]any)
	seen := map[string]int{}
	for _, item := range data {
		m, _ := item.(map[string]any)
		id, _ := m["id"].(string)
		seen[id]++
	}
	// Virtual endpoint first, enabled only; auto + claude-auto appear once
	// deployments exist; deployment id and its aliases are deduplicated.
	for _, want := range []string{"ve-model", "auto", "claude-auto", "a/m", "upstream-model", "client-model"} {
		if seen[want] != 1 {
			t.Fatalf("model %q appears %d times, want 1; full list=%v", want, seen[want], seen)
		}
	}
	if seen["ve-disabled"] != 0 {
		t.Fatalf("disabled virtual endpoint leaked into model list: %v", seen)
	}
}

func TestWave3ModelsLegacyPublicModel(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.PublicModel = "legacy-model"
	s := testGateway(t, cfg)
	s.routeResolver = nil
	rr := wave3Request(s, "GET", "/v1/models", "")
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := wave3JSON(t, rr)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("legacy model list len=%d want 1 body=%s", len(data), rr.Body.String())
	}
	m, _ := data[0].(map[string]any)
	if m["id"] != "legacy-model" {
		t.Fatalf("legacy model id=%v want legacy-model", m["id"])
	}
}

func TestWave3MetricsSeries(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	s.hm.RecordTTFT("a/m", 5*time.Millisecond)
	s.hm.RecordProviderFailure("a", "a/m", "boom")
	s.mcpStatsMu.Lock()
	if s.mcpStats == nil {
		s.mcpStats = map[string]int64{}
	}
	s.mcpStats["srv|ok"] = 3
	s.mcpStatsMu.Unlock()
	s.taskMu.Lock()
	if s.taskClassCounts == nil {
		s.taskClassCounts = map[string]uint64{}
	}
	s.taskClassCounts["coding|simple"] = 2
	s.taskClassCounts["bogus-no-separator"] = 7
	s.taskMu.Unlock()

	if rr := wave3Request(s, "POST", "/metrics", ""); rr.Code != 405 {
		t.Fatalf("POST /metrics status=%d want 405", rr.Code)
	}
	rr := wave3Request(s, "GET", "/metrics", "")
	if rr.Code != 200 {
		t.Fatalf("GET /metrics status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`nexaroute_deployment_ttft_ms_bucket{deployment="a/m"`,
		`nexaroute_provider_incident_state{provider="a"`,
		`nexaroute_provider_incident_evidence{provider="a"} 1`,
		`nexaroute_mcp_calls_total{server="srv",result="ok"} 3`,
		`nexaroute_task_classifications_total{task_type="coding",complexity="simple"} 2`,
		`nexaroute_activity_attempts{outcome="started"} 0`,
		`nexaroute_decision_latency_avg_ms 0`,
		`nexaroute_decision_latency_count 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "bogus-no-separator") {
		t.Fatalf("malformed task classification key leaked into metrics")
	}
}

func TestWave3ReadyGuards(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	if rr := wave3Request(s, "POST", "/readyz", ""); rr.Code != 405 {
		t.Fatalf("POST /readyz status=%d want 405", rr.Code)
	}
	rr := wave3Request(s, "GET", "/readyz", "")
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "no enabled model deployments") {
		t.Fatalf("empty /readyz status=%d body=%s want 503 no enabled model deployments", rr.Code, rr.Body.String())
	}

	cfg2 := config.Default()
	cfg2.Probe.Enabled = false
	cfg2.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1}},
	}}
	s2 := testGateway(t, cfg2)
	rr = wave3Request(s2, "GET", "/readyz", "")
	if rr.Code != 200 {
		t.Fatalf("healthy /readyz status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	out := wave3JSON(t, rr)
	if out["ok"] != true || out["deployments"] != float64(1) {
		t.Fatalf("healthy /readyz body=%s want ok=true deployments=1", rr.Body.String())
	}
}

func TestWave3AdminEventStreamParams(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/admin/api/events/stream?limit=abc", "limit"},
		{"/admin/api/events/stream?limit=0", "limit"},
		{"/admin/api/events/stream?limit=257", "limit"},
		{"/admin/api/events/stream?since=abc", "since"},
	} {
		rr := wave3AdminRequest(s, "GET", tc.path, "")
		if rr.Code != 400 || !strings.Contains(rr.Body.String(), tc.want) {
			t.Fatalf("GET %s status=%d body=%s want 400 mentioning %q", tc.path, rr.Code, rr.Body.String(), tc.want)
		}
	}
	// Last-Event-ID parse error surfaces as 400 as well.
	req := httptest.NewRequest("GET", "http://127.0.0.1/admin/api/events/stream", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:29371"
	req.Header.Set("Last-Event-ID", "not-a-number")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("bad Last-Event-ID status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
}

func TestWave3AdminEventStreamCapacity(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	var cancels []func()
	for i := 0; i < 64; i++ {
		_, _, cancel, ok := s.bus.SubscribeSnapshotSince(0, 64)
		if !ok {
			t.Fatalf("subscription %d unexpectedly refused", i)
		}
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	rr := wave3AdminRequest(s, "GET", "/admin/api/events/stream", "")
	if rr.Code != 503 {
		t.Fatalf("over-capacity stream status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
}

func TestWave3AdminEventStreamSnapshotEmitFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	s.bus.Add(events.Event{RequestID: "wave3-snap", Kind: "route_ok", Message: "snapshot marker"})
	w := newWave3FailWriter(1) // ": connected" succeeds, snapshot emit fails.
	req := httptest.NewRequest("GET", "http://127.0.0.1/admin/api/events/stream", nil)
	s.adminEventStream(w, req)
	if !strings.Contains(w.body.String(), ": connected") {
		t.Fatalf("snapshot-failure body=%q want : connected prefix", w.body.String())
	}
	if strings.Contains(w.body.String(), "snapshot marker") {
		t.Fatalf("snapshot event leaked despite write failure: %q", w.body.String())
	}
}

func TestWave3AdminEventStreamLiveEmitFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	w := newWave3FailWriter(1) // ": connected" succeeds, live emit fails.
	req := httptest.NewRequest("GET", "http://127.0.0.1/admin/api/events/stream", nil)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.adminEventStream(w, req)
		close(done)
	}()
	// The first write happens after subscription, so it proves the handler
	// is live and waiting; then a live event trips the failing writer.
	<-w.wrote
	s.bus.Add(events.Event{RequestID: "wave3-live", Kind: "route_ok", Message: "live marker"})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("live-emit-failure handler did not return")
	}
	wg.Wait()
	if strings.Contains(w.body.String(), "live marker") {
		t.Fatalf("live event leaked despite write failure: %q", w.body.String())
	}
}

func TestWave3AdminEventStreamDrainingReady(t *testing.T) {
	// Draining is a ready() branch, grouped here with the handler guards.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	s.draining.Store(true)
	rr := wave3Request(s, "GET", "/readyz", "")
	if rr.Code != 503 || rr.Header().Get("Retry-After") != "5" || !strings.Contains(rr.Body.String(), "gateway is draining") {
		t.Fatalf("draining /readyz status=%d retry=%q body=%s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
}

// Video force-fail stub: implements http.Handler (the videoHandler field
// type) plus the ForceFail control used by adminVideoForceFail.
type wave3VideoStub struct {
	attempted, confirmed bool
	err                  error
	gotID                string
}

func (s *wave3VideoStub) ServeHTTP(http.ResponseWriter, *http.Request) {}

func (s *wave3VideoStub) ForceFail(_ context.Context, id string) (bool, bool, error) {
	s.gotID = id
	return s.attempted, s.confirmed, s.err
}

func TestWave3AdminVideoForceFail(t *testing.T) {
	// Method guard: a routed GET is denied 403 by the admin-authz
	// layer before the handler runs (same rule as provider-test).
	{
		s := wave3Gateway(t, nil, nil)
		rr := wave3AdminRequest(s, "GET", "/admin/api/video/jobs/job1/force-fail", "")
		if rr.Code != 403 {
			t.Fatalf("GET force-fail status=%d want 403", rr.Code)
		}
	}
	// The handler's own method check, reached by direct call.
	{
		s := wave3Gateway(t, nil, nil)
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/video/jobs/job1/force-fail", nil)
		rr := httptest.NewRecorder()
		s.adminVideoForceFail(rr, req)
		if rr.Code != 405 || rr.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("direct GET force-fail status=%d allow=%q want 405/POST", rr.Code, rr.Header().Get("Allow"))
		}
	}
	// Invalid job ids via direct handler call: the mux never yields
	// an empty, padded, or slash-containing id segment.
	for _, id := range []string{"", " job1 ", "a/b", `a\b`} {
		s := wave3Gateway(t, nil, nil)
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/video/jobs/x/force-fail", strings.NewReader(`{"confirm_job_id":"x"}`))
		req.SetPathValue("id", id)
		rr := httptest.NewRecorder()
		s.adminVideoForceFail(rr, req)
		if rr.Code != 400 {
			t.Fatalf("id %q status=%d want 400", id, rr.Code)
		}
	}
	// Body-shape guards (routed, no handler attached: body checks run first).
	for name, body := range map[string]string{
		"mismatch": `{"confirm_job_id":"other"}`,
		"trailing": `{"confirm_job_id":"job1"} {"x":1}`,
		"unknown":  `{"confirm_job_id":"job1","zzz":1}`,
		"bad-json": `{bad`,
		"empty":    ``,
	} {
		s := wave3Gateway(t, nil, nil)
		rr := wave3AdminRequest(s, "POST", "/admin/api/video/jobs/job1/force-fail", body)
		if rr.Code != 400 {
			t.Fatalf("%s body status=%d want 400", name, rr.Code)
		}
	}
	// No handler attached: valid body reaches the 503 unavailable branch.
	{
		s := wave3Gateway(t, nil, nil)
		rr := wave3AdminRequest(s, "POST", "/admin/api/video/jobs/job1/force-fail", `{"confirm_job_id":"job1"}`)
		if rr.Code != 503 {
			t.Fatalf("nil handler status=%d want 503", rr.Code)
		}
	}
	// ForceFail error mapping.
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not-found":  {queue.ErrNotFound, 404},
		"not-active": {orchestrator.ErrForceFailNotActive, 409},
		"terminal":   {queue.ErrTerminalJob, 409},
		"other":      {errWave3WriteFail, 503},
	} {
		stub := &wave3VideoStub{err: tc.err}
		s := wave3Gateway(t, nil, nil)
		s.AttachVideoHandler(stub)
		rr := wave3AdminRequest(s, "POST", "/admin/api/video/jobs/job1/force-fail", `{"confirm_job_id":"job1"}`)
		if rr.Code != tc.want {
			t.Fatalf("%s status=%d want %d", name, rr.Code, tc.want)
		}
		if stub.gotID != "job1" {
			t.Fatalf("%s stub got id %q want job1", name, stub.gotID)
		}
	}
	// Success message variants.
	for name, tc := range map[string]struct {
		attempted, confirmed bool
		wantMsg              string
	}{
		"both":      {true, true, "acknowledged cancellation"},
		"attempted": {true, false, "not confirmed"},
		"neither":   {false, false, "could be attempted"},
	} {
		stub := &wave3VideoStub{attempted: tc.attempted, confirmed: tc.confirmed}
		s := wave3Gateway(t, nil, nil)
		s.AttachVideoHandler(stub)
		rr := wave3AdminRequest(s, "POST", "/admin/api/video/jobs/job1/force-fail", `{"confirm_job_id":"job1"}`)
		if rr.Code != 200 {
			t.Fatalf("%s status=%d want 200 body=%s", name, rr.Code, rr.Body.String())
		}
		out := wave3JSON(t, rr)
		if out["job_id"] != "job1" || out["state"] != "failed" || out["operator_force_failed"] != true ||
			out["provider_cancellation_attempted"] != tc.attempted ||
			out["provider_cancellation_confirmed"] != tc.confirmed {
			t.Fatalf("%s fields=%v", name, out)
		}
		msg, _ := out["message"].(string)
		if !strings.Contains(msg, tc.wantMsg) {
			t.Fatalf("%s message=%q want substring %q", name, msg, tc.wantMsg)
		}
	}
}
