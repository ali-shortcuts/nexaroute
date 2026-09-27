package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func baseValidChainConfig() Config {
	cfg := Default()
	cfg.Providers = []ProviderConfig{
		{
			ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: "https://example.com/v1",
			Enabled: true,
			Models:  []ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1}},
		},
	}
	cfg.DecisionProviders = []DecisionProviderConfig{
		{ID: "jev-main", Type: "jev", BaseURL: "https://jev.example.com", PrivacyMode: "metadata_only"},
		{ID: "jev-backup", Type: "jev", BaseURL: "https://jev2.example.com", PrivacyMode: "metadata_only"},
	}
	cfg.DecisionChains = []DecisionChainConfig{
		{ID: "external-policy", Steps: []DecisionChainStep{{Provider: "jev-main"}, {Provider: "policy"}}},
	}
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "external-policy"
	cfg.ApplyDefaults()
	return cfg
}

func TestChainConfig_HybridRequiresChain(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.Decision.Chain = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "decision.chain is required") {
		t.Fatalf("expected hybrid requires chain error, got %v", err)
	}
}

func TestChainConfig_ChainReferenceMustExist(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.Decision.Chain = "missing-chain"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "references unknown") {
		t.Fatalf("expected unknown chain error, got %v", err)
	}
}

func TestChainConfig_Max64Chains(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains = nil
	for i := 0; i < 65; i++ {
		id := "chain-" + string(rune('a'+(i%26))) + string(rune('b'+(i/26)%26)) + string(rune('0'+(i%10)))
		if i >= 60 {
			id = "chain-extra-" + string(rune('a'+(i%26))) + string(rune('0'+i%10))
		}
		// ensure validLocalID (no hyphen issues? hyphen allowed)
		cfg.DecisionChains = append(cfg.DecisionChains, DecisionChainConfig{ID: id, Steps: []DecisionChainStep{{Provider: "local"}}})
	}
	cfg.Decision.Chain = cfg.DecisionChains[0].ID
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("expected max chains error, got %v", err)
	}
}

func TestChainConfig_StepsBounds(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains[0].Steps = nil
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "at least one step") {
		t.Fatalf("expected empty steps error, got %v", err)
	}
	cfg = baseValidChainConfig()
	// create 9 distinct providers
	cfg.DecisionProviders = nil
	for i := 0; i < 9; i++ {
		cfg.DecisionProviders = append(cfg.DecisionProviders, DecisionProviderConfig{ID: "jev-" + string(rune('a'+i)), Type: "jev"})
	}
	steps := make([]DecisionChainStep, 9)
	for i := 0; i < 9; i++ {
		steps[i] = DecisionChainStep{Provider: "jev-" + string(rune('a'+i))}
	}
	cfg.DecisionChains[0].Steps = steps
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected too many steps error, got %v", err)
	}
}

func TestChainConfig_DuplicateChainID(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains = []DecisionChainConfig{
		{ID: "dup", Steps: []DecisionChainStep{{Provider: "local"}}},
		{ID: "dup", Steps: []DecisionChainStep{{Provider: "policy"}}},
	}
	cfg.Decision.Chain = "dup"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate decision chain") {
		t.Fatalf("expected duplicate chain error, got %v", err)
	}
}

func TestChainConfig_DuplicateProviderStep(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains[0].Steps = []DecisionChainStep{{Provider: "jev-main"}, {Provider: "jev-main"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate provider") {
		t.Fatalf("expected duplicate provider step error, got %v", err)
	}
}

func TestChainConfig_UnknownProvider(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains[0].Steps = []DecisionChainStep{{Provider: "unknown-provider"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}

func TestChainConfig_CollisionWithProviderID(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains[0].ID = "jev-main"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "collides with decision provider") {
		t.Fatalf("expected collision error, got %v", err)
	}
	cfg = baseValidChainConfig()
	cfg.DecisionChains[0].ID = "local"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "collides with built-in") {
		t.Fatalf("expected built-in collision, got %v", err)
	}
}

func TestChainConfig_StepTimeoutBounds(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains[0].Steps[0].TimeoutMS = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Fatalf("expected timeout bounds error for -1, got %v", err)
	}
	cfg = baseValidChainConfig()
	cfg.DecisionChains[0].Steps[0].TimeoutMS = 6000
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Fatalf("expected timeout bounds error for 6000, got %v", err)
	}
	cfg = baseValidChainConfig()
	cfg.DecisionChains[0].Steps[0].TimeoutMS = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("0 should be allowed, got %v", err)
	}
	cfg.DecisionChains[0].Steps[0].TimeoutMS = 5000
	if err := cfg.Validate(); err != nil {
		t.Fatalf("5000 should be allowed, got %v", err)
	}
}

func TestChainConfig_MaxProviderCallsBounds(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.Decision.MaxProviderCalls = 9
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_provider_calls") {
		t.Fatalf("expected bounds error, got %v", err)
	}
	cfg.Decision.MaxProviderCalls = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_provider_calls") {
		t.Fatalf("expected bounds error for -1, got %v", err)
	}
	cfg.Decision.MaxProviderCalls = 8
	if err := cfg.Validate(); err != nil {
		t.Fatalf("8 should be allowed, got %v", err)
	}
}

func TestChainConfig_LocalPolicyStepsAccepted(t *testing.T) {
	cfg := baseValidChainConfig()
	cfg.DecisionChains[0].Steps = []DecisionChainStep{{Provider: "local"}, {Provider: "policy"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("local/policy should be accepted, got %v", err)
	}
}

func TestChainConfig_DisabledProviderMayBeReferenced(t *testing.T) {
	cfg := baseValidChainConfig()
	disabled := false
	cfg.DecisionProviders[0].Enabled = &disabled
	cfg.DecisionChains[0].Steps = []DecisionChainStep{{Provider: "jev-main"}, {Provider: "policy"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled provider reference should be allowed, got %v", err)
	}
}

func TestChainConfig_AssistedUnchanged(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true, Models: []ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}}}}
	cfg.DecisionProviders = []DecisionProviderConfig{{ID: "jev-main", Type: "jev", BaseURL: "https://jev.example.com"}}
	cfg.Decision.Mode = "assisted"
	cfg.Decision.Provider = "jev-main"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("assisted should still validate, got %v", err)
	}
	cfg.Decision.Chain = "some"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "only valid when") {
		t.Fatalf("chain should be rejected in assisted, got %v", err)
	}
}

func TestChainConfig_LocalUnchanged(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true, Models: []ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}}}}
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.DecisionPolicies = []DecisionPolicyConfig{{ID: "balanced", Weights: DecisionPolicyWeights{RouterBaseline: 1}}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("local should validate, got %v", err)
	}
}

func TestChainConfig_OffUnchanged(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true, Models: []ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}}}}
	cfg.Decision.Mode = "off"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("off should validate, got %v", err)
	}
}

func TestDefault_ReturnsValidHealth(t *testing.T) {
	cfg := Default()
	if cfg.DecisionProviderHealth.FailureThreshold != 3 || cfg.DecisionProviderHealth.FailureWindowSeconds != 30 || cfg.DecisionProviderHealth.CooldownSeconds != 60 {
		t.Fatalf("Default health mismatch: %+v", cfg.DecisionProviderHealth)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default should validate, got %v", err)
	}
}

func TestApplyDefaults_ProducesSameHealth(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	if cfg.DecisionProviderHealth.FailureThreshold != 3 || cfg.DecisionProviderHealth.FailureWindowSeconds != 30 || cfg.DecisionProviderHealth.CooldownSeconds != 60 {
		t.Fatalf("ApplyDefaults health mismatch: %+v", cfg.DecisionProviderHealth)
	}
}

func TestHybridMaxProviderCallsDefault(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true, Models: []ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}}}}
	cfg.DecisionProviders = []DecisionProviderConfig{{ID: "jev-main", Type: "jev"}, {ID: "jev-backup", Type: "jev"}}
	cfg.DecisionChains = []DecisionChainConfig{{ID: "chain1", Steps: []DecisionChainStep{{Provider: "jev-main"}, {Provider: "policy"}, {Provider: "local"}}}}
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "chain1"
	cfg.Decision.MaxProviderCalls = 0
	cfg.ApplyDefaults()
	if cfg.Decision.MaxProviderCalls != 3 {
		t.Fatalf("expected default 3, got %d", cfg.Decision.MaxProviderCalls)
	}
	// test 8
	cfg = Default()
	cfg.Providers = []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true, Models: []ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}}}}
	cfg.DecisionProviders = nil
	for i := 0; i < 8; i++ {
		cfg.DecisionProviders = append(cfg.DecisionProviders, DecisionProviderConfig{ID: "jev" + string(rune('a'+i)), Type: "jev"})
	}
	steps := []DecisionChainStep{}
	for i := 0; i < 8; i++ {
		steps = append(steps, DecisionChainStep{Provider: "jev" + string(rune('a'+i))})
	}
	cfg.DecisionChains = []DecisionChainConfig{{ID: "chain8", Steps: steps}}
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "chain8"
	cfg.Decision.MaxProviderCalls = 0
	cfg.ApplyDefaults()
	if cfg.Decision.MaxProviderCalls != 8 {
		t.Fatalf("expected 8, got %d", cfg.Decision.MaxProviderCalls)
	}
}

func TestDecisionChainStep_UnmarshalStringAndObject(t *testing.T) {
	var step DecisionChainStep
	if err := json.Unmarshal([]byte(`"jev-main"`), &step); err != nil || step.Provider != "jev-main" || step.TimeoutMS != 0 {
		t.Fatalf("string unmarshal failed: %v %+v", err, step)
	}
	if err := json.Unmarshal([]byte(`{"provider":"policy","timeout_ms":123}`), &step); err != nil || step.Provider != "policy" || step.TimeoutMS != 123 {
		t.Fatalf("object unmarshal failed: %v %+v", err, step)
	}
	// also test marshal
	b, err := json.Marshal(DecisionChainStep{Provider: "jev-main"})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s != "jev-main" {
		t.Fatalf("marshal as string failed: %s err %v", string(b), err)
	}
	b, err = json.Marshal(DecisionChainStep{Provider: "policy", TimeoutMS: 50})
	if err != nil {
		t.Fatalf("marshal failed")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil || m["provider"] != "policy" {
		t.Fatalf("marshal object failed: %s", string(b))
	}
}
