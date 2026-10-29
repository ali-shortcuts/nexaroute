package config

import (
	"math"
	"strings"
	"testing"
)

// Wave-3 validation coverage for validateDecisionCore,
// validateDecisionProvidersAndChains and validateCrossReferences.
// Every case asserts the exact documented rejection; cases that are
// unreachable by construction (a dead branch guarded by an earlier
// check) are deliberately NOT faked here and are listed in the wave-3
// report instead.

func wave3ValidPolicy(id string) DecisionPolicyConfig {
	return DecisionPolicyConfig{ID: id, Weights: DecisionPolicyWeights{RouterBaseline: 1}}
}

func wave3AssertError(t *testing.T, mutate func(*Config), want string) {
	t.Helper()
	cfg := Default()
	cfg.ApplyDefaults()
	mutate(&cfg)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Validate error=%v, want substring %q", err, want)
	}
}

func TestWave3DecisionCoreValidationBranches(t *testing.T) {
	long256 := strings.Repeat("x", 257)
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"provider too long", func(c *Config) { c.Decision.Provider = long256 }, "decision.provider is too long"},
		{"provider invalid id", func(c *Config) { c.Decision.Provider = "bad/id" }, "decision provider id"},
		{"chain too long", func(c *Config) { c.Decision.Chain = long256 }, "decision.chain is too long"},
		{"health window", func(c *Config) { c.DecisionProviderHealth.FailureWindowSeconds = 0 }, "failure_window_seconds"},
		{"health cooldown", func(c *Config) { c.DecisionProviderHealth.CooldownSeconds = 0 }, "cooldown_seconds"},
		{"policy too long", func(c *Config) { c.Decision.Policy = long256 }, "decision.policy is too long"},
		{"policy invalid id", func(c *Config) { c.Decision.Policy = "bad/id" }, "decision policy id"},
		{"policies count", func(c *Config) { c.DecisionPolicies = make([]DecisionPolicyConfig, maxDecisionPolicies+1) }, "decision_policies exceeds"},
		{"policy id required", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{{Weights: DecisionPolicyWeights{RouterBaseline: 1}}}
		}, "decision_policies[0].id is required"},
		{"policy duplicate", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p"), wave3ValidPolicy("p")}
		}, "duplicate decision policy id"},
		{"policy name too long", func(c *Config) {
			p := wave3ValidPolicy("p")
			p.Name = long256
			c.DecisionPolicies = []DecisionPolicyConfig{p}
		}, "name too long"},
		{"policy weights NaN", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{{ID: "p", Weights: DecisionPolicyWeights{RouterBaseline: math.NaN()}}}
		}, "must be finite"},
		{"task overrides count", func(c *Config) {
			over := make(map[string]DecisionPolicyWeights, maxTaskOverrides+1)
			for i := 0; i < maxTaskOverrides+1; i++ {
				over[strings.Repeat("k", i+1)] = DecisionPolicyWeights{RouterBaseline: 1}
			}
			p := wave3ValidPolicy("p")
			p.TaskOverrides = over
			c.DecisionPolicies = []DecisionPolicyConfig{p}
		}, "task_overrides exceeds limit"},
		{"task override empty key", func(c *Config) {
			p := wave3ValidPolicy("p")
			p.TaskOverrides = map[string]DecisionPolicyWeights{"": {RouterBaseline: 1}}
			c.DecisionPolicies = []DecisionPolicyConfig{p}
		}, "empty task override key"},
		{"task override key too long", func(c *Config) {
			p := wave3ValidPolicy("p")
			p.TaskOverrides = map[string]DecisionPolicyWeights{long256: {RouterBaseline: 1}}
			c.DecisionPolicies = []DecisionPolicyConfig{p}
		}, "task override"},
		{"task override weights NaN", func(c *Config) {
			p := wave3ValidPolicy("p")
			p.TaskOverrides = map[string]DecisionPolicyWeights{"coding": {RouterBaseline: math.NaN()}}
			c.DecisionPolicies = []DecisionPolicyConfig{p}
		}, "must be finite"},
		{"task override no positive weight", func(c *Config) {
			p := wave3ValidPolicy("p")
			p.TaskOverrides = map[string]DecisionPolicyWeights{"coding": {}}
			c.DecisionPolicies = []DecisionPolicyConfig{p}
		}, "must have at least one positive weight"},
		{"local policy provider without policy", func(c *Config) {
			c.Decision.Mode = "local"
			c.Decision.Provider = "policy"
			c.Decision.Policy = ""
		}, "decision.policy is required when decision.mode=local"},
		{"autopilot target invalid", func(c *Config) {
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "bad/id"
		}, "autopilot policy id"},
		{"autopilot candidate invalid", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p")}
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "p"
			c.Autopilot.CandidatePolicyID = "bad/id"
		}, "autopilot candidate policy id"},
		{"autopilot canary max negative", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p")}
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "p"
			c.Autopilot.CanaryMaxRequests = -1
		}, "autopilot.canary_max_requests must not be negative"},
		{"autopilot min samples negative", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p")}
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "p"
			c.Autopilot.MinSamples = -1
		}, "autopilot.min_samples"},
		{"autopilot min promote negative", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p")}
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "p"
			c.Autopilot.MinPromoteSamples = -1
		}, "autopilot.min_promote_samples"},
		{"autopilot error rate out of range", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p")}
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "p"
			c.Autopilot.MaxErrorRate = 2
		}, "must be finite and between 0 and 1"},
		{"autopilot latency negative", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p")}
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "p"
			c.Autopilot.MaxLatencyP95MS = -1
		}, "autopilot latency ceilings must not be negative"},
		{"autopilot budget anomalies negative", func(c *Config) {
			c.DecisionPolicies = []DecisionPolicyConfig{wave3ValidPolicy("p")}
			c.Autopilot.Enabled = true
			c.Autopilot.PolicyID = "p"
			c.Autopilot.MaxBudgetAnomalies = -1
		}, "autopilot.max_budget_anomalies must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wave3AssertError(t, tc.edit, tc.want) })
	}
}

func TestWave3DecisionProvidersAndChainsBranches(t *testing.T) {
	disabled := false
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"providers count", func(c *Config) { c.DecisionProviders = make([]DecisionProviderConfig, maxDecisionProviders+1) }, "decision_providers exceeds"},
		{"provider id invalid", func(c *Config) { c.DecisionProviders = []DecisionProviderConfig{{ID: "bad/id", Type: "jev"}} }, "decision provider id"},
		{"provider duplicate", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "ext", Type: "jev"}, {ID: "ext", Type: "jev"}}
		}, "duplicate decision provider id"},
		{"provider type required", func(c *Config) { c.DecisionProviders = []DecisionProviderConfig{{ID: "ext"}} }, "type is required"},
		{"provider api key too long", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "ext", Type: "jev", APIKey: strings.Repeat("k", 4097)}}
		}, "api_key too long"},
		{"provider api key env too long", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "ext", Type: "jev", APIKeyEnv: strings.Repeat("E", 257)}}
		}, "api_key_env too long"},
		{"provider base url too long", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "ext", Type: "jev", BaseURL: strings.Repeat("x", maxURLBytes+1)}}
		}, "base_url too long"},
		{"chain id required", func(c *Config) {
			c.DecisionChains = []DecisionChainConfig{{Steps: []DecisionChainStep{{Provider: "local"}}}}
		}, "decision_chains[0].id is required"},
		{"chain id invalid", func(c *Config) {
			c.DecisionChains = []DecisionChainConfig{{ID: "bad/id", Steps: []DecisionChainStep{{Provider: "local"}}}}
		}, "decision chain id"},
		{"chain step provider required", func(c *Config) { c.DecisionChains = []DecisionChainConfig{{ID: "c1", Steps: []DecisionChainStep{{}}}} }, "provider is required"},
		{"chain step provider invalid", func(c *Config) {
			c.DecisionChains = []DecisionChainConfig{{ID: "c1", Steps: []DecisionChainStep{{Provider: "bad/id"}}}}
		}, "provider"},
		{"assisted unknown provider", func(c *Config) {
			c.Decision.Mode = "assisted"
			c.Decision.Provider = "ghost"
		}, "references unknown decision_providers"},
		{"assisted disabled provider", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "ext", Type: "jev", Enabled: &disabled}}
			c.Decision.Mode = "assisted"
			c.Decision.Provider = "ext"
		}, "is disabled"},
		{"local external provider", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "ext", Type: "jev"}}
			c.Decision.Mode = "local"
			c.Decision.Provider = "ext"
		}, "is external but mode is local"},
		{"hybrid external provider", func(c *Config) {
			c.DecisionProviders = []DecisionProviderConfig{{ID: "ext", Type: "jev"}}
			c.DecisionChains = []DecisionChainConfig{{ID: "c1", Steps: []DecisionChainStep{{Provider: "local"}}}}
			c.Decision.Mode = "hybrid"
			c.Decision.Chain = "c1"
			c.Decision.Provider = "ext"
		}, "is external but mode is hybrid"},
		{"virtual endpoints count", func(c *Config) { c.VirtualEndpoints = make([]VirtualEndpointConfig, maxVirtualEndpoints+1) }, "virtual_endpoints exceeds"},
		{"route profiles count", func(c *Config) { c.RouteProfiles = make([]RouteProfileConfig, maxRouteProfiles+1) }, "route_profiles exceeds"},
		{"candidate pools count", func(c *Config) { c.CandidatePools = make([]CandidatePoolConfig, maxCandidatePools+1) }, "candidate_pools exceeds"},
		{"fallback chains count", func(c *Config) { c.FallbackChains = make([]FallbackChainConfig, maxFallbackChains+1) }, "fallback_chains exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wave3AssertError(t, tc.edit, tc.want) })
	}
}

func TestWave3CrossReferenceBranches(t *testing.T) {
	long256 := strings.Repeat("x", 257)
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"pool duplicate", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}, {ID: "pool", Mode: "all"}}
		}, "duplicate candidate pool id"},
		{"pool name too long", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Name: long256, Mode: "all"}}
		}, "name too long"},
		{"pool deployments limit", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: make([]string, maxPoolDeployments+1)}}
		}, "deployments exceeds limit"},
		{"pool deployment too long", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{long256}}}
		}, "deployment"},
		{"fallback chain id invalid", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.FallbackChains = []FallbackChainConfig{{ID: "bad/id", Pools: []string{"pool"}}}
		}, "fallback chain id"},
		{"fallback chain duplicate", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.FallbackChains = []FallbackChainConfig{{ID: "chain", Pools: []string{"pool"}}, {ID: "chain", Pools: []string{"pool"}}}
		}, "duplicate fallback chain id"},
		{"fallback chain name too long", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.FallbackChains = []FallbackChainConfig{{ID: "chain", Name: long256, Pools: []string{"pool"}}}
		}, "name too long"},
		{"fallback chain pools limit", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			pools := make([]string, maxFallbackPools+1)
			for i := range pools {
				pools[i] = "pool"
			}
			c.FallbackChains = []FallbackChainConfig{{ID: "chain", Pools: pools}}
		}, "exceeds pool limit"},
		{"profile id invalid", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "bad/id", CandidatePool: "pool"}}
		}, "route profile id"},
		{"profile duplicate", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}, {ID: "profile", CandidatePool: "pool"}}
		}, "duplicate route profile id"},
		{"profile name too long", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", Name: long256, CandidatePool: "pool"}}
		}, "name too long"},
		{"profile unknown decision policy", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool", DecisionPolicy: "ghost"}}
		}, "references unknown decision policy"},
		{"endpoint id invalid", func(c *Config) {
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "bad/id", PublicModel: "pub", RouteProfile: "profile"}}
		}, "virtual endpoint id"},
		{"endpoint duplicate", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: "a", RouteProfile: "profile"}, {ID: "ep", PublicModel: "b", RouteProfile: "profile"}}
		}, "duplicate virtual endpoint id"},
		{"endpoint name too long", func(c *Config) {
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", Name: long256, PublicModel: "pub", RouteProfile: "profile"}}
		}, "name too long"},
		{"endpoint public model too long", func(c *Config) {
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: strings.Repeat("m", maxPublicModelBytes+1), RouteProfile: "profile"}}
		}, "public_model too long"},
		{"endpoint route profile required", func(c *Config) {
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: "pub"}}
		}, "route_profile is required"},
		{"endpoint protocols limit", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
			protos := make([]string, maxProtocols+1)
			for i := range protos {
				protos[i] = "http"
			}
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: "pub", RouteProfile: "profile", Protocols: protos}}
		}, "protocols exceeds limit"},
		{"endpoint empty protocol", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: "pub", RouteProfile: "profile", Protocols: []string{""}}}
		}, "has empty protocol"},
		{"endpoint protocol too long", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: "pub", RouteProfile: "profile", Protocols: []string{strings.Repeat("p", 65)}}}
		}, "protocol"},
		{"endpoint protocol invalid chars", func(c *Config) {
			c.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
			c.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
			c.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: "pub", RouteProfile: "profile", Protocols: []string{"bad proto!"}}}
		}, "protocol"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wave3AssertError(t, tc.edit, tc.want) })
	}
}

// A disabled provider model must not reserve its identifier: the
// physical-ID collection skips disabled models, so a virtual endpoint
// may legitimately reuse that public model name. This pins the
// disabled-model skip branch with a behavioral assertion, not just
// coverage of the loop.
func TestWave3DisabledModelDoesNotReservePublicModel(t *testing.T) {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "provider", Name: "Provider", Type: "openai_compatible",
		BaseURL: "https://example.test", Enabled: true,
		Models: []ModelConfig{{ID: "retired-model", Model: "retired-model", Enabled: false, Weight: 1}},
	}}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
	cfg.VirtualEndpoints = []VirtualEndpointConfig{{ID: "ep", PublicModel: "retired-model", RouteProfile: "profile"}}
	// ApplyDefaults after the provider is set so its concurrency and
	// timeout defaults are filled like any operator-configured one.
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled model must not collide with a virtual endpoint public model: %v", err)
	}
}

// A canary deployment check must skip pools that are not the
// profile's candidate pool: the loop's continue branch. The config
// is otherwise fully valid, so reaching Validate()==nil proves the
// branch executed on the way through.
func TestWave3CanaryCheckSkipsUnrelatedPools(t *testing.T) {
	cfg := Default()
	cfg.ApplyDefaults()
	cfg.CandidatePools = []CandidatePoolConfig{
		{ID: "pool", Mode: "explicit", Deployments: []string{"dep1"}},
		{ID: "other", Mode: "all"},
	}
	cfg.RouteProfiles = []RouteProfileConfig{{
		ID: "profile", CandidatePool: "pool", CanaryPercent: 50, CanaryDeploymentID: "dep1",
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("canary check with an unrelated pool must validate: %v", err)
	}
}
