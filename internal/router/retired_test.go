package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestRetiredDeploymentNeverSelectedUntilInvalidated(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "a", Model: "a", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1},
			{ID: "b", Model: "b", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1},
		},
	}}
	h := health.New(3, time.Hour)
	h.Retire("p/a", "EOL", "model_retired")
	r := New(cfg, h)

	got := r.Candidates(Requirement{Model: "coding"})
	if len(got) != 1 || got[0].Deployment.ID != "p/b" {
		t.Fatalf("retired deployment remained selectable: %#v", got)
	}

	h.Invalidate("p/a")
	got = r.Candidates(Requirement{Model: "coding"})
	if len(got) != 2 {
		t.Fatalf("identity invalidation did not restore deployment eligibility: %#v", got)
	}
}
