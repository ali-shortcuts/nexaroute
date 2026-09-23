package router

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func requireStress(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
}

func TestStressTenThousandDeploymentsConcurrentIndexedRouting(t *testing.T) {
	requireStress(t)
	cfg := config.Default()
	cfg.Routing.SessionAffinity = true
	for p := 0; p < 100; p++ {
		pc := config.ProviderConfig{
			ID: fmt.Sprintf("p%03d", p), Name: "P", Type: "openai_compatible",
			BaseURL: "http://example.invalid", Enabled: true,
		}
		for m := 0; m < 100; m++ {
			pc.Models = append(pc.Models, config.ModelConfig{
				ID: fmt.Sprintf("m%03d", m), Model: fmt.Sprintf("model-%03d-%03d", p, m),
				Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true},
			})
		}
		cfg.Providers = append(cfg.Providers, pc)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	r := New(cfg, hm)
	all := r.All()
	if len(all) != 10000 {
		t.Fatalf("deployments=%d want 10000", len(all))
	}
	for _, d := range all {
		hm.RecordSuccess(d.ID, time.Millisecond)
	}

	const goroutines = 64
	const operations = 500
	var failures atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < operations; i++ {
				p := (g*operations + i) % 100
				m := (g + i) % 100
				model := fmt.Sprintf("model-%03d-%03d", p, m)
				session := fmt.Sprintf("stress-%d-%d", g, i)
				req := Requirement{Model: model, Streaming: true, Tools: true, SessionKey: session}
				candidates := r.Candidates(req)
				if len(candidates) != 1 || candidates[0].Deployment.Model != model {
					failures.Add(1)
					continue
				}
				r.ObserveSession(req, candidates[0].Deployment.ID)
				if pinned := r.Candidates(req); len(pinned) != 1 || pinned[0].Deployment.ID != candidates[0].Deployment.ID {
					failures.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("routing failures=%d", failures.Load())
	}
	if got := r.SessionCount(); got > maxSessionPins {
		t.Fatalf("session state=%d exceeds limit=%d", got, maxSessionPins)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("indexed routing stress took %s; likely hot-path regression", d)
	}
}

func TestStressFullPoolAutoRoutingRemainsStable(t *testing.T) {
	requireStress(t)
	cfg := config.Default()
	for p := 0; p < 40; p++ {
		pc := config.ProviderConfig{ID: fmt.Sprintf("p%02d", p), Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true}
		for m := 0; m < 50; m++ {
			pc.Models = append(pc.Models, config.ModelConfig{ID: fmt.Sprintf("m%02d", m), Model: fmt.Sprintf("model-%02d-%02d", p, m), Aliases: []string{"coding"}, Enabled: true, Weight: 1})
		}
		cfg.Providers = append(cfg.Providers, pc)
	}
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	r := New(cfg, hm)
	for _, d := range r.All() {
		hm.RecordSuccess(d.ID, time.Millisecond)
	}
	for i := 0; i < 100; i++ {
		c := r.Candidates(Requirement{Model: "coding", SelectionKey: fmt.Sprintf("req-%d", i)})
		if len(c) != 2000 {
			t.Fatalf("iteration=%d candidates=%d want 2000", i, len(c))
		}
	}
}
