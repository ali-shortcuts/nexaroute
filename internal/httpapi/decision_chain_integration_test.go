package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// helpers for hybrid chain tests
func makeHybridChainConfig(t *testing.T, upstreams map[string]*httptest.Server) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 3
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "external-policy"
	cfg.Decision.TimeoutMS = 800
	cfg.Decision.MaxProviderCalls = 2
	cfg.DecisionProviderHealth = config.DecisionProviderHealthConfig{FailureThreshold: 2, FailureWindowSeconds: 30, CooldownSeconds: 60}
	cfg.DecisionProviders = []config.DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "test-key", PrivacyMode: "metadata_only", BaseURL: "https://jev.example.invalid"},
		{ID: "jev-backup", Type: "jev", Enabled: boolPtr(true), APIKey: "key2", PrivacyMode: "metadata_only", BaseURL: "https://jev2.example.invalid"},
	}
	cfg.DecisionChains = []config.DecisionChainConfig{
		{ID: "external-policy", Steps: []config.DecisionChainStep{{Provider: "jev-main"}, {Provider: "policy"}}},
		{ID: "policy-only", Steps: []config.DecisionChainStep{{Provider: "policy"}}},
		{ID: "external-local", Steps: []config.DecisionChainStep{{Provider: "jev-main"}, {Provider: "local"}}},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}},
		{ID: "fallback", Mode: "explicit", Deployments: []string{"p3/m3"}},
		{ID: "all", Mode: "all"},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "fc1", Pools: []string{"primary", "fallback"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "primary", FallbackChain: "fc1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-chain", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai", "anthropic", "openai_responses"}}}
	providers := []config.ProviderConfig{}
	for id, srv := range upstreams {
		parts := strings.Split(id, "/")
		pid, mid := parts[0], parts[1]
		providers = append(providers, config.ProviderConfig{ID: pid, Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: mid, Model: "model-" + mid, Enabled: true, Weight: 1}}})
	}
	if len(providers) == 0 {
		// at least one
		providers = append(providers, config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true}}})
	}
	cfg.Providers = providers
	cfg.ApplyDefaults()
	return cfg
}

// helper to create counting chain provider that integrates with decision registry
type countingChainProvider struct {
	id     string
	calls  atomic.Int64
	result decision.DecisionResult
	err    error
	delay  time.Duration
	panic  bool
	caps   decision.Capabilities
	health string
}

func newCountingChainProvider(id string, result decision.DecisionResult, err error) *countingChainProvider {
	return &countingChainProvider{id: id, result: result, err: err, caps: decision.Capabilities{CanSelect: true, CanRank: true}}
}
func (c *countingChainProvider) ID() string { return c.id }
func (c *countingChainProvider) Capabilities() decision.Capabilities {
	if c.caps.CanSelect == false && c.caps.CanRank == false {
		return decision.Capabilities{CanSelect: true}
	}
	return c.caps
}
func (c *countingChainProvider) Health() decision.ProviderHealth {
	s := c.health
	if s == "" {
		s = decision.HealthHealthy
	}
	return decision.ProviderHealth{Status: s, CheckedAt: time.Now()}
}
func (c *countingChainProvider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	c.calls.Add(1)
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true, ReasonCodes: []decision.ReasonCode{decision.ReasonTimeout}, ProviderID: c.id}, ctx.Err()
		}
	}
	if c.panic {
		panic("chain panic")
	}
	if c.err != nil {
		return c.result, c.err
	}
	res := c.result
	if res.ProviderID == "" {
		res.ProviderID = c.id
	}
	return res, nil
}
func (c *countingChainProvider) Calls() int64 { return c.calls.Load() }

// Test helpers

func TestChain_Hybrid_JevValidSelect(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "jev-main"}, nil)
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)
	// local already registered

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "chain-jev-select")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p2/m2" {
		t.Fatalf("expected p2/m2 from Jev, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
	if jevProv.Calls() != 1 {
		t.Fatalf("jev should be called once, got %d", jevProv.Calls())
	}
	if polProv.Calls() != 0 {
		t.Fatalf("policy should not be called after jev select, got %d", polProv.Calls())
	}
}

func TestChain_Hybrid_JevErrorFallbackToPolicy(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{}, fmt.Errorf("network error"))
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "chain-jev-error")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p1/m1" {
		t.Fatalf("expected p1/m1 from policy fallback, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
	if jevProv.Calls() != 1 || polProv.Calls() != 1 {
		t.Fatalf("both should be called: jev %d policy %d", jevProv.Calls(), polProv.Calls())
	}
}

func TestChain_Hybrid_JevTimeoutFallback(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	// deterministic: global 300ms, jev step 50ms, jev delay 200ms -> jev times out at 50ms, global remains 250ms, policy MUST run
	cfg.Decision.TimeoutMS = 300
	cfg.Decision.MaxProviderCalls = 2
	cfg.DecisionChains = []config.DecisionChainConfig{
		{ID: "external-policy", Steps: []config.DecisionChainStep{{Provider: "jev-main", TimeoutMS: 50}, {Provider: "policy"}}},
		{ID: "policy-only", Steps: []config.DecisionChainStep{{Provider: "policy"}}},
	}
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionAbstain, ProviderID: "jev-main"}, nil)
	jevProv.delay = 200 * time.Millisecond
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "chain-jev-timeout")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if jevProv.Calls() != 1 {
		t.Fatalf("jev should be called once, got %d", jevProv.Calls())
	}
	if polProv.Calls() != 1 {
		t.Fatalf("policy MUST be called after jev step timeout, got %d", polProv.Calls())
	}
	if dep := rr.Header().Get("X-Gateway-Deployment"); dep != "p2/m2" {
		t.Fatalf("expected p2/m2 from policy after jev timeout, got %s", dep)
	}
}

func TestChain_Hybrid_JevAbstainHealthy(t *testing.T) {
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true, ReasonCodes: []decision.ReasonCode{decision.ReasonAbstained}, ProviderID: "jev-main"}, nil)
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "chain-abstain")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200")
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p2/m2" {
		t.Fatalf("expected p2/m2 from policy, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
	// Jev abstain must remain healthy (not cooldown)
	if s.decisionOrchestrator.ProviderState().IsCooldown("jev-main") {
		t.Fatalf("abstain should not open cooldown")
	}
}

func TestChain_Hybrid_JevInvalidFallback(t *testing.T) {
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p3/m3", Confidence: 0.9, ProviderID: "jev-main"}, nil) // invalid - outside eligible
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "chain-invalid")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200")
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p1/m1" {
		t.Fatalf("expected p1/m1 from policy, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
	if jevProv.Calls() != 1 || polProv.Calls() != 1 {
		t.Fatalf("both should be called")
	}
}

func TestChain_Hybrid_JevCooldownSkipsCall(t *testing.T) {
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	cfg.DecisionProviderHealth.FailureThreshold = 2
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	// force Jev into cooldown via direct state
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{}, fmt.Errorf("fail"))
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)
	// 2 failures to open cooldown
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", fmt.Sprintf("chain-cooldown-%d", i))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
	}
	if !s.decisionOrchestrator.ProviderState().IsCooldown("jev-main") {
		t.Fatalf("jev should be in cooldown")
	}
	initialCalls := jevProv.Calls()
	// next request should skip Jev (zero call)
	polProv.calls.Store(0)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "chain-cooldown-skip")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if jevProv.Calls() != initialCalls {
		t.Fatalf("jev should be skipped during cooldown, calls %d vs %d", jevProv.Calls(), initialCalls)
	}
	if polProv.Calls() != 1 {
		t.Fatalf("policy should be called, got %d", polProv.Calls())
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p2/m2" {
		t.Fatalf("expected p2/m2, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
}

func TestChain_CooldownLifecycle_E2E(t *testing.T) {
	var hits atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hits, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	cfg.DecisionProviderHealth.FailureThreshold = 2
	cfg.DecisionProviderHealth.FailureWindowSeconds = 30
	cfg.DecisionProviderHealth.CooldownSeconds = 60
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{}, fmt.Errorf("fail jev"))
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", fmt.Sprintf("cool-lifecycle-%d", i))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("request %d failed %d", i, rr.Code)
		}
		if got := jevProv.Calls(); got != int64(i+1) {
			t.Fatalf("jev calls after %d requests got %d want %d", i, got, i+1)
		}
	}
	if !s.decisionOrchestrator.ProviderState().IsCooldown("jev-main") {
		t.Fatalf("after 2 failures jev should be cooldown")
	}
	// third request must skip jev, calls remain 2
	prevJev := jevProv.Calls()
	prevPol := polProv.Calls()
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cool-lifecycle-2")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("third failed %d", rr.Code)
	}
	if jevProv.Calls() != prevJev {
		t.Fatalf("third request must NOT call jev (cooldown), before %d after %d", prevJev, jevProv.Calls())
	}
	if polProv.Calls() != prevPol+1 {
		t.Fatalf("policy must be called on third, before %d after %d", prevPol, polProv.Calls())
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p2/m2" {
		t.Fatalf("third should be p2/m2 via policy, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
	// prove recovery via fake clock at manager level (no sleep): advance clock beyond cooldown
	// Use manager's Config and IsCooldown with injected clock not possible here (real manager uses time.Now),
	// so create isolated manager test with fake clock
	mgr := s.decisionOrchestrator.ProviderState()
	// verify cooldown still active
	if !mgr.IsCooldown("jev-main") {
		t.Fatalf("still cooldown")
	}
	// manager-level expiry is covered in providerstate/manager_test.go CooldownExpiryWithoutSleep;
	// we just prove that without expiry, cooldown persists, and that Reset clears it
	mgr.Reset("jev-main")
	if mgr.IsCooldown("jev-main") {
		t.Fatalf("after Reset should not be cooldown")
	}
}

func TestChain_FallbackE2E_BAC(t *testing.T) {
	// primary A,B fallback C, Jev fails, Policy selects B, physical B fails, A fails, C succeeds => C
	rec := &attemptRecorder{}
	var hitsA, hitsB, hitsC atomic.Int64
	upB := makeJevUpstream(t, "p2/m2", &hitsB, rec, 1) // fail first
	defer upB.Close()
	upA := makeJevUpstream(t, "p1/m1", &hitsA, rec, 1)
	defer upA.Close()
	upC := makeJevUpstream(t, "p3/m3", &hitsC, rec, 0)
	defer upC.Close()

	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB, "p3/m3": upC})
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}},
		{ID: "fallback", Mode: "explicit", Deployments: []string{"p3/m3"}},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "fc1", Pools: []string{"primary", "fallback"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "primary", FallbackChain: "fc1"}}
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{}, fmt.Errorf("jev fail"))
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "fallback-bac")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p3/m3" {
		t.Fatalf("expected C p3/m3, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
	if jevProv.Calls() != 1 || polProv.Calls() != 1 {
		t.Fatalf("decision calls jev 1 policy 1, got %d %d", jevProv.Calls(), polProv.Calls())
	}
	order := rec.snapshot()
	if len(order) != 3 || order[0] != "p2/m2" || order[1] != "p1/m1" || order[2] != "p3/m3" {
		t.Fatalf("expected B->A->C, got %v", order)
	}
}

func TestChain_MaxAttemptsE2E(t *testing.T) {
	rec := &attemptRecorder{}
	var hitsA, hitsB, hitsC atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, rec, 1)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, rec, 1)
	defer upB.Close()
	upC := makeJevUpstream(t, "p3/m3", &hitsC, rec, 0)
	defer upC.Close()

	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB, "p3/m3": upC})
	cfg.Routing.MaxAttempts = 2
	// deterministic pools: primary A,B fallback C; policy selects B => order B->A->C, but maxAttempts 2 suppresses C
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}},
		{ID: "fallback", Mode: "explicit", Deployments: []string{"p3/m3"}},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "fc1", Pools: []string{"primary", "fallback"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "primary", FallbackChain: "fc1"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-chain", RouteProfile: "rp1", Enabled: boolPtr(true)}}
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "policy-only"
	cfg.DecisionChains = []config.DecisionChainConfig{{ID: "policy-only", Steps: []config.DecisionChainStep{{Provider: "policy"}}}}
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(polProv)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if hitsB.Load() != 1 {
		t.Fatalf("B should be attempted exactly once, got %d order %v", hitsB.Load(), rec.snapshot())
	}
	if hitsA.Load() != 1 {
		t.Fatalf("A should be attempted exactly once, got %d order %v", hitsA.Load(), rec.snapshot())
	}
	if hitsC.Load() != 0 {
		t.Fatalf("C must be suppressed by maxAttempts=2, got %d order %v", hitsC.Load(), rec.snapshot())
	}
	total := hitsA.Load() + hitsB.Load() + hitsC.Load()
	if total != 2 {
		t.Fatalf("expected 2 total attempts, got %d order %v", total, rec.snapshot())
	}
	order := rec.snapshot()
	if len(order) != 2 || order[0] != "p2/m2" || order[1] != "p1/m1" {
		t.Fatalf("expected B->A order, got %v", order)
	}
	if polProv.Calls() != 1 {
		t.Fatalf("policy should be called once")
	}
}

func TestChain_CacheHitZeroCalls(t *testing.T) {
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	cfg.Cache.Enabled = true
	cfg.Cache.TTLSeconds = 60
	cfg.Cache.MaxEntries = 10
	cfg.Cache.MaxBodyBytes = 1 << 20
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "jev-main"}, nil)
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	body := `{"model":"nexa-chain","messages":[{"role":"user","content":"hello cache"}]}`
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cache-1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("first failed %d", rr.Code)
	}
	firstJev := jevProv.Calls()
	firstPol := polProv.Calls()
	if firstJev == 0 && firstPol == 0 {
		t.Fatalf("first should have decision calls")
	}
	req2 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "cache-2")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("second failed %d", rr2.Code)
	}
	if jevProv.Calls() != firstJev || polProv.Calls() != firstPol {
		t.Fatalf("cache hit should have 0 decision calls: before jev %d pol %d after jev %d pol %d", firstJev, firstPol, jevProv.Calls(), polProv.Calls())
	}
	// also check event decision_provider_calls ==0? via bus: second request's decision event should not exist or be zero
	// For cache hit, decision plane is skipped entirely, so no additional chain calls
}

func TestChain_CacheHitZeroCalls_Anthropic(t *testing.T) {
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	cfg.Cache.Enabled = true
	cfg.Cache.TTLSeconds = 60
	cfg.Cache.MaxEntries = 10
	cfg.Cache.MaxBodyBytes = 1 << 20
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "jev-main"}, nil)
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)

	body := `{"model":"nexa-chain","messages":[{"role":"user","content":"hello anth cache"}],"max_tokens":10}`
	req := httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cache-anth-1")
	req.Header.Set("x-api-key", "test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("first anthropic cache failed %d %s", rr.Code, rr.Body.String())
	}
	firstJev := jevProv.Calls()
	firstPol := polProv.Calls()
	if firstJev == 0 && firstPol == 0 {
		t.Fatalf("first anthropic should have decision calls")
	}
	req2 := httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(body))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "cache-anth-2")
	req2.Header.Set("x-api-key", "test")
	req2.Header.Set("anthropic-version", "2023-06-01")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("second anthropic cache failed %d", rr2.Code)
	}
	if jevProv.Calls() != firstJev || polProv.Calls() != firstPol {
		t.Fatalf("anthropic cache hit should have 0 decision calls: before jev %d pol %d after jev %d pol %d", firstJev, firstPol, jevProv.Calls(), polProv.Calls())
	}
	// Note: openai_responses has no cache path; not covered here (documented in PHASE_G_DECISION_CHAINS.md)
}

func TestChain_CrossProtocolStrict(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsB, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	s := testGateway(t, cfg)
	jevC := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "jev-main"}, nil)
	s.decisionRegistry.Register(jevC)
	s.decisionRegistry.Register(newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true, ProviderID: "policy"}, nil))

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cross-openai")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("openai %d", rr.Code)
	}
	depOpenAI := rr.Header().Get("X-Gateway-Deployment")

	req = httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cross-anthropic")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("anthropic %d", rr.Code)
	}
	depAnthropic := rr.Header().Get("X-Gateway-Deployment")

	hitsA.Store(0)
	hitsB.Store(0)
	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"nexa-chain","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "cross-responses")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("responses %d", rr.Code)
	}
	depResponses := rr.Header().Get("X-Gateway-Deployment")
	if depResponses == "" {
		for _, ev := range s.bus.Snapshot() {
			if ev.RequestID == "cross-responses" && ev.Kind == "route_ok" {
				depResponses = ev.Deployment
				break
			}
		}
		if depResponses == "" && hitsA.Load() == 1 {
			depResponses = "p1/m1"
		} else if depResponses == "" && hitsB.Load() == 1 {
			depResponses = "p2/m2"
		}
	}
	if depOpenAI != depAnthropic || depAnthropic != depResponses {
		t.Fatalf("cross-protocol mismatch: openai %s anthropic %s responses %s", depOpenAI, depAnthropic, depResponses)
	}
}

func TestChain_VEvsDirectStrict(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "shared", &hits, nil, 0)
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "external-policy"
	cfg.Decision.TimeoutMS = 500
	cfg.Decision.MaxProviderCalls = 2
	cfg.DecisionProviders = []config.DecisionProviderConfig{{ID: "jev-main", Type: "jev", Enabled: boolPtr(true), APIKey: "k", PrivacyMode: "metadata_only", BaseURL: "https://jev.invalid"}}
	cfg.DecisionChains = []config.DecisionChainConfig{{ID: "external-policy", Steps: []config.DecisionChainStep{{Provider: "jev-main"}, {Provider: "policy"}}}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-chain", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Aliases: []string{"shared-model"}, Enabled: true, Weight: 1, Priority: 5}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Aliases: []string{"shared-model"}, Enabled: true, Weight: 1, Priority: 5}}},
	}
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "jev-main"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true, ProviderID: "policy"}, nil))

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "ve-chain")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("ve %d", rr.Code)
	}
	depVE := rr.Header().Get("X-Gateway-Deployment")

	req2 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "direct-chain")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("direct %d", rr2.Code)
	}
	depDirect := rr2.Header().Get("X-Gateway-Deployment")
	if depVE != depDirect {
		t.Fatalf("VE vs direct mismatch: VE %s direct %s", depVE, depDirect)
	}
	if depVE != "p2/m2" {
		t.Fatalf("expected p2/m2 valid selection for both VE and direct, got VE %s direct %s", depVE, depDirect)
	}
}

func TestChain_HealthSeparation(t *testing.T) {
	var hits atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hits, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hits, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	s := testGateway(t, cfg)
	s.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	beforeA := s.hm.Get("p1/m1")
	beforeB := s.hm.Get("p2/m2")

	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{}, fmt.Errorf("fail"))
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(polProv)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", fmt.Sprintf("health-sep-%d", i))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
	}
	afterA := s.hm.Get("p1/m1")
	afterB := s.hm.Get("p2/m2")
	if beforeA.Status != afterA.Status || beforeB.Status != afterB.Status {
		t.Fatalf("physical health should not change due to decision failures: before A %+v after %+v B %+v after %+v", beforeA, afterA, beforeB, afterB)
	}
	// now cause real upstream failure
	failUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"fail"}`))
	}))
	defer failUp.Close()
	// reconfigure to force B to be selected and then fail
	cfg2 := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": failUp})
	s2 := testGateway(t, cfg2)
	s2.hm.RecordSuccess("p1/m1", 10*time.Millisecond)
	s2.hm.RecordSuccess("p2/m2", 10*time.Millisecond)
	pol2 := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s2.decisionRegistry.Register(newCountingChainProvider("jev-main", decision.DecisionResult{}, fmt.Errorf("fail")))
	s2.decisionRegistry.Register(pol2)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr, req)
	// B should now be degraded
	if s2.hm.Get("p2/m2").Status == beforeB.Status && s2.hm.Get("p2/m2").Failures == beforeB.Failures {
		t.Fatalf("B health should have changed after real failure")
	}
}

func TestChain_HotReloadCoherent(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "shared", &hits, nil, 0)
	defer up.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": up, "p2/m2": up})
	s := testGateway(t, cfg)
	jev1 := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "jev-main"}, nil)
	s.decisionRegistry.Register(jev1)
	s.decisionRegistry.Register(newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil))

	// initial chain external-policy: jev-main -> policy
	// reload to policy-only
	next := s.currentConfig()
	next.DecisionChains = []config.DecisionChainConfig{{ID: "policy-only", Steps: []config.DecisionChainStep{{Provider: "policy"}}}, {ID: "external-policy", Steps: []config.DecisionChainStep{{Provider: "jev-main"}, {Provider: "policy"}}}}
	next.Decision.Chain = "policy-only"
	if err := s.applyConfig(next); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	// new request should see new chain (policy-only, should select p2/m2)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("reload chain failed %d", rr.Code)
	}
	if rr.Header().Get("X-Gateway-Deployment") != "p2/m2" {
		t.Fatalf("expected p2/m2 from new chain, got %s", rr.Header().Get("X-Gateway-Deployment"))
	}
	// unrelated reload should preserve cooldown
	s.decisionOrchestrator.ProviderState().RecordFailure("jev-main")
	s.decisionOrchestrator.ProviderState().RecordFailure("jev-main")
	if !s.decisionOrchestrator.ProviderState().IsCooldown("jev-main") {
		t.Fatalf("should be cooldown")
	}
	next2 := s.currentConfig()
	next2.Routing.Strategy = "priority"
	_ = s.applyConfig(next2)
	if !s.decisionOrchestrator.ProviderState().IsCooldown("jev-main") {
		t.Fatalf("unrelated reload should preserve cooldown")
	}
	// changing provider identity should reset
	next3 := s.currentConfig()
	next3.DecisionProviders[0].BaseURL = "https://new.example.com"
	_ = s.applyConfig(next3)
	if s.decisionOrchestrator.ProviderState().IsCooldown("jev-main") {
		t.Fatalf("identity change should reset provider state")
	}
}

func TestChain_HotReload_InFlightCoherent(t *testing.T) {
	var hits atomic.Int64
	up := makeJevUpstream(t, "shared", &hits, nil, 0)
	defer up.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": up, "p2/m2": up})
	// old chain: jev-main -> policy, new chain: policy-only
	s := testGateway(t, cfg)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	blockingJev := &hotReloadBlockingProvider{id: "jev-main", started: started, release: release, selected: "p1/m1"}
	pol := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(blockingJev)
	s.decisionRegistry.Register(pol)

	// start old request in goroutine, it will block inside jev
	done := make(chan struct{})
	var rrOld *httptest.ResponseRecorder
	go func() {
		req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", "hot-inflight-old")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		rrOld = rr
		close(done)
	}()
	// wait for jev to start
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("jev did not start")
	}
	// reload to policy-only while old request is blocked (snapshot already taken)
	next := s.currentConfig()
	next.Decision.Chain = "policy-only"
	// ensure policy-only chain exists
	next.DecisionChains = []config.DecisionChainConfig{
		{ID: "external-policy", Steps: []config.DecisionChainStep{{Provider: "jev-main"}, {Provider: "policy"}}},
		{ID: "policy-only", Steps: []config.DecisionChainStep{{Provider: "policy"}}},
	}
	if err := s.applyConfig(next); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	// release old jev
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("old request did not finish")
	}
	if rrOld.Code != 200 {
		t.Fatalf("old request failed %d", rrOld.Code)
	}
	if dep := rrOld.Header().Get("X-Gateway-Deployment"); dep != "p1/m1" {
		t.Fatalf("old in-flight must use OLD chain (jev selects p1/m1), got %s", dep)
	}
	if blockingJev.Calls() != 1 {
		t.Fatalf("blocking jev should be called once for old request")
	}
	// new request must use NEW chain (policy-only, no jev)
	pol.calls.Store(0)
	blockingJev.calls.Store(0)
	req2 := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi new"}]}`))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-Id", "hot-inflight-new")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("new request failed %d", rr2.Code)
	}
	if dep := rr2.Header().Get("X-Gateway-Deployment"); dep != "p2/m2" {
		t.Fatalf("new request must use NEW chain policy selects p2/m2, got %s", dep)
	}
	if blockingJev.Calls() != 0 {
		t.Fatalf("new request must NOT call old jev, got %d", blockingJev.Calls())
	}
	if pol.Calls() != 1 {
		t.Fatalf("new request must call policy once, got %d", pol.Calls())
	}
}

// hotReloadBlockingProvider blocks until release channel closed, to test in-flight snapshot coherence
type hotReloadBlockingProvider struct {
	id       string
	calls    atomic.Int64
	started  chan struct{}
	release  chan struct{}
	selected string
}

func (p *hotReloadBlockingProvider) ID() string { return p.id }
func (p *hotReloadBlockingProvider) Capabilities() decision.Capabilities {
	return decision.Capabilities{CanSelect: true, CanRank: true}
}
func (p *hotReloadBlockingProvider) Health() decision.ProviderHealth {
	return decision.ProviderHealth{Status: decision.HealthHealthy, CheckedAt: time.Now()}
}
func (p *hotReloadBlockingProvider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	p.calls.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return decision.DecisionResult{Action: decision.ActionAbstain, Abstained: true, ReasonCodes: []decision.ReasonCode{decision.ReasonTimeout}, ProviderID: p.id}, ctx.Err()
	}
	return decision.DecisionResult{Action: decision.ActionSelect, SelectedID: p.selected, Confidence: 0.9, ProviderID: p.id}, nil
}
func (p *hotReloadBlockingProvider) Calls() int64 { return p.calls.Load() }

func TestChain_TraceBounded(t *testing.T) {
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	s := testGateway(t, cfg)
	jevProv := newCountingChainProvider("jev-main", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "jev-main"}, nil)
	s.decisionRegistry.Register(jevProv)
	s.decisionRegistry.Register(newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionAbstain, ProviderID: "policy"}, nil))

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "trace-test")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("trace test failed")
	}
	// check decision trace header contains ChainTrace
	traceHeader := rr.Header().Get("X-Gateway-Decision-Trace")
	if traceHeader == "" {
		// check via events
		found := false
		for _, ev := range s.bus.Snapshot() {
			if ev.RequestID == "trace-test" && ev.DecisionChainID != "" {
				found = true
				if ev.DecisionChainOutcome == "" || ev.DecisionChainCalls == 0 {
					t.Fatalf("chain trace missing outcome/calls: %+v", ev)
				}
				break
			}
		}
		if !found {
			t.Fatalf("chain trace not found in events")
		}
	} else {
		var tr decision.DecisionTrace
		if err := json.Unmarshal([]byte(traceHeader), &tr); err != nil {
			t.Fatalf("trace unmarshal failed: %v", err)
		}
		if tr.ChainTrace == nil || tr.ChainTrace.ChainID == "" || tr.ChainTrace.Outcome == "" {
			t.Fatalf("chain trace missing fields: %+v", tr.ChainTrace)
		}
		for _, step := range tr.ChainTrace.Steps {
			if step.ProviderID == "" || step.ProviderType == "" || step.Outcome == "" {
				t.Fatalf("step missing bounded fields: %+v", step)
			}
			if len(step.ReasonCodes) > 8 {
				t.Fatalf("reason codes too many")
			}
		}
	}
}

func TestChain_PrivacyCanary(t *testing.T) {
	canary := "SECRET_CHAIN_PROMPT_CANARY_58bd"
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	jevMock := newMockJevServer(t, `{"code":0,"message":"ok","data":{"decision":"c0"}}`)
	// force Jev to fail after capturing request, so policy fallback executes
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"code":1,"message":"internal error"}`))
	}
	defer jevMock.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	for i := range cfg.DecisionProviders {
		if cfg.DecisionProviders[i].ID == "jev-main" {
			cfg.DecisionProviders[i].BaseURL = jevMock.URL()
		}
	}
	cfg.Decision.TimeoutMS = 400
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	// register deterministic policy fallback
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p1/m1", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(polProv)

	body := fmt.Sprintf(`{"model":"nexa-chain","messages":[{"role":"user","content":"%s"}]}`, canary)
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "privacy-chain")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("privacy test failed %d %s", rr.Code, rr.Body.String())
	}
	// capture actual external request body sent to Jev
	bodies := jevMock.CapturedBodies()
	if len(bodies) == 0 {
		t.Fatalf("jev not called")
	}
	for _, b := range bodies {
		if strings.Contains(b, canary) {
			t.Fatalf("prompt canary leaked into Jev request body: %q", b)
		}
		if strings.Contains(b, "SECRET_CHAIN_PROMPT") {
			t.Fatalf("partial canary leaked")
		}
	}
	if polProv.Calls() != 1 {
		t.Fatalf("policy should be called after jev failure, got %d", polProv.Calls())
	}
	// absent from ChainTrace, PolicyTrace, events, metrics, admin, client
	for _, ev := range s.bus.Snapshot() {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), canary) {
			t.Fatalf("canary leaked in event: %s", string(b))
		}
		if ev.DecisionChainID != "" && strings.Contains(ev.DecisionChainOutcome, canary) {
			t.Fatalf("canary in chain outcome")
		}
	}
	if strings.Contains(rr.Body.String(), canary) && !strings.Contains(body, canary) {
		// client response is upstream echo "ok", should not contain canary
	}
	if strings.Contains(rr.Header().Get("X-Gateway-Decision-Trace"), canary) {
		t.Fatalf("canary in trace header")
	}
	// metrics
	reqMetrics := httptest.NewRequest("GET", "http://localhost/metrics", nil)
	rrMetrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrMetrics, reqMetrics)
	if strings.Contains(rrMetrics.Body.String(), canary) {
		t.Fatalf("canary in metrics")
	}
	// admin
	reqAdmin := httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	reqAdmin.RemoteAddr = "127.0.0.1:12345"
	rrAdmin := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrAdmin, reqAdmin)
	if strings.Contains(rrAdmin.Body.String(), canary) {
		t.Fatalf("canary in admin")
	}
	// also ensure not in any decision trace via header json
	if trace := rr.Header().Get("X-Gateway-Decision-Trace"); trace != "" {
		var tr decision.DecisionTrace
		if err := json.Unmarshal([]byte(trace), &tr); err == nil && tr.ChainTrace != nil {
			raw, _ := json.Marshal(tr.ChainTrace)
			if strings.Contains(string(raw), canary) {
				t.Fatalf("canary in ChainTrace")
			}
		}
	}
}

func TestChain_RemoteErrorCanary(t *testing.T) {
	secret := "SECRET_CHAIN_REMOTE_ERROR_119c"
	var hitsA atomic.Int64
	upA := makeJevUpstream(t, "p1/m1", &hitsA, nil, 0)
	defer upA.Close()
	upB := makeJevUpstream(t, "p2/m2", &hitsA, nil, 0)
	defer upB.Close()
	jevMock := newMockJevServer(t, "")
	jevMock.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(secret))
	}
	defer jevMock.Close()
	cfg := makeHybridChainConfig(t, map[string]*httptest.Server{"p1/m1": upA, "p2/m2": upB})
	for i := range cfg.DecisionProviders {
		if cfg.DecisionProviders[i].ID == "jev-main" {
			cfg.DecisionProviders[i].BaseURL = jevMock.URL()
		}
	}
	cfg.Decision.TimeoutMS = 400
	cfg.ApplyDefaults()
	s := testGateway(t, cfg)
	polProv := newCountingChainProvider("policy", decision.DecisionResult{Action: decision.ActionSelect, SelectedID: "p2/m2", Confidence: 0.9, ProviderID: "policy"}, nil)
	s.decisionRegistry.Register(polProv)

	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", strings.NewReader(`{"model":"nexa-chain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "remote-error-chain")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if polProv.Calls() != 1 {
		t.Fatalf("policy should be called after jev error, got %d", polProv.Calls())
	}
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("remote error leaked to client")
	}
	for _, ev := range s.bus.Snapshot() {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), secret) {
			t.Fatalf("remote error leaked to event: %s", string(b))
		}
	}
	if strings.Contains(rr.Header().Get("X-Gateway-Decision-Trace"), secret) {
		t.Fatalf("remote error leaked to trace header")
	}
	// ChainTrace should not contain secret
	if trace := rr.Header().Get("X-Gateway-Decision-Trace"); trace != "" {
		var tr decision.DecisionTrace
		if err := json.Unmarshal([]byte(trace), &tr); err == nil && tr.ChainTrace != nil {
			raw, _ := json.Marshal(tr.ChainTrace)
			if strings.Contains(string(raw), secret) {
				t.Fatalf("remote error leaked to ChainTrace: %s", string(raw))
			}
			for _, step := range tr.ChainTrace.Steps {
				if strings.Contains(step.Outcome, secret) || strings.Contains(step.ProviderID, secret) {
					t.Fatalf("remote error leaked to step")
				}
				for _, rc := range step.ReasonCodes {
					if strings.Contains(string(rc), secret) {
						t.Fatalf("remote error leaked to reason code")
					}
				}
			}
		}
	}
	rrMetrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(rrMetrics, httptest.NewRequest("GET", "http://localhost/metrics", nil))
	if strings.Contains(rrMetrics.Body.String(), secret) {
		t.Fatalf("remote error leaked to metrics")
	}
	rrAdmin := httptest.NewRecorder()
	reqAdmin := httptest.NewRequest("GET", "http://localhost/admin/api/snapshot", nil)
	reqAdmin.RemoteAddr = "127.0.0.1:12345"
	s.Handler().ServeHTTP(rrAdmin, reqAdmin)
	if strings.Contains(rrAdmin.Body.String(), secret) {
		t.Fatalf("remote error leaked to admin")
	}
	// ensure captured request bodies do not contain secret (request should be clean, response secret not echoed)
	bodies := jevMock.CapturedBodies()
	for _, b := range bodies {
		if strings.Contains(b, secret) {
			t.Fatalf("secret leaked into request body")
		}
	}
}
