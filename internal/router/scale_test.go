package router

import (
	"fmt"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"testing"
	"time"
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
