package router

import (
	"fmt"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// M6-4: candidate scoring is on every data-plane request. Pool
// sizes 10 and 100 deployments bracket realistic self-hosted
// installs. Issue #437 adds 1000 to prove routing overhead stays
// sub-linear-per-request at the large end of the candidate-pool
// profile set.

func benchRouter(b *testing.B, deployments int) *Router {
	b.Helper()
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	providers := make([]config.ProviderConfig, 0, deployments)
	for i := 0; i < deployments; i++ {
		providers = append(providers, config.ProviderConfig{
			ID: fmt.Sprintf("p%d", i), Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
		})
	}
	cfg.Providers = providers
	h := health.New(1, time.Hour)
	for i := 0; i < deployments; i++ {
		h.RecordSuccess(fmt.Sprintf("p%d/m", i), 25*time.Millisecond)
	}
	return New(cfg, h)
}

func BenchmarkCandidates10(b *testing.B) {
	r := benchRouter(b, 10)
	req := Requirement{Model: "m", Tools: true, Streaming: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := r.Candidates(req); len(got) != 10 {
			b.Fatalf("candidates = %d", len(got))
		}
	}
}

func BenchmarkCandidates100(b *testing.B) {
	r := benchRouter(b, 100)
	req := Requirement{Model: "m", Tools: true, Streaming: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := r.Candidates(req); len(got) != 100 {
			b.Fatalf("candidates = %d", len(got))
		}
	}
}

// BenchmarkCandidates1000 is the large candidate-pool scale point for
// issue #437. It exercises the same selection hot path as the 10/100
// points and reports allocations so a bounded per-candidate cost can be
// compared across pool sizes.
func BenchmarkCandidates1000(b *testing.B) {
	r := benchRouter(b, 1000)
	req := Requirement{Model: "m", Tools: true, Streaming: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := r.Candidates(req); len(got) != 1000 {
			b.Fatalf("candidates = %d", len(got))
		}
	}
}
