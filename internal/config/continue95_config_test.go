package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func Continue95ValidProvider(id string) ProviderConfig {
	return ProviderConfig{ID: id, Type: "openai_compatible", BaseURL: "https://api.example", MaxConcurrency: 1, StreamIdleTimeoutSeconds: 30}
}

func TestContinue95MigrateAndResolvedHelpers(t *testing.T) {
	// Duplicate plaintext keys collapse to one digest (seen-map branch).
	cfg := Default()
	cfg.ClientAuth.Keys = []string{"dup-key", "dup-key"}
	if n := cfg.MigrateClientKeysToHashes(); n != 2 || len(cfg.ClientAuth.KeyHashes) != 1 || len(cfg.ClientAuth.Keys) != 0 {
		t.Fatalf("migrate n=%d hashes=%v keys=%v", n, cfg.ClientAuth.KeyHashes, cfg.ClientAuth.Keys)
	}
	// Credential env hit: the env value wins over the stored fallback.
	t.Setenv("CONTINUE95_API_KEY_ENV", "env-key-value")
	cred := CredentialConfig{APIKeyEnv: "CONTINUE95_API_KEY_ENV", APIKey: "fallback"}
	if got := cred.Resolved(); got != "env-key-value" {
		t.Fatalf("Resolved() = %q", got)
	}
	// ResolvedCredentialEntries: unnamed pool key defaults to key-<i+1>
	// and a non-positive weight normalises to 1.
	p := ProviderConfig{APIKey: "primary-key", Credentials: []CredentialConfig{{Enabled: true, APIKey: "pool-key", Weight: 0}}}
	entries := p.ResolvedCredentialEntries()
	if len(entries) != 2 || entries[1].Name != "key-1" || entries[1].Weight != 1 || entries[1].PoolIndex != 0 {
		t.Fatalf("entries = %+v", entries)
	}
	// warnDefault deduplicates identical messages.
	cfg2 := Default()
	cfg2.warnDefault("k", "b")
	cfg2.warnDefault("k", "b")
	if len(cfg2.Warnings) != 1 {
		t.Fatalf("warnings = %v", cfg2.Warnings)
	}
	// Empty threshold input normalises to nil.
	if got := normalizeBudgetAlertThresholds(nil); got != nil {
		t.Fatalf("normalize(nil) = %v", got)
	}
	// Probe recovery accessors: below-1 default, clamp to attempts,
	// non-positive cooldown default.
	probeCfg := Default()
	probeCfg.Probe.RecoverySuccessThreshold = 0
	probeCfg.Probe.RecoveryAttempts = 2
	if got := probeCfg.ProbeRecoverySuccessThreshold(); got != defaultRecoverySuccessThreshold && got != 2 {
		t.Fatalf("threshold default = %d", got)
	}
	probeCfg.Probe.RecoverySuccessThreshold = 9
	probeCfg.Probe.RecoveryAttempts = 2
	if got := probeCfg.ProbeRecoverySuccessThreshold(); got != 2 {
		t.Fatalf("threshold clamp = %d", got)
	}
	probeCfg.Probe.RecoveryCooldownSeconds = 0
	if got := probeCfg.ProbeRecoveryCooldown(); got.String() != "15m0s" {
		t.Fatalf("cooldown default = %v", got)
	}
}

func TestContinue95ApplyDefaultsBranches(t *testing.T) {
	cfg := Default()
	cfg.Evaluation.TelemetryMinSamples = maxTelemetryMinSamples + 1
	cfg.ApplyDefaults()
	if cfg.Evaluation.TelemetryMinSamples != maxTelemetryMinSamples {
		t.Fatalf("telemetry clamp = %d", cfg.Evaluation.TelemetryMinSamples)
	}
	// Hybrid with no matching chain defaults to 1 call.
	cfg = Default()
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.MaxProviderCalls = 0
	cfg.ApplyDefaults()
	if cfg.Decision.MaxProviderCalls != 1 {
		t.Fatalf("hybrid default calls = %d", cfg.Decision.MaxProviderCalls)
	}
	// Hybrid above the chain-step cap clamps to the cap: a 9-step
	// matching chain seeds 9 calls, then the cap applies.
	cfg = Default()
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.MaxProviderCalls = 0
	cfg.Decision.Chain = "ch1"
	steps := make([]DecisionChainStep, 9)
	for i := range steps {
		steps[i] = DecisionChainStep{Provider: "local"}
	}
	cfg.DecisionChains = []DecisionChainConfig{{ID: "ch1", Steps: steps}}
	cfg.ApplyDefaults()
	if cfg.Decision.MaxProviderCalls != maxDecisionChainSteps {
		t.Fatalf("hybrid clamp = %d", cfg.Decision.MaxProviderCalls)
	}
	// Task-override keys normalise; whitespace-only keys are skipped.
	cfg = Default()
	cfg.DecisionPolicies = []DecisionPolicyConfig{{ID: "pol", TaskOverrides: map[string]DecisionPolicyWeights{" ": {}, "Task": {}}}}
	cfg.ApplyDefaults()
	if _, ok := cfg.DecisionPolicies[0].TaskOverrides[""]; ok {
		t.Fatal("empty task-override key survived")
	}
	if _, ok := cfg.DecisionPolicies[0].TaskOverrides["task"]; !ok {
		t.Fatalf("task overrides = %v", cfg.DecisionPolicies[0].TaskOverrides)
	}
	// Legacy public_model synthesis with empty pool/profile IDs.
	cfg = Default()
	cfg.Routing.PublicModel = "gpt-x"
	cfg.CandidatePools = []CandidatePoolConfig{{ID: ""}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: ""}}
	cfg.VirtualEndpoints = nil
	cfg.ApplyDefaults()
	if len(cfg.VirtualEndpoints) != 1 || cfg.VirtualEndpoints[0].RouteProfile != "default" {
		t.Fatalf("synthesised endpoints = %+v", cfg.VirtualEndpoints)
	}
	// opencode-console providers gain the console models path.
	cfg = Default()
	cfg.Providers = []ProviderConfig{{ID: "op", Type: "opencode-console"}}
	cfg.ApplyDefaults()
	if cfg.Providers[0].ModelsPath != OpencodeModelsPath {
		t.Fatalf("models path = %q", cfg.Providers[0].ModelsPath)
	}
}

func TestContinue95ValidatePlanes(t *testing.T) {
	// Video postgres URL too long, and unknown video backend.
	cfg := Default()
	cfg.Video.JobStoreBackend = "postgres"
	cfg.Video.JobStoreURL = strings.Repeat("x", 2049)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "video.job_store_url") {
		t.Fatalf("video url error = %v", err)
	}
	cfg = Default()
	cfg.Video.JobStoreBackend = "bogus"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "video.job_store_backend") {
		t.Fatalf("video backend error = %v", err)
	}
	// Admin security-store URL too long, and unknown backend.
	cfg = Default()
	cfg.Admin.SecurityStoreBackend = "redis"
	cfg.Admin.SecurityStoreURL = strings.Repeat("x", 2049)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "admin.security_store_url") {
		t.Fatalf("security url error = %v", err)
	}
	cfg = Default()
	cfg.Admin.SecurityStoreBackend = "bogus"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "admin.security_store_backend") {
		t.Fatalf("security backend error = %v", err)
	}
	// Empty listen host defaults to 0.0.0.0 and validates.
	cfg = Default()
	cfg.Listen = ":8080"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty-host listen error = %v", err)
	}
	// Semantic cache: a non-matching provider is skipped, then a
	// non-openai_compatible match is rejected.
	cfg = Default()
	cfg.Providers = []ProviderConfig{Continue95ValidProvider("other"), {ID: "p1", Type: "anthropic", BaseURL: "https://api.example", MaxConcurrency: 1, StreamIdleTimeoutSeconds: 30}}
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "p1/m"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be openai_compatible") {
		t.Fatalf("semantic provider error = %v", err)
	}
	// Negative semantic TTL with a valid deployment.
	cfg = Default()
	cfg.Providers = []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "https://api.example", MaxConcurrency: 1, StreamIdleTimeoutSeconds: 30, Models: []ModelConfig{{ID: "m"}}}}
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "p1/m"
	cfg.Cache.Semantic.Threshold = 0.9
	cfg.Cache.Semantic.MaxEntries = 100
	cfg.Cache.Semantic.TTLSeconds = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ttl_seconds") {
		t.Fatalf("semantic ttl error = %v", err)
	}
	// MCP auth_env must name an environment variable.
	cfg = Default()
	cfg.MCP.Enabled = true
	cfg.MCP.Servers = []MCPServerConfig{{ID: "s1", URL: "http://localhost:9000", AuthEnv: "not valid!"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "auth_env") {
		t.Fatalf("mcp auth_env error = %v", err)
	}
	// MCP max_response_bytes out of range.
	cfg = Default()
	cfg.MCP.Enabled = true
	cfg.MCP.Servers = []MCPServerConfig{{ID: "s1", URL: "http://localhost:9000"}}
	cfg.MCP.MaxRequestBytes = 4096
	cfg.MCP.MaxResponseBytes = 100
	cfg.MCP.CallTimeoutSeconds = 30
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_response_bytes") {
		t.Fatalf("mcp response bytes error = %v", err)
	}
	// Client-auth key hashes: over-limit, then non-SHA-256-hex.
	cfg = Default()
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.KeyHashes = make([]string, 1025)
	for i := range cfg.ClientAuth.KeyHashes {
		cfg.ClientAuth.KeyHashes[i] = strings.Repeat("a", 64)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "key_hashes exceeds") {
		t.Fatalf("key_hashes limit error = %v", err)
	}
	cfg = Default()
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.KeyHashes = []string{strings.Repeat("z", 64)}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "SHA-256 hex") {
		t.Fatalf("key_hashes hex error = %v", err)
	}
	// Static idempotency headers are rejected.
	cfg = Default()
	p := Continue95ValidProvider("p1")
	p.Headers = map[string]string{"Idempotency-Key": "abc"}
	cfg.Providers = []ProviderConfig{p}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("idempotency error = %v", err)
	}
}

func TestContinue95DecisionProvidersAndChainsDirect(t *testing.T) {
	// validateDecisionCore already rejects out-of-range calls, so the
	// providers-and-chains copy is exercised directly.
	cfg := Default()
	cfg.Decision.Mode = "hybrid"
	cfg.Decision.Chain = "ch1"
	cfg.DecisionChains = []DecisionChainConfig{{ID: "ch1", Steps: []DecisionChainStep{{Provider: "local", TimeoutMS: 100}}}}
	cfg.Decision.MaxProviderCalls = 99
	if err := cfg.validateDecisionProvidersAndChains(); err == nil || !strings.Contains(err.Error(), "max_provider_calls") {
		t.Fatalf("hybrid calls error = %v", err)
	}
}

func TestContinue95WebhookEgressAndEndpointHelpers(t *testing.T) {
	if err := validateBudgetWebhookURL(strings.Repeat("https://example.com/", 300), "webhook"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("webhook length error = %v", err)
	}
	if err := validateBudgetWebhookURL("not-a-url", "webhook"); err == nil || !strings.Contains(err.Error(), "absolute http(s) URL") {
		t.Fatalf("webhook absolute error = %v", err)
	}
	// Egress allow-list accessors return the resolved lists.
	p := ProviderConfig{}
	p.resolvedEgressAllowHosts = []string{"api.example"}
	p.resolvedEgressAllowCIDRs = []string{"10.0.0.0/8"}
	if got := p.EgressAllowHosts(); len(got) != 1 || got[0] != "api.example" {
		t.Fatalf("allow hosts = %v", got)
	}
	if got := p.EgressAllowCIDRs(); len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("allow cidrs = %v", got)
	}
	// proxyEgressViolation: unparseable proxy URL, non-IP hostname,
	// and a bare-IP deny-CIDR match.
	cfg := Default()
	if got := cfg.proxyEgressViolation(ProviderConfig{ProxyURL: "http://[::1"}); got != "" {
		t.Fatalf("unparseable proxy violation = %q", got)
	}
	if got := cfg.proxyEgressViolation(ProviderConfig{ProxyURL: "http://proxy.example"}); got != "" {
		t.Fatalf("hostname proxy violation = %q", got)
	}
	cfg.Egress.DenyCIDRs = []string{"10.0.0.5"}
	if got := cfg.proxyEgressViolation(ProviderConfig{BaseURL: "https://api.example", ProxyURL: "http://10.0.0.5:8080"}); !strings.Contains(got, "denied") {
		t.Fatalf("cidr proxy violation = %q", got)
	}
	// Fragment in a provider URL is rejected.
	if err := validateEndpointURL("https://example.com/x#frag", "provider base_url", false); err == nil || !strings.Contains(err.Error(), "fragment") {
		t.Fatalf("fragment error = %v", err)
	}
}

func TestContinue95LoadPaths(t *testing.T) {
	// LoadBase on a directory: open succeeds, read fails.
	if _, err := LoadBase(t.TempDir()); err == nil {
		t.Fatal("LoadBase accepted a directory")
	}
	// LoadBase on malformed JSON.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(bad); err == nil {
		t.Fatal("LoadBase accepted malformed JSON")
	}
	// Load with an invalid boolean env override.
	good := filepath.Join(dir, "good.json")
	raw, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "notabool")
	if _, err := Load(good); err == nil || !strings.Contains(err.Error(), "must be a valid boolean") {
		t.Fatalf("Load env-override error = %v", err)
	}
}

func TestContinue95LoadFinalValidateError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	raw, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_CLIENT_BASE_URL", "not a url")
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an env override that breaks validation")
	}
}

func TestContinue95LoadBaseSecretMigration(t *testing.T) {
	// A local master key (env, no Vault) lets the legacy-plaintext
	// migration run: backup + atomically rewritten encrypted file.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	plain := `{"admin":{"api_key":"synthetic-admin-key-123"}}`
	if err := os.WriteFile(path, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(path); err != nil {
		t.Fatalf("LoadBase migration error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "synthetic-admin-key-123") {
		t.Fatal("migrated file still holds the plaintext secret")
	}
	if !strings.Contains(string(after), "nxs1:") {
		t.Fatalf("migrated file lacks an nxs1 envelope: %s", string(after)[:200])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected a backup file beside the config, entries = %v", entries)
	}
}

func TestContinue95SaveAtomicPaths(t *testing.T) {
	// Env-controlled fields are restored from the on-disk document.
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:9090","client_base_url":"https://base.example","admin":{"bind_local_only":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_LISTEN", "127.0.0.1:7070")
	t.Setenv("NEXAROUTE_CLIENT_BASE_URL", "https://env.example")
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "true")
	cfg := Default()
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatalf("SaveAtomic error = %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(saved, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["listen"] != "127.0.0.1:9090" {
		t.Fatalf("saved listen = %v", doc["listen"])
	}
	if doc["client_base_url"] != "https://base.example" {
		t.Fatalf("saved client_base_url = %v", doc["client_base_url"])
	}
	admin, _ := doc["admin"].(map[string]any)
	if admin["bind_local_only"] != false {
		t.Fatalf("saved bind_local_only = %v", admin["bind_local_only"])
	}
	// SaveAtomic on an invalid config fails validation (ApplyDefaults
	// repairs an empty Listen, so use a backend it cannot repair).
	cfg = Default()
	cfg.Admin.SecurityStoreBackend = "bogus"
	if err := SaveAtomic(filepath.Join(dir, "invalid.json"), cfg); err == nil {
		t.Fatal("SaveAtomic accepted an invalid config")
	}
	// SaveAtomic with secrets but no usable cipher fails at LoadCipher.
	// (An explicit missing key file errors; the default path would
	// auto-generate a key instead.)
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", filepath.Join(dir, "missing.key"))
	cfg = Default()
	p := Continue95ValidProvider("p1")
	p.APIKey = "synthetic-provider-key"
	cfg.Providers = []ProviderConfig{p}
	if err := SaveAtomic(filepath.Join(dir, "secret.json"), cfg); err == nil {
		t.Fatal("SaveAtomic accepted secrets without a cipher")
	}
}

func TestContinue95SaveAtomicOversize(t *testing.T) {
	// 512 providers at the header cap with 8 KiB values serialise past
	// the 16 MiB safe limit while each provider stays valid. Headers
	// count as secret values, so a master key is supplied for the
	// (size-preserving) encrypt pass that precedes the length check.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	cfg := Default()
	big := strings.Repeat("v", 8192)
	for i := 0; i < 512; i++ {
		p := Continue95ValidProvider("p" + Continue95Itoa(i))
		p.Headers = map[string]string{"h1": big, "h2": big, "h3": big, "h4": big}
		cfg.Providers = append(cfg.Providers, p)
	}
	if err := SaveAtomic(filepath.Join(t.TempDir(), "big.json"), cfg); err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("oversize error = %v", err)
	}
}

func Continue95Itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

func TestContinue95WriteAtomicErrors(t *testing.T) {
	// CreateTemp fails when the parent directory does not exist.
	if err := writeAtomic(filepath.Join(t.TempDir(), "nope", "f.json"), []byte("x")); err == nil {
		t.Fatal("writeAtomic accepted a missing parent directory")
	}
	// Rename fails when the target is an existing directory.
	dir := t.TempDir()
	target := filepath.Join(dir, "adir")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(target, []byte("x")); err == nil {
		t.Fatal("writeAtomic renamed over an existing directory")
	}
}

func TestContinue95LoadAndValidateErrorReturns(t *testing.T) {
	// LoadBase: valid JSON that fails Validate returns at the
	// post-ApplyDefaults error site.
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-video.json")
	if err := os.WriteFile(path, []byte(`{"video":{"job_store_backend":"bogus"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(path); err == nil || !strings.Contains(err.Error(), "video.job_store_backend") {
		t.Fatalf("LoadBase validate error = %v", err)
	}
	// Load: a LoadBase failure propagates at Load's first error site.
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load accepted a directory")
	}
	// Validate: an invalid egress deny entry fails before later planes.
	cfg := Default()
	cfg.Egress.DenyCIDRs = []string{"bogus"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "deny_cidrs") {
		t.Fatalf("egress error = %v", err)
	}
	// Validate: an enabled OIDC plane with no issuer fails in the
	// admin plane.
	cfg = Default()
	cfg.Admin.OIDC.Enabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "issuer_url") {
		t.Fatalf("oidc error = %v", err)
	}
}

func TestContinue95SynthesisPoolDefault(t *testing.T) {
	// Empty pool ID defaults when the profile list is synthesised.
	cfg := Default()
	cfg.Routing.PublicModel = "gpt-x"
	cfg.CandidatePools = []CandidatePoolConfig{{ID: ""}}
	cfg.RouteProfiles = nil
	cfg.VirtualEndpoints = nil
	cfg.ApplyDefaults()
	if len(cfg.RouteProfiles) != 1 || cfg.RouteProfiles[0].CandidatePool != "default" {
		t.Fatalf("synthesised profiles = %+v", cfg.RouteProfiles)
	}
}

func TestContinue95MigratePreexistingHash(t *testing.T) {
	// The seen-map is pre-populated from existing hashes.
	cfg := Default()
	cfg.ClientAuth.KeyHashes = []string{ClientKeyDigest("dup-key")}
	cfg.ClientAuth.Keys = []string{"dup-key"}
	if n := cfg.MigrateClientKeysToHashes(); n != 1 || len(cfg.ClientAuth.KeyHashes) != 1 {
		t.Fatalf("migrate n=%d hashes=%v", n, cfg.ClientAuth.KeyHashes)
	}
}

func TestContinue95NormalizeNonEmpty(t *testing.T) {
	got := normalizeBudgetAlertThresholds([]float64{0.9, 0.5, 0.9})
	if len(got) != 2 || got[0] != 0.5 || got[1] != 0.9 {
		t.Fatalf("normalize = %v", got)
	}
}

func TestContinue95EligibilityHelpers(t *testing.T) {
	// warnEligibility deduplicates identical messages.
	cfg := Default()
	cfg.warnEligibility("same")
	cfg.warnEligibility("same")
	if len(cfg.EligibilityWarnings) != 1 {
		t.Fatalf("eligibility warnings = %v", cfg.EligibilityWarnings)
	}
	// A pool entry matching a deployment alias resolves the pool, and
	// an empty entry is skipped without failing the pool.
	cfg = Default()
	cfg.Providers = []ProviderConfig{{ID: "p1", Type: "openai_compatible", BaseURL: "https://api.example", Enabled: true, MaxConcurrency: 1, StreamIdleTimeoutSeconds: 30, Models: []ModelConfig{{ID: "m1", Enabled: true, Aliases: []string{"alias1"}}}}}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"", "alias1"}}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "rp", CandidatePool: "pool1"}}
	cfg.computeEligibilityWarnings()
	for _, w := range cfg.EligibilityWarnings {
		if strings.Contains(w, "pool1") {
			t.Fatalf("pool1 should resolve via alias, warnings = %v", cfg.EligibilityWarnings)
		}
	}
}
