package config

import "testing"

// M5-6 price book + budget validation.
func TestPricingAndBudgetValidation(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.Providers = []ProviderConfig{{
			ID: "llm", Name: "L", Type: "openai_compatible", BaseURL: "https://x.example.com", Enabled: true,
			Models: []ModelConfig{{ID: "m", Model: "chat", Enabled: true}},
		}}
		cfg.ApplyDefaults()
		return cfg
	}
	cfg := base()
	cfg.Pricing.Versions = []PriceVersionConfig{{
		Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z",
		Prices: map[string]PriceEntryConfig{"llm/m": {InputPerMTok: 1, OutputPerMTok: 2}},
	}}
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "vk", KeyHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BudgetUSD: 25}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid pricing config rejected: %v", err)
	}
	cfg.Pricing.Versions[0].EffectiveFrom = "not-a-date"
	if err := cfg.Validate(); err == nil {
		t.Fatal("bad effective_from accepted")
	}
	cfg.Pricing.Versions[0].EffectiveFrom = "2026-01-01T00:00:00Z"
	cfg.Pricing.Versions[0].Prices = map[string]PriceEntryConfig{"llm/nope": {InputPerMTok: 1}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown deployment in price book accepted")
	}
	cfg.Pricing.Versions[0].Prices = map[string]PriceEntryConfig{"llm/m": {InputPerMTok: -1}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative price accepted")
	}
	cfg.Pricing.Versions[0].Prices = map[string]PriceEntryConfig{"llm/m": {InputPerMTok: 1}}
	cfg.ClientAuth.VirtualKeys[0].BudgetUSD = -5
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative budget accepted")
	}
}
