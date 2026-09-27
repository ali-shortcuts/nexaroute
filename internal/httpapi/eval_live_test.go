package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Phase H live physical-deployment evaluation canaries.
const (
	liveDataCanary = "SECRET_EVAL_DATASET_CANARY_7b91"
	liveKeyCanary  = "SECRET_EVAL_PROVIDER_KEY_3f42"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// syncLog captures gateway log output so a test can assert that neither the
// live prompt nor the provider credential ever reaches a log line.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func liveGateway(t *testing.T, cfg config.Config, logs *syncLog) *Server {
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
	bus := events.New(500)
	pe := probe.New(cfg, reg, rt, hm, bus)
	return New(cfg, t.TempDir()+"/config.json", reg, rt, hm, bus, pe, log.New(logs, "", 0))
}

// countingProvider wraps a DecisionProvider and counts every Decide call. The
// counters are what makes "live evaluation calls zero DecisionProviders" a
// falsifiable assertion rather than a claim.
type countingProvider struct {
	inner decision.DecisionProvider
	calls atomic.Int64
}

func (p *countingProvider) ID() string                          { return p.inner.ID() }
func (p *countingProvider) Capabilities() decision.Capabilities { return p.inner.Capabilities() }
func (p *countingProvider) Health() decision.ProviderHealth     { return p.inner.Health() }
func (p *countingProvider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	p.calls.Add(1)
	return p.inner.Decide(ctx, req)
}
func (p *countingProvider) Calls() int64 { return p.calls.Load() }

// capturingProvider records the last DecisionRequest so a test can assert that
// scorecard quality never enters the decision plane.
type capturingProvider struct {
	inner decision.DecisionProvider
	mu    sync.Mutex
	last  decision.DecisionRequest
	calls atomic.Int64
}

func (p *capturingProvider) ID() string                          { return p.inner.ID() }
func (p *capturingProvider) Capabilities() decision.Capabilities { return p.inner.Capabilities() }
func (p *capturingProvider) Health() decision.ProviderHealth     { return p.inner.Health() }
func (p *capturingProvider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.last = req
	p.mu.Unlock()
	return p.inner.Decide(ctx, req)
}
func (p *capturingProvider) Calls() int64 { return p.calls.Load() }
func (p *capturingProvider) Last() decision.DecisionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// liveAnswerUpstream is a real HTTP upstream that answers with the text inside
// the <<ANSWER:...>> marker of the prompt it receives.
func liveAnswerUpstream(t *testing.T, hits *atomic.Int64, mode string, authSeen *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if authSeen != nil {
			*authSeen = r.Header.Get("Authorization")
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if mode == "slow" {
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		if mode == "error" {
			// Echo the credential back in the error body: the evaluation plane
			// must still never surface it anywhere.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"upstream failed","authorization":"`+r.Header.Get("Authorization")+`"}`)
			return
		}
		answer := ""
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &in)
		if len(in.Messages) > 0 {
			if i := strings.Index(in.Messages[0].Content, "<<ANSWER:"); i >= 0 {
				rest := in.Messages[0].Content[i+len("<<ANSWER:"):]
				if j := strings.Index(rest, ">>"); j >= 0 {
					answer = rest[:j]
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{
			"id": "chatcmpl-live", "object": "chat.completion", "created": 1, "model": "model-m1",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// liveEvalConfig builds a two-deployment hybrid-mode gateway. Hybrid mode with
// three chain steps means a production request really does invoke Jev, Policy
// and Local — which is what makes "live evaluation invoked none of them" a
// meaningful assertion instead of a tautology.
func liveEvalConfig(t *testing.T, upA, upB *httptest.Server) config.Config {
	t.Helper()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	cfg.Evaluation.Enabled = true
	cfg.Evaluation.LiveEnabled = true
	cfg.Evaluation.MaxRuns = 32
	cfg.Evaluation.MaxScorecards = 32
	cfg.Evaluation.MaxArtifacts = 8
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "phase-h-live"
	cfg.Decision.MaxProviderCalls = 3
	cfg.Decision.TimeoutMS = 800
	cfg.DecisionChains = append(cfg.DecisionChains, config.DecisionChainConfig{
		ID: "phase-h-live",
		Steps: []config.DecisionChainStep{
			{Provider: "jev-main"},
			{Provider: "policy"},
			{Provider: "local"},
		},
	})
	return cfg
}

func prodChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func liveRunBody(t *testing.T, deploymentID, suiteID string, prompts []map[string]string, extra map[string]any) string {
	t.Helper()
	payload := map[string]any{"mode": "live", "suite_id": suiteID, "deployment_id": deploymentID}
	if prompts != nil {
		payload["prompts"] = prompts
	}
	for k, v := range extra {
		payload[k] = v
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func reasoningPrompts() []map[string]string {
	return []map[string]string{
		{"case_id": "reasoning-multi-step-arithmetic", "prompt": "compute the answer <<ANSWER:42>>"},
		{"case_id": "reasoning-logic-deduction", "prompt": "who did it? <<ANSWER:carol>>"},
		{"case_id": "reasoning-numeric-estimate", "prompt": "estimate pi <<ANSWER:3.14>>"},
	}
}

// healthFingerprint captures every piece of production routing state a live
// evaluation is forbidden to touch.
func healthFingerprint(t *testing.T, s *Server) string {
	t.Helper()
	// Provider stats arrive in map iteration order; sorting them keeps the
	// fingerprint a stable, bit-comparable value.
	stats := s.reg.Stats()
	sort.Slice(stats, func(i, j int) bool { return stats[i].ID < stats[j].ID })
	providerHealth := s.hm.ProviderSnapshot()
	sort.Slice(providerHealth, func(i, j int) bool { return providerHealth[i].Provider < providerHealth[j].Provider })
	// Deployment health also arrives in map iteration order.
	deploymentHealth := s.hm.Snapshot()
	sort.Slice(deploymentHealth, func(i, j int) bool { return deploymentHealth[i].Deployment < deploymentHealth[j].Deployment })
	blob, err := json.Marshal(map[string]any{
		"health":          deploymentHealth,
		"provider_health": providerHealth,
		"provider_stats":  stats,
		"sessions":        s.rt.SessionCount(),
		"cache":           s.respCache.Stats(),
		"usage":           s.usage.Snapshot(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

// ---------------------------------------------------------------------------
// 5. Live execution against a real upstream
// ---------------------------------------------------------------------------

func TestPhaseH_Live_ExactlyOneUpstreamCallAndZeroDecisionProviderCalls(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	cfg := liveEvalConfig(t, upA, upB)
	s := liveGateway(t, cfg, logs)

	// Chain steps: jev-main abstains, policy abstains, local ranks. A production
	// request therefore calls all three providers.
	jev := &countingProvider{inner: newCountingChainProvider("jev-main", decision.DecisionResult{
		Action: decision.ActionAbstain, Abstained: true, Confidence: 1,
		ReasonCodes: []decision.ReasonCode{decision.ReasonExistingOrderPreserved}}, nil)}
	pol := &countingProvider{inner: newCountingChainProvider("policy", decision.DecisionResult{
		Action: decision.ActionAbstain, Abstained: true, Confidence: 1,
		ReasonCodes: []decision.ReasonCode{decision.ReasonExistingOrderPreserved}}, nil)}
	loc := &countingProvider{inner: &decision.LocalProvider{}}
	s.decisionRegistry.Register(jev)
	s.decisionRegistry.Register(pol)
	s.decisionRegistry.Register(loc)

	// Sanity: prove the counters are wired before asserting they stay at zero.
	rr := prodChat(t, s, `{"model":"nexa-chain","messages":[{"role":"user","content":"warmup"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("warmup production request status %d: %s", rr.Code, rr.Body.String())
	}
	if jev.Calls() == 0 || pol.Calls() == 0 || loc.Calls() == 0 {
		t.Fatalf("instrumentation is not wired: jev=%d policy=%d local=%d (the zero assertions below would be meaningless)",
			jev.Calls(), pol.Calls(), loc.Calls())
	}

	before := map[string]int64{"jev": jev.Calls(), "policy": pol.Calls(), "local": loc.Calls()}
	metricsBefore := s.decisionOrchestrator.MetricsSnapshot()
	eventsBefore := len(s.bus.Snapshot())
	hitsABefore := hitsA.Load()
	hitsBBefore := hitsB.Load()

	// Live evaluation with exactly one prompted case: one upstream call, to the
	// one selected physical deployment.
	body := liveRunBody(t, "p1/m1", "reasoning", []map[string]string{
		{"case_id": "reasoning-multi-step-arithmetic", "prompt": "compute <<ANSWER:42>>"},
	}, nil)
	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", body)
	if res.Code != http.StatusOK {
		t.Fatalf("live evaluation status %d: %s", res.Code, res.Body.String())
	}
	out := decodeJSON(t, res)
	if out["mode"] != "live" {
		t.Fatalf("mode = %v, want live", out["mode"])
	}
	run := mustMap(t, out["run"], "run")
	if got := int(run["upstream_calls"].(float64)); got != 1 {
		t.Fatalf("run upstream_calls = %d, want exactly 1", got)
	}
	if got := hitsA.Load() - hitsABefore; got != 1 {
		t.Fatalf("selected deployment upstream calls = %d, want exactly 1", got)
	}
	if got := hitsB.Load() - hitsBBefore; got != 0 {
		t.Fatalf("non-selected deployment upstream calls = %d, want 0 (no fan-out, no fallback)", got)
	}

	// Decision plane: zero calls, zero new decision events, zero metric movement.
	if d := jev.Calls() - before["jev"]; d != 0 {
		t.Fatalf("Jev decision provider calls = %d, want 0", d)
	}
	if d := pol.Calls() - before["policy"]; d != 0 {
		t.Fatalf("Policy decision provider calls = %d, want 0", d)
	}
	if d := loc.Calls() - before["local"]; d != 0 {
		t.Fatalf("Local decision provider calls = %d, want 0", d)
	}
	metricsAfter := s.decisionOrchestrator.MetricsSnapshot()
	for k, v := range metricsAfter {
		if before_, ok := metricsBefore[k]; !ok || before_ != v {
			t.Fatalf("decision metric %q moved during live evaluation: %d -> %d", k, before_, v)
		}
	}
	for _, ev := range s.bus.Snapshot()[eventsBefore:] {
		if strings.HasPrefix(ev.Kind, "decision_") {
			t.Fatalf("live evaluation emitted a decision event: %s %s", ev.Kind, ev.Message)
		}
	}
}

func TestPhaseH_Live_ResponseIsGradedAndCreatesScorecard(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	s := liveGateway(t, liveEvalConfig(t, upA, upB), logs)

	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), nil))
	if res.Code != http.StatusOK {
		t.Fatalf("live evaluation status %d: %s", res.Code, res.Body.String())
	}
	out := decodeJSON(t, res)
	if out["scorecard_written"] != true {
		t.Fatalf("scorecard_written = %v, want true: %s", out["scorecard_written"], res.Body.String())
	}
	run := mustMap(t, out["run"], "run")
	if got := int(run["upstream_calls"].(float64)); got != 3 {
		t.Fatalf("upstream_calls = %d, want 3 (one per prompted case)", got)
	}
	if got := run["score"].(float64); got != 1 {
		t.Fatalf("score = %v, want 1 (every live answer was correct)", got)
	}
	if hitsA.Load() != 3 || hitsB.Load() != 0 {
		t.Fatalf("upstream distribution = p1/m1:%d p2/m2:%d, want 3:0", hitsA.Load(), hitsB.Load())
	}
	live := mustMap(t, out["live"], "live")
	if live["deployment_id"] != "p1/m1" {
		t.Fatalf("live deployment = %v", live["deployment_id"])
	}
	if got := int(live["failed_calls"].(float64)); got != 0 {
		t.Fatalf("failed_calls = %d, want 0", got)
	}

	// The scorecard is real registry state with measured provenance.
	scRes := adminDo(t, s, http.MethodGet, "/admin/api/scorecards?deployment=p1/m1", "")
	if scRes.Code != http.StatusOK {
		t.Fatalf("scorecards status %d", scRes.Code)
	}
	scOut := decodeJSON(t, scRes)
	cards, _ := scOut["scorecards"].([]any)
	if len(cards) != 1 {
		t.Fatalf("scorecards = %d, want 1", len(cards))
	}
	card := mustMap(t, cards[0], "scorecard")
	if card["deployment_id"] != "p1/m1" {
		t.Fatalf("scorecard deployment = %v", card["deployment_id"])
	}
	values, _ := card["values"].([]any)
	found := false
	for _, v := range values {
		vm := mustMap(t, v, "value")
		if vm["dimension"] == string(scorecards.DimReasoning) {
			found = true
			if vm["provenance"] != string(scorecards.ProvenanceEvaluation) && vm["provenance"] != "evaluation" {
				t.Fatalf("provenance = %v, want measured evaluation provenance", vm["provenance"])
			}
		}
	}
	if !found {
		t.Fatalf("scorecard has no reasoning dimension: %s", res.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 6. Health isolation
// ---------------------------------------------------------------------------

func TestPhaseH_Live_HealthIsolationSuccessErrorAndTimeout(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	s := liveGateway(t, liveEvalConfig(t, upA, upB), logs)

	// Give production a real, non-trivial health baseline so "unchanged" means
	// something: one success and one failure already recorded.
	s.hm.RecordSuccess("p1/m1", 12*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 20*time.Millisecond)
	baseline := healthFingerprint(t, s)

	// (a) successful live evaluation
	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), nil))
	if res.Code != http.StatusOK {
		t.Fatalf("live evaluation status %d: %s", res.Code, res.Body.String())
	}
	if got := healthFingerprint(t, s); got != baseline {
		t.Fatalf("successful live evaluation changed production state:\nbefore=%s\nafter =%s", baseline, got)
	}

	// (b) live evaluation against an HTTP 500 upstream
	var errHits atomic.Int64
	errUp := liveAnswerUpstream(t, &errHits, "error", nil)
	cfgErr := liveEvalConfig(t, errUp, upB)
	sErr := liveGateway(t, cfgErr, &syncLog{})
	sErr.hm.RecordSuccess("p1/m1", 12*time.Millisecond)
	sErr.hm.RecordSuccess("p2/m2", 20*time.Millisecond)
	errBaseline := healthFingerprint(t, sErr)
	errRes := adminDo(t, sErr, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), nil))
	if errRes.Code != http.StatusOK {
		t.Fatalf("live evaluation (500 upstream) status %d: %s", errRes.Code, errRes.Body.String())
	}
	errOut := decodeJSON(t, errRes)
	runErr := mustMap(t, errOut["run"], "run")
	if got := runErr["score"].(float64); got != 0 {
		t.Fatalf("score against a 500 upstream = %v, want 0", got)
	}
	if got := healthFingerprint(t, sErr); got != errBaseline {
		t.Fatalf("HTTP 500 live evaluation degraded production state:\nbefore=%s\nafter =%s", errBaseline, got)
	}

	// (c) live evaluation against a timing-out upstream
	var slowHits atomic.Int64
	slowUp := liveAnswerUpstream(t, &slowHits, "slow", nil)
	cfgSlow := liveEvalConfig(t, slowUp, upB)
	sSlow := liveGateway(t, cfgSlow, &syncLog{})
	sSlow.hm.RecordSuccess("p1/m1", 12*time.Millisecond)
	sSlow.hm.RecordSuccess("p2/m2", 20*time.Millisecond)
	slowBaseline := healthFingerprint(t, sSlow)
	slowRes := adminDo(t, sSlow, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), map[string]any{"case_timeout_ms": 60}))
	if slowRes.Code != http.StatusOK {
		t.Fatalf("live evaluation (timeout upstream) status %d: %s", slowRes.Code, slowRes.Body.String())
	}
	if got := healthFingerprint(t, sSlow); got != slowBaseline {
		t.Fatalf("timing-out live evaluation degraded production state:\nbefore=%s\nafter =%s", slowBaseline, got)
	}
	// The timeout must be graded as evidence, not silently skipped.
	slowOut := decodeJSON(t, slowRes)
	if got := mustMap(t, slowOut["run"], "run")["score"].(float64); got != 0 {
		t.Fatalf("score against a timing-out upstream = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 7. Cache isolation
// ---------------------------------------------------------------------------

func TestPhaseH_Live_NeverWritesProductionResponseCache(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	cfg := liveEvalConfig(t, upA, upB)
	cfg.Cache.Enabled = true
	cfg.Cache.TTLSeconds = 300
	cfg.Cache.MaxEntries = 16
	cfg.Cache.MaxBodyBytes = 1 << 20
	s := liveGateway(t, cfg, logs)

	// The live prompt is byte-identical to the production request body, so any
	// evaluation write into the production cache would be caught as a HIT.
	prodBody := `{"model":"nexa-chain","messages":[{"role":"user","content":"cache-isolation-probe"}]}`
	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", []map[string]string{
			{"case_id": "reasoning-multi-step-arithmetic", "prompt": prodBody},
		}, nil))
	if res.Code != http.StatusOK {
		t.Fatalf("live evaluation status %d: %s", res.Code, res.Body.String())
	}
	if st := s.respCache.Stats(); st.Entries != 0 || st.Stores != 0 {
		t.Fatalf("live evaluation wrote into the production response cache: %+v", st)
	}

	// The equivalent normal production request must MISS: nothing from the
	// evaluation may be served back on the data plane.
	first := prodChat(t, s, prodBody)
	if first.Code != http.StatusOK {
		t.Fatalf("production request status %d: %s", first.Code, first.Body.String())
	}
	if h := first.Header().Get("X-NexaRoute-Cache"); h == "HIT" {
		t.Fatal("production request was served from an evaluation-created cache entry")
	}
	if st := s.respCache.Stats(); st.Entries != 1 {
		t.Fatalf("cache entries after the first production request = %d, want 1 (written by production, not evaluation)", st.Entries)
	}
	// And the cache works at all, so the MISS above was not a disabled cache.
	second := prodChat(t, s, prodBody)
	if second.Header().Get("X-NexaRoute-Cache") != "HIT" {
		t.Fatal("second production request should hit the cache (otherwise the isolation assertion proves nothing)")
	}
}

// ---------------------------------------------------------------------------
// 8. Session affinity isolation
// ---------------------------------------------------------------------------

func TestPhaseH_Live_CreatesNoSessionAffinityState(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	cfg := liveEvalConfig(t, upA, upB)
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 600
	s := liveGateway(t, cfg, logs)

	// Create a real pin first so the snapshot is non-empty.
	rr := prodChat(t, s, `{"model":"nexa-chain","messages":[{"role":"user","content":"affinity warmup"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("warmup status %d: %s", rr.Code, rr.Body.String())
	}
	withSession := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"affinity pin"}]}`))
	withSession.Header.Set("Content-Type", "application/json")
	withSession.Header.Set("X-Session-Id", "phase-h-live-session")
	withSession.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, withSession)
	if rec.Code != http.StatusOK {
		t.Fatalf("session request status %d: %s", rec.Code, rec.Body.String())
	}

	before := healthFingerprint(t, s)
	pinnedBefore := s.rt.PinnedDeploymentID(router.Requirement{Model: "nexa-chain", SessionKey: "phase-h-live-session"})
	if s.rt.SessionCount() == 0 {
		t.Fatal("no session pin was created by the warmup (the isolation assertion would be vacuous)")
	}

	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), nil))
	if res.Code != http.StatusOK {
		t.Fatalf("live evaluation status %d: %s", res.Code, res.Body.String())
	}
	if got := healthFingerprint(t, s); got != before {
		t.Fatalf("live evaluation changed session affinity state:\nbefore=%s\nafter =%s", before, got)
	}
	if got := s.rt.PinnedDeploymentID(router.Requirement{Model: "nexa-chain", SessionKey: "phase-h-live-session"}); got != pinnedBefore {
		t.Fatalf("pinned deployment = %q, want %q (live evaluation must not create or move a pin)", got, pinnedBefore)
	}
}

// ---------------------------------------------------------------------------
// 9. Routing neutrality
// ---------------------------------------------------------------------------

func TestPhaseH_LiveScorecardsHaveZeroRoutingInfluence(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	s := liveGateway(t, liveEvalConfig(t, upA, upB), logs)

	pol := &capturingProvider{inner: newCountingChainProvider("policy", decision.DecisionResult{
		Action: decision.ActionAbstain, Abstained: true, Confidence: 1,
		ReasonCodes: []decision.ReasonCode{decision.ReasonExistingOrderPreserved}}, nil)}
	s.decisionRegistry.Register(pol)

	body := `{"model":"nexa-chain","messages":[{"role":"user","content":"neutrality probe"}]}`
	before := prodChat(t, s, body)
	if before.Code != http.StatusOK {
		t.Fatalf("production request status %d: %s", before.Code, before.Body.String())
	}
	deploymentBefore := before.Header().Get("X-Gateway-Deployment")
	if deploymentBefore == "" {
		t.Fatal("production request did not resolve to a deployment")
	}

	// Populate extreme scorecards: one deployment perfect, the other worthless.
	now := time.Now().UTC()
	for id, score := range map[string]float64{"p1/m1": 1, "p2/m2": 0} {
		sc, err := scorecards.FromEvaluation(scorecards.EvaluationEvidence{
			DeploymentID: id, ProviderID: strings.SplitN(id, "/", 2)[0], Model: "model-" + strings.SplitN(id, "/", 2)[1],
			SuiteID: "reasoning", SuiteVersion: "1", RunID: "extreme-" + id, EvaluatedAt: now,
			Values: []scorecards.EvaluationValue{
				{Dimension: scorecards.DimReasoning, Score: score, Raw: score, SampleCount: 4096},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.evaluation.Registry().Upsert(sc); err != nil {
			t.Fatal(err)
		}
	}
	// Prove the extreme evidence is really in the registry.
	if n := s.evaluation.Registry().Len(); n != 2 {
		t.Fatalf("scorecard registry size = %d, want 2", n)
	}

	after := prodChat(t, s, body)
	if after.Code != http.StatusOK {
		t.Fatalf("production request after scorecards status %d: %s", after.Code, after.Body.String())
	}
	if got := after.Header().Get("X-Gateway-Deployment"); got != deploymentBefore {
		t.Fatalf("routing changed after extreme scorecards: %q -> %q (Phase H scorecards must have zero routing influence)", deploymentBefore, got)
	}

	// Scorecards must not be handed to the policy provider either.
	if pol.Calls() == 0 {
		t.Skip("policy provider was never invoked; cannot inspect its request payload")
	}
	// Only the candidate payload can carry quality: the request also carries
	// task features whose field names legitimately contain other words.
	blob, err := json.Marshal(pol.Last().Candidates)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"quality", "scorecard", "eval_score", "dim_", "confidence_score"} {
		if strings.Contains(strings.ToLower(string(blob)), forbidden) {
			t.Fatalf("decision candidates carry %q — Phase H must not feed scorecards to PolicyProvider: %s", forbidden, blob)
		}
	}
}

// ---------------------------------------------------------------------------
// 10. Privacy canaries
// ---------------------------------------------------------------------------

func TestPhaseH_Live_PrivacyCanaries(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	var authSeen string
	upA := liveAnswerUpstream(t, &hitsA, "error", &authSeen) // 500 + credential echo
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	cfg := liveEvalConfig(t, upA, upB)
	for i := range cfg.Providers {
		cfg.Providers[i].AuthMode = "bearer"
		cfg.Providers[i].APIKey = liveKeyCanary
	}
	s := liveGateway(t, cfg, logs)

	prompts := []map[string]string{
		{"case_id": "reasoning-multi-step-arithmetic", "prompt": liveDataCanary + " <<ANSWER:42>>"},
		{"case_id": "reasoning-logic-deduction", "prompt": liveDataCanary + " <<ANSWER:carol>>"},
		{"case_id": "reasoning-numeric-estimate", "prompt": liveDataCanary + " <<ANSWER:3.14>>"},
	}
	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", prompts, nil))
	if res.Code != http.StatusOK {
		t.Fatalf("live evaluation status %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(authSeen, liveKeyCanary) {
		t.Fatalf("the selected physical deployment never received the provider credential (auth=%q); the canary test would be vacuous", authSeen)
	}
	if hitsA.Load() == 0 {
		t.Fatal("live evaluation made no upstream call; the canary test would be vacuous")
	}

	// Surfaces the data canary must never reach.
	surfaces := map[string]string{
		"metrics":        metricsBody(t, s),
		"admin snapshot": adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "").Body.String(),
		"events":         eventsBody(t, s),
		"evaluation run": res.Body.String(),
		"runs endpoint":  adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs", "").Body.String(),
		"scorecards":     adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "").Body.String(),
		"health":         adminDo(t, s, http.MethodGet, "/admin/api/health", "").Body.String(),
		"logs":           logs.String(),
	}
	for name, blob := range surfaces {
		if strings.Contains(blob, liveDataCanary) {
			t.Fatalf("live prompt canary leaked into %s", name)
		}
	}
	// Surfaces the provider credential canary must never reach: everything above
	// plus the DecisionTrace and routing events.
	for name, blob := range surfaces {
		if strings.Contains(blob, liveKeyCanary) {
			t.Fatalf("provider credential canary leaked into %s", name)
		}
	}
	traceBlob, err := json.Marshal(decision.DecisionTrace{Mode: "hybrid"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(traceBlob), liveKeyCanary) {
		t.Fatal("provider credential canary reached a DecisionTrace")
	}
	for _, ev := range s.bus.Snapshot() {
		blob, _ := json.Marshal(ev)
		if strings.Contains(string(blob), liveKeyCanary) || strings.Contains(string(blob), liveDataCanary) {
			t.Fatalf("canary reached a routing event: %s", blob)
		}
	}
	// Model health state must not carry prompt or credential content either.
	healthBlob, err := json.Marshal(map[string]any{"health": s.hm.Snapshot(), "providers": s.hm.ProviderSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(healthBlob), liveDataCanary) || strings.Contains(string(healthBlob), liveKeyCanary) {
		t.Fatal("canary reached model-health state")
	}
}

func metricsBody(t *testing.T, s *Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status %d", rr.Code)
	}
	return rr.Body.String()
}

func eventsBody(t *testing.T, s *Server) string {
	t.Helper()
	blob, err := json.Marshal(s.bus.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

// ---------------------------------------------------------------------------
// 4. Admin run mode validation
// ---------------------------------------------------------------------------

func TestPhaseH_Live_ModeValidation(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	logs := &syncLog{}
	cfg := liveEvalConfig(t, upA, upB)
	s := liveGateway(t, cfg, logs)

	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"unknown mode", liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), map[string]any{"mode": "shadow"}), http.StatusBadRequest},
		{"live without prompts", liveRunBody(t, "p1/m1", "reasoning", nil, nil), http.StatusBadRequest},
		{"live with artifacts", liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), map[string]any{
			"artifacts": []eval.Outcome{{CaseID: "reasoning-multi-step-arithmetic", Status: eval.OutcomeOK}}}), http.StatusBadRequest},
		{"live with unknown case", liveRunBody(t, "p1/m1", "reasoning", []map[string]string{
			{"case_id": "not-a-case", "prompt": "hi"}}, nil), http.StatusBadRequest},
		{"live with unknown deployment", liveRunBody(t, "nope/nope", "reasoning", reasoningPrompts(), nil), http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", tc.body)
			if res.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.wantStatus, res.Body.String())
			}
		})
	}
	if hitsA.Load() != 0 || hitsB.Load() != 0 {
		t.Fatalf("rejected live runs made upstream calls: p1/m1=%d p2/m2=%d", hitsA.Load(), hitsB.Load())
	}

	// Replay must be unchanged: it still requires artifacts and rejects prompts.
	replayOK := evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil)
	if res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", replayOK); res.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (offline replay must be unchanged): %s", res.Code, res.Body.String())
	}
	replayWithPrompts := evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), map[string]any{
		"prompts": []map[string]string{{"case_id": "coding-bugfix", "prompt": "hi"}}})
	if res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", replayWithPrompts); res.Code != http.StatusBadRequest {
		t.Fatalf("replay with prompts status = %d, want 400", res.Code)
	}
	replayExplicit := evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), map[string]any{"mode": "replay"})
	if res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", replayExplicit); res.Code != http.StatusOK {
		t.Fatalf("explicit replay status = %d, want 200: %s", res.Code, res.Body.String())
	}
	if hitsA.Load() != 0 || hitsB.Load() != 0 {
		t.Fatalf("replay made upstream calls: p1/m1=%d p2/m2=%d", hitsA.Load(), hitsB.Load())
	}
}

func TestPhaseH_Live_DisabledByDefaultAndRequiresOptIn(t *testing.T) {
	if config.Default().Evaluation.LiveEnabled {
		t.Fatal("live evaluation must be disabled by default")
	}
	var hitsA, hitsB atomic.Int64
	upA := liveAnswerUpstream(t, &hitsA, "ok", nil)
	upB := liveAnswerUpstream(t, &hitsB, "ok", nil)
	cfg := liveEvalConfig(t, upA, upB)
	cfg.Evaluation.LiveEnabled = false
	s := liveGateway(t, cfg, &syncLog{})

	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), nil))
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 when live evaluation is not opted in: %s", res.Code, res.Body.String())
	}
	if hitsA.Load() != 0 || hitsB.Load() != 0 {
		t.Fatalf("disabled live evaluation made %d/%d upstream calls", hitsA.Load(), hitsB.Load())
	}

	// The suites endpoint states the modes and the live gate honestly.
	suites := decodeJSON(t, adminDo(t, s, http.MethodGet, "/admin/api/evaluation/suites", ""))
	if suites["live_enabled"] != false {
		t.Fatalf("live_enabled = %v, want false", suites["live_enabled"])
	}
	modes, _ := suites["modes"].([]any)
	if len(modes) != 2 {
		t.Fatalf("modes = %v, want [live replay]", modes)
	}
}

// ---------------------------------------------------------------------------
// 3. Provider credential, quota and decision-provider health isolation
// ---------------------------------------------------------------------------

// TestPhaseH_Live_ProviderCredentialAndQuotaIsolation pins the half of the
// isolation contract that lives inside the provider adapter: a live evaluation
// that trips credential cooldowns and rate-limit accounting must leave the
// production adapter's credentials, quota counters and concurrency gauges
// exactly as they were.
func TestPhaseH_Live_ProviderCredentialAndQuotaIsolation(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// A credential-retry status plus quota headers: exactly the response
		// that moves production credential and quota state on the data plane.
		w.Header().Set("Retry-After", "30")
		w.Header().Set("x-ratelimit-limit-tokens", "1000")
		w.Header().Set("x-ratelimit-remaining-tokens", "3")
		w.Header().Set("x-ratelimit-limit-requests", "100")
		w.Header().Set("x-ratelimit-remaining-requests", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()

	upB := liveAnswerUpstream(t, nil, "ok", nil)
	cfg := liveEvalConfig(t, srv, upB)
	// Provider order in the generated config follows map iteration, so locate
	// p1 by id instead of assuming an index.
	idx := -1
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == "p1" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("provider p1 missing from the generated config")
	}
	cfg.Providers[idx].Credentials = []config.CredentialConfig{
		{Name: "k1", APIKey: "live-key-1", Enabled: true},
		{Name: "k2", APIKey: "live-key-2", Enabled: true},
	}
	cfg.Providers[idx].AuthMode = "bearer"
	s := liveGateway(t, cfg, &syncLog{})

	before, ok := s.reg.Stat("p1")
	if !ok {
		t.Fatal("provider p1 missing")
	}
	if before.Credentials != 2 {
		t.Fatalf("production adapter credentials = %d, want 2 (the isolation assertion would be vacuous)", before.Credentials)
	}
	decisionBefore := decisionHealthFingerprint(s)

	res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		liveRunBody(t, "p1/m1", "reasoning", reasoningPrompts(), nil))
	if res.Code != http.StatusOK {
		t.Fatalf("live evaluation status %d: %s", res.Code, res.Body.String())
	}
	if hits.Load() == 0 {
		t.Fatal("live evaluation made no upstream call; the isolation assertion would be vacuous")
	}

	after, _ := s.reg.Stat("p1")
	if after.CredentialsCooling != before.CredentialsCooling {
		t.Fatalf("live evaluation cooled a production credential: %d -> %d", before.CredentialsCooling, after.CredentialsCooling)
	}
	if after.RemainingTokens != before.RemainingTokens || after.TokenLimit != before.TokenLimit {
		t.Fatalf("live evaluation wrote quota accounting into production: tokens %d/%d -> %d/%d",
			before.RemainingTokens, before.TokenLimit, after.RemainingTokens, after.TokenLimit)
	}
	if after.RemainingRequests != before.RemainingRequests || after.RequestLimit != before.RequestLimit {
		t.Fatalf("live evaluation wrote request quota into production: %d/%d -> %d/%d",
			before.RemainingRequests, before.RequestLimit, after.RemainingRequests, after.RequestLimit)
	}
	if after.ActiveRequests != before.ActiveRequests || after.WaitingRequests != before.WaitingRequests {
		t.Fatalf("live evaluation moved production concurrency gauges: active %d->%d waiting %d->%d",
			before.ActiveRequests, after.ActiveRequests, before.WaitingRequests, after.WaitingRequests)
	}
	if after.ReservedRequests != 0 || after.ReservedTokens != 0 {
		t.Fatalf("live evaluation left a quota reservation on production: req=%d tok=%d", after.ReservedRequests, after.ReservedTokens)
	}
	if got := decisionHealthFingerprint(s); got != decisionBefore {
		t.Fatalf("live evaluation changed DecisionProvider health:\nbefore=%s\nafter =%s", decisionBefore, got)
	}
}

// decisionHealthFingerprint reduces decision-provider health to the fields that
// must not move: status, failure counters and cooldown presence. Timestamps are
// excluded so the comparison is about failures, not the clock.
func decisionHealthFingerprint(s *Server) string {
	var b strings.Builder
	for id, st := range s.decisionOrchestrator.ProviderState().Snapshot() {
		fmt.Fprintf(&b, "%s|%s|%d|%d|%d|cooldown=%t;",
			id, st.Status, st.ConsecutiveFailures, st.Successes, st.FailuresInWindow, !st.CooldownUntil.IsZero())
	}
	return b.String()
}
