package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// Regression: Candidates applies Routing.FallbackOnUnknownModel (it selects
// fallback deployments with the model-match check relaxed), but Eligible —
// the per-attempt re-check behind currentRouteCandidate — hardcoded the
// strict model match. Every fallback candidate was therefore silently
// dropped at attempt time and the request died with zero attempts made.
func fallbackTestRouter(t *testing.T, fallback bool) *Router {
	t.Helper()
	cfg := config.Default()
	cfg.ApplyDefaults()
	cfg.Routing.Strategy = "priority"
	cfg.Routing.FallbackOnUnknownModel = fallback
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.invalid", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "vendor/model-v1", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true},
		}},
	}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/m", time.Millisecond)
	return New(cfg, h)
}

func TestEligibleHonorsFallbackOnUnknownModel(t *testing.T) {
	r := fallbackTestRouter(t, true)
	req := Requirement{Model: "totally-unknown", Streaming: true}
	if got := r.Candidates(req); len(got) != 1 {
		t.Fatalf("fallback selection should yield 1 candidate, got %d", len(got))
	}
	if _, ok := r.Eligible("p/m", req); !ok {
		t.Fatal("Eligible must honor fallback-on-unknown-model: the re-check dropped the fallback candidate Candidates selected")
	}
}

func TestEligibleStrictWhenFallbackDisabled(t *testing.T) {
	r := fallbackTestRouter(t, false)
	req := Requirement{Model: "totally-unknown", Streaming: true}
	if _, ok := r.Eligible("p/m", req); ok {
		t.Fatal("with fallback disabled, an unknown model must stay ineligible")
	}
}

func TestEligibleKnownModelStaysStrictForOtherDeployments(t *testing.T) {
	r := fallbackTestRouter(t, true)
	// Model "m" is known (served by p/m itself); asking about a deployment
	// that does not serve a DIFFERENT known model must stay strict — the
	// fallback relaxation applies only to wholly unknown models.
	if _, ok := r.Eligible("p/m", Requirement{Model: "m", Streaming: true}); !ok {
		t.Fatal("the serving deployment must remain eligible for its own model")
	}
	if _, ok := r.Eligible("p/m", Requirement{Model: "vendor/model-v1", Streaming: true}); !ok {
		t.Fatal("the serving deployment must remain eligible for its upstream model name")
	}
}
