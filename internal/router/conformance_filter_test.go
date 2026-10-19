package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// #436: eligibility must consult the conformance manifest, not
// only declared model capabilities. Bedrock's manifest entry
// marks structured output, parallel tools and reasoning
// UNSUPPORTED (dropped on encode); Gemini marks parallel tools
// UNSUPPORTED. A deployment on such a dialect is ineligible for
// a request that requires the dimension — even when its model
// config declares the capability — and Explain must name the
// conformance reason.
func conformanceFilterRouter(t *testing.T) *Router {
	t.Helper()
	cfg := config.Default()
	mk := func(id string) config.ModelConfig {
		return config.ModelConfig{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "br", Name: "BR", Type: "bedrock", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{mk("br")}},
		{ID: "gm", Name: "GM", Type: "gemini", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{mk("gm")}},
		{ID: "oa", Name: "OA", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{mk("oa")}},
	}
	h := health.New(4, time.Hour)
	for _, id := range []string{"br/m", "gm/m", "oa/m"} {
		h.RecordSuccess(id, time.Millisecond)
	}
	return New(cfg, h)
}

func candidateIDs(c []Scored) map[string]bool {
	out := map[string]bool{}
	for _, s := range c {
		out[s.Deployment.ID] = true
	}
	return out
}

func TestConformanceManifestFiltersCandidates(t *testing.T) {
	r := conformanceFilterRouter(t)

	// Baseline: no dialect-sensitive demand — everyone eligible.
	if got := candidateIDs(r.Candidates(Requirement{Model: "m", Streaming: true})); len(got) != 3 {
		t.Fatalf("baseline candidates = %v, want all 3", got)
	}
	// Structured output: bedrock is manifest-UNSUPPORTED.
	if got := candidateIDs(r.Candidates(Requirement{Model: "m", Streaming: true, StructuredOutput: true})); got["br/m"] || !got["oa/m"] || !got["gm/m"] {
		t.Fatalf("structured-output candidates = %v, want bedrock excluded only", got)
	}
	// Parallel tools: bedrock AND gemini are manifest-UNSUPPORTED.
	if got := candidateIDs(r.Candidates(Requirement{Model: "m", Streaming: true, ParallelTools: true})); got["br/m"] || got["gm/m"] || !got["oa/m"] {
		t.Fatalf("parallel-tools candidates = %v, want only openai_compatible", got)
	}
	// Reasoning: bedrock is manifest-UNSUPPORTED even though the
	// model declares the reasoning capability in config.
	if got := candidateIDs(r.Candidates(Requirement{Model: "m", Streaming: true, Reasoning: true})); got["br/m"] || !got["oa/m"] {
		t.Fatalf("reasoning candidates = %v, want bedrock excluded by the manifest", got)
	}

	// Explainability: the exclusion reason names the manifest.
	exp := r.Explain(Requirement{Model: "m", Streaming: true, StructuredOutput: true})
	found := false
	for _, d := range exp.Decisions {
		if d.DeploymentID == "br/m" {
			found = true
			if d.ExcludedReason != ExcludeConformanceStructuredOutput {
				t.Fatalf("bedrock exclusion reason = %q, want %q", d.ExcludedReason, ExcludeConformanceStructuredOutput)
			}
		}
	}
	if !found {
		t.Fatalf("explain decisions missing br/m: %+v", exp.Decisions)
	}
}
