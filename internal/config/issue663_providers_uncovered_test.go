package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Credential resolution: an environment value wins over the literal, and
// the resolved egress lists come from ApplyDefaults.
func TestIssue663ProvidersResolvedAndEgress(t *testing.T) {
	t.Setenv("ISSUE663_CFG_KEY", "env-key-value")
	c := CredentialConfig{APIKey: "literal-key", APIKeyEnv: "ISSUE663_CFG_KEY"}
	if got := c.Resolved(); got != "env-key-value" {
		t.Fatalf("env credential = %q, want env-key-value", got)
	}
	c2 := CredentialConfig{APIKey: "literal-key", APIKeyEnv: "ISSUE663_CFG_UNSET"}
	if got := c2.Resolved(); got != "literal-key" {
		t.Fatalf("literal credential = %q, want literal-key", got)
	}

	cfg := Default()
	cfg.Egress.AllowHosts = []string{"api.example.com"}
	cfg.Egress.AllowCIDRs = []string{"10.0.0.0/8"}
	cfg.Providers = []ProviderConfig{{ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", Enabled: true}}
	cfg.ApplyDefaults()
	hosts := cfg.Providers[0].EgressAllowHosts()
	if len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Fatalf("resolved allow hosts = %v, want [api.example.com]", hosts)
	}
	cidrs := cfg.Providers[0].EgressAllowCIDRs()
	if len(cidrs) != 1 || cidrs[0] != "10.0.0.0/8" {
		t.Fatalf("resolved allow CIDRs = %v, want [10.0.0.0/8]", cidrs)
	}
}

// ResolvedCredentialEntries: primary first, disabled pool entries skipped,
// duplicates collapsed, unnamed keys named key-<i+1>, non-positive weight
// defaulted to 1.
func TestIssue663ProvidersResolvedCredentialEntries(t *testing.T) {
	p := ProviderConfig{
		APIKey: "primary-key",
		Credentials: []CredentialConfig{
			{APIKey: "disabled-key", Enabled: false},
			{APIKey: "primary-key", Enabled: true, Name: "dup"},
			{APIKey: "pool-key", Enabled: true, Weight: 0},
		},
	}
	got := p.ResolvedCredentialEntries()
	if len(got) != 2 {
		t.Fatalf("resolved entries = %d, want 2: %+v", len(got), got)
	}
	if got[0].Key != "primary-key" || got[0].Name != "primary" {
		t.Fatalf("primary entry = %+v, want primary-key/primary", got[0])
	}
	if got[1].Key != "pool-key" || got[1].Name != "key-3" || got[1].Weight != 1 {
		t.Fatalf("pool entry = %+v, want pool-key/key-3/weight 1", got[1])
	}
}

// ApplyDefaults clamps and synthesizes defaults.
func TestIssue663ProvidersApplyDefaultsBranches(t *testing.T) {
	cfg := Default()
	cfg.Evaluation.TelemetryMinSamples = 2000000
	// The max-provider-calls defaults live in the hybrid branch.
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.MaxProviderCalls = 0
	cfg.ApplyDefaults()
	if cfg.Evaluation.TelemetryMinSamples != maxTelemetryMinSamples {
		t.Fatalf("telemetry min samples = %d, want clamp %d", cfg.Evaluation.TelemetryMinSamples, maxTelemetryMinSamples)
	}
	if cfg.Decision.MaxProviderCalls != 1 {
		t.Fatalf("zero max provider calls = %d, want 1", cfg.Decision.MaxProviderCalls)
	}

	// Hybrid with zero calls defaults to the chain length, clamped to the
	// chain-step cap when the chain is longer than the cap.
	cfg2 := Default()
	cfg2.Decision.Mode = "hybrid"
	cfg2.Decision.Chain = "long"
	cfg2.Decision.MaxProviderCalls = 0
	steps := make([]DecisionChainStep, 0, 12)
	for i := 0; i < 12; i++ {
		steps = append(steps, DecisionChainStep{})
	}
	cfg2.DecisionChains = []DecisionChainConfig{{ID: "long", Steps: steps}}
	cfg2.ApplyDefaults()
	if cfg2.Decision.MaxProviderCalls != maxDecisionChainSteps {
		t.Fatalf("max provider calls = %d, want clamp %d", cfg2.Decision.MaxProviderCalls, maxDecisionChainSteps)
	}

	// Blank task-override keys are dropped during normalization.
	cfg3 := Default()
	cfg3.DecisionPolicies = []DecisionPolicyConfig{{
		ID:            "pol",
		TaskOverrides: map[string]DecisionPolicyWeights{"  ": {}, "task-a": {}},
	}}
	cfg3.ApplyDefaults()
	if _, ok := cfg3.DecisionPolicies[0].TaskOverrides[""]; ok {
		t.Fatal("blank task-override key survived normalization")
	}
	if _, ok := cfg3.DecisionPolicies[0].TaskOverrides["task-a"]; !ok {
		t.Fatal("valid task-override key was dropped")
	}

	// Empty pool/profile ids default to "default" (inside the legacy
	// public-model synthesis block).
	cfg4 := Default()
	cfg4.Routing.PublicModel = "pub-model"
	cfg4.CandidatePools = []CandidatePoolConfig{{ID: ""}}
	cfg4.RouteProfiles = nil
	cfg4.ApplyDefaults()
	if len(cfg4.RouteProfiles) == 0 || cfg4.RouteProfiles[0].CandidatePool != "default" {
		t.Fatalf("route profile pool = %+v, want default", cfg4.RouteProfiles)
	}
	cfg5 := Default()
	cfg5.Routing.PublicModel = "pub-model"
	cfg5.RouteProfiles = []RouteProfileConfig{{ID: ""}}
	cfg5.ApplyDefaults()
	if len(cfg5.VirtualEndpoints) == 0 || cfg5.VirtualEndpoints[0].RouteProfile != "default" {
		t.Fatalf("virtual endpoint profile = %+v, want default", cfg5.VirtualEndpoints)
	}

	// opencode-console providers get the documented models path.
	cfg6 := Default()
	cfg6.Providers = []ProviderConfig{{ID: "console", Type: "opencode-console", BaseURL: "http://127.0.0.1:9", Enabled: true}}
	cfg6.ApplyDefaults()
	if cfg6.Providers[0].ModelsPath != OpencodeModelsPath {
		t.Fatalf("opencode-console models path = %q, want %q", cfg6.Providers[0].ModelsPath, OpencodeModelsPath)
	}
}

// MigrateClientKeysToHashes deduplicates against pre-existing hashes and
// counts every converted key.
func TestIssue663ProvidersMigrateClientKeys(t *testing.T) {
	cfg := Default()
	existing := ClientKeyDigest("alpha")
	cfg.ClientAuth.KeyHashes = []string{existing}
	cfg.ClientAuth.Keys = []string{"alpha", "beta"}
	if n := cfg.MigrateClientKeysToHashes(); n != 2 {
		t.Fatalf("converted = %d, want 2", n)
	}
	if len(cfg.ClientAuth.Keys) != 0 {
		t.Fatal("plaintext client keys were not cleared")
	}
	if len(cfg.ClientAuth.KeyHashes) != 2 {
		t.Fatalf("hashes = %v, want 2 entries (alpha deduped)", cfg.ClientAuth.KeyHashes)
	}
	if cfg.ClientAuth.KeyHashes[0] != existing {
		t.Fatalf("first hash = %q, want pre-existing %q", cfg.ClientAuth.KeyHashes[0], existing)
	}
}

// warnDefault deduplicates identical messages.
func TestIssue663ProvidersWarnDefaultDedup(t *testing.T) {
	cfg := Default()
	cfg.warnDefault("some.key", "not set; defaulted")
	cfg.warnDefault("some.key", "not set; defaulted")
	cfg.warnDefault("some.key", "different behavior")
	if len(cfg.Warnings) != 2 {
		t.Fatalf("warnings = %v, want 2 entries", cfg.Warnings)
	}
}

// LoadBase/Load error returns.
func TestIssue663ProvidersLoadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadBase(dir); err == nil {
		t.Fatal("LoadBase of a directory was accepted")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"listen":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(bad); err == nil {
		t.Fatal("LoadBase with an invalid listen value was accepted")
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("Load of a missing file was accepted")
	}
	// A runtime override that passes ApplyEnvOverrides but fails final
	// validation surfaces from Load's last Validate call.
	good := filepath.Join(dir, "good.json")
	if err := SaveAtomic(good, Default()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_LISTEN", ":99999")
	if _, err := Load(good); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Load with an out-of-range override listen = %v, want a listen error", err)
	}
}

// SaveAtomic preserves environment-controlled fields from the on-disk
// document and rejects invalid configs.
func TestIssue663ProvidersSaveAtomicEnvPreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	base := Default()
	base.Listen = ":9999"
	base.ClientBaseURL = "https://client.example.com"
	base.Admin.BindLocalOnly = true
	base.Logging.File = "/tmp/issue663-base.log"
	if err := SaveAtomic(path, base); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_LISTEN", ":1111")
	t.Setenv("NEXAROUTE_CLIENT_BASE_URL", "https://other.example.com")
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "false")
	t.Setenv("NEXAROUTE_LOG_FILE", "/tmp/issue663-new.log")
	next := Default()
	next.Listen = ":2222"
	next.ClientBaseURL = "https://new.example.com"
	next.Admin.BindLocalOnly = false
	next.Logging.File = "/tmp/issue663-new.log"
	if err := SaveAtomic(path, next); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Listen != ":9999" {
		t.Fatalf("saved listen = %q, want preserved :9999", saved.Listen)
	}
	if saved.ClientBaseURL != "https://client.example.com" {
		t.Fatalf("saved client base URL = %q, want preserved base value", saved.ClientBaseURL)
	}
	if !saved.Admin.BindLocalOnly {
		t.Fatal("saved admin bind-local-only was not preserved from base")
	}
	if saved.Logging.File != "/tmp/issue663-base.log" {
		t.Fatalf("saved log file = %q, want preserved base value", saved.Logging.File)
	}

	invalid := Default()
	invalid.Listen = "bad"
	if err := SaveAtomic(filepath.Join(dir, "invalid.json"), invalid); err == nil {
		t.Fatal("SaveAtomic with an invalid config was accepted")
	}
}

// SaveAtomic with a secret-bearing config and an unusable master key
// fails closed at cipher load instead of persisting plaintext.
func TestIssue663ProvidersSaveAtomicCipherLoadFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKey: "dummy-test-key", AuthMode: "bearer", Enabled: true,
	}}
	t.Setenv("NEXAROUTE_MASTER_KEY", "not-base64!")
	if err := SaveAtomic(filepath.Join(dir, "config.json"), cfg); err == nil {
		t.Fatal("SaveAtomic with secrets and an invalid master key was accepted")
	}
}

// SaveAtomic with a valid master key encrypts provider secrets at rest.
func TestIssue663ProvidersSaveAtomicEncryptsSecrets(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("NEXAROUTE_MASTER_KEY", key)
	dir := t.TempDir()
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKey: "dummy-test-key", AuthMode: "bearer", Enabled: true,
	}}
	path := filepath.Join(dir, "config.json")
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "dummy-test-key") {
		t.Fatal("saved config still contains a plaintext provider key")
	}
}

// writeAtomic fails when the temp directory does not exist and when the
// destination is a directory (rename onto a directory).
func TestIssue663ProvidersWriteAtomicErrors(t *testing.T) {
	if err := writeAtomic(filepath.Join(t.TempDir(), "missing", "config.json"), []byte("x")); err == nil {
		t.Fatal("writeAtomic into a missing directory was accepted")
	}
	dir := t.TempDir()
	if err := writeAtomic(dir, []byte("x")); err == nil {
		t.Fatal("writeAtomic onto a directory was accepted")
	}
}

// Validation branches across the config validators.
func TestIssue663ProvidersValidateBranches(t *testing.T) {
	valid := func(t *testing.T) Config {
		t.Helper()
		cfg := Default()
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("default config does not validate: %v", err)
		}
		return cfg
	}
	expectErr := func(t *testing.T, cfg Config, want string) {
		t.Helper()
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate() = %v, want an error containing %q", err, want)
		}
	}

	// Video job-store URL length (postgres backend; the file backend
	// rejects any URL with a different message).
	cfg := valid(t)
	cfg.Video.JobStoreBackend = "postgres"
	cfg.Video.JobStoreURL = strings.Repeat("x", 2049)
	expectErr(t, cfg, "video.job_store_url is invalid or too long")

	// Egress deny CIDR syntax.
	cfg = valid(t)
	cfg.Egress.DenyCIDRs = []string{"not-an-ip"}
	expectErr(t, cfg, "egress deny_cidrs")

	// Admin security-store URL length and OIDC issuer validation.
	cfg = valid(t)
	cfg.Admin.SecurityStoreBackend = "postgres"
	cfg.Admin.SecurityStoreURL = strings.Repeat("x", 2049)
	expectErr(t, cfg, "admin.security_store_url is invalid or too long")
	cfg = valid(t)
	cfg.Admin.OIDC.Enabled = true
	cfg.Admin.OIDC.IssuerURL = "http://example.com"
	expectErr(t, cfg, "admin.oidc.issuer_url")

	// Empty listen host is accepted (defaults to 0.0.0.0 at runtime).
	cfg = valid(t)
	cfg.Listen = ":8080"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty listen host rejected: %v", err)
	}

	// Semantic cache: provider iteration skips non-matching ids, rejects
	// a non-OpenAI provider, and rejects a negative TTL.
	mkProvider := func(id, typ string) ProviderConfig {
		return ProviderConfig{ID: id, Type: typ, BaseURL: "http://127.0.0.1:9", Enabled: true, MaxConcurrency: 1,
			StreamIdleTimeoutSeconds: 60,
			Models:                   []ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}}}
	}
	cfg = valid(t)
	cfg.Providers = []ProviderConfig{mkProvider("other", "anthropic_compatible"), mkProvider("p1", "openai_compatible")}
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "p1/m1"
	cfg.Cache.Semantic.Threshold = 0.9
	cfg.Cache.Semantic.MaxEntries = 10
	cfg.Cache.Semantic.TTLSeconds = 60
	if err := cfg.Validate(); err != nil {
		t.Fatalf("semantic cache with a skipped provider rejected: %v", err)
	}
	cfg = valid(t)
	cfg.Providers = []ProviderConfig{mkProvider("p2", "anthropic_compatible")}
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "p2/m1"
	cfg.Cache.Semantic.Threshold = 0.9
	cfg.Cache.Semantic.MaxEntries = 10
	expectErr(t, cfg, "must be openai_compatible")
	cfg = valid(t)
	cfg.Providers = []ProviderConfig{mkProvider("p1", "openai_compatible")}
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "p1/m1"
	cfg.Cache.Semantic.Threshold = 0.9
	cfg.Cache.Semantic.MaxEntries = 10
	cfg.Cache.Semantic.TTLSeconds = -1
	expectErr(t, cfg, "cache.semantic.ttl_seconds")

	// MCP server auth_env naming and response-byte bounds.
	cfg = valid(t)
	cfg.MCP.Enabled = true
	cfg.MCP.Servers = []MCPServerConfig{{ID: "s1", URL: "http://127.0.0.1:9", AuthEnv: "not an env name"}}
	expectErr(t, cfg, "auth_env")
	cfg = valid(t)
	cfg.MCP.Enabled = true
	cfg.MCP.Servers = []MCPServerConfig{{ID: "s1", URL: "http://127.0.0.1:9"}}
	cfg.MCP.MaxResponseBytes = 100
	expectErr(t, cfg, "mcp.max_response_bytes")

	// Client-auth hash limits and hex shape.
	cfg = valid(t)
	cfg.ClientAuth.Enabled = true
	for i := 0; i < 1025; i++ {
		cfg.ClientAuth.KeyHashes = append(cfg.ClientAuth.KeyHashes, strings.Repeat("a", 64))
	}
	expectErr(t, cfg, "key_hashes exceeds")
	cfg = valid(t)
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.KeyHashes = []string{strings.Repeat("z", 64)}
	expectErr(t, cfg, "SHA-256 hex digests")

	// A static idempotency header on a provider is a misconfiguration.
	cfg = valid(t)
	cfg.Providers = []ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", Enabled: true, MaxConcurrency: 1,
		StreamIdleTimeoutSeconds: 60,
		Models:                   []ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}},
		Headers:                  map[string]string{"Idempotency-Key": "fixed"},
	}}
	expectErr(t, cfg, "idempotency")

	// Decision max-provider-calls bounds (set after ApplyDefaults so the
	// clamp does not hide the validation error).
	cfg = valid(t)
	cfg.Decision.MaxProviderCalls = 99
	expectErr(t, cfg, "decision.max_provider_calls")
}

// Threshold normalization: empty in, nil out; duplicates collapsed.
func TestIssue663ProvidersNormalizeBudgetAlertThresholds(t *testing.T) {
	if got := normalizeBudgetAlertThresholds(nil); got != nil {
		t.Fatalf("empty thresholds = %v, want nil", got)
	}
	got := normalizeBudgetAlertThresholds([]float64{0.9, 0.5, 0.9})
	if len(got) != 2 || got[0] != 0.5 || got[1] != 0.9 {
		t.Fatalf("normalized thresholds = %v, want [0.5 0.9]", got)
	}
}

// Budget webhook URL validation: length cap and host requirement.
func TestIssue663ProvidersValidateBudgetWebhookURL(t *testing.T) {
	if err := validateBudgetWebhookURL(strings.Repeat("x", 4097), "budget.webhook_url"); err == nil {
		t.Fatal("over-long webhook URL was accepted")
	}
	if err := validateBudgetWebhookURL("https:///path", "budget.webhook_url"); err == nil {
		t.Fatal("hostless webhook URL was accepted")
	}
	if err := validateBudgetWebhookURL("https://hooks.example.com/hook", "budget.webhook_url"); err != nil {
		t.Fatalf("valid webhook URL rejected: %v", err)
	}
}

// Proxy egress violations: unparseable proxy, hostname passthrough,
// denied CIDR prefix, and denied literal address.
func TestIssue663ProvidersProxyEgressViolation(t *testing.T) {
	cfg := Default()
	if got := cfg.proxyEgressViolation(ProviderConfig{ProxyURL: "://bad", BaseURL: "http://127.0.0.1:9"}); got != "" {
		t.Fatalf("unparseable proxy violation = %q, want empty (syntax is the URL validator's job)", got)
	}
	if got := cfg.proxyEgressViolation(ProviderConfig{ProxyURL: "http://proxy.example.com:8080", BaseURL: "http://127.0.0.1:9"}); got != "" {
		t.Fatalf("hostname proxy violation = %q, want empty", got)
	}
	cfg.Egress.DenyCIDRs = []string{"10.0.0.0/8"}
	if got := cfg.proxyEgressViolation(ProviderConfig{ProxyURL: "http://10.1.2.3:8080", BaseURL: "http://127.0.0.1:9"}); !strings.Contains(got, "denied by egress policy") {
		t.Fatalf("denied-CIDR proxy violation = %q, want a denial", got)
	}
	cfg.Egress.DenyCIDRs = []string{"10.1.2.3"}
	if got := cfg.proxyEgressViolation(ProviderConfig{ProxyURL: "http://10.1.2.3:8080", BaseURL: "http://127.0.0.1:9"}); !strings.Contains(got, "denied by egress policy") {
		t.Fatalf("denied-address proxy violation = %q, want a denial", got)
	}
	cfg.Egress.DenyCIDRs = nil
	cfg.Egress.DenyHosts = []string{"proxy.example.com"}
	if got := cfg.proxyEgressViolation(ProviderConfig{ProxyURL: "http://proxy.example.com:8080", BaseURL: "http://127.0.0.1:9"}); !strings.Contains(got, "denied by egress policy") {
		t.Fatalf("denied-host proxy violation = %q, want a denial", got)
	}
}

// Endpoint URL validation: scheme, host, userinfo, fragment.
func TestIssue663ProvidersValidateEndpointURL(t *testing.T) {
	if err := validateEndpointURL("ftp://example.com", "base_url", false); err == nil {
		t.Fatal("non-http(s) endpoint URL was accepted")
	}
	if err := validateEndpointURL("https:///path", "base_url", false); err == nil {
		t.Fatal("hostless endpoint URL was accepted")
	}
	if err := validateEndpointURL("https://user:pass@example.com", "base_url", false); err == nil {
		t.Fatal("endpoint URL with userinfo was accepted")
	}
	if err := validateEndpointURL("https://example.com/#frag", "base_url", false); err == nil {
		t.Fatal("endpoint URL with a fragment was accepted")
	}
	if err := validateEndpointURL("https://example.com/v1", "base_url", false); err != nil {
		t.Fatalf("valid endpoint URL rejected: %v", err)
	}
}

// Recovery threshold/cooldown defaults and bounds.
func TestIssue663ProvidersProbeRecoveryDefaults(t *testing.T) {
	cfg := Default()
	cfg.Probe.RecoverySuccessThreshold = 0
	if got := cfg.ProbeRecoverySuccessThreshold(); got != defaultRecoverySuccessThreshold {
		t.Fatalf("zero threshold = %d, want default %d", got, defaultRecoverySuccessThreshold)
	}
	cfg.Probe.RecoverySuccessThreshold = 5
	cfg.Probe.RecoveryAttempts = 2
	if got := cfg.ProbeRecoverySuccessThreshold(); got != 2 {
		t.Fatalf("threshold above attempts = %d, want 2", got)
	}
	cfg.Probe.RecoveryCooldownSeconds = 0
	if got := cfg.ProbeRecoveryCooldown(); got != time.Duration(defaultRecoveryCooldownSeconds)*time.Second {
		t.Fatalf("zero cooldown = %v, want default", got)
	}
}

// Eligibility helpers: alias matches, blank pool entries skipped, warning
// deduplication.
func TestIssue663ProvidersEligibilityBranches(t *testing.T) {
	d := eligibilityDeployment{id: "p/m", model: "m", aliases: []string{"alias-m"}}
	if !matchesEligibilityEntry(d, "alias-m") {
		t.Fatal("alias entry did not match the deployment")
	}
	if matchesEligibilityEntry(d, "other") {
		t.Fatal("non-matching entry matched the deployment")
	}
	if !matchesEligibilityEntry(d, "m") {
		t.Fatal("model entry did not match (suffix form)")
	}
	all := []eligibilityDeployment{d}
	pool := CandidatePoolConfig{Mode: "explicit", Deployments: []string{"", "p/m"}}
	set := expandEligibilityPool(pool, all)
	if len(set) != 1 || set["p/m"].id != "p/m" {
		t.Fatalf("expanded pool = %v, want only p/m", set)
	}
	cfg := Default()
	cfg.warnEligibility("pool p is empty")
	cfg.warnEligibility("pool p is empty")
	cfg.warnEligibility("another warning")
	if len(cfg.EligibilityWarnings) != 2 {
		t.Fatalf("eligibility warnings = %v, want 2 entries", cfg.EligibilityWarnings)
	}
}
