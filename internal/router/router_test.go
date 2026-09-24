package router

import (
	"fmt"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestCandidatesExcludeCooldown(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}, {ID: "b", Model: "b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	h := health.New(1, time.Hour)
	h.RecordFailure("p/a", "x", time.Millisecond)
	h.RecordSuccess("p/b", time.Millisecond)
	r := New(cfg, h)
	c := r.Candidates(Requirement{Model: "auto", Tools: true, Streaming: true})
	if len(c) != 1 || c[0].Deployment.ID != "p/b" {
		t.Fatalf("unexpected candidates %#v", c)
	}
}

func TestAdaptiveRoundRobinRotatesHealthyTopTier(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "adaptive_round_robin"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "a", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		{ID: "b", Model: "b", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
	}}}
	h := health.New(4, time.Hour)
	r := New(cfg, h)
	first := r.Candidates(Requirement{Model: "coding", Streaming: true})
	second := r.Candidates(Requirement{Model: "coding", Streaming: true})
	if len(first) != 2 || len(second) != 2 || first[0].Deployment.ID == second[0].Deployment.ID {
		t.Fatalf("expected rotation: first=%v second=%v", first, second)
	}
}

func TestFallbackOnUnknownClientModel(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.FallbackOnUnknownModel = true
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "deepseek-chat", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}}}
	h := health.New(4, time.Hour)
	r := New(cfg, h)
	h.RecordSuccess("p/m", time.Millisecond)
	got := r.Candidates(Requirement{Model: "claude-sonnet-custom", Streaming: true})
	if len(got) != 1 || got[0].Deployment.Model != "deepseek-chat" {
		t.Fatalf("unexpected fallback %#v", got)
	}
}

func TestFallbackDoesNotEscapeKnownUnavailableModel(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.FallbackOnUnknownModel = true
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "known", Model: "known-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		{ID: "other", Model: "other-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
	}}}
	h := health.New(1, time.Hour)
	h.RecordFailure("p/known", "down", time.Millisecond)
	r := New(cfg, h)

	got := r.Candidates(Requirement{Model: "known-model", Streaming: true})
	if len(got) != 0 {
		t.Fatalf("known model in cooldown must not fall back to unrelated deployment: %#v", got)
	}
}

func TestFallbackDoesNotBypassKnownModelCapabilities(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.FallbackOnUnknownModel = true
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "known", Model: "known-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: false}},
		{ID: "other", Model: "other-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
	}}}
	r := New(cfg, health.New(4, time.Hour))
	got := r.Candidates(Requirement{Model: "known-model", Streaming: true, Tools: true})
	if len(got) != 0 {
		t.Fatalf("known model capability mismatch must not route to unrelated model: %#v", got)
	}
}

func TestRoundRobinRotatesAcrossHealthyCandidates(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "round_robin"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "a", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
		{ID: "b", Model: "b", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
		{ID: "c", Model: "c", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
	}}}
	h := health.New(4, time.Hour)
	for _, id := range []string{"p/a", "p/b", "p/c"} {
		h.RecordSuccess(id, 10*time.Millisecond)
	}
	r := New(cfg, h)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		c := r.Candidates(Requirement{Model: "auto"})
		if len(c) != 3 {
			t.Fatalf("want 3 candidates got %d", len(c))
		}
		seen[c[0].Deployment.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("round robin did not rotate all first choices: %#v", seen)
	}
}

func TestLeastLatencyPrefersMeasuredFastHealthy(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "least_latency"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "slow", Model: "slow", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
		{ID: "fast", Model: "fast", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
	}}}
	h := health.New(4, time.Hour)
	h.RecordSuccess("p/slow", 200*time.Millisecond)
	h.RecordSuccess("p/fast", 10*time.Millisecond)
	r := New(cfg, h)
	c := r.Candidates(Requirement{Model: "auto"})
	if len(c) != 2 || c[0].Deployment.ID != "p/fast" {
		t.Fatalf("least_latency did not prefer fast: %#v", c)
	}
}

func TestClaudeAutoMatchesAllDeployments(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
	h := health.New(4, time.Hour)
	r := New(cfg, h)
	h.RecordSuccess("p/m", time.Millisecond)
	if got := len(r.Candidates(Requirement{Model: "claude-auto"})); got != 1 {
		t.Fatalf("claude-auto should match all; got %d", got)
	}
}

func TestRoundRobinNeverPromotesDegradedAheadOfHealthy(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "round_robin"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "a", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
		{ID: "b", Model: "b", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
		{ID: "bad", Model: "bad", Aliases: []string{"auto"}, Enabled: true, Weight: 1},
	}}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/a", 10*time.Millisecond)
	h.RecordSuccess("p/b", 11*time.Millisecond)
	h.RecordFailure("p/bad", "temporary", 12*time.Millisecond) // degraded, not cooldown
	r := New(cfg, h)
	for i := 0; i < 12; i++ {
		c := r.Candidates(Requirement{Model: "auto"})
		if len(c) != 3 {
			t.Fatalf("want 3 candidates got %d", len(c))
		}
		if c[0].Deployment.ID == "p/bad" {
			t.Fatalf("degraded deployment promoted ahead of healthy candidates on iteration %d: %#v", i, c)
		}
	}
}

func TestAdaptivePrefersConfiguredStrongHealthyDeployment(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "strong", Model: "strong", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 2},
		{ID: "weak", Model: "weak", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1},
	}}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/strong", 60*time.Millisecond)
	h.RecordSuccess("p/weak", 20*time.Millisecond)
	r := New(cfg, h)
	c := r.Candidates(Requirement{Model: "coding"})
	if len(c) != 2 || c[0].Deployment.ID != "p/strong" {
		t.Fatalf("adaptive routing did not preserve configured model strength: %#v", c)
	}
}

func TestAdaptiveNeverPromotesDegradedHighWeightAheadOfHealthy(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "healthy", Model: "healthy", Aliases: []string{"coding"}, Enabled: true, Priority: 100, Weight: 1},
		{ID: "degraded", Model: "degraded", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 100},
	}}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/healthy", 200*time.Millisecond)
	h.RecordFailure("p/degraded", "temporary", time.Millisecond)
	r := New(cfg, h)
	got := r.Candidates(Requirement{Model: "coding"})
	if len(got) != 2 || got[0].Deployment.ID != "p/healthy" {
		t.Fatalf("degraded deployment outranked healthy deployment: %#v", got)
	}
}

func TestAdaptiveRoundRobinNeverPromotesDegradedHighWeightAheadOfHealthy(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "adaptive_round_robin"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "healthy", Model: "healthy", Aliases: []string{"coding"}, Enabled: true, Priority: 100, Weight: 1},
		{ID: "degraded", Model: "degraded", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 100},
	}}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/healthy", 200*time.Millisecond)
	h.RecordFailure("p/degraded", "temporary", time.Millisecond)
	r := New(cfg, h)
	for i := 0; i < 8; i++ {
		got := r.Candidates(Requirement{Model: "coding"})
		if len(got) != 2 || got[0].Deployment.ID != "p/healthy" {
			t.Fatalf("degraded deployment outranked healthy deployment on iteration %d: %#v", i, got)
		}
	}
}

func TestReadyQueueRequiresSuccessfulHealthProofAndStaysSticky(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "strong", Model: "strong", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 2, Capabilities: config.Capabilities{Streaming: true}},
		{ID: "weak", Model: "weak", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
	}}}
	h := health.New(5, 30*time.Minute)
	r := New(cfg, h)

	if got := r.Candidates(Requirement{Model: "coding", Streaming: true}); len(got) != 0 {
		t.Fatalf("unprobed models must not enter ready queue: %#v", got)
	}

	h.RecordSuccess("p/strong", 100*time.Millisecond)
	h.RecordSuccess("p/weak", 10*time.Millisecond)
	first := r.Candidates(Requirement{Model: "coding", Streaming: true})
	second := r.Candidates(Requirement{Model: "coding", Streaming: true})
	if len(first) != 2 || len(second) != 2 || first[0].Deployment.ID != "p/strong" || second[0].Deployment.ID != "p/strong" {
		t.Fatalf("ready queue must stay sticky on configured strongest model: first=%#v second=%#v", first, second)
	}

	h.Quarantine("p/strong", "request failed", time.Millisecond)
	after := r.Candidates(Requirement{Model: "coding", Streaming: true})
	if len(after) != 1 || after[0].Deployment.ID != "p/weak" {
		t.Fatalf("quarantined first model must leave ready queue immediately: %#v", after)
	}
}

func TestReadyMeshP2CPrefersUnsaturatedProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.P2CWindow = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "busy", Name: "Busy", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m1", Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "free", Name: "Free", Type: "openai_compatible", BaseURL: "http://y", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m2", Enabled: true, Priority: 0, Weight: 1}}},
	}
	h := health.New(5, time.Hour)
	h.RecordSuccess("busy/m", 10*time.Millisecond)
	h.RecordSuccess("free/m", 10*time.Millisecond)
	r := New(cfg, h)
	got := r.Candidates(Requirement{
		Model:        "auto",
		SelectionKey: "req",
		ProviderLoad: map[string]ProviderLoad{
			"busy": {Active: 32, Waiting: 8, Limit: 32},
			"free": {Active: 1, Limit: 32},
		},
	})
	if len(got) != 2 || got[0].Deployment.ProviderID != "free" {
		t.Fatalf("ready mesh did not avoid saturation: %#v", got)
	}
}

func TestReadyMeshSessionAffinityPinsAfterSuccess(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.SessionAffinity = true
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "a", Enabled: true, Weight: 1},
		{ID: "b", Model: "b", Enabled: true, Weight: 1},
	}}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/a", 10*time.Millisecond)
	h.RecordSuccess("p/b", 10*time.Millisecond)
	r := New(cfg, h)
	req := Requirement{Model: "auto", SessionKey: "claude-session", SelectionKey: "first"}
	first := r.Candidates(req)
	if len(first) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(first))
	}
	r.ObserveSession(req, first[0].Deployment.ID)
	req.SelectionKey = "different"
	second := r.Candidates(req)
	if second[0].Deployment.ID != first[0].Deployment.ID {
		t.Fatalf("session moved from %s to %s", first[0].Deployment.ID, second[0].Deployment.ID)
	}
}

func TestReadyMeshCapabilityCooldownIsScoped(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		{ID: "b", Model: "b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
	}}}
	h := health.New(5, time.Hour)
	h.ConfigureAdvanced(5, time.Hour, 2, time.Hour)
	h.RecordSuccess("p/a", time.Millisecond)
	h.RecordSuccess("p/b", time.Millisecond)
	h.RecordScopeFailure("p/a", []string{"streaming"}, "x")
	h.RecordScopeFailure("p/a", []string{"streaming"}, "x")
	r := New(cfg, h)
	stream := r.Candidates(Requirement{Model: "auto", Streaming: true})
	if len(stream) != 1 || stream[0].Deployment.ID != "p/b" {
		t.Fatalf("bad scoped filter %#v", stream)
	}
	if plain := r.Candidates(Requirement{Model: "auto"}); len(plain) != 2 {
		t.Fatalf("global health poisoned %#v", plain)
	}
}

func TestEligibleRejectsCandidateAfterQuarantine(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true}}}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/m", time.Millisecond)
	r := New(cfg, h)
	if _, ok := r.Eligible("p/m", Requirement{Model: "auto"}); !ok {
		t.Fatal("healthy deployment unexpectedly ineligible")
	}
	h.Quarantine("p/m", "x", time.Millisecond)
	if _, ok := r.Eligible("p/m", Requirement{Model: "auto"}); ok {
		t.Fatal("stale candidate remained eligible")
	}
}

func TestSessionAffinityTableRemainsBoundedUnderUniqueIDs(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.SessionAffinity = true
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/m", time.Millisecond)
	r := New(cfg, h)
	for i := 0; i < maxSessionPins+500; i++ {
		r.ObserveSession(Requirement{Model: "auto", SessionKey: fmt.Sprintf("session-%d", i)}, "p/m")
	}
	if got := r.SessionCount(); got > maxSessionPins {
		t.Fatalf("session table grew to %d, limit=%d", got, maxSessionPins)
	}
}

func TestSpecificModelIndexCoversAllMatchForms(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "short", Model: "vendor/model-v1", Aliases: []string{"coding", "fast"},
			Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true},
		}},
	}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/short", time.Millisecond)
	r := New(cfg, h)

	for _, key := range []string{"p/short", "short", "vendor/model-v1", "coding", "fast"} {
		got := r.Candidates(Requirement{Model: key, Streaming: true})
		if len(got) != 1 || got[0].Deployment.ID != "p/short" {
			t.Fatalf("model key %q routed to %#v", key, got)
		}
		if indexed := r.byModel[key]; len(indexed) != 1 || indexed[0].ID != "p/short" {
			t.Fatalf("model key %q missing from index: %#v", key, indexed)
		}
	}
}

func TestProviderTypeRequirementFiltersCandidates(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{
		{ID: "o", Name: "O", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "m", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Reasoning: true}}}},
		{ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: "http://example.invalid", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "m", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Reasoning: true}}}},
	}
	h := health.New(5, time.Hour)
	h.RecordSuccess("o/m", time.Millisecond)
	h.RecordSuccess("a/m", time.Millisecond)
	r := New(cfg, h)
	got := r.Candidates(Requirement{Model: "coding", Reasoning: true, ProviderType: "anthropic_compatible"})
	if len(got) != 1 || got[0].Deployment.ProviderType != "anthropic_compatible" {
		t.Fatalf("provider type requirement ignored: %#v", got)
	}
}


func TestReadyMeshDiversifiesFailoverAcrossProviders(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.P2CWindow = 1
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: "http://p1.invalid", Enabled: true, Models: []config.ModelConfig{
			{ID: "a", Model: "a", Enabled: true, Priority: 0, Weight: 1},
			{ID: "b", Model: "b", Enabled: true, Priority: 0, Weight: 1},
		}},
		{ID: "p2", Name: "P2", Type: "openai_compatible", BaseURL: "http://p2.invalid", Enabled: true, Models: []config.ModelConfig{
			{ID: "c", Model: "c", Enabled: true, Priority: 0, Weight: 1},
		}},
	}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p1/a", time.Millisecond)
	h.RecordSuccess("p1/b", 2*time.Millisecond)
	h.RecordSuccess("p2/c", 3*time.Millisecond)
	r := New(cfg, h)

	got := r.Candidates(Requirement{Model: "auto", SelectionKey: "stable"})
	if len(got) != 3 {
		t.Fatalf("want 3 candidates got %d", len(got))
	}
	if got[0].Deployment.ID != "p1/a" {
		t.Fatalf("primary quality selection changed unexpectedly: %#v", got)
	}
	if got[1].Deployment.ProviderID != "p2" {
		t.Fatalf("first failover should leave the failed provider domain: %#v", got)
	}
	if got[2].Deployment.ID != "p1/b" {
		t.Fatalf("same-provider fallback should remain available after diversification: %#v", got)
	}
}
