package providers

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestRegistryPrepareReusesUnchangedAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		Enabled: true,
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := reg.Get("p")
	if !ok {
		t.Fatal("missing initial adapter")
	}
	next, err := reg.Prepare(cfg, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	after, ok := next.Get("p")
	if !ok || before != after {
		t.Fatal("unchanged provider adapter was rebuilt")
	}
}

func TestRegistryPrepareRebuildsChangedAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		Enabled: true,
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := reg.Get("p")
	next, err := reg.Prepare(cfg, map[string]struct{}{"p": {}})
	if err != nil {
		t.Fatal(err)
	}
	after, ok := next.Get("p")
	if !ok || before == after {
		t.Fatal("changed provider adapter was incorrectly reused")
	}
}
