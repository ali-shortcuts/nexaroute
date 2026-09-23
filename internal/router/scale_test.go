package router

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestTwentyProvidersHundredModels(t *testing.T) {
	cfg := config.Default()
	for p := 0; p < 20; p++ {
		pc := config.ProviderConfig{ID: fmt.Sprintf("p%02d", p), Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true}
		for m := 0; m < 5; m++ {
			pc.Models = append(pc.Models, config.ModelConfig{ID: fmt.Sprintf("m%d", m), Model: fmt.Sprintf("model-%02d-%d", p, m), Aliases: []string{"auto"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}})
		}
		cfg.Providers = append(cfg.Providers, pc)
	}
	h := health.New(4, time.Hour)
	r := New(cfg, h)
	for _, d := range r.All() {
		h.RecordSuccess(d.ID, time.Millisecond)
	}
	c := r.Candidates(Requirement{Model: "auto", Streaming: true, Tools: true})
	if len(c) != 100 {
		t.Fatalf("want 100 candidates got %d", len(c))
	}
}

func TestConcurrentSessionFloodRemainsBounded(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	h := health.New(5, time.Hour)
	h.RecordSuccess("p/m", time.Millisecond)
	r := New(cfg, h)

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				key := fmt.Sprintf("g-%d-session-%d", g, i)
				r.ObserveSession(Requirement{Model: "auto", SessionKey: key}, "p/m")
				if got := r.Candidates(Requirement{Model: "auto", SessionKey: key}); len(got) == 0 {
					t.Errorf("no candidate for %s", key)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if got := r.SessionCount(); got > maxSessionPins {
		t.Fatalf("session state grew to %d, limit=%d", got, maxSessionPins)
	}
}
