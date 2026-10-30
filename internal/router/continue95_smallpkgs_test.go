package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func Continue95TestConfig() config.Config {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}},
		{ID: "b", Model: "b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}},
	}}}
	return cfg
}

func Continue95HealthyRouter(t *testing.T) *Router {
	t.Helper()
	cfg := Continue95TestConfig()
	h := health.New(4, time.Hour)
	h.RecordSuccess("p/a", time.Millisecond)
	h.RecordSuccess("p/b", time.Millisecond)
	return New(cfg, h)
}

func TestContinue95ReloadSkips(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{
		{ID: "off", Name: "Off", Type: "openai_compatible", BaseURL: "http://x", Enabled: false, Models: []config.ModelConfig{{ID: "z", Model: "z", Enabled: true}}},
		{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{
			{ID: "off-model", Model: "off-model", Enabled: false},
			{ID: "a", Model: "a", Aliases: []string{"", "a", "a"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		}},
	}
	r := New(cfg, health.New(4, time.Hour))
	all := r.All()
	if len(all) != 1 || all[0].ID != "p/a" {
		t.Fatalf("Reload should keep only the enabled model, got %v", all)
	}
	// Session pruning on Reload: an invalid-deployment pin and an
	// expired pin are both dropped when affinity stays enabled.
	cfg.Routing.SessionAffinity = true
	r.Reload(cfg)
	req := Requirement{Model: "a", SessionKey: "sess-1"}
	r.ObserveSession(req, "p/a")
	r.sessionMu.Lock()
	r.sessions["stale-key"] = sessionPin{Deployment: "gone/dep", Expires: time.Now().Add(time.Hour)}
	r.sessions["expired-key"] = sessionPin{Deployment: "p/a", Expires: time.Now().Add(-time.Hour)}
	r.sessionMu.Unlock()
	r.Reload(cfg)
	r.sessionMu.RLock()
	n := len(r.sessions)
	r.sessionMu.RUnlock()
	if n != 1 {
		t.Errorf("Reload should prune invalid/expired pins, %d remain", n)
	}
	// Disabling affinity clears every pin.
	cfg.Routing.SessionAffinity = false
	r.Reload(cfg)
	if got := r.SessionCount(); got != 0 {
		t.Errorf("disabling affinity should clear pins, count = %d", got)
	}
}

func TestContinue95HealthRankAndPressure(t *testing.T) {
	if got := healthRank(health.HalfOpen); got != 2 {
		t.Errorf("HalfOpen rank = %d, want 2", got)
	}
	if got := healthRank(health.Degraded); got != 3 {
		t.Errorf("Degraded rank = %d, want 3 (default)", got)
	}
	if got := capacityPressure(ProviderLoad{QuotaExhausted: true}); got != 4 {
		t.Errorf("quota-exhausted pressure = %v, want 4", got)
	}
	if got := capacityPressure(ProviderLoad{Active: -5, Limit: 10}); got != 0 {
		t.Errorf("negative pressure should clamp to 0, got %v", got)
	}
	if got := capacityPressure(ProviderLoad{Active: 100, Limit: 10}); got != 4 {
		t.Errorf("over-limit pressure should clamp to 4, got %v", got)
	}
	if got := capacityPressure(ProviderLoad{QuotaPressure: 5}); got != 4 {
		t.Errorf("quota pressure above 4 should return 4, got %v", got)
	}
	if got := capacityPressure(ProviderLoad{Active: 1, Limit: 10, QuotaPressure: 0.5}); got != 0.5 {
		t.Errorf("quota pressure should win when higher, got %v", got)
	}
	// scored(): HalfOpen adjustment and LoadForProvider override.
	r := &Router{}
	cfg := config.Default()
	sc := r.scored(Deployment{ID: "p/a", ProviderID: "p", Weight: 1}, health.State{Status: health.HalfOpen}, Requirement{LoadForProvider: func(string) ProviderLoad { return ProviderLoad{Active: 0, Limit: 0} }}, cfg)
	if sc.HealthAdjustment != -10 {
		t.Errorf("HalfOpen adjustment = %v, want -10", sc.HealthAdjustment)
	}
	// simulatedRotationOffset edge cases.
	if got := simulatedRotationOffset(Requirement{}, 1); got != 0 {
		t.Errorf("window<=1 offset = %d, want 0", got)
	}
	if got := simulatedRotationOffset(Requirement{}, 5); got != 0 {
		t.Errorf("keyless modeless offset = %d, want 0", got)
	}
	if got := simulatedRotationOffset(Requirement{SelectionKey: "k", Model: "a"}, 5); got < 0 || got >= 5 {
		t.Errorf("keyed offset = %d, want in [0,5)", got)
	}
}

func TestContinue95EligibleExclusions(t *testing.T) {
	r := Continue95HealthyRouter(t)
	cfg := config.Default()
	dep := Deployment{ID: "p/a", ProviderID: "p", ProviderType: "openai_compatible", Model: "a", Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}}
	cases := []struct {
		name string
		req  Requirement
		caps config.Capabilities
		want string
	}{
		{"vision", Requirement{Model: "auto", Vision: true}, config.Capabilities{Streaming: true, Tools: true, Reasoning: true}, ExcludeMissingVision},
		{"streaming", Requirement{Model: "auto", Streaming: true}, config.Capabilities{Tools: true, Vision: true, Reasoning: true}, ExcludeMissingStreaming},
		{"reasoning", Requirement{Model: "auto", Reasoning: true}, config.Capabilities{Streaming: true, Tools: true, Vision: true}, ExcludeMissingReasoning},
	}
	for _, tc := range cases {
		d := dep
		d.Capabilities = tc.caps
		if _, reason := r.evaluateDeployment(d, tc.req, cfg, false, nil); reason != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, reason, tc.want)
		}
	}
	if _, ok := r.Eligible("p/a", Requirement{Model: "auto", Vision: true, Streaming: true, Tools: true, Reasoning: true}); !ok {
		t.Error("fully capable deployment should be eligible")
	}
	if _, ok := r.Eligible("missing/dep", Requirement{Model: "auto"}); ok {
		t.Error("unknown deployment should not be eligible")
	}
}

func TestContinue95PinsAndSessionRecords(t *testing.T) {
	cfg := Continue95TestConfig()
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 0 // default TTL branch: one hour
	h := health.New(4, time.Hour)
	h.RecordSuccess("p/a", time.Millisecond)
	h.RecordSuccess("p/b", time.Millisecond)
	r := New(cfg, h)
	req := Requirement{Model: "a", SessionKey: "sess-9"}
	r.ObserveSession(req, "p/a")
	if got := r.PinnedDeploymentID(req); got != "p/a" {
		t.Errorf("pinned deployment = %q, want p/a", got)
	}
	// SessionCount drops expired pins and keeps live ones.
	r.sessionMu.Lock()
	r.sessions["expired"] = sessionPin{Deployment: "p/a", Expires: time.Now().Add(-time.Hour)}
	r.sessionMu.Unlock()
	if got := r.SessionCount(); got != 1 {
		t.Errorf("SessionCount = %d, want 1 live pin", got)
	}
	// ObserveSessionCache early return when affinity is off, and the
	// pin-record lookup early return for the same reason.
	cfg.Routing.SessionAffinity = false
	r.Reload(cfg)
	r.ObserveSessionCache(req, "p/a", 100, 50)
	if _, ok := r.sessionPinRecord(req, cfg); ok {
		t.Error("pin record with affinity off should report false")
	}
	// ObserveSession with affinity off is a no-op.
	r.ObserveSession(req, "p/a")
	if got := r.SessionCount(); got != 0 {
		t.Errorf("ObserveSession with affinity off should not pin, count = %d", got)
	}
	// Default TTL: a fresh pin expires about an hour out.
	cfg.Routing.SessionAffinity = true
	r.Reload(cfg)
	r.ObserveSession(req, "p/a")
	bucket := r.affinityBucket(req)
	r.sessionMu.RLock()
	pin, ok := r.sessions[bucket]
	r.sessionMu.RUnlock()
	if !ok || time.Until(pin.Expires) < 30*time.Minute {
		t.Errorf("default-TTL pin = %+v ok=%v, want ~1h expiry", pin, ok)
	}
}

func TestContinue95CostAndBetter(t *testing.T) {
	d := Deployment{InputCostPerMTok: -10, OutputCostPerMTok: 1}
	if cost, ok := estimatedRequestCost(d, Requirement{EstimatedInputTokens: 1000, MaxOutputTokens: 1000}); ok || cost != 0 {
		t.Errorf("negative total cost = %v, %v, want 0,false", cost, ok)
	}
	if cost, ok := estimatedRequestCost(d, Requirement{EstimatedInputTokens: 0, MaxOutputTokens: 1000}); ok || cost != 0 {
		t.Errorf("zero input tokens = %v, %v, want 0,false", cost, ok)
	}
	d2 := Deployment{InputCostPerMTok: 2, OutputCostPerMTok: 3}
	if cost, ok := estimatedRequestCost(d2, Requirement{EstimatedInputTokens: 1000, MaxOutputTokens: 1000}); !ok || cost != 0.005 {
		t.Errorf("cost = %v, %v, want 0.005,true", cost, ok)
	}
	a := Scored{Score: 10, CapacityPressure: 2, Deployment: Deployment{ID: "p/a"}}
	b := Scored{Score: 10, CapacityPressure: 1, Deployment: Deployment{ID: "p/b"}}
	if !better(b, a) {
		t.Error("equal scores should prefer lower capacity pressure")
	}
	if better(a, b) {
		t.Error("higher capacity pressure should not win on equal scores")
	}
	// diversify with start < 1 clamps to 1 instead of indexing -1.
	out := []Scored{
		{Deployment: Deployment{ID: "p/a", ProviderID: "p", Priority: 0}},
		{Deployment: Deployment{ID: "p/a2", ProviderID: "p", Priority: 0}},
		{Deployment: Deployment{ID: "q/b", ProviderID: "q", Priority: 0}},
	}
	diversifyProviderFailover(out, 0, 0)
	if out[1].Deployment.ProviderID != "q" {
		t.Errorf("diversified order = %v, want provider q second", out)
	}
}

func TestContinue95OrderAndStrategy(t *testing.T) {
	r := Continue95HealthyRouter(t)
	cfg := config.Default()
	// orderCostAware with fewer than 2 candidates returns early.
	one := []Scored{{Deployment: Deployment{ID: "p/a", Priority: 0}, Score: 5}}
	r.orderCostAware(one, Requirement{Model: "a"}, cfg)
	if len(one) != 1 {
		t.Error("single-candidate orderCostAware should leave the slice alone")
	}
	// orderCostAware pin promotion: the pinned deployment moves first
	// even when another candidate scores higher.
	cfg.Routing.SessionAffinity = true
	r.Reload(cfg)
	req := Requirement{Model: "a", SessionKey: "sess-pin"}
	r.ObserveSession(req, "p/a")
	out := []Scored{
		{Deployment: Deployment{ID: "p/b", Priority: 0}, Score: 90},
		{Deployment: Deployment{ID: "p/a", Priority: 0}, Score: 10},
	}
	r.orderCostAware(out, req, cfg)
	if out[0].Deployment.ID != "p/a" {
		t.Errorf("pin promotion failed, order = %v", out)
	}
	// orderReadyMesh with fewer than 2 candidates returns early.
	r.orderReadyMesh(one, req, cfg, false)
	if len(one) != 1 {
		t.Error("single-candidate orderReadyMesh should leave the slice alone")
	}
	// least_latency comparator branches: rank, zero-latency-last both ways.
	lat := []Scored{
		{Deployment: Deployment{ID: "p/b"}, Health: health.State{Status: health.Healthy, EWMALatencyMS: 0}},
		{Deployment: Deployment{ID: "p/a"}, Health: health.State{Status: health.Healthy, EWMALatencyMS: 50}},
		{Deployment: Deployment{ID: "q/c"}, Health: health.State{Status: health.Degraded, EWMALatencyMS: 10}},
	}
	cfg.Routing.Strategy = "least_latency"
	r.applyStrategyMode(lat, Requirement{}, cfg, false)
	if lat[0].Deployment.ID != "p/a" || lat[1].Deployment.ID != "p/b" || lat[2].Deployment.ID != "q/c" {
		t.Errorf("least_latency order = %v, want a(50), b(0), c(degraded)", lat)
	}
	// AllLimit caps at the limit and still reports the total.
	r2 := Continue95HealthyRouter(t)
	got, total := r2.AllLimit(1)
	if len(got) != 1 || total != 2 {
		t.Errorf("AllLimit(1) = %d items, total %d, want 1/2", len(got), total)
	}
	// Readiness skips providers whose circuit is open.
	cfg3 := Continue95TestConfig()
	h3 := health.New(1, time.Hour)
	// Provider evidence counts distinct deployments in the window, so
	// three distinct deployment ids are needed to open the circuit.
	for _, dep := range []string{"p/a", "p/b", "p/c"} {
		h3.RecordProviderFailure("p", dep, "down")
	}
	r3 := New(cfg3, h3)
	if h3.ProviderAvailable("p") {
		t.Fatal("provider circuit should be open after 3 failures")
	}
	total3, usable3 := r3.Readiness("priority")
	if total3 != 2 || usable3 != 0 {
		t.Errorf("Readiness with open circuit = %d/%d, want 2/0", total3, usable3)
	}
}
