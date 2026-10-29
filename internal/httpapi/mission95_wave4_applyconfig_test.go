package httpapi

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave4ApplyBase(t *testing.T, decision []config.DecisionProviderConfig) (*Server, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	cfg.DecisionProviders = decision
	s := testGateway(t, cfg)
	return s, s.currentConfig()
}

// A rebuilt provider whose new base URL url.Parse rejects fails the
// whole reload before any state is swapped: Prepare runs before the
// disk write and the runtime lock.
func TestWave4ApplyConfigPrepareFailure(t *testing.T) {
	s, cfg := wave4ApplyBase(t, nil)
	cfg.Providers[0].BaseURL = "http://127.0.0.1:9/\x7f"
	if err := s.applyConfigLocked(cfg); err == nil {
		t.Fatal("reload with an unparseable base URL must fail in Prepare")
	}
	still := s.currentConfig()
	if len(still.Providers) != 1 || still.Providers[0].BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("failed reload must leave the old config in place: %+v", still.Providers)
	}
	if len(s.rt.All()) != 1 {
		t.Fatalf("router deployments = %d, want 1", len(s.rt.All()))
	}
}

func wave4Jev(id, key, keyEnv string) config.DecisionProviderConfig {
	return config.DecisionProviderConfig{ID: id, Type: "jev", APIKey: key, APIKeyEnv: keyEnv}
}

func TestWave4ApplyConfigDecisionProviderIdentity(t *testing.T) {
	t.Run("new-provider-registered", func(t *testing.T) {
		s, cfg := wave4ApplyBase(t, nil)
		cfg.DecisionProviders = []config.DecisionProviderConfig{wave4Jev("jev-new", "k1", "")}
		if err := s.applyConfigLocked(cfg); err != nil {
			t.Fatalf("reload: %v", err)
		}
		p, ok := s.decisionRegistry.Get("jev-new")
		if !ok {
			t.Fatal("new decision provider must be registered")
		}
		if h := p.Health(); h.Status != "healthy" {
			t.Fatalf("jev-new health = %+v, want healthy", h)
		}
	})

	t.Run("old-env-key-change-resets-state", func(t *testing.T) {
		t.Setenv("WAVE4_OLD_KEY", "old-key")
		s, cfg := wave4ApplyBase(t, []config.DecisionProviderConfig{wave4Jev("jev-old", "", "WAVE4_OLD_KEY")})
		s.decisionOrchestrator.ProviderState().RecordFailure("jev-old")
		cfg.DecisionProviders = []config.DecisionProviderConfig{wave4Jev("jev-old", "different-key", "")}
		if err := s.applyConfigLocked(cfg); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if _, ok := s.decisionOrchestrator.ProviderState().SnapshotOne("jev-old"); ok {
			t.Fatal("key identity change must reset provider state")
		}
	})

	t.Run("new-env-key-equal-preserves-state", func(t *testing.T) {
		t.Setenv("WAVE4_NEW_KEY", "shared-key")
		s, cfg := wave4ApplyBase(t, []config.DecisionProviderConfig{wave4Jev("jev-old", "shared-key", "")})
		s.decisionOrchestrator.ProviderState().RecordFailure("jev-old")
		cfg.DecisionProviders = []config.DecisionProviderConfig{wave4Jev("jev-old", "", "WAVE4_NEW_KEY")}
		if err := s.applyConfigLocked(cfg); err != nil {
			t.Fatalf("reload: %v", err)
		}
		st, ok := s.decisionOrchestrator.ProviderState().SnapshotOne("jev-old")
		if !ok || st.ConsecutiveFailures != 1 {
			t.Fatalf("equal env-resolved key must preserve state: %+v ok=%v", st, ok)
		}
	})

	t.Run("jev-creation-failure-skipped", func(t *testing.T) {
		s, cfg := wave4ApplyBase(t, nil)
		// Config validation accepts this URL shape, but the jev client
		// rejects embedded userinfo at construction time.
		bad := wave4Jev("jev-bad", "k", "")
		bad.BaseURL = "https://user:pass@example.com"
		cfg.DecisionProviders = []config.DecisionProviderConfig{bad}
		if err := s.applyConfigLocked(cfg); err != nil {
			t.Fatalf("a failed decision provider must not fail the reload: %v", err)
		}
		if _, ok := s.decisionRegistry.Get("jev-bad"); ok {
			t.Fatal("uncreatable decision provider must not be registered")
		}
	})

	t.Run("removed-provider-placeholder-and-reset", func(t *testing.T) {
		s, cfg := wave4ApplyBase(t, []config.DecisionProviderConfig{wave4Jev("jev-gone", "k", "")})
		s.decisionOrchestrator.ProviderState().RecordFailure("jev-gone")
		cfg.DecisionProviders = nil
		if err := s.applyConfigLocked(cfg); err != nil {
			t.Fatalf("reload: %v", err)
		}
		p, ok := s.decisionRegistry.Get("jev-gone")
		if !ok {
			t.Fatal("removed provider must keep a disabled placeholder")
		}
		if h := p.Health(); h.Message != "disabled" {
			t.Fatalf("jev-gone health = %+v, want disabled", h)
		}
		if _, ok := s.decisionOrchestrator.ProviderState().SnapshotOne("jev-gone"); ok {
			t.Fatal("removed provider state must be reset")
		}
	})
}

// Enabling autopilot on reload rebuilds the controller when the target
// policy exists; the previous nil controller forces a rebuild.
func TestWave4ApplyConfigAutopilotRebuild(t *testing.T) {
	s, cfg := wave4ApplyBase(t, nil)
	if s.autopilotSnapshot() != nil {
		t.Fatal("autopilot must start disabled")
	}
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{{ID: "pol1", Weights: config.DecisionPolicyWeights{Reliability: 1}}}
	cfg.Autopilot.Enabled = true
	cfg.Autopilot.PolicyID = "pol1"
	if err := s.applyConfigLocked(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	ctrl := s.autopilotSnapshot()
	if ctrl == nil {
		t.Fatal("autopilot controller must be built on reload")
	}
	if ctrl.TargetPolicyID() != "pol1" {
		t.Fatalf("target = %q, want pol1", ctrl.TargetPolicyID())
	}
}
