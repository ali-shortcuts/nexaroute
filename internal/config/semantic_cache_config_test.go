package config

import "testing"

// M5-4 semantic cache validation: opt-in requires a resolvable
// openai_compatible embedding deployment and sane bounds.
func TestSemanticCacheValidation(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.Providers = []ProviderConfig{{
			ID: "emb", Name: "E", Type: "openai_compatible", BaseURL: "https://e.example.com", Enabled: true,
			Models: []ModelConfig{{ID: "e1", Model: "emb-model", Enabled: true}},
		}}
		cfg.ApplyDefaults()
		return cfg
	}
	cfg := base()
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "emb/e1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid semantic config rejected: %v", err)
	}
	if cfg.Cache.Semantic.Threshold != 0.95 || cfg.Cache.Semantic.MaxEntries != 1024 || cfg.Cache.Semantic.EmbeddingPath != "/v1/embeddings" {
		t.Fatalf("defaults not applied: %+v", cfg.Cache.Semantic)
	}

	cfg = base()
	cfg.Cache.Semantic.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("enabled without embedding deployment accepted")
	}
	cfg.Cache.Semantic.EmbeddingDeployment = "emb/nope"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown embedding model accepted")
	}
	cfg.Cache.Semantic.EmbeddingDeployment = "emb/e1"
	cfg.Cache.Semantic.Threshold = 0.4
	if err := cfg.Validate(); err == nil {
		t.Fatal("threshold 0.4 accepted")
	}
	cfg.Cache.Semantic.Threshold = 0.95
	cfg.Cache.Semantic.MaxEntries = 99999
	if err := cfg.Validate(); err == nil {
		t.Fatal("max_entries overflow accepted")
	}
}
