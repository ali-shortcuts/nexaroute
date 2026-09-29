package router

// Part A6: concurrency evidence (run with -race).
// With 1, 2, 4, 8 and 16 healthy deployments under concurrent load, every
// deployment receives primary traffic (no starvation), primaries are evenly
// distributed (no bias), and concurrent Candidates/RecordSuccess access is
// race-free.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestFailoverEvidence_Concurrency_DistributionNoRacesNoStarvation(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8, 16} {
		t.Run(fmt.Sprintf("deployments_%d", n), func(t *testing.T) {
			cfg := config.Default()
			cfg.Routing.Strategy = "round_robin"
			models := make([]config.ModelConfig, 0, n)
			ids := make([]string, 0, n)
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("m%02d", i)
				ids = append(ids, "p/"+id)
				models = append(models, config.ModelConfig{ID: id, Model: id, Aliases: []string{"coding"}, Enabled: true, Weight: 1})
			}
			cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: models}}
			h := health.New(100, time.Hour)
			for _, id := range ids {
				h.RecordSuccess(id, time.Millisecond)
			}
			r := New(cfg, h)

			const goroutines = 8
			const perG = 250
			total := goroutines * perG
			var mu sync.Mutex
			counts := map[string]int{}
			var wg sync.WaitGroup
			// Success heartbeats concurrent with reads exercise the shared
			// health/router locks the same way production traffic does.
			stop := make(chan struct{})
			var hb sync.WaitGroup
			hb.Add(1)
			go func() {
				defer hb.Done()
				i := 0
				for {
					select {
					case <-stop:
						return
					default:
						h.RecordSuccess(ids[i%len(ids)], time.Millisecond)
						i++
					}
				}
			}()
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < perG; i++ {
						got := r.Candidates(Requirement{Model: "coding"})
						if len(got) != n {
							t.Errorf("want %d candidates, got %d", n, len(got))
							return
						}
						mu.Lock()
						counts[got[0].Deployment.ID]++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			close(stop)
			hb.Wait()

			for _, id := range ids {
				if counts[id] == 0 {
					t.Fatalf("starvation: %s never selected as primary: %v", id, counts)
				}
			}
			want := total / n
			for id, c := range counts {
				if c != want {
					t.Fatalf("unbalanced distribution: %s got %d primaries, want %d each (total %d): %v", id, c, want, total, counts)
				}
			}
			t.Logf("n=%d total=%d per-deployment primaries=%d", n, total, want)
		})
	}
}
