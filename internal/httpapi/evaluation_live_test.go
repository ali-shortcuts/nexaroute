package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// ---------------------------------------------------------------------------
// Live-mode fixtures
// ---------------------------------------------------------------------------

const (
	// evalCanary is the dataset canary: safe to send to the explicitly selected
	// model as evaluation input, and it must NEVER surface in metrics, the admin
	// snapshot, decision traces, routing events or model health state.
	evalCanary = "SECRET_EVAL_DATASET_CANARY_7b91"
	// evalProviderKey is the provider credential canary: it authenticates the
	// evaluation request upstream and must NEVER surface in evaluation records,
	// scorecards, events, metrics, admin surfaces or errors.
	evalProviderKey = "SECRET_EVAL_PROVIDER_KEY_3f42"
)

// fakeModel is a deterministic stand-in for a real physical model upstream. It
// records every hit (count, auth header, prompt) and answers the prompt's
// "LIVE_ANSWER:<answer>" marker, exactly like a cheap real model would.
type fakeModel struct {
	srv *httptest.Server

	hits   atomic.Int64
	status atomic.Int64 // forced HTTP status (0 = answer 200)
	delay  atomic.Int64 // milliseconds to sleep before answering

	mu      sync.Mutex
	auths   []string
	prompts []string
}

func newFakeModel(t testing.TB) *fakeModel {
	t.Helper()
	f := &fakeModel{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.prompts = append(f.prompts, chatPromptFromBody(t, string(body)))
		f.mu.Unlock()
		if d := f.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		if st := f.status.Load(); st != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(st))
			fmt.Fprint(w, `{"error":{"message":"forced upstream failure"}}`)
			return
		}
		answer := "ok"
		f.mu.Lock()
		prompt := f.prompts[len(f.prompts)-1]
		f.mu.Unlock()
		if idx := strings.Index(prompt, "LIVE_ANSWER:"); idx >= 0 {
			rest := prompt[idx+len("LIVE_ANSWER:"):]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				rest = rest[:nl]
			}
			if v := strings.TrimSpace(rest); v != "" {
				answer = v
			}
		}
		encoded, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":23,"completion_tokens":5,"total_tokens":28}}`, encoded)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// chatPromptFromBody extracts the user prompt from the openai_chat payload the
// production adapter sends. It proves the real request carried the case input.
func chatPromptFromBody(t testing.TB, body string) string {
	t.Helper()
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("upstream received non-openai-chat payload: %v (%s)", err, body)
	}
	for _, m := range payload.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

func (f *fakeModel) hitCount() int { return int(f.hits.Load()) }

func (f *fakeModel) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auths) == 0 {
		return ""
	}
	return f.auths[len(f.auths)-1]
}

func (f *fakeModel) allPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// phaseHLiveConfig builds an enabled evaluation config with live mode on,
// two providers (p1 with the provider-key canary and models a/b, p2 without a
// key with model-c) and three fake-upstream URLs.
func phaseHLiveConfig(statePath, up1URL, up2URL string) config.Config {
	cfg := phaseHConfig(statePath, "")
	cfg.Evaluation.LiveEnabled = true
	cfg.Providers = []config.ProviderConfig{
		{
			ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: up1URL,
			APIKey: evalProviderKey, Enabled: true,
			Models: []config.ModelConfig{
				{ID: "m1", Model: "model-a", Enabled: true, Weight: 1},
				{ID: "m2", Model: "model-b", Enabled: true, Weight: 1},
			},
		},
		{
			ID: "p2", Name: "P2", Type: "openai_compatible", BaseURL: up2URL,
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "n1", Model: "model-c", Enabled: true, Weight: 1}},
		},
	}
	return cfg
}

func liveReasoningInputs(answers map[string]string) []map[string]any {
	suite, _ := eval.LookupSuite("reasoning")
	out := make([]map[string]any, 0, len(answers))
	for _, c := range suite.Cases {
		answer, ok := answers[c.ID]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"case_id": c.ID,
			"prompt":  "LIVE_ANSWER:" + answer + "\nAnswer exactly and only with the value.",
		})
	}
	return out
}

func liveRunBody(t *testing.T, deploymentID, suiteID string, inputs []map[string]any, extra map[string]any) string {
	t.Helper()
	payload := map[string]any{"mode": "live", "suite_id": suiteID, "deployment_id": deploymentID, "inputs": inputs}
	for k, v := range extra {
		payload[k] = v
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func postLiveRun(t *testing.T, s *Server, deploymentID, suiteID string, inputs []map[string]any, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", liveRunBody(t, deploymentID, suiteID, inputs, extra))
}

func providerStatsByID(stats []providers.ProviderStats) map[string]providers.ProviderStats {
	out := make(map[string]providers.ProviderStats, len(stats))
	for _, st := range stats {
		out[st.ID] = st
	}
	return out
}

// ---------------------------------------------------------------------------
// Mode gating: replay stays the safe default; live requires explicit opt-in
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationRequiresExplicitOptIn(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)

	// Plane disabled: everything is closed, even with mode=live.
	cfgOff := phaseHLiveConfig("", up1.srv.URL, up2.srv.URL)
	cfgOff.Evaluation.Enabled = false
	cfgOff.Evaluation.LiveEnabled = false
	sOff := testGateway(t, cfgOff)
	rr := postLiveRun(t, sOff, "p1/m1", "reasoning", liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("disabled plane live run = %d, want 409", rr.Code)
	}

	// Plane enabled but live gated: mode=live is rejected before any network use.
	cfgNoLive := phaseHLiveConfig("", up1.srv.URL, up2.srv.URL)
	cfgNoLive.Evaluation.LiveEnabled = false
	sNoLive := testGateway(t, cfgNoLive)
	rr = postLiveRun(t, sNoLive, "p1/m1", "reasoning", liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("gated live run = %d, want 409 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "live evaluation is disabled") {
		t.Fatalf("gated live run must explain the gate: %s", rr.Body.String())
	}

	// Replay still works without live: safe default preserved.
	rr = adminDo(t, sNoLive, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("replay run on gated plane = %d: %s", rr.Code, rr.Body.String())
	}
	if up1.hitCount() != 0 || up2.hitCount() != 0 {
		t.Fatalf("gated live attempts touched the network: %d/%d", up1.hitCount(), up2.hitCount())
	}

	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	// Unknown modes are rejected.
	rr = postLiveRun(t, s, "p1/m1", "reasoning", nil, map[string]any{"mode": "simulate"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode = %d, want 400", rr.Code)
	}
	// Live without inputs is rejected.
	rr = postLiveRun(t, s, "p1/m1", "reasoning", nil, nil)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "inputs are required") {
		t.Fatalf("live without inputs = %d (%s)", rr.Code, rr.Body.String())
	}
	// Inputs in replay mode are rejected.
	rr = adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), map[string]any{"inputs": liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"})}))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "mode=live") {
		t.Fatalf("replay with inputs = %d (%s)", rr.Code, rr.Body.String())
	}
	// Artifacts in live mode are rejected.
	rr = postLiveRun(t, s, "p1/m1", "reasoning", liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}),
		map[string]any{"artifacts": codingOutcomes(t, true)})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "replay") {
		t.Fatalf("live with artifacts = %d (%s)", rr.Code, rr.Body.String())
	}
	// Inputs for unknown cases are rejected (live mode cannot invent coverage).
	rr = postLiveRun(t, s, "p1/m1", "reasoning", []map[string]any{{"case_id": "no-such-case", "prompt": "hello"}}, nil)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown suite case") {
		t.Fatalf("unknown case input = %d (%s)", rr.Code, rr.Body.String())
	}
	// Duplicate case inputs are rejected.
	dup := []map[string]any{
		{"case_id": "reasoning-multi-step-arithmetic", "prompt": "a"},
		{"case_id": "reasoning-multi-step-arithmetic", "prompt": "b"},
	}
	rr = postLiveRun(t, s, "p1/m1", "reasoning", dup, nil)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "duplicate") {
		t.Fatalf("duplicate input = %d (%s)", rr.Code, rr.Body.String())
	}
	// Empty prompts are rejected.
	rr = postLiveRun(t, s, "p1/m1", "reasoning", []map[string]any{{"case_id": "reasoning-multi-step-arithmetic", "prompt": "  "}}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt = %d", rr.Code)
	}
	if up1.hitCount() != 0 || up2.hitCount() != 0 {
		t.Fatalf("rejected payload touched the network: %d/%d", up1.hitCount(), up2.hitCount())
	}
}

// ---------------------------------------------------------------------------
// The selected physical deployment is called exactly once per case input —
// and nothing else is called.
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationCallsSelectedDeploymentExactlyOnce(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	rr := postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("live run = %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	run := mustMap(t, resp["run"], "run")
	if run["mode"] != "live" {
		t.Fatalf("run mode = %v", run["mode"])
	}
	if run["upstream_calls"] != float64(1) {
		t.Fatalf("upstream_calls = %v, want exactly 1", run["upstream_calls"])
	}
	// The physical deployment was called exactly once; every other physical
	// upstream saw zero traffic (no router fan-out, no hedging, no retry to
	// another deployment).
	if up1.hitCount() != 1 {
		t.Fatalf("selected deployment upstream hits = %d, want 1", up1.hitCount())
	}
	if up2.hitCount() != 0 {
		t.Fatalf("unselected provider was called %d times", up2.hitCount())
	}
	// The request was made by the production adapter with the deployment's
	// configured credential and carried the declared evaluation input.
	if auth := up1.lastAuth(); auth != "Bearer "+evalProviderKey {
		t.Fatalf("upstream auth = %q (the production credential path must be reused)", auth)
	}
	prompts := up1.allPrompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "LIVE_ANSWER:42") {
		t.Fatalf("upstream prompts = %#v", prompts)
	}
	// Only the case with a declared input was judged; the rest are missing
	// evidence, and honesty rules forbid a scorecard below min_samples.
	verdicts := mustMap(t, run["verdicts"], "verdicts")
	if verdicts["pass"] != float64(1) || verdicts["missing"] != float64(2) {
		t.Fatalf("verdicts = %v", verdicts)
	}
	if resp["scorecard_written"] != false || run["scoreable"] != false {
		t.Fatalf("single-sample run must not write a scorecard: %s", rr.Body.String())
	}
	// The run is the only thing the plane recorded; replay stats stay clean.
	stats := planeStats(t, s)
	if stats["live_runs_total"] != float64(1) || stats["upstream_calls_total"] != float64(1) {
		t.Fatalf("live stats = %v", stats)
	}
	if stats["runs_total"] != float64(1) {
		t.Fatalf("runs_total = %v", stats)
	}
}

// TestPhaseH_LiveExecutorUsesOnlyTheExistingAdapterStack is the structural
// guarantee behind "no second provider client stack": the live executor holds
// only the providers.Completer interface plus plain data. There is no
// net/http field of any shape it could use to build its own transport.
func TestPhaseH_LiveExecutorUsesOnlyTheExistingAdapterStack(t *testing.T) {
	typ := reflect.TypeOf(LiveEvaluationExecutor{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if strings.Contains(f.Type.PkgPath(), "net/http") {
			t.Fatalf("live executor field %s reaches net/http directly: %s", f.Name, f.Type)
		}
	}
	adapted, err := NewLiveEvaluationExecutor(nil, "model-a", nil)
	if err == nil || adapted != nil {
		t.Fatal("nil adapter must be rejected — no fallback client may exist")
	}
}

// ---------------------------------------------------------------------------
// Full live suite against the physical deployment -> provenance scorecard,
// and routing is byte-identical before and after.
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationScorecardAndRoutingUnchanged(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	candidatesBefore := s.rt.Candidates(routerRequirement("auto"))
	healthBefore := healthByDeployment(s.hm.Snapshot())

	rr := postLiveRun(t, s, "p1/m2", "reasoning",
		liveReasoningInputs(map[string]string{
			"reasoning-multi-step-arithmetic": "42",
			"reasoning-logic-deduction":       "carol",
			"reasoning-numeric-estimate":      "3.141",
		}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("live run = %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	run := mustMap(t, resp["run"], "run")
	if run["upstream_calls"] != float64(3) {
		t.Fatalf("upstream_calls = %v, want 3 (one per case)", run["upstream_calls"])
	}
	if up1.hitCount() != 3 || up2.hitCount() != 0 {
		t.Fatalf("upstream distribution p1=%d p2=%d, want 3/0", up1.hitCount(), up2.hitCount())
	}
	if run["scoreable"] != true || run["score"] != float64(1) {
		t.Fatalf("all-correct live answers must score 1: %v", run)
	}
	if resp["scorecard_written"] != true {
		t.Fatalf("live run must write a provenance scorecard: %s", rr.Body.String())
	}
	// The scorecard values all carry provenance from the live evaluation run.
	sc := mustMap(t, resp["scorecard"], "scorecard")
	if sc["deployment_id"] != "p1/m2" || sc["version"] != float64(1) {
		t.Fatalf("unexpected scorecard: %v", sc)
	}
	rr = adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/m2", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("scorecard detail = %d", rr.Code)
	}
	detail := mustMap(t, decodeJSON(t, rr)["scorecard"], "scorecard detail")
	values := detail["values"].([]any)
	if len(values) == 0 {
		t.Fatal("scorecard has no values")
	}
	for _, v := range values {
		row := v.(map[string]any)
		if row["provenance"] != "evaluation" || row["sample_count"] == nil || row["evaluated_at"] == nil {
			t.Fatalf("scorecard value without provenance: %v", row)
		}
	}
	// Routing result is identical before and after the live run + scorecard:
	// candidates, scores and health are untouched.
	candidatesAfter := s.rt.Candidates(routerRequirement("auto"))
	if !reflect.DeepEqual(candidatesBefore, candidatesAfter) {
		t.Fatalf("routing changed by live evaluation:\nbefore=%#v\nafter=%#v", candidatesBefore, candidatesAfter)
	}
	if after := healthByDeployment(s.hm.Snapshot()); !reflect.DeepEqual(healthBefore, after) {
		t.Fatalf("health changed by live evaluation:\nbefore=%#v\nafter=%#v", healthBefore, after)
	}
	// Provider-level health incidents are equally untouched.
	if len(s.hm.ProviderSnapshot()) != 0 {
		t.Fatalf("provider incidents recorded by live evaluation: %#v", s.hm.ProviderSnapshot())
	}
}

// ---------------------------------------------------------------------------
// Decision providers (local / policy / jev / hybrid chain) are never called
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationBypassesDecisionProviders(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	cfg := phaseHLiveConfig("", up1.srv.URL, up2.srv.URL)
	// Hybrid decision plane armed: if evaluation leaked into the decision
	// path, the counting providers below would record it.
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "external-policy"
	cfg.Decision.MaxProviderCalls = 3
	cfg.DecisionChains = []config.DecisionChainConfig{
		{ID: "external-policy", Steps: []config.DecisionChainStep{{Provider: "policy"}, {Provider: "jev-sim"}, {Provider: "local"}}},
	}
	s := testGateway(t, cfg)

	// Replace every decision provider with a counting double (last-wins).
	localCount := newCountingChainProvider("local", decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true}, nil)
	policyCount := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true}, nil)
	jevCount := newCountingChainProvider("jev-sim", decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true}, nil)
	s.decisionRegistry.Register(localCount)
	s.decisionRegistry.Register(policyCount)
	s.decisionRegistry.Register(jevCount)

	metricsBefore := s.decisionOrchestrator.MetricsSnapshot()
	providerStateBefore := s.decisionOrchestrator.ProviderState().Snapshot()

	rr := postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{
			"reasoning-multi-step-arithmetic": "42",
			"reasoning-logic-deduction":       "carol",
			"reasoning-numeric-estimate":      "3.141",
		}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("live run = %d: %s", rr.Code, rr.Body.String())
	}

	if got := localCount.Calls(); got != 0 {
		t.Fatalf("local DecisionProvider was called %d times", got)
	}
	if got := policyCount.Calls(); got != 0 {
		t.Fatalf("policy DecisionProvider was called %d times", got)
	}
	if got := jevCount.Calls(); got != 0 {
		t.Fatalf("jev DecisionProvider was called %d times", got)
	}
	if after := s.decisionOrchestrator.MetricsSnapshot(); !reflect.DeepEqual(metricsBefore, after) {
		t.Fatalf("decision metrics changed:\nbefore=%#v\nafter=%#v", metricsBefore, after)
	}
	if after := s.decisionOrchestrator.ProviderState().Snapshot(); !reflect.DeepEqual(providerStateBefore, after) {
		t.Fatalf("DecisionProvider health state changed:\nbefore=%#v\nafter=%#v", providerStateBefore, after)
	}
	// No routing/decision event was emitted by the evaluation.
	for _, ev := range s.bus.Snapshot() {
		if ev.DecisionProvider != "" || ev.DecisionAction != "" || ev.DecisionChainID != "" {
			t.Fatalf("evaluation emitted a decision-plane event: %#v", ev)
		}
		if ev.Kind == "route" || ev.Kind == "decision" {
			t.Fatalf("evaluation emitted a routing event: %#v", ev)
		}
	}
}

// ---------------------------------------------------------------------------
// Production health is untouched by live evaluation — success, HTTP 500 and
// timeout alike.
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationLeavesProductionHealthUnchangedOnSuccess(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	healthBefore := healthByDeployment(s.hm.Snapshot())
	providerBefore := s.hm.ProviderSnapshot()
	adapterBefore := providerStatsByID(s.reg.Stats())

	rr := postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{
			"reasoning-multi-step-arithmetic": "42",
			"reasoning-logic-deduction":       "carol",
			"reasoning-numeric-estimate":      "3.141",
		}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("live run = %d: %s", rr.Code, rr.Body.String())
	}
	if after := healthByDeployment(s.hm.Snapshot()); !reflect.DeepEqual(healthBefore, after) {
		t.Fatalf("model health changed (success):\nbefore=%#v\nafter=%#v", healthBefore, after)
	}
	if after := s.hm.ProviderSnapshot(); !reflect.DeepEqual(providerBefore, after) {
		t.Fatalf("provider health changed (success):\nbefore=%#v\nafter=%#v", providerBefore, after)
	}
	adapterAfter := providerStatsByID(s.reg.Stats())
	for id, before := range adapterBefore {
		after := adapterAfter[id]
		if before.ActiveRequests != after.ActiveRequests || after.ActiveRequests != 0 {
			t.Fatalf("provider %s active requests leaked: %d -> %d", id, before.ActiveRequests, after.ActiveRequests)
		}
		if before.CredentialsCooling != after.CredentialsCooling {
			t.Fatalf("provider %s credential cooldown changed: %d -> %d", id, before.CredentialsCooling, after.CredentialsCooling)
		}
	}
}

func TestPhaseH_LiveEvaluationLeavesProductionHealthUnchangedOnHTTP500(t *testing.T) {
	up1 := newFakeModel(t)
	up1.status.Store(500)
	up2 := newFakeModel(t)
	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	healthBefore := healthByDeployment(s.hm.Snapshot())
	providerBefore := s.hm.ProviderSnapshot()

	rr := postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{
			"reasoning-multi-step-arithmetic": "42",
			"reasoning-logic-deduction":       "carol",
			"reasoning-numeric-estimate":      "3.141",
		}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("500 run still answers the admin caller: %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	run := mustMap(t, resp["run"], "run")
	verdicts := mustMap(t, run["verdicts"], "verdicts")
	if verdicts["fail"] != float64(3) {
		t.Fatalf("HTTP 500 evidence must be recorded as honest failure: %v", verdicts)
	}
	if run["upstream_calls"] != float64(3) || up1.hitCount() != 3 {
		t.Fatalf("upstream calls = %v / hits = %d", run["upstream_calls"], up1.hitCount())
	}
	if after := healthByDeployment(s.hm.Snapshot()); !reflect.DeepEqual(healthBefore, after) {
		t.Fatalf("model health changed after HTTP 500:\nbefore=%#v\nafter=%#v", healthBefore, after)
	}
	if after := s.hm.ProviderSnapshot(); !reflect.DeepEqual(providerBefore, after) {
		t.Fatalf("provider health changed after HTTP 500:\nbefore=%#v\nafter=%#v", providerBefore, after)
	}
	// No credential cooldown tripped on a 500.
	for _, st := range s.reg.Stats() {
		if st.CredentialsCooling != 0 {
			t.Fatalf("provider %s credentials cooling after HTTP 500 evaluation", st.ID)
		}
	}
}

func TestPhaseH_LiveEvaluationLeavesProductionHealthUnchangedOnTimeout(t *testing.T) {
	up1 := newFakeModel(t)
	up1.delay.Store(400) // the model hangs; the case timeout must cut it
	up2 := newFakeModel(t)
	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	healthBefore := healthByDeployment(s.hm.Snapshot())
	providerBefore := s.hm.ProviderSnapshot()

	rr := postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}),
		map[string]any{"case_timeout_ms": 100})
	if rr.Code != http.StatusOK {
		t.Fatalf("timeout run still answers the admin caller: %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	run := mustMap(t, resp["run"], "run")
	verdicts := mustMap(t, run["verdicts"], "verdicts")
	if verdicts["error"] != float64(1) {
		t.Fatalf("timeout must be recorded as an error verdict: %v", verdicts)
	}
	if up1.hitCount() != 1 {
		t.Fatalf("timeout upstream hits = %d, want 1 (the attempt happened)", up1.hitCount())
	}
	if after := healthByDeployment(s.hm.Snapshot()); !reflect.DeepEqual(healthBefore, after) {
		t.Fatalf("model health changed after timeout:\nbefore=%#v\nafter=%#v", healthBefore, after)
	}
	if after := s.hm.ProviderSnapshot(); !reflect.DeepEqual(providerBefore, after) {
		t.Fatalf("provider health changed after timeout:\nbefore=%#v\nafter=%#v", providerBefore, after)
	}
	// Concurrency accounting is fully released after the timeout.
	for _, st := range s.reg.Stats() {
		if st.ActiveRequests != 0 {
			t.Fatalf("provider %s active requests leaked after timeout: %d", st.ID, st.ActiveRequests)
		}
	}
}

// ---------------------------------------------------------------------------
// Production cache and session affinity are untouched
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationDoesNotPopulateProductionCache(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	cfg := phaseHLiveConfig("", up1.srv.URL, up2.srv.URL)
	cfg.Cache.Enabled = true
	s := testGateway(t, cfg)

	// Populate the production cache through the real request path.
	prodBody := `{"model":"auto","max_tokens":16,"messages":[{"role":"user","content":"cache me"}]}`
	prodReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(prodBody))
	prodReq.Header.Set("Content-Type", "application/json")
	prodRR := httptest.NewRecorder()
	s.Handler().ServeHTTP(prodRR, prodReq)
	if prodRR.Code != http.StatusOK {
		t.Fatalf("production request = %d: %s", prodRR.Code, prodRR.Body.String())
	}
	cacheBefore := s.respCache.Stats()
	if cacheBefore.Entries != 1 {
		t.Fatalf("production cache not primed: %#v", cacheBefore)
	}

	rr := postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{
			"reasoning-multi-step-arithmetic": "42",
			"reasoning-logic-deduction":       "carol",
			"reasoning-numeric-estimate":      "3.141",
		}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("live run = %d: %s", rr.Code, rr.Body.String())
	}
	if after := s.respCache.Stats(); !reflect.DeepEqual(cacheBefore, after) {
		t.Fatalf("production cache changed:\nbefore=%#v\nafter=%#v", cacheBefore, after)
	}
}

func TestPhaseH_LiveEvaluationDoesNotTouchSessionAffinity(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	// Establish a production session pin through the real request path.
	prodBody := `{"model":"auto","max_tokens":16,"messages":[{"role":"user","content":"pin me"}]}`
	prodReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(prodBody))
	prodReq.Header.Set("Content-Type", "application/json")
	prodReq.Header.Set("X-Session-ID", "sess-live-eval")
	prodRR := httptest.NewRecorder()
	s.Handler().ServeHTTP(prodRR, prodReq)
	if prodRR.Code != http.StatusOK {
		t.Fatalf("production request = %d: %s", prodRR.Code, prodRR.Body.String())
	}
	pinned := prodRR.Header().Get("X-Gateway-Deployment")
	if pinned == "" {
		t.Fatal("production request did not report its deployment")
	}
	sessionsBefore := s.rt.SessionCount()
	if sessionsBefore != 1 {
		t.Fatalf("session pins = %d, want 1", sessionsBefore)
	}
	if got := s.rt.PinnedDeploymentID(router.Requirement{Model: "auto", SessionKey: "sess-live-eval"}); got != pinned {
		t.Fatalf("pin = %q, want %q", got, pinned)
	}

	// Live-evaluate the pinned deployment AND another one: affinity must not move.
	for _, dep := range []string{"p1/m1", "p1/m2"} {
		rr := postLiveRun(t, s, dep, "reasoning",
			liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("live run on %s = %d: %s", dep, rr.Code, rr.Body.String())
		}
	}
	if after := s.rt.PinnedDeploymentID(router.Requirement{Model: "auto", SessionKey: "sess-live-eval"}); after != pinned {
		t.Fatalf("session affinity moved: %q -> %q", pinned, after)
	}
	if after := s.rt.SessionCount(); after != sessionsBefore {
		t.Fatalf("session table changed: %d -> %d", sessionsBefore, after)
	}
}

// ---------------------------------------------------------------------------
// Privacy: the dataset canary may reach the selected model, but never escapes
// into metrics, admin surfaces, events, traces or health state.
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationCanaryPrivacy(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	statePath := filepath.Join(t.TempDir(), "evaluation-state.json")
	s := testGateway(t, phaseHLiveConfig(statePath, up1.srv.URL, up2.srv.URL))

	// The canary is evaluation input — and the mock even echoes it back as
	// output (worst case: the model repeats the dataset).
	suite, _ := eval.LookupSuite("reasoning")
	inputs := make([]map[string]any, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		inputs = append(inputs, map[string]any{
			"case_id": c.ID,
			"prompt":  "LIVE_ANSWER:" + evalCanary + "\n" + evalCanary + " study material. Reply with the study token.",
		})
	}
	rr := postLiveRun(t, s, "p1/m1", "reasoning", inputs, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("live run = %d: %s", rr.Code, rr.Body.String())
	}
	// Sanity: the canary really was sent to the explicitly selected model.
	joined := strings.Join(up1.allPrompts(), "\n")
	if !strings.Contains(joined, evalCanary) {
		t.Fatal("canary never reached the selected deployment (test broken)")
	}

	// 1. The run response itself carries only verdicts and bounded categories.
	assertNoCanary := func(where, blob string) {
		t.Helper()
		if strings.Contains(blob, evalCanary) {
			t.Fatalf("canary leaked into %s:\n%s", where, blob)
		}
	}
	assertNoCanary("run response", rr.Body.String())

	// 2. Metrics.
	rr = adminDo(t, s, http.MethodGet, "/metrics", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics = %d", rr.Code)
	}
	assertNoCanary("metrics", rr.Body.String())

	// 3. The full admin snapshot (scorecards, evaluation, events, health...).
	rr = adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot = %d", rr.Code)
	}
	assertNoCanary("admin snapshot", rr.Body.String())

	// 4. Routing/decision events.
	events, _ := json.Marshal(s.bus.Snapshot())
	assertNoCanary("events", string(events))

	// 5. Model health state.
	healthJSON, _ := json.Marshal(s.hm.Snapshot())
	assertNoCanary("model health", string(healthJSON))
	providerHealthJSON, _ := json.Marshal(s.hm.ProviderSnapshot())
	assertNoCanary("provider health", string(providerHealthJSON))

	// 6. Evaluation records and decision plane surfaces.
	rr = adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?limit=10", "")
	assertNoCanary("run history", rr.Body.String())
	rr = adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/m1", "")
	assertNoCanary("scorecard detail", rr.Body.String())
	traces, _ := json.Marshal(s.decisionOrchestrator.MetricsSnapshot())
	assertNoCanary("decision traces/metrics", string(traces))

	// 7. The durable evaluation state file.
	if data, err := os.ReadFile(statePath); err != nil {
		t.Fatalf("state file missing: %v", err)
	} else {
		assertNoCanary("evaluation state file", string(data))
	}
}

// The provider credential authenticates the evaluation request upstream and
// never appears in evaluation records, scorecards, events, metrics, admin
// surfaces or errors — even when the upstream echoes it in an error body.
func TestPhaseH_LiveEvaluationProviderKeyNeverLeaks(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	statePath := filepath.Join(t.TempDir(), "evaluation-state.json")
	s := testGateway(t, phaseHLiveConfig(statePath, up1.srv.URL, up2.srv.URL))

	// First: one successful live run proves the real adapter authenticated
	// with the configured credential.
	rr := postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("live run = %d: %s", rr.Code, rr.Body.String())
	}
	if auth := up1.lastAuth(); auth != "Bearer "+evalProviderKey {
		t.Fatalf("upstream did not receive the provider credential (test broken): %q", auth)
	}

	// Now the upstream answers 500 and echoes the raw key in its error body.
	up1.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		up1.hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":{"message":"model on fire using key %s"}}`, evalProviderKey)
	})

	healthBefore := healthByDeployment(s.hm.Snapshot())
	rr = postLiveRun(t, s, "p1/m1", "reasoning",
		liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("failure run = %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	run := mustMap(t, resp["run"], "run")
	verdicts := mustMap(t, run["verdicts"], "verdicts")
	if verdicts["fail"] != float64(1) {
		t.Fatalf("upstream HTTP 500 must produce an honest fail verdict: %v", verdicts)
	}

	assertNoKey := func(where, blob string) {
		t.Helper()
		if strings.Contains(blob, evalProviderKey) {
			t.Fatalf("provider key leaked into %s:\n%s", where, blob)
		}
	}
	assertNoKey("run response", rr.Body.String())

	rr = adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?limit=10", "")
	assertNoKey("evaluation records", rr.Body.String())
	rr = adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	assertNoKey("scorecards", rr.Body.String())
	rr = adminDo(t, s, http.MethodGet, "/metrics", "")
	assertNoKey("metrics", rr.Body.String())
	rr = adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
	assertNoKey("admin snapshot", rr.Body.String())
	events, _ := json.Marshal(s.bus.Snapshot())
	assertNoKey("events", string(events))
	if data, err := os.ReadFile(statePath); err != nil {
		t.Fatalf("state file missing: %v", err)
	} else {
		assertNoKey("evaluation state file", string(data))
	}
	// Health stays honest: 500s from live evaluation never cool the credential
	// and never mark deployment/provider health.
	if after := healthByDeployment(s.hm.Snapshot()); !reflect.DeepEqual(healthBefore, after) {
		t.Fatalf("model health changed after key-echoing 500:\nbefore=%#v\nafter=%#v", healthBefore, after)
	}
	for _, st := range s.reg.Stats() {
		if st.CredentialsCooling != 0 {
			t.Fatalf("provider %s credential cooled by live evaluation failure", st.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency: parallel live runs against the same adapter stay race-free and
// the production plane stays observationally identical.
// ---------------------------------------------------------------------------

func TestPhaseH_LiveEvaluationConcurrentRunsRaceFree(t *testing.T) {
	up1 := newFakeModel(t)
	up2 := newFakeModel(t)
	s := testGateway(t, phaseHLiveConfig("", up1.srv.URL, up2.srv.URL))

	healthBefore := healthByDeployment(s.hm.Snapshot())
	const workers = 8
	const runsEach = 6
	var wg sync.WaitGroup
	errs := make(chan error, workers*runsEach)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < runsEach; i++ {
				rr := postLiveRun(t, s, "p1/m1", "reasoning",
					liveReasoningInputs(map[string]string{"reasoning-multi-step-arithmetic": "42"}), nil)
				if rr.Code != http.StatusOK {
					errs <- fmt.Errorf("worker %d run %d = %d", w, i, rr.Code)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got, want := up1.hitCount(), workers*runsEach; got != want {
		t.Fatalf("upstream hits = %d, want %d", got, want)
	}
	if after := healthByDeployment(s.hm.Snapshot()); !reflect.DeepEqual(healthBefore, after) {
		t.Fatalf("concurrent live evaluation changed health")
	}
	stats := planeStats(t, s)
	if stats["live_runs_total"] != float64(workers*runsEach) {
		t.Fatalf("live_runs_total = %v, want %d", stats["live_runs_total"], workers*runsEach)
	}
	if stats["upstream_calls_total"] != float64(workers*runsEach) {
		t.Fatalf("upstream_calls_total = %v, want %d", stats["upstream_calls_total"], workers*runsEach)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: the live evaluation core — one real completion through the
// production adapter, and one full runner pass over the reasoning suite.
// (Measured below the admin endpoint on purpose: the admin plane is token
// bucket rate-limited by design, which is a safety control, not the hot path.)
// ---------------------------------------------------------------------------

func benchLiveExecutor(tb testing.TB) (*LiveEvaluationExecutor, eval.Runner, eval.Suite) {
	tb.Helper()
	up := newFakeModel(tb)
	adapter, err := providers.NewAdapter(config.ProviderConfig{
		ID: "p1", Type: "openai_compatible", BaseURL: up.srv.URL, AuthMode: "none", Enabled: true,
	}, 5*time.Second)
	if err != nil {
		tb.Fatal(err)
	}
	suite, _ := eval.LookupSuite("reasoning")
	inputs := make([]eval.Input, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		inputs = append(inputs, eval.Input{CaseID: c.ID, Prompt: "LIVE_ANSWER:42\nAnswer with the value."})
	}
	exec, err := NewLiveEvaluationExecutor(adapter, "model-a", inputs)
	if err != nil {
		tb.Fatal(err)
	}
	return exec, *eval.NewRunner(), suite
}

func BenchmarkLiveEvaluationExecutor(b *testing.B) {
	exec, _, suite := benchLiveExecutor(b)
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := exec.Execute(ctx, suite.Cases[0])
		if err != nil {
			b.Fatal(err)
		}
		if out.Output != "42" {
			b.Fatalf("output = %q", out.Output)
		}
	}
}

func BenchmarkLiveEvaluationRun(b *testing.B) {
	exec, runner, suite := benchLiveExecutor(b)
	req := eval.Request{
		Mode: eval.ModeLive, DeploymentID: "p1/m1", ProviderID: "p1",
		Model: "model-a", SuiteID: suite.ID, CaseTimeoutMS: 5000,
	}
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res, err := runner.RunWithExecutor(ctx, req, exec)
		if err != nil {
			b.Fatal(err)
		}
		if !res.Scoreable {
			b.Fatalf("run not scoreable: %+v", res)
		}
	}
}
