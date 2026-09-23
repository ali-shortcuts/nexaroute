package router

import (
	"fmt"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func benchConfig(providers, modelsPer int) config.Config {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Providers = cfg.Providers[:0]
	for p := 0; p < providers; p++ {
		models := make([]config.ModelConfig, 0, modelsPer)
		for m := 0; m < modelsPer; m++ {
			models = append(models, config.ModelConfig{
				ID: fmt.Sprintf("m%d-%d", p, m), Model: fmt.Sprintf("up-model-%d-%d", p, m),
				Aliases: []string{fmt.Sprintf("alias-%d-%d", p, m)}, Enabled: true, Priority: m % 3, Weight: 1,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: m%2 == 0, Reasoning: m%4 == 0},
			})
		}
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: fmt.Sprintf("prov%d", p), Name: fmt.Sprintf("Provider %d", p), Type: "openai_compatible",
			BaseURL: "http://127.0.0.1:9/v1", AuthMode: "none", Enabled: true, Models: models,
		})
	}
	return cfg
}

func benchRouter(b *testing.B, providers, modelsPer int) (*Router, *health.Manager) {
	b.Helper()
	cfg := benchConfig(providers, modelsPer)
	hm := health.New(5, time.Hour)
	rt := New(cfg, hm)
	for _, d := range rt.All() {
		hm.RecordSuccess(d.ID, 25_000_000) // 25 ms proof
	}
	return rt, hm
}

func benchmarkCandidates(b *testing.B, providers, modelsPer int) {
	b.Helper()
	rt, _ := benchRouter(b, providers, modelsPer)
	req := Requirement{Model: fmt.Sprintf("up-model-%d-%d", providers/2, modelsPer/2), Tools: true, Streaming: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if c := rt.Candidates(req); len(c) == 0 {
			b.Fatal("no candidates")
		}
	}
}

func BenchmarkCandidates_10Providers(b *testing.B) { benchmarkCandidates(b, 10, 1) }
func BenchmarkCandidates_100Models(b *testing.B)   { benchmarkCandidates(b, 10, 10) }
func BenchmarkCandidates_1000Models(b *testing.B)  { benchmarkCandidates(b, 30, 34) }
func BenchmarkCandidates_3000Models(b *testing.B)  { benchmarkCandidates(b, 100, 30) }

func BenchmarkCandidates_AliasVirtualScan(b *testing.B) {
	rt, _ := benchRouter(b, 30, 34)
	req := Requirement{Model: "auto", Tools: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if c := rt.Candidates(req); len(c) == 0 {
			b.Fatal("no candidates")
		}
	}
}

func BenchmarkEligibleSingle(b *testing.B) {
	rt, _ := benchRouter(b, 30, 34)
	req := Requirement{Model: "up-model-15-17", Tools: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := rt.Eligible("prov15/m15-17", req); !ok {
			b.Fatal("not eligible")
		}
	}
}
