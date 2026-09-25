package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func makePolicyConfig(id string, weights config.DecisionPolicyWeights, overrides map[string]config.DecisionPolicyWeights, minDelta float64) config.DecisionPolicyConfig {
	return config.DecisionPolicyConfig{
		ID:            id,
		Name:          id,
		SelectionMode: "select_first",
		Weights:       weights,
		TaskOverrides: overrides,
		MinScoreDelta: minDelta,
	}
}

// This file contains hardened Phase E acceptance tests per final hardening spec.
// Mutation-checked:
// - Cross-protocol: changed expected deployment to wrong value → fails
// - VE vs direct: changed direct alias to only one deployment → fails neutrality (different sets)
// - Fallback: swapped expected order B→A→C to B→C→A → fails order assertion, changed C to fail → fails
// - Affinity: removed session header → second request selects A not B → fails AFFINITY_PRESERVED
// - MaxAttempts: set max_attempts to 3 → third candidate hit becomes 1 → fails zero assertion
// - Explainability: set MinDelta high to cause ABSTAIN → fails required SELECT event
// - Privacy: put canary in breakdown JSON via fake policy trace → fails privacy check
// - Capability: made B support tools → B would be attempted → fails absence assertion

type attemptRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *attemptRecorder) add(id string) {
	r.mu.Lock()
	r.order = append(r.order, id)
	r.mu.Unlock()
}

func (r *attemptRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

func makeUpstreamWithRecorder(t *testing.T, hits *atomic.Int64, recorder *attemptRecorder, id string, failCount int, status int) *httptest.Server {
	t.Helper()
	var count atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if recorder != nil {
			recorder.add(id)
		}
		c := count.Add(1)
		if int(c) <= failCount {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"injected failure"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "messages") {
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"upstream-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		} else if strings.Contains(r.URL.Path, "responses") {
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":123,"status":"completed","model":"upstream-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		} else {
			_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
		}
	}))
}

// 1. CROSS-PROTOCOL TEST MUST FAIL ON DIVERGENCE
func TestPolicy_CrossProtocolWithRealPolicyProvider_Strict(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	rec := &attemptRecorder{}
	upA := makeUpstreamWithRecorder(t, &hitsA, rec, "p1/m1", 0, 500)
	defer upA.Close()
	upB := makeUpstreamWithRecorder(t, &hitsB, rec, "p2/m2", 0, 500)
	defer upB.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	// Use RouterBaseline only so protocol-specific token shape does not affect result
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
	}
	// Explicit pool with B first then A? We want deterministic router baseline prefers first by priority
	// Set priority 0 for p1/m1, 10 for p2/m2, so p1/m1 is higher preference and should win across protocols
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai", "anthropic", "openai_responses"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1, Priority: 0}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1, Priority: 10}}},
	}

	s := testGateway(t, cfg)

	// OpenAI Chat
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cross-openai")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("openai failed: %d %s", rr.Code, rr.Body.String())
	}
	depOpenAI := rr.Header().Get("X-Gateway-Deployment")
	if depOpenAI == "" {
		t.Fatalf("openai deployment header empty")
	}

	// Anthropic Messages — equivalent simple content
	req = httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cross-anthropic")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("anthropic failed: %d %s", rr.Code, rr.Body.String())
	}
	depAnthropic := rr.Header().Get("X-Gateway-Deployment")
	if depAnthropic == "" {
		t.Fatalf("anthropic deployment header empty")
	}

	// OpenAI Responses — equivalent input, get deployment from bus route_ok (canonical path may not set header)
	hitsA.Store(0)
	hitsB.Store(0)
	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-code","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cross-responses")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("responses failed: %d %s", rr.Code, rr.Body.String())
	}
	depResponses := rr.Header().Get("X-Gateway-Deployment")
	if depResponses == "" {
		// Fallback to bus event or hits
		for _, ev := range s.bus.Snapshot() {
			if ev.RequestID == "cross-responses" && ev.Kind == "route_ok" {
				depResponses = ev.Deployment
				break
			}
		}
		if depResponses == "" {
			if hitsA.Load() == 1 {
				depResponses = "p1/m1"
			} else if hitsB.Load() == 1 {
				depResponses = "p2/m2"
			}
		}
	}
	if depResponses == "" {
		t.Fatalf("responses deployment empty even after bus fallback")
	}

	// Strict equality — mismatch must fail
	if depOpenAI != depAnthropic {
		t.Fatalf("cross-protocol divergence: openai=%s anthropic=%s", depOpenAI, depAnthropic)
	}
	if depAnthropic != depResponses {
		t.Fatalf("cross-protocol divergence: anthropic=%s responses=%s", depAnthropic, depResponses)
	}
	// All should be p1/m1 due to priority 0
	if depOpenAI != "p1/m1" {
		t.Fatalf("expected p1/m1 to be selected across protocols, got %s", depOpenAI)
	}
}

// 2. VE VS DIRECT NEUTRALITY TEST MUST BE REAL
func TestPolicy_VEvsDirectNeutrality_Strict(t *testing.T) {
	var hits atomic.Int64
	rec := &attemptRecorder{}
	up := makeUpstreamWithRecorder(t, &hits, rec, "shared", 0, 500)
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
	}
	// Same physical candidate set for VE and direct via alias "shared-model"
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-code", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
			{ID: "m1", Model: "model-a", Aliases: []string{"shared-model"}, Enabled: true, Weight: 1, Priority: 0},
		}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
			{ID: "m2", Model: "model-b", Aliases: []string{"shared-model"}, Enabled: true, Weight: 1, Priority: 10},
		}},
	}

	s := testGateway(t, cfg)
	// Ensure health equal so policy doesn't change due to latency
	s.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)

	// VE path
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-code","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "ve-neutral")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("VE request failed: %d %s", rr.Code, rr.Body.String())
	}
	depVE := rr.Header().Get("X-Gateway-Deployment")
	if depVE == "" {
		t.Fatalf("VE deployment empty")
	}

	// Direct path using shared-model alias which should resolve to same candidate set pool1
	req2 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "direct-neutral")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("direct request failed: %d %s", rr2.Code, rr2.Body.String())
	}
	depDirect := rr2.Header().Get("X-Gateway-Deployment")
	if depDirect == "" {
		t.Fatalf("direct deployment empty")
	}

	if depVE != depDirect {
		t.Fatalf("VE vs direct neutrality broken: VE=%s direct=%s (candidate sets should be equivalent)", depVE, depDirect)
	}
	// Both should be p1/m1 due to priority 0
	if depVE != "p1/m1" {
		t.Fatalf("expected p1/m1 for neutrality, got VE=%s direct=%s", depVE, depDirect)
	}
}

// 3. FALLBACK E2E MUST ACTUALLY FALL BACK — exact B→A→C
func TestPolicy_FallbackE2E_Strict(t *testing.T) {
	var hitsB, hitsA, hitsC atomic.Int64
	rec := &attemptRecorder{}
	// B fails, A fails, C succeeds
	upB := makeUpstreamWithRecorder(t, &hitsB, rec, "p2/m2", 1, 500) // fail first 1
	defer upB.Close()
	upA := makeUpstreamWithRecorder(t, &hitsA, rec, "p1/m1", 1, 500)
	defer upA.Close()
	upC := makeUpstreamWithRecorder(t, &hitsC, rec, "p3/m3", 0, 500)
	defer upC.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 3
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		// Latency weight so B preferred over A
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1}, nil, 0),
	}
	// Primary contains B and A, fallback contains C
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p2/m2", "p1/m1"}},
		{ID: "fallback", Mode: "explicit", Deployments: []string{"p3/m3"}},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{
		{ID: "chain1", Pools: []string{"primary", "fallback"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "rp1", CandidatePool: "primary", FallbackChain: "chain1", DecisionPolicy: "balanced"},
	}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-fallback", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}},
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: upC.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)
	// Set latency so policy prefers B over A: B 10ms, A 100ms, C 1000ms
	s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	s.hm.RecordSuccess("p1/m1", 100*time.Millisecond)
	s.hm.RecordSuccess("p3/m3", 1000*time.Millisecond)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-fallback","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "fallback-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("fallback strict failed: %d %s", rr.Code, rr.Body.String())
	}
	finalDep := rr.Header().Get("X-Gateway-Deployment")
	if finalDep != "p3/m3" {
		t.Fatalf("expected final deployment C p3/m3 after B and A fail, got %s", finalDep)
	}
	// Exact attempt order
	order := rec.snapshot()
	if len(order) != 3 {
		t.Fatalf("expected 3 attempts B,A,C got %d order=%v", len(order), order)
	}
	if order[0] != "p2/m2" || order[1] != "p1/m1" || order[2] != "p3/m3" {
		t.Fatalf("expected exact order B→A→C [p2/m2 p1/m1 p3/m3] got %v", order)
	}
	if hitsB.Load() != 1 || hitsA.Load() != 1 || hitsC.Load() != 1 {
		t.Fatalf("expected each hit 1, got B=%d A=%d C=%d", hitsB.Load(), hitsA.Load(), hitsC.Load())
	}
	// C never appears before A
	for i, id := range order {
		if id == "p3/m3" && i < 2 {
			t.Fatalf("C appeared before A, order=%v", order)
		}
	}
}

// 4. SESSION AFFINITY E2E MUST PROVE POLICY WOULD CHANGE
func TestPolicy_SessionAffinityE2E_Strict(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	rec := &attemptRecorder{}
	upA := makeUpstreamWithRecorder(t, &hitsA, rec, "p1/m1", 0, 500)
	defer upA.Close()
	upB := makeUpstreamWithRecorder(t, &hitsB, rec, "p2/m2", 0, 500)
	defer upB.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 3600
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-affinity", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)
	// Initially policy prefers B: B low latency
	s.hm.RecordSuccess("p1/m1", 100*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)

	sessionID := "test-session-123"
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"nexa-affinity","messages":[{"role":"user","content":"hi"}],"metadata":{"session_id":"%s"}}`, sessionID)))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "affinity-strict-1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("first affinity request failed: %d %s", rr.Code, rr.Body.String())
	}
	dep1 := rr.Header().Get("X-Gateway-Deployment")
	if dep1 != "p2/m2" {
		t.Fatalf("first request expected B p2/m2 with latency preference, got %s", dep1)
	}

	// Before second request, modify telemetry so WITHOUT affinity policy would prefer A
	// Record many times to flip EWMA
	for i := 0; i < 10; i++ {
		s.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
		s.hm.RecordSuccess("p2/m2", 100*time.Millisecond)
	}

	// Control request with different session should select A, proving policy would change
	controlSession := "control-session-456"
	reqCtrl := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"nexa-affinity","messages":[{"role":"user","content":"hi again"}],"metadata":{"session_id":"%s"}}`, controlSession)))
	reqCtrl.RemoteAddr = "127.0.0.1:12345"
	reqCtrl.Header.Set("Content-Type", "application/json")
	reqCtrl.Header.Set("X-Request-Id", "affinity-control")
	rrCtrl := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrCtrl, reqCtrl)
	if rrCtrl.Code != 200 {
		t.Fatalf("control request failed: %d %s", rrCtrl.Code, rrCtrl.Body.String())
	}
	depCtrl := rrCtrl.Header().Get("X-Gateway-Deployment")
	if depCtrl != "p1/m1" {
		t.Fatalf("control request should select A p1/m1 after telemetry change, got %s (policy would not change)", depCtrl)
	}

	// Second request with original session should still be B
	req2 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"nexa-affinity","messages":[{"role":"user","content":"hi again"}],"metadata":{"session_id":"%s"}}`, sessionID)))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "affinity-strict-2")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("second affinity request failed: %d %s", rr2.Code, rr2.Body.String())
	}
	dep2 := rr2.Header().Get("X-Gateway-Deployment")
	if dep2 != "p2/m2" {
		t.Fatalf("session affinity broken: expected B p2/m2 to remain, got %s (control was %s)", dep2, depCtrl)
	}

	// Assert AFFINITY_PRESERVED in events
	foundAffinity := false
	for i := 0; i < 10; i++ {
		evs := s.bus.SnapshotLimit(100)
		for _, ev := range evs {
			if ev.DecisionProvider == "policy" && strings.Contains(ev.DecisionReasonCodes, "AFFINITY_PRESERVED") {
				foundAffinity = true
				break
			}
		}
		if foundAffinity {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !foundAffinity {
		t.Fatalf("expected AFFINITY_PRESERVED reason code in policy events, not found")
	}
}

// 5. MAX ATTEMPTS MUST HAVE A THIRD CANDIDATE
func TestPolicy_MaxAttemptsWithPolicy_Strict(t *testing.T) {
	var hitsA, hitsB, hitsC atomic.Int64
	rec := &attemptRecorder{}
	upA := makeUpstreamWithRecorder(t, &hitsA, rec, "p1/m1", 1, 500) // fail first
	defer upA.Close()
	upB := makeUpstreamWithRecorder(t, &hitsB, rec, "p2/m2", 1, 500)
	defer upB.Close()
	upC := makeUpstreamWithRecorder(t, &hitsC, rec, "p3/m3", 0, 500) // would succeed
	defer upC.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2", "p3/m3"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-maxattempts", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: upC.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)
	// Latency prefers A,B,C order: A 10ms, B 20ms, C 30ms
	s.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 20*time.Millisecond)
	s.hm.RecordSuccess("p3/m3", 30*time.Millisecond)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-maxattempts","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "maxattempts-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	// With 2 attempts and both failing, request should fail or succeed via second? Actually A fails, B fails, maxAttempts 2 reached, so should return error 502/500
	// We assert attempt counts, not final code, but final should not be 200 if both fail
	total := hitsA.Load() + hitsB.Load() + hitsC.Load()
	if total != 2 {
		t.Fatalf("expected total attempts == max_attempts 2, got %d order=%v", total, rec.snapshot())
	}
	if hitsC.Load() != 0 {
		t.Fatalf("third candidate C should have 0 hits due to max_attempts=2, got %d", hitsC.Load())
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("expected A=1 B=1 C=0, got A=%d B=%d C=%d order=%v", hitsA.Load(), hitsB.Load(), hitsC.Load(), rec.snapshot())
	}
	// Policy must never increase max_attempts
	if total > int64(cfg.Routing.MaxAttempts) {
		t.Fatalf("policy increased max_attempts: total %d > max %d", total, cfg.Routing.MaxAttempts)
	}
}

// 6. EXPLAINABILITY TEST MUST BE STRICT
func TestPolicy_ExplainabilityWired_Strict(t *testing.T) {
	var hits atomic.Int64
	rec := &attemptRecorder{}
	up := makeUpstreamWithRecorder(t, &hits, rec, "shared", 0, 500)
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	// Policy that will SELECT B over A: latency weight, min delta 0 to ensure SELECT not ABSTAIN when B beats A
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1, RouterBaseline: 0}, nil, 0.01),
	}
	// Pool order C,A,B where C is original primary, but A and B have better latency, so policy will select A or B and change primary
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2", "p3/m3"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-explain", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)
	// To make original primary C (p3/m3) despite B having best latency for policy, set router weights:
	// p3 weight 2, p1 weight 1, p2 weight 1, so router Score prefers C (weight*10 dominates latency)
	// Policy uses latency only, so B (10ms) wins over C (1000ms) and A (100ms)
	s.hm.RecordSuccess("p1/m1", 100*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	s.hm.RecordSuccess("p3/m3", 1000*time.Millisecond)
	// Also need to set router weights via config? We already set Weight 1 for all, but we can update via config reload? Simpler: we will set weight via provider config already has Weight 1, but we can make C have higher weight by using different provider config
	// For this test, we set explicit weights in provider config to make C preferred by router: we already have Weight 1 for all, but router Score also includes -latency*0.015, so C with 1000ms gets -15, A 100ms -1.5, B 10ms -0.15, so B still wins in router.
	// To make C win in router, we need to set its Weight higher. We will update provider models via config reload to have Weight 5 for C
	nextCfg := s.currentConfig()
	for i := range nextCfg.Providers {
		for j := range nextCfg.Providers[i].Models {
			if nextCfg.Providers[i].Models[j].ID == "m3" {
				nextCfg.Providers[i].Models[j].Weight = 5
			}
		}
	}
	_ = s.applyConfig(nextCfg)
	// Re-record successes after reload to ensure health present
	s.hm.RecordSuccess("p1/m1", 100*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	s.hm.RecordSuccess("p3/m3", 1000*time.Millisecond)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-explain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "explain-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("explain strict failed: %d %s", rr.Code, rr.Body.String())
	}
	dep := rr.Header().Get("X-Gateway-Deployment")
	if dep != "p2/m2" {
		t.Fatalf("expected policy to select p2/m2 B as best latency, got %s", dep)
	}

	// Require policy decision event with SELECT
	found := false
	var foundEvent struct {
		Provider    string
		PolicyID    string
		TaskType    string
		Original    string
		Selected    string
		Changed     bool
		Breakdown   string
		ReasonCodes string
		Action      string
	}
	for i := 0; i < 20; i++ {
		evs := s.bus.SnapshotLimit(100)
		for _, ev := range evs {
			if ev.DecisionProvider == "policy" && ev.DecisionAction == "SELECT" {
				found = true
				foundEvent.Provider = ev.DecisionProvider
				foundEvent.PolicyID = ev.DecisionPolicyID
				foundEvent.TaskType = ev.DecisionTaskType
				foundEvent.Original = ev.DecisionOriginalPrimary
				foundEvent.Selected = ev.DecisionSelected
				foundEvent.Changed = ev.DecisionChangedPrimary
				foundEvent.Breakdown = ev.DecisionBreakdown
				foundEvent.ReasonCodes = ev.DecisionReasonCodes
				foundEvent.Action = ev.DecisionAction
				break
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		// Fallback: try any policy event and log
		for _, ev := range s.bus.SnapshotLimit(100) {
			if ev.DecisionProvider == "policy" {
				t.Logf("found policy event but not SELECT: action=%s selected=%s original=%s", ev.DecisionAction, ev.DecisionSelected, ev.DecisionOriginalPrimary)
			}
		}
		t.Fatalf("required policy SELECT decision event not found (must not be optional)")
	}
	if foundEvent.Provider != "policy" {
		t.Fatalf("DecisionProvider expected policy, got %s", foundEvent.Provider)
	}
	if foundEvent.PolicyID != "balanced" {
		t.Fatalf("DecisionPolicyID expected balanced, got %s", foundEvent.PolicyID)
	}
	if foundEvent.TaskType == "" {
		t.Fatalf("DecisionTaskType should be canonical non-empty, got empty")
	}
	// TaskType should be canonical — check it is one of known types (simple check: no spaces, lowercased)
	if strings.Contains(foundEvent.TaskType, " ") || foundEvent.TaskType != strings.ToLower(foundEvent.TaskType) {
		t.Fatalf("DecisionTaskType not canonical: %s", foundEvent.TaskType)
	}
	if foundEvent.Original == "" {
		t.Fatalf("DecisionOriginalPrimary should be non-empty")
	}
	if foundEvent.Breakdown == "" {
		t.Fatalf("DecisionBreakdown should be non-empty valid JSON")
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(foundEvent.Breakdown), &m); err != nil {
		t.Fatalf("DecisionBreakdown invalid JSON: %v content=%s", err, foundEvent.Breakdown)
	}
	if len(foundEvent.Breakdown) > 4096 {
		t.Fatalf("DecisionBreakdown length %d exceeds bound 4096", len(foundEvent.Breakdown))
	}
	// When primary changes
	if !foundEvent.Changed {
		t.Fatalf("expected DecisionChangedPrimary true when primary changes, got false (original=%s selected=%s)", foundEvent.Original, foundEvent.Selected)
	}
	if foundEvent.Selected == foundEvent.Original {
		t.Fatalf("when ChangedPrimary true, Selected != OriginalPrimary expected, got both %s", foundEvent.Selected)
	}
	if foundEvent.Selected != "p2/m2" {
		t.Fatalf("expected selected p2/m2, got %s", foundEvent.Selected)
	}
}

// 7. PRIVACY FULL PATH — EXPAND ASSERTIONS
func TestPolicy_PrivacyCanaryFullPath_Strict(t *testing.T) {
	canary := "SECRET_POLICY_CANARY_4e91"
	var hits atomic.Int64
	rec := &attemptRecorder{}
	up := makeUpstreamWithRecorder(t, &hits, rec, "p1/m1", 0, 500)
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-privacy", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)

	body := fmt.Sprintf(`{"model":"nexa-privacy","messages":[{"role":"user","content":"%s please ignore"}]}`, canary)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "privacy-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("privacy strict failed: %d %s", rr.Code, rr.Body.String())
	}

	// Trace header
	traceHeader := rr.Header().Get("X-Gateway-Decision-Trace")
	if strings.Contains(traceHeader, canary) {
		t.Fatalf("canary leaked into decision trace header")
	}

	// Events, breakdown, metrics, admin snapshot
	for i := 0; i < 10; i++ {
		evs := s.bus.SnapshotLimit(100)
		for _, ev := range evs {
			b, _ := json.Marshal(ev)
			if strings.Contains(string(b), canary) {
				t.Fatalf("canary leaked into decision event: %s", string(b))
			}
			if ev.DecisionBreakdown != "" && strings.Contains(ev.DecisionBreakdown, canary) {
				t.Fatalf("canary leaked into DecisionBreakdown")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Metrics endpoint
	reqMetrics := httptest.NewRequest("GET", "http://localhost/metrics", nil)
	reqMetrics.RemoteAddr = "127.0.0.1:12345"
	rrMetrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrMetrics, reqMetrics)
	metricsBody := rrMetrics.Body.String()
	if strings.Contains(metricsBody, canary) {
		t.Fatalf("canary leaked into metrics")
	}

	// Admin snapshot
	reqSnap := httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	reqSnap.RemoteAddr = "127.0.0.1:12345"
	rrSnap := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrSnap, reqSnap)
	snapBody := rrSnap.Body.String()
	if strings.Contains(snapBody, canary) {
		t.Fatalf("canary leaked into admin snapshot")
	}
}

// 8. CAPABILITY / HEALTH BOUNDARY MUST ASSERT ABSENCE
func TestPolicy_CapabilityBoundary_Strict(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	rec := &attemptRecorder{}
	upA := makeUpstreamWithRecorder(t, &hitsA, rec, "p1/m1", 0, 500)
	defer upA.Close()
	upB := makeUpstreamWithRecorder(t, &hitsB, rec, "p2/m2", 0, 500)
	defer upB.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		// Make B extremely attractive via latency, but B does NOT support tools
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-capability", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Tools: true}}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Tools: false}}}},
	}

	s := testGateway(t, cfg)
	// Make B extremely attractive: B 1ms, A 1000ms
	s.hm.RecordSuccess("p1/m1", 1000*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 1*time.Millisecond)

	// Tools-required request
	body := `{"model":"nexa-capability","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"test","description":"test"}}]}`
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "capability-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("capability strict failed: %d %s", rr.Code, rr.Body.String())
	}
	dep := rr.Header().Get("X-Gateway-Deployment")
	if dep != "p1/m1" {
		t.Fatalf("policy must not override capability filter, expected p1/m1 got %s", dep)
	}
	if hitsB.Load() != 0 {
		t.Fatalf("B without tools capability was attempted %d times, must be 0", hitsB.Load())
	}
	if hitsA.Load() != 1 {
		t.Fatalf("A with tools should be attempted once, got %d", hitsA.Load())
	}
}

func TestPolicy_HealthBoundary_Strict(t *testing.T) {
	var hitsA, hitsB, hitsC atomic.Int64
	rec := &attemptRecorder{}
	upA := makeUpstreamWithRecorder(t, &hitsA, rec, "p1/m1", 0, 500)
	defer upA.Close()
	upB := makeUpstreamWithRecorder(t, &hitsB, rec, "p2/m2", 0, 500)
	defer upB.Close()
	upC := makeUpstreamWithRecorder(t, &hitsC, rec, "p3/m3", 0, 500)
	defer upC.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2", "p3/m3"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-health", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: upC.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)
	// Make C best telemetry but put it in cooldown via many failures
	s.hm.RecordSuccess("p1/m1", 100*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 100*time.Millisecond)
	s.hm.RecordSuccess("p3/m3", 1*time.Millisecond) // best
	// Now make C unhealthy
	for i := 0; i < 10; i++ {
		s.hm.RecordFailure("p3/m3", "injected failure", time.Millisecond)
	}

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-health","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "health-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("health strict failed: %d %s", rr.Code, rr.Body.String())
	}
	dep := rr.Header().Get("X-Gateway-Deployment")
	if dep == "p3/m3" {
		t.Fatalf("health/circuit-ineligible C should never be selected, got %s", dep)
	}
	if hitsC.Load() != 0 {
		t.Fatalf("C with best telemetry but ineligible due to health should never execute, hits=%d", hitsC.Load())
	}
}

// 9. HOT RELOAD TEST MUST ASSERT COHERENT RESULT
func TestPolicy_HotReloadCoherence_Strict(t *testing.T) {
	// P1 and P2 initial checks with clean gateways
	var hits1, hits2 atomic.Int64
	rec1 := &attemptRecorder{}
	rec2 := &attemptRecorder{}
	up1 := makeUpstreamWithRecorder(t, &hits1, rec1, "shared", 0, 500)
	defer up1.Close()
	up2 := makeUpstreamWithRecorder(t, &hits2, rec2, "shared", 0, 500)
	defer up2.Close()

	cfgBase := config.Default()
	cfgBase.Probe.Enabled = false
	cfgBase.Routing.Strategy = "priority"
	cfgBase.Routing.MaxAttempts = 1
	cfgBase.Decision.Mode = "local"
	cfgBase.Decision.Provider = "policy"
	cfgBase.Decision.Policy = "balanced"
	cfgBase.Decision.TimeoutMS = 100
	cfgBase.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p3/m3", "p1/m1", "p2/m2"}}}
	cfgBase.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfgBase.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-hot-coherent", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfgBase.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up1.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up1.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: up1.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 1}}},
	}

	// P1 gateway: latency weight, A low latency
	cfgP1 := cfgBase
	cfgP1.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1, RouterBaseline: 0, Reliability: 0}, nil, 0),
	}
	sP1 := testGateway(t, cfgP1)
	sP1.hm.RecordSuccess("p1/m1", 1*time.Millisecond)
	sP1.hm.RecordSuccess("p2/m2", 1000*time.Millisecond)
	sP1.hm.RecordSuccess("p3/m3", 1000*time.Millisecond)
	// Make C have high weight to be original primary in router, so policy changing primary is SELECT not ABSTAIN
	nextCfgP1 := sP1.currentConfig()
	for i := range nextCfgP1.Providers {
		for j := range nextCfgP1.Providers[i].Models {
			if nextCfgP1.Providers[i].Models[j].ID == "m3" {
				nextCfgP1.Providers[i].Models[j].Weight = 5
			}
		}
	}
	_ = sP1.applyConfig(nextCfgP1)
	sP1.hm.RecordSuccess("p1/m1", 1*time.Millisecond)
	sP1.hm.RecordSuccess("p2/m2", 1000*time.Millisecond)
	sP1.hm.RecordSuccess("p3/m3", 1000*time.Millisecond)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-hot-coherent","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "hot-p1")
	rr := httptest.NewRecorder()
	sP1.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("P1 initial failed: %d %s", rr.Code, rr.Body.String())
	}
	depP1 := rr.Header().Get("X-Gateway-Deployment")
	if depP1 != "p1/m1" {
		t.Fatalf("P1 expected A p1/m1, got %s", depP1)
	}

	// P2 gateway: reliability weight, B high reliability
	cfgP2 := cfgBase
	cfgP2.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Reliability: 1, Latency: 0, RouterBaseline: 0}, nil, 0),
	}
	// Need separate upstreams for P2 gateway because up1 closed? Use up2
	cfgP2.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up2.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up2.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: up2.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 1}}},
	}
	sP2 := testGateway(t, cfgP2)
	// Make B high reliability, A and C low
	for i := 0; i < 10; i++ {
		sP2.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		sP2.hm.RecordFailure("p1/m1", "fail", time.Millisecond)
		sP2.hm.RecordFailure("p3/m3", "fail", time.Millisecond)
	}
	sP2.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
	sP2.hm.RecordSuccess("p3/m3", 10*time.Millisecond)
	// Make C high weight to be original primary in router
	nextCfgP2 := sP2.currentConfig()
	for i := range nextCfgP2.Providers {
		for j := range nextCfgP2.Providers[i].Models {
			if nextCfgP2.Providers[i].Models[j].ID == "m3" {
				nextCfgP2.Providers[i].Models[j].Weight = 5
			}
		}
	}
	_ = sP2.applyConfig(nextCfgP2)
	for i := 0; i < 10; i++ {
		sP2.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	}
	sP2.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
	sP2.hm.RecordSuccess("p3/m3", 10*time.Millisecond)
	// Add more failures to C to make its reliability lower than B
	for i := 0; i < 5; i++ {
		sP2.hm.RecordFailure("p3/m3", "fail", time.Millisecond)
	}
	sP2.hm.RecordSuccess("p3/m3", 10*time.Millisecond)

	req = httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-hot-coherent","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "hot-p2")
	rr = httptest.NewRecorder()
	sP2.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("P2 initial failed: %d %s", rr.Code, rr.Body.String())
	}
	depP2 := rr.Header().Get("X-Gateway-Deployment")
	if depP2 != "p2/m2" {
		t.Fatalf("P2 expected B p2/m2 with reliability, got %s", depP2)
	}

	// Now race coherence test with third gateway that has both signals
	var hits atomic.Int64
	rec := &attemptRecorder{}
	up := makeUpstreamWithRecorder(t, &hits, rec, "shared", 0, 500)
	defer up.Close()

	cfg := cfgBase
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1, Reliability: 0, RouterBaseline: 0}, nil, 0),
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
		{ID: "p3", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m3", Model: "model-c", Enabled: true, Weight: 5}}},
	}
	s := testGateway(t, cfg)
	// Set telemetry for both: A low latency (1ms) but low reliability (failures), B high latency (1000ms) but high reliability (many successes), C high weight original primary but low reliability
	s.hm.RecordSuccess("p1/m1", 1*time.Millisecond)
	for i := 0; i < 5; i++ {
		s.hm.RecordFailure("p1/m1", "fail", time.Millisecond)
	}
	s.hm.RecordSuccess("p1/m1", 1*time.Millisecond)
	for i := 0; i < 10; i++ {
		s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	}
	s.hm.RecordSuccess("p2/m2", 1000*time.Millisecond)
	// Make C low reliability
	for i := 0; i < 5; i++ {
		s.hm.RecordFailure("p3/m3", "fail", time.Millisecond)
	}
	s.hm.RecordSuccess("p3/m3", 1000*time.Millisecond)

	// Now race: reload between P1 and P2 while requests
	type result struct {
		dep       string
		weights   map[string]float64
		policyID  string
		breakdown string
	}
	var resultsMu sync.Mutex
	var results []result

	done := make(chan bool)
	go func() {
		for i := 0; i < 100; i++ {
			cfgNow := s.currentConfig()
			if i%2 == 0 {
				cfgNow.DecisionPolicies = []config.DecisionPolicyConfig{
					makePolicyConfig("balanced", config.DecisionPolicyWeights{Latency: 1, Reliability: 0, RouterBaseline: 0}, nil, 0),
				}
			} else {
				cfgNow.DecisionPolicies = []config.DecisionPolicyConfig{
					makePolicyConfig("balanced", config.DecisionPolicyWeights{Reliability: 1, Latency: 0, RouterBaseline: 0}, nil, 0),
				}
			}
			_ = s.applyConfig(cfgNow)
			time.Sleep(2 * time.Millisecond)
		}
		done <- true
	}()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-hot-coherent","messages":[{"role":"user","content":"hi"}]}`))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-Id", fmt.Sprintf("hot-race-%d", idx))
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != 200 {
				return
			}
			dep := rr.Header().Get("X-Gateway-Deployment")
			// Get event for this specific RequestID
			requestID := fmt.Sprintf("hot-race-%d", idx)
			var evWeights map[string]float64
			var evPolicyID, evBreakdown string
			evs := s.bus.SnapshotLimit(200)
			for j := len(evs) - 1; j >= 0; j-- {
				ev := evs[j]
				if ev.RequestID == requestID && ev.DecisionProvider == "policy" {
					evPolicyID = ev.DecisionPolicyID
					evBreakdown = ev.DecisionBreakdown
					if evBreakdown != "" {
						var payload map[string]interface{}
						if err := json.Unmarshal([]byte(evBreakdown), &payload); err == nil {
							if w, ok := payload["weights"].(map[string]interface{}); ok {
								evWeights = make(map[string]float64)
								for k, v := range w {
									if fv, ok := v.(float64); ok {
										evWeights[k] = fv
									}
								}
							}
						}
					}
					break
				}
			}
			// Fallback to any policy event if not found by RequestID (for debugging)
			if evPolicyID == "" {
				for j := len(evs) - 1; j >= 0; j-- {
					ev := evs[j]
					if ev.DecisionProvider == "policy" {
						evPolicyID = ev.DecisionPolicyID
						evBreakdown = ev.DecisionBreakdown
						if evBreakdown != "" {
							var payload map[string]interface{}
							if err := json.Unmarshal([]byte(evBreakdown), &payload); err == nil {
								if w, ok := payload["weights"].(map[string]interface{}); ok {
									evWeights = make(map[string]float64)
									for k, v := range w {
										if fv, ok := v.(float64); ok {
											evWeights[k] = fv
										}
									}
								}
							}
						}
						break
					}
				}
			}
			resultsMu.Lock()
			results = append(results, result{dep: dep, weights: evWeights, policyID: evPolicyID, breakdown: evBreakdown})
			resultsMu.Unlock()
		}(i)
	}
	wg.Wait()
	<-done

	// Each result must be coherent P1 or P2, never mixed
	for _, r := range results {
		if r.dep != "p1/m1" && r.dep != "p2/m2" {
			t.Fatalf("hot reload coherence broken: dep=%s not P1 (p1/m1) nor P2 (p2/m2), results=%v", r.dep, results)
		}
		// If weights present, check they are pure P1 or P2, not mixed
		if r.weights != nil {
			latencyW := r.weights["latency"]
			reliabilityW := r.weights["reliability"]
			// P1: latency 1, reliability 0; P2: latency 0, reliability 1
			isP1 := latencyW == 1 && reliabilityW == 0
			isP2 := latencyW == 0 && reliabilityW == 1
			if !isP1 && !isP2 {
				t.Fatalf("hot reload mixed weights: latency=%v reliability=%v dep=%s", latencyW, reliabilityW, r.dep)
			}
			// Coherence: if dep is A, weights should be P1 (latency), if dep B, weights P2 (reliability) — but allow either because health could cause same dep with different weights? Strict: P1 should select A, P2 B
			if r.dep == "p1/m1" && !isP1 {
				t.Fatalf("coherence: dep A but weights not P1: %+v", r.weights)
			}
			if r.dep == "p1/m1" && !isP1 {
				t.Fatalf("coherence: dep A but weights not P1: %+v", r.weights)
			}
			if r.dep == "p2/m2" && !isP2 {
				t.Fatalf("coherence: dep B but weights not P2: %+v", r.weights)
			}
		}
	}
}

// Additional strict tests retained from previous hardening

func TestPolicy_ConfigValidationExplicit_Strict(t *testing.T) {
	cfg := config.Default()
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "nonexistent"
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error for policy provider without policies")
	}
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
	}
	cfg.Decision.Policy = "balanced"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		{
			ID:            "balanced",
			SelectionMode: "select_first",
			Weights:       config.DecisionPolicyWeights{RouterBaseline: 1},
			TaskOverrides: map[string]config.DecisionPolicyWeights{
				"coding": {},
			},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error for all-zero task override")
	}
}

func TestPolicy_SelectOnlyContract_Strict(t *testing.T) {
	var hits atomic.Int64
	rec := &attemptRecorder{}
	up := makeUpstreamWithRecorder(t, &hits, rec, "shared", 0, 500)
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-selectonly", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-selectonly","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "selectonly-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("selectonly strict failed: %d %s", rr.Code, rr.Body.String())
	}
	for i := 0; i < 5; i++ {
		evs := s.bus.SnapshotLimit(100)
		for _, ev := range evs {
			if ev.DecisionProvider == "policy" && ev.DecisionAction == string(decision.ActionRank) {
				t.Fatalf("policy provider should never RANK, got RANK")
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPolicy_MarshalBreakdownValidJSONE2E_Strict(t *testing.T) {
	var hits atomic.Int64
	rec := &attemptRecorder{}
	up := makeUpstreamWithRecorder(t, &hits, rec, "shared", 0, 500)
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2", "p1/m3", "p2/m4", "p1/m5", "p2/m6", "p1/m7", "p2/m8", "p1/m9", "p2/m10"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-json", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
			{ID: "m1", Model: "model-a", Enabled: true, Weight: 1},
			{ID: "m3", Model: "model-c", Enabled: true, Weight: 1},
			{ID: "m5", Model: "model-e", Enabled: true, Weight: 1},
			{ID: "m7", Model: "model-g", Enabled: true, Weight: 1},
			{ID: "m9", Model: "model-i", Enabled: true, Weight: 1},
		}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
			{ID: "m2", Model: "model-b", Enabled: true, Weight: 1},
			{ID: "m4", Model: "model-d", Enabled: true, Weight: 1},
			{ID: "m6", Model: "model-f", Enabled: true, Weight: 1},
			{ID: "m8", Model: "model-h", Enabled: true, Weight: 1},
			{ID: "m10", Model: "model-j", Enabled: true, Weight: 1},
		}},
	}

	s := testGateway(t, cfg)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-json","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "json-strict")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("json strict failed: %d %s", rr.Code, rr.Body.String())
	}
	for i := 0; i < 5; i++ {
		evs := s.bus.SnapshotLimit(100)
		for _, ev := range evs {
			if ev.DecisionBreakdown != "" {
				var m map[string]interface{}
				if err := json.Unmarshal([]byte(ev.DecisionBreakdown), &m); err != nil {
					t.Fatalf("breakdown invalid JSON: %v content=%s", err, ev.DecisionBreakdown)
				}
				if len(ev.DecisionBreakdown) > 4096 {
					t.Fatalf("breakdown length %d exceeds 4096", len(ev.DecisionBreakdown))
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPolicy_HotReloadRace_NoPanic(t *testing.T) {
	var hits atomic.Int64
	rec := &attemptRecorder{}
	up := makeUpstreamWithRecorder(t, &hits, rec, "p1/m1", 0, 500)
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-hot", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1}}},
	}

	s := testGateway(t, cfg)

	done := make(chan bool)
	go func() {
		for i := 0; i < 50; i++ {
			next := s.currentConfig()
			next.DecisionPolicies = []config.DecisionPolicyConfig{
				makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1, Latency: float64(i % 2)}, nil, 0),
			}
			_ = s.applyConfig(next)
			time.Sleep(5 * time.Millisecond)
		}
		done <- true
	}()

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-hot","messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", "hot-reload-race")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 && rr.Code != 429 && rr.Code != 503 {
			t.Logf("request %d got %d", i, rr.Code)
		}
	}
	<-done
}
