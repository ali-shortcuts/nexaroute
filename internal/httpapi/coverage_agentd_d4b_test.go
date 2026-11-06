package httpapi

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
	"github.com/ali-shortcuts/nexaroute/internal/telemetry"
)

// ---------- server.go ----------

func TestD4MutateConfigPrepareError(t *testing.T) {
	// A bedrock provider with a NAMED but UNSET secret env var passes
	// config validation (the name is well-formed) yet fails adapter
	// construction in Registry.Prepare, which applyConfigLocked must
	// surface instead of persisting a broken config.
	srv := testGateway(t, config.Default())
	t.Setenv("D4_BEDROCK_SECRET", "")
	before := len(srv.configHistory)
	_, err := srv.mutateConfig(func(cfg *config.Config) error {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "d4-bedrock", Type: "bedrock", BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
			AuthMode: "none", Enabled: true, APIKey: "d4-test-access-key", SecretKeyEnv: "D4_BEDROCK_SECRET",
			Models: []config.ModelConfig{{ID: "m", Model: "amazon.nova-micro-v1:0", Enabled: true, Weight: 1}},
		})
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "bedrock secret") {
		t.Fatalf("mutateConfig err=%v, want bedrock secret failure", err)
	}
	if got := len(srv.configHistory); got != before {
		t.Fatalf("failed mutation left history length %d, want %d", got, before)
	}
	if got := srv.currentConfig(); len(got.Providers) != 0 {
		t.Fatalf("failed mutation changed providers to %#v", got.Providers)
	}
}

func TestD4CandidatesForRequirementResolverRace(t *testing.T) {
	// candidatesForRequirement snapshots the resolver, then
	// resolveVirtualEndpoint re-reads it. If a reload installs a
	// resolver between the two reads, the virtual branch falls through
	// to the unfiltered return (server.go:989). That interleaving is
	// inherently racy: the installer must win the gap between the
	// snapshot read and the re-read. Sleep-based RWMutex queueing
	// alone proved unreliable under -race on a loaded host (a
	// standalone replica of the single-attempt choreography failed
	// 7/20 under -race, 0/20 without it), and removing the sleeps
	// makes it deterministically installer-first (runnext ordering).
	// The residual race is structural: the installer announces
	// itself as a pending writer only after acquiring rw.w, which
	// happens only once the test releases the write lock — the same
	// release that wakes the caller — so the installer's announce
	// races the caller's second RLock. So this test retries the race
	// until the interleaving is OBSERVED, and it proves the
	// nil-snapshot branch ran: only that path returns the unfiltered
	// candidate set, which contains gb/m (pool-a holds only ga/m, so
	// the installer-first path returns just ga/m). An attempt that
	// observes any other outcome is retried, not failed; only 30
	// consecutive misses fail the test.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{
		{ID: "ga", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0}}},
		{ID: "gb", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1}}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool-a"}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool-a", Deployments: []string{"ga/m"}}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "pub", RouteProfile: "rp1"}}
	srv := testGateway(t, cfg)
	realResolver := route.NewResolver(cfg, srv.rt.All())

	type result struct {
		cands    []router.Scored
		resolved *route.ResolvedRoute
		err      error
	}
	// Only the nil-snapshot path returns the unfiltered set (ga/m and
	// gb/m); the installer-first path is pool-filtered to ga/m alone.
	observed := func(res result) bool {
		if res.resolved == nil || res.resolved.VirtualEndpointID != "ve1" || len(res.cands) != 2 {
			return false
		}
		ids := map[string]bool{}
		for _, c := range res.cands {
			ids[c.Deployment.ID] = true
		}
		return ids["ga/m"] && ids["gb/m"]
	}
	var last result
	for attempt := 0; attempt < 30; attempt++ {
		srv.runtimeMu.Lock()
		srv.routeResolver = nil
		srv.runtimeMu.Unlock()

		resCh := make(chan result, 1)
		srv.runtimeMu.Lock() // caller blocks on its first RLock
		go func() {
			_, cands, resolved, err := srv.candidatesForRequirement(router.Requirement{Model: "pub"}, "openai")
			resCh <- result{cands: cands, resolved: resolved, err: err}
		}()
		// Let the caller queue as a reader before the installer queues
		// as a writer: a blocked writer excludes new readers
		// (documented RWMutex semantics), so once the installer is
		// queued it wins the gap between the snapshot and the re-read.
		// The sleeps are load-bearing scheduling windows, not
		// assertions: without them the installer (spawned last) wins
		// runnext and installs before the caller reads, so the
		// attempt observes the installer-first outcome and retries.
		time.Sleep(200 * time.Millisecond)
		wDone := make(chan struct{})
		go func() {
			srv.runtimeMu.Lock() // installer queues as a writer behind the reader
			srv.routeResolver = realResolver
			srv.runtimeMu.Unlock()
			close(wDone)
		}()
		time.Sleep(200 * time.Millisecond) // installer is queued
		srv.runtimeMu.Unlock()             // reader reads nil, releases; writer installs; reader resolves virtual
		<-wDone
		last = <-resCh
		if last.err != nil {
			t.Fatalf("candidatesForRequirement err=%v", last.err)
		}
		if observed(last) {
			return // nil-snapshot branch observed (server.go:989)
		}
	}
	t.Fatalf("resolver race never produced the nil-snapshot interleaving in 30 attempts; last resolved=%+v cands=%d", last.resolved, len(last.cands))
}

func TestD4AdminAuditCompletionUnavailable(t *testing.T) {
	srv := testGateway(t, config.Default())
	var logBuf bytes.Buffer
	srv.log = log.New(&logBuf, "", 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/settings", strings.NewReader(`{}`))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:23456"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.middleware(next).ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("admin middleware never reached the next handler (pre-operation audit must have succeeded first)")
	}
	// The pre-operation audit succeeded; closing the store now makes
	// only the completion audit fail.
	if err := srv.CloseSecurityStore(); err != nil {
		t.Fatalf("CloseSecurityStore: %v", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("admin middleware did not finish")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200: a completion-audit failure must not fail the request", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "admin_audit_completion_unavailable=true") {
		t.Fatalf("log missing completion-unavailable line: %q", logBuf.String())
	}
}

// ---------- settings.go ----------

func TestD4AdminSettingsPreconditionAndValidation(t *testing.T) {
	srv := testGateway(t, config.Default())
	// Stale If-Match: the stored revision is 1.
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/admin/api/settings", strings.NewReader(`{}`))
	req.Header.Set("If-Match", "999")
	rec := httptest.NewRecorder()
	srv.adminSettings(rec, req)
	if rec.Code != http.StatusPreconditionFailed || !strings.Contains(rec.Body.String(), "config revision changed") {
		t.Fatalf("stale If-Match status=%d body=%s, want 412 config revision changed", rec.Code, rec.Body.String())
	}
	// A client_base_url that fails validation surfaces as 400.
	req = httptest.NewRequest(http.MethodPut, "http://127.0.0.1/admin/api/settings", strings.NewReader(`{"client_base_url":"ftp://example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.adminSettings(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "client_base_url") {
		t.Fatalf("bad client_base_url status=%d body=%s, want 400 mentioning client_base_url", rec.Code, rec.Body.String())
	}
}

// ---------- sse.go ----------

func TestD4SSEEventLimit(t *testing.T) {
	var sb strings.Builder
	line := "event: " + strings.Repeat("x", 1<<20) + "\n"
	for i := 0; i < 9; i++ {
		sb.WriteString(line)
	}
	sb.WriteString("\n")
	r := newSSEReader(strings.NewReader(sb.String()))
	_, _, err := r.Next()
	if err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("oversized event err=%v, want sse event exceeds safe limit", err)
	}
}

// ---------- task_intelligence.go ----------

func TestD4TaskEvalGateSatisfied(t *testing.T) {
	// No evaluation plane at all.
	if (&Server{}).taskEvalGateSatisfied("pool1", "suite1") {
		t.Fatal("gate must fail closed with no evaluation plane")
	}
	// Plane present but no resolver.
	plane := newEvaluationPlane(config.EvaluationConfig{Enabled: true}, nil)
	if (&Server{evaluation: plane}).taskEvalGateSatisfied("pool1", "suite1") {
		t.Fatal("gate must fail closed with no route resolver")
	}
	// Plane and resolver present but the pool does not expand.
	srv := testGateway(t, config.Default())
	if srv.taskEvalGateSatisfied("d4-no-such-pool", "suite1") {
		t.Fatal("gate must fail closed for an unknown pool")
	}
}

func TestD4RecordTaskClassificationNilMap(t *testing.T) {
	s := &Server{}
	s.recordTaskClassification(taskprofile.TaskType("coding"), taskprofile.Complexity("medium"))
	if got := s.taskClassCounts["coding|medium"]; got != 1 {
		t.Fatalf("taskClassCounts[coding|medium]=%d, want 1", got)
	}
	if got := s.taskAnalysisTotal.Load(); got != 1 {
		t.Fatalf("taskAnalysisTotal=%d, want 1", got)
	}
}

// ---------- telemetry.go ----------

func TestD4DataPlaneOperationAndExportSpan(t *testing.T) {
	if got := dataPlaneOperation("/v1/responses"); got != "responses" {
		t.Fatalf("dataPlaneOperation(/v1/responses)=%q, want responses", got)
	}
	if got := dataPlaneOperation("/v1/messages/count_tokens"); got != "count_tokens" {
		t.Fatalf("dataPlaneOperation(count_tokens)=%q, want count_tokens", got)
	}
	if got := dataPlaneOperation("/v1/unknown"); got != "" {
		t.Fatalf("dataPlaneOperation(unknown)=%q, want empty", got)
	}
	// No exporter configured: export is a silent no-op.
	(&Server{}).exportSpan(
		httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil),
		traceContext{TraceID: "abc", SpanID: "def"}, time.Now(), time.Second, 200)

	// An exporter is attached, but an unrecognized operation is not
	// exported; a recognized one is queued without being dropped.
	exporter, err := telemetry.New("http://127.0.0.1:9/v1/traces", "d4-test")
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(exporter.Close)
	srv := &Server{}
	srv.SetTelemetryExporter(exporter)
	srv.exportSpan(
		httptest.NewRequest(http.MethodPost, "http://gateway/nope", nil),
		traceContext{TraceID: "abc", SpanID: "def"}, time.Now(), time.Second, 200)
	if got := exporter.Dropped(); got != 0 {
		t.Fatalf("exporter dropped %d spans for an unknown operation, want 0", got)
	}
	srv.exportSpan(
		httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", nil),
		traceContext{TraceID: "abc", SpanID: "def"}, time.Now(), time.Second, 200)
	if got := exporter.Dropped(); got != 0 {
		t.Fatalf("exporter dropped %d spans for a recognized operation, want 0", got)
	}
}

// ---------- telemetry_ingest.go ----------

func TestD4TelemetryAggregator(t *testing.T) {
	// Nil buckets map is lazily initialized.
	a := &telemetryAggregator{}
	a.observe("d1", "p1", "m", true, 10, 5, time.Now())
	if a.deployments() != 1 {
		t.Fatalf("deployments=%d, want 1 after first observe on a zero aggregator", a.deployments())
	}
	// Cardinality cap: the 4097th deployment is dropped.
	b := newTelemetryAggregator()
	for i := 0; i < maxTelemetryDeployments; i++ {
		b.observe(fmt.Sprintf("dep-%d", i), "p1", "m", true, 1, 0, time.Now())
	}
	if b.deployments() != maxTelemetryDeployments {
		t.Fatalf("deployments=%d, want %d", b.deployments(), maxTelemetryDeployments)
	}
	b.observe("dep-overflow", "p1", "m", true, 1, 0, time.Now())
	if b.deployments() != maxTelemetryDeployments {
		t.Fatalf("deployments=%d after overflow, want capped at %d", b.deployments(), maxTelemetryDeployments)
	}
	// TTFT ring trims to the newest 256 samples.
	c := newTelemetryAggregator()
	for i := 0; i < 300; i++ {
		c.observe("d1", "p1", "m", true, 0, 1, time.Now())
	}
	c.mu.Lock()
	n := len(c.buckets["d1"].ttfts)
	c.mu.Unlock()
	if n != maxTelemetrySamplesPerDeployment {
		t.Fatalf("ttfts=%d, want %d", n, maxTelemetrySamplesPerDeployment)
	}
	// A NaN percentile degrades to the largest sample.
	if got := percentileCopy([]float64{1, 2, 3}, math.NaN()); got != 3 {
		t.Fatalf("percentileCopy(NaN)=%v, want 3 (max sample)", got)
	}
}

func TestD4FlushTelemetryErrors(t *testing.T) {
	// FromTelemetry rejects the bucket: an invalid provider id fails
	// scorecard validation, so nothing is written.
	p := newEvaluationPlane(config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: true, TelemetryMinSamples: 1}, nil)
	p.telemetry.observe("dep-x", "bad provider!", "", true, 10, 0, time.Now().UTC())
	if got := p.flushTelemetry(time.Now().UTC()); got != 0 {
		t.Fatalf("flushTelemetry=%d, want 0 for an unscoreable bucket", got)
	}
	if _, ok := p.Registry().Get("dep-x"); ok {
		t.Fatal("unscoreable telemetry must not produce a scorecard")
	}

	// Upsert rejects the bucket: a one-slot registry already holds
	// another deployment, so the flush writes nothing.
	p2 := newEvaluationPlane(config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: true, TelemetryMinSamples: 1, MaxScorecards: 1}, nil)
	sc, err := scorecards.FromTelemetry(scorecards.TelemetryEvidence{
		DeploymentID: "other-dep", ProviderID: "p1", Model: "m",
		ObservedAt: time.Now().UTC(), SampleCount: 1, SuccessRate: 1,
	})
	if err != nil {
		t.Fatalf("seed FromTelemetry: %v", err)
	}
	if _, err := p2.Registry().Upsert(sc); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	p2.telemetry.observe("dep-x", "p1", "m", true, 10, 0, time.Now().UTC())
	if got := p2.flushTelemetry(time.Now().UTC()); got != 0 {
		t.Fatalf("flushTelemetry=%d, want 0 for a full registry", got)
	}
	if _, ok := p2.Registry().Get("dep-x"); ok {
		t.Fatal("dep-x must not be stored in a full registry")
	}
	if _, ok := p2.Registry().Get("other-dep"); !ok {
		t.Fatal("seed scorecard must survive a failed flush")
	}
}

func TestD4ObserveTelemetryTTFTNoPlane(t *testing.T) {
	// No evaluation plane: the hook is a silent no-op, never a panic.
	(&Server{}).observeTelemetryTTFT("d1", time.Second)

	// A plane exists and telemetry ingest is enabled, but the
	// aggregator itself is nil: both completion and TTFT hooks return
	// without observing anything.
	srv := testGateway(t, config.Default())
	srv.runtimeMu.Lock()
	srv.evaluation = &evaluationPlane{cfg: config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: true}}
	srv.runtimeMu.Unlock()
	srv.observeCompletedTelemetry("d1", "p1", true, time.Second, time.Second)
	srv.observeTelemetryTTFT("d1", time.Second)
	if srv.evaluation.telemetry != nil {
		t.Fatal("telemetry aggregator must stay nil; the hooks must not create one")
	}
}

// ---------- virtual.go ----------

func TestD4AdminEndpointMutateConfigError(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	// Invalid per Validate, but the server still builds: the failure
	// must surface only when a mutation tries to persist the config.
	cfg.Admin.SessionTTLSeconds = 100
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/endpoint", strings.NewReader(`{"model":"my-model"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.adminEndpoint(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "session_ttl") {
		t.Fatalf("adminEndpoint status=%d body=%s, want 400 mentioning session_ttl", rec.Code, rec.Body.String())
	}
}
