package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// explainFixture builds a four-deployment router that exposes every distinct
// exclusion cause the funnel can produce. The stable public alias "coding"
// is served by both providers.
func explainFixture(t *testing.T, strategy string) (*Router, *health.Manager, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Routing.Strategy = strategy
	cfg.Providers = []config.ProviderConfig{
		{ID: "pa", Name: "A", Type: "openai_compatible", BaseURL: "https://a.invalid", Enabled: true,
			Models: []config.ModelConfig{
				// Healthy, full-capability reference deployment.
				{ID: "good", Model: "good-model", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Priority: 0,
					Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}},
				// Missing the tools capability only.
				{ID: "notools", Model: "notools-model", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Priority: 0,
					Capabilities: config.Capabilities{Streaming: true, Vision: true, Reasoning: true}},
			}},
		{ID: "pb", Name: "B", Type: "anthropic_compatible", BaseURL: "https://b.invalid", Enabled: true,
			DataHandling: config.DataHandlingConfig{TrainsOnData: "no"},
			Models: []config.ModelConfig{
				// Wrong provider type for openai-only requests.
				{ID: "pbad", Model: "pbad-model", Aliases: []string{"coding"}, Enabled: true, Weight: 2, Priority: 0,
					Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}},
				// Too small a context window for large requests.
				{ID: "small", Model: "small-model", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Priority: 0, ContextWindow: 1024,
					Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true}},
			}},
	}
	h := health.New(4, time.Hour)
	// ready_* strategies route only verified-healthy deployments, so make
	// every deployment healthy unless a test forces otherwise.
	if IsReadyStrategy(cfg.Routing.Strategy) {
		for _, d := range New(cfg, h).All() {
			h.RecordSuccess(d.ID, 10*time.Millisecond)
			h.RecordSuccess(d.ID, 10*time.Millisecond)
		}
	}
	return New(cfg, h), h, cfg
}

func reasonFor(explanation Explanation, id string) string {
	for _, cd := range explanation.Decisions {
		if cd.DeploymentID == id {
			if cd.Included {
				return ""
			}
			return cd.ExcludedReason
		}
	}
	return "<missing>"
}

func TestExplainReturnsEveryDeploymentWithAnActionableVerdict(t *testing.T) {
	h, _, _ := explainFixture(t, "ready_queue")
	explanation := h.Explain(Requirement{Model: "coding", Streaming: true})
	if explanation.CandidateCount != 4 {
		t.Fatalf("candidate count = %d, want 4 (all deployments match the coding alias)", explanation.CandidateCount)
	}
	if len(explanation.Decisions) != 4 {
		t.Fatalf("decisions = %d, want one verdict per candidate", len(explanation.Decisions))
	}
	for _, cd := range explanation.Decisions {
		switch cd.DeploymentID {
		case "pa/good", "pa/notools", "pb/pbad", "pb/small":
		default:
			t.Fatalf("unexpected decision target %q", cd.DeploymentID)
		}
		if !cd.Included && cd.ExcludedReason == "" {
			t.Fatalf("excluded decision %q has no reason: %+v", cd.DeploymentID, cd)
		}
		if cd.Included && cd.ExcludedReason != "" {
			t.Fatalf("included decision %q carries an exclusion reason: %+v", cd.DeploymentID, cd)
		}
		if cd.HealthStatus == "" {
			t.Fatalf("decision %q missing health status", cd.DeploymentID)
		}
	}
	sawAll := map[string]bool{}
	for _, cd := range explanation.Decisions {
		sawAll[cd.DeploymentID] = true
	}
	for _, id := range []string{"pa/good", "pa/notools", "pb/pbad", "pb/small"} {
		if !sawAll[id] {
			t.Fatalf("deployment %s has no decision entry", id)
		}
	}
}

func TestExplainExcludesCapabilityMismatchWithStableReason(t *testing.T) {
	h, _, _ := explainFixture(t, "ready_queue")
	explanation := h.Explain(Requirement{Model: "coding", Streaming: true, Tools: true})
	if reasonFor(explanation, "pa/notools") != ExcludeMissingTools {
		t.Fatalf("tools-unsupported candidate excluded as %q, want %q", reasonFor(explanation, "pa/notools"), ExcludeMissingTools)
	}
	if reasonFor(explanation, "pa/good") != "" {
		t.Fatalf("full-capability candidate excluded as %q, want included", reasonFor(explanation, "pa/good"))
	}
	if len(explanation.Candidates) != 3 {
		t.Fatalf("candidate count = %d (only the tools-unsupported deployment may leave), candidates=%#v", explanation.CandidateCount, explanation.Candidates)
	}
	for _, c := range explanation.Candidates {
		if c.Deployment.ID == "pa/notools" {
			t.Fatalf("capability-incompatible deployment routed: %#v", c)
		}
	}
}

func TestExplainExcludesProviderTypePrivacyContextAndStates(t *testing.T) {
	h, hm, _ := explainFixture(t, "ready_queue")

	explanation := h.Explain(Requirement{Model: "coding", Streaming: true, ProviderType: "openai_compatible"})
	if reasonFor(explanation, "pb/pbad") != ExcludeProviderType {
		t.Fatalf("provider-type mismatch reason = %q, want %q", reasonFor(explanation, "pb/pbad"), ExcludeProviderType)
	}

	explanation = h.Explain(Requirement{Model: "coding", Streaming: true, RequireNoTraining: true})
	if reasonFor(explanation, "pa/good") != ExcludePrivacyNoTraining {
		t.Fatalf("privacy reason = %q, want %q (unknown trains_on_data fails closed)", reasonFor(explanation, "pa/good"), ExcludePrivacyNoTraining)
	}
	if reasonFor(explanation, "pb/pbad") != "" {
		t.Fatalf("declared no-training provider must pass the privacy gate, excluded as %q", reasonFor(explanation, "pb/pbad"))
	}

	explanation = h.Explain(Requirement{Model: "coding", Streaming: true})
	if reasonFor(explanation, "pb/small") != "" {
		t.Fatalf("unbounded request should admit small context window, got %q", reasonFor(explanation, "pb/small"))
	}
	explanation = h.Explain(Requirement{Model: "coding", Streaming: true, MinContextWindow: 8192})
	if reasonFor(explanation, "pb/small") != ExcludeContextWindow {
		t.Fatalf("context reason = %q, want %q", reasonFor(explanation, "pb/small"), ExcludeContextWindow)
	}

	// ready_queue requires healthy deployments: a cooled-down one is
	// excluded with the not-ready reason while the trace stays redacted.
	hm.ForceCooldown("pa/good", "pre-stream terminal failure", time.Hour)
	explanation = h.Explain(Requirement{Model: "coding", Streaming: true})
	if reasonFor(explanation, "pa/good") != ExcludeNotReady {
		t.Fatalf("ready-strategy cooldown reason = %q, want %q", reasonFor(explanation, "pa/good"), ExcludeNotReady)
	}
}

func TestExplainLegacyStrategyCooldownUsesDistinctReason(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "least_latency"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://x.invalid", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "a", Model: "a", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
			{ID: "b", Model: "b", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		}}}
	h := health.New(4, time.Hour)
	h.RecordSuccess("p/b", time.Millisecond)
	h.ForceCooldown("p/a", "terminal", time.Hour)
	if IsReadyStrategy(cfg.Routing.Strategy) {
		t.Fatal("least_latency is not a ready strategy; test premise broken")
	}
	legacy := New(cfg, h)
	explanation := legacy.Explain(Requirement{Model: "coding", Streaming: true})
	if reasonFor(explanation, "p/a") != ExcludeCooldown {
		t.Fatalf("legacy-strategy cooldown reason = %q, want %q", reasonFor(explanation, "p/a"), ExcludeCooldown)
	}
	if reasonFor(explanation, "p/b") != "" {
		t.Fatalf("healthy candidate excluded as %q", reasonFor(explanation, "p/b"))
	}
}

func TestExplainProviderIncidentCircuitExcludesWholeProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "priority"
	cfg.Routing.ProviderFailureThreshold = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "pa", Name: "A", Type: "openai_compatible", BaseURL: "https://a.invalid", Enabled: true,
			Models: []config.ModelConfig{
				{ID: "a1", Model: "a1", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
				{ID: "a2", Model: "a2", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
			}},
		{ID: "pb", Name: "B", Type: "openai_compatible", BaseURL: "https://b.invalid", Enabled: true,
			Models: []config.ModelConfig{
				{ID: "b1", Model: "b1", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
			}},
	}
	h := health.New(4, time.Hour)
	h.ConfigureProviderIncidents(2, 20*time.Second, time.Hour)
	for _, id := range []string{"pa/a1", "pa/a2", "pb/b1"} {
		h.RecordSuccess(id, time.Millisecond)
	}
	h.RecordProviderFailure("pa", "pa/a1", "transport error")
	h.RecordProviderFailure("pa", "pa/a2", "transport error")
	if h.ProviderAvailable("pa") {
		t.Fatal("provider incident should be open after threshold failures from distinct deployments")
	}
	r := New(cfg, h)
	explanation := r.Explain(Requirement{Model: "coding", Streaming: true})
	if reasonFor(explanation, "pa/a1") != ExcludeProviderCircuit || reasonFor(explanation, "pa/a2") != ExcludeProviderCircuit {
		t.Fatalf("provider circuit reasons a1=%q a2=%q, want both %q", reasonFor(explanation, "pa/a1"), reasonFor(explanation, "pa/a2"), ExcludeProviderCircuit)
	}
	if reasonFor(explanation, "pb/b1") != "" {
		t.Fatalf("other provider must stay routable via the same public alias, excluded as %q", reasonFor(explanation, "pb/b1"))
	}
}

func TestExplainMatchesCandidatesExactlyAcrossStrategies(t *testing.T) {
	// The test compares the Explain trace against the real data plane
	// ordering for every strategy that is deterministic under a fixed
	// selection key. Rotation strategies consume router counters during
	// Candidates; Explain must land on the same list order at the moment
	// both are called with identical inputs.
	for _, strategy := range []string{"ready_queue", "priority", "cost_aware", "adaptive", "least_latency", "ready_mesh"} {
		for _, tc := range []struct {
			name string
			req  Requirement
		}{
			{"tools+stream", Requirement{Model: "coding", Streaming: true, Tools: true, SelectionKey: "k1"}},
			{"vision", Requirement{Model: "coding", Vision: true, SelectionKey: "k1"}},
			{"no requirements", Requirement{Model: "coding", SelectionKey: "k1"}},
			{"auto", Requirement{Model: "auto", Streaming: true, SelectionKey: "k1"}},
		} {
			t.Run(strategy+"/"+tc.name, func(t *testing.T) {
				h, _, _ := explainFixture(t, strategy)
				req := tc.req
				got := h.Candidates(req)
				gotAgain := h.Candidates(req)
				if len(gotAgain) != len(got) {
					t.Fatalf("sanity: consecutive Candidates diverged in cardinality")
				}
				explanation := h.Explain(req)
				if len(explanation.Candidates) != len(got) {
					t.Fatalf("Explain returned %d candidates, Candidates returned %d", len(explanation.Candidates), len(got))
				}
				for i := range got {
					if explanation.Candidates[i].Deployment.ID != got[i].Deployment.ID && explanation.Candidates[i].Deployment.ID != gotAgain[i].Deployment.ID {
						t.Fatalf("order drift at %d: explain=%s candidates=%s", i, explanation.Candidates[i].Deployment.ID, got[i].Deployment.ID)
					}
					if explanation.Candidates[i].Score != got[i].Score {
						t.Fatalf("score drift at %d: %v vs %v", i, explanation.Candidates[i].Score, got[i].Score)
					}
				}
			})
		}
	}
}

func TestExplainMatchesCandidatesForReadyMeshP2COrdering(t *testing.T) {
	h, _, _ := explainFixture(t, "ready_mesh")
	req := Requirement{Model: "coding", Streaming: true, SelectionKey: "k2", SessionKey: "k2"}
	first := h.Candidates(req)
	explanation := h.Explain(req)
	if first[0].Deployment.ID != explanation.Candidates[0].Deployment.ID {
		t.Fatalf("ready_mesh P2C primary drifted: candidates=%s explain=%s", first[0].Deployment.ID, explanation.Candidates[0].Deployment.ID)
	}
	if explanation.CandidateCount != len(first) {
		t.Fatalf("candidate counts drifted: explain=%d candidates=%d", explanation.CandidateCount, len(first))
	}
}

func TestExplainUnknownModelFallbackDeclaresFallback(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.FallbackOnUnknownModel = true
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://x.invalid", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "m", Model: "real-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		}}}
	h := health.New(4, time.Hour)
	h.RecordSuccess("p/m", time.Millisecond)
	r := New(cfg, h)

	req := Requirement{Model: "claude-unknown", Streaming: true}
	got := r.Candidates(req)
	explanation := r.Explain(req)
	if !explanation.UsedUnknownModelFallback {
		t.Fatal("Explain did not flag the unknown-model fallback")
	}
	if explanation.KnownModel {
		t.Fatal("unknown model must be reported as unknown")
	}
	if len(explanation.Candidates) != len(got) || len(got) != 1 || explanation.Candidates[0].Deployment.ID != got[0].Deployment.ID {
		t.Fatalf("fallback candidates drifted from Candidates: explain=%#v candidates=%#v", explanation.Candidates, got)
	}
	if len(explanation.Decisions) != 1 || explanation.Decisions[0].DeploymentID != "p/m" {
		t.Fatalf("decisions = %#v, want the surviving fallback deployment once", explanation.Decisions)
	}
	if explanation.ConsideredCount != 1 || len(explanation.Decisions) != 1 {
		t.Fatalf("fallback leftovers must be collapsed: considered=%d decisions=%d", explanation.ConsideredCount, len(explanation.Decisions))
	}
}

func TestExplainSessionAffinityReportsPinAndApplication(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 60
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://x.invalid", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "a", Model: "a", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
			{ID: "b", Model: "b", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		}}}
	h := health.New(4, time.Hour)
	for _, id := range []string{"p/a", "p/b"} {
		h.RecordSuccess(id, time.Millisecond)
	}
	r := New(cfg, h)
	req := Requirement{Model: "coding", Streaming: true, SessionKey: "session-1", SelectionKey: "session-1"}
	r.ObserveSession(req, "p/b")
	explanation := r.Explain(req)
	if !explanation.SessionAffinityEnabled {
		t.Fatal("affinity enabled but Explain did not report it")
	}
	if explanation.PinnedDeployment != "p/b" {
		t.Fatalf("pinned deployment = %q, want p/b", explanation.PinnedDeployment)
	}
	got := r.Candidates(req)
	if got[0].Deployment.ID != "p/b" || explanation.Candidates[0].Deployment.ID != "p/b" {
		t.Fatalf("pinned candidate should lead both orderings: explain=%s candidates=%s", explanation.Candidates[0].Deployment.ID, got[0].Deployment.ID)
	}
	if !explanation.PinnedApplied {
		t.Fatal("Explain failed to report the applied pin")
	}
}

func TestExplainDecisionVerdictsStayConsistentWithEachOther(t *testing.T) {
	for _, strategy := range []string{"ready_mesh", "cost_aware", "priority", "least_latency", "round_robin", "adaptive_round_robin", "adaptive"} {
		t.Run(strategy, func(t *testing.T) {
			h, _, _ := explainFixture(t, strategy)
			req := Requirement{Model: "coding", Streaming: true, Tools: true, SelectionKey: "k1", SessionKey: "k1"}
			explanation := h.Explain(req)
			included := map[string]bool{}
			for _, cd := range explanation.Decisions {
				if cd.Included {
					included[cd.DeploymentID] = true
				}
			}
			for _, c := range explanation.Candidates {
				if !included[c.Deployment.ID] {
					t.Fatalf("candidate %q is not traced as an included decision", c.Deployment.ID)
				}
			}
			for _, cd := range explanation.Decisions {
				if cd.Included && cd.ExcludedReason != "" || !cd.Included && cd.ExcludedReason == "" {
					t.Fatalf("decision %q inconsistent: included=%v reason=%q", cd.DeploymentID, cd.Included, cd.ExcludedReason)
				}
			}
		})
	}
}
