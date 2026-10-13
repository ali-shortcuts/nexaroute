package privacyaudit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

const canarySecret = "sk-CANARY-SECRET-VALUE-12345"

func codesOf(rep Report) map[string]Severity {
	out := map[string]Severity{}
	for _, f := range rep.Findings {
		out[f.Code] = f.Severity
	}
	return out
}

func baseConfig() *config.Config {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: "https://api.example.com/v1",
		Enabled: true, APIKeyEnv: "P1_KEY",
		DataHandling: config.DataHandlingConfig{TrainsOnData: "no", Retention: "none"},
		Models:       []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true}},
	}}
	cfg.Egress.DenyHosts = []string{"blocked.example.com"}
	return &cfg
}

func TestAuditCleanConfigHasNoErrors(t *testing.T) {
	rep := Audit(baseConfig())
	if rep.HasErrors() {
		t.Fatalf("clean config must have no errors, got %+v", rep.Findings)
	}
	if rep.Warnings != 0 {
		t.Fatalf("clean config must have no warnings, got %+v", rep.Findings)
	}
}

func TestAuditPlaintextSecretDetectedAndRedacted(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].APIKey = canarySecret
	cfg.Providers[0].APIKeyEnv = ""
	rep := Audit(cfg)
	if codesOf(rep)["PLAINTEXT_SECRET"] != SeverityError {
		t.Fatalf("expected PLAINTEXT_SECRET error, got %+v", rep.Findings)
	}
	blob, _ := json.Marshal(rep)
	if strings.Contains(string(blob), canarySecret) {
		t.Fatal("report must never contain the secret value")
	}
}

func TestAuditEncryptedSecretIsClean(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].APIKey = "nxs1:encryptedblob"
	cfg.Providers[0].APIKeyEnv = ""
	rep := Audit(cfg)
	if _, ok := codesOf(rep)["PLAINTEXT_SECRET"]; ok {
		t.Fatalf("nxs1: value must not be flagged: %+v", rep.Findings)
	}
}

func TestAuditCredentialHeaderDetected(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].Headers = map[string]string{"Authorization": "Bearer " + canarySecret}
	rep := Audit(cfg)
	if codesOf(rep)["PLAINTEXT_SECRET"] != SeverityError {
		t.Fatalf("expected PLAINTEXT_SECRET for header, got %+v", rep.Findings)
	}
	blob, _ := json.Marshal(rep)
	if strings.Contains(string(blob), canarySecret) {
		t.Fatal("report must never contain the header credential")
	}
}

func TestAuditURLCredentialsAndCleartext(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].BaseURL = "https://user:passw0rd-canary@api.example.com/v1"
	rep := Audit(cfg)
	if codesOf(rep)["URL_CREDENTIALS"] != SeverityError {
		t.Fatalf("expected URL_CREDENTIALS error, got %+v", rep.Findings)
	}
	blob, _ := json.Marshal(rep)
	if strings.Contains(string(blob), "passw0rd-canary") {
		t.Fatal("report must never contain URL userinfo")
	}

	cfg2 := baseConfig()
	cfg2.Providers[0].BaseURL = "http://api.example.com/v1"
	if codesOf(Audit(cfg2))["CLEARTEXT_ENDPOINT"] != SeverityError {
		t.Fatal("http on a public host must be flagged")
	}

	cfg3 := baseConfig()
	cfg3.Providers[0].BaseURL = "http://127.0.0.1:5000/v1"
	if _, ok := codesOf(Audit(cfg3))["CLEARTEXT_ENDPOINT"]; ok {
		t.Fatal("http on loopback must not be flagged")
	}
}

func noTrainingConfig(poolDeployments []string, trains map[string]string) *config.Config {
	cfg := config.Default()
	cfg.Providers = nil
	for pid, tr := range trains {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: pid, Name: pid, Type: "openai_compatible", BaseURL: "https://example.com/v1",
			Enabled: true, APIKeyEnv: "K",
			DataHandling: config.DataHandlingConfig{TrainsOnData: tr},
			Models:       []config.ModelConfig{{ID: "m", Model: "model-" + pid, Enabled: true}},
		})
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: poolDeployments}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "strict", CandidatePool: "pool", Privacy: "no_training"}}
	cfg.Egress.DenyHosts = []string{"x.example.com"}
	return &cfg
}

func TestAuditNoTrainingUnsatisfiable(t *testing.T) {
	cfg := noTrainingConfig([]string{"a/m"}, map[string]string{"a": "yes"})
	rep := Audit(cfg)
	if codesOf(rep)["NO_TRAINING_UNSATISFIABLE"] != SeverityError {
		t.Fatalf("expected NO_TRAINING_UNSATISFIABLE error, got %+v", rep.Findings)
	}
}

func TestAuditNoTrainingUnknownClassFailsClosed(t *testing.T) {
	// Undeclared trains_on_data must not satisfy no_training (runtime gate
	// is fail-closed), so this is unsatisfiable too.
	cfg := noTrainingConfig([]string{"a/m"}, map[string]string{"a": ""})
	rep := Audit(cfg)
	if codesOf(rep)["NO_TRAINING_UNSATISFIABLE"] != SeverityError {
		t.Fatalf("expected unsatisfiable for undeclared class, got %+v", rep.Findings)
	}
}

func TestAuditNoTrainingPartial(t *testing.T) {
	cfg := noTrainingConfig([]string{"a/m", "b/m"}, map[string]string{"a": "no", "b": "yes"})
	rep := Audit(cfg)
	codes := codesOf(rep)
	if codes["NO_TRAINING_PARTIAL"] != SeverityWarning {
		t.Fatalf("expected NO_TRAINING_PARTIAL warning, got %+v", rep.Findings)
	}
	if _, ok := codes["NO_TRAINING_UNSATISFIABLE"]; ok {
		t.Fatal("partial pool must not be reported unsatisfiable")
	}
}

func TestAuditNoTrainingSatisfied(t *testing.T) {
	cfg := noTrainingConfig([]string{"a/m"}, map[string]string{"a": "no"})
	rep := Audit(cfg)
	codes := codesOf(rep)
	if _, ok := codes["NO_TRAINING_UNSATISFIABLE"]; ok {
		t.Fatalf("satisfied pool must be clean, got %+v", rep.Findings)
	}
	if _, ok := codes["NO_TRAINING_PARTIAL"]; ok {
		t.Fatalf("fully satisfying pool must not warn partial, got %+v", rep.Findings)
	}
}

func TestAuditUnknownPoolReference(t *testing.T) {
	cfg := baseConfig()
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "broken", CandidatePool: "does-not-exist", Privacy: "no_training"}}
	rep := Audit(cfg)
	if codesOf(rep)["PROFILE_POOL_UNKNOWN"] != SeverityError {
		t.Fatalf("expected PROFILE_POOL_UNKNOWN error, got %+v", rep.Findings)
	}
}

func TestAuditVirtualKeyNoTraining(t *testing.T) {
	cfg := baseConfig() // p1 trains_on_data=no -> satisfiable globally
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{ID: "k1", RequirePrivacy: "no_training"}}
	if codes := codesOf(Audit(cfg)); codes["NO_TRAINING_UNSATISFIABLE"] == SeverityError {
		t.Fatalf("key must be satisfiable when a no-training deployment exists: %+v", Audit(cfg).Findings)
	}

	cfg.Providers[0].DataHandling.TrainsOnData = "yes"
	rep := Audit(cfg)
	found := false
	for _, f := range rep.Findings {
		if f.Code == "NO_TRAINING_UNSATISFIABLE" && f.Scope == "virtual_key:k1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected virtual-key unsatisfiable finding, got %+v", rep.Findings)
	}

	// A revoked key imposes no requirement.
	cfg.ClientAuth.VirtualKeys[0].Revoked = true
	for _, f := range Audit(cfg).Findings {
		if f.Scope == "virtual_key:k1" {
			t.Fatalf("revoked key must not produce findings, got %+v", f)
		}
	}
}

func TestAuditInfoFindings(t *testing.T) {
	cfg := baseConfig()
	cfg.Egress.DenyHosts = nil
	cfg.Cache.Enabled = true
	cfg.Logging.AccessMode = "all"
	cfg.Providers[0].DataHandling.TrainsOnData = ""
	rep := Audit(cfg)
	codes := codesOf(rep)
	if codes["EGRESS_DENY_POLICY_EMPTY"] != SeverityInfo {
		t.Fatal("expected egress info finding")
	}
	if codes["RESPONSE_CACHE_ENABLED"] != SeverityInfo {
		t.Fatal("expected cache info finding")
	}
	if codes["ACCESS_LOG_ALL"] != SeverityWarning {
		t.Fatal("expected access-log warning")
	}
	if codes["DATA_HANDLING_UNDECLARED"] != SeverityInfo {
		t.Fatal("expected data-handling info finding")
	}
	if rep.HasErrors() {
		t.Fatalf("info/warning-only report must have no errors: %+v", rep.Findings)
	}
}

func TestAuditDeterministicOrdering(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].APIKey = canarySecret
	cfg.Cache.Enabled = true
	cfg.Egress.DenyHosts = nil
	r1 := Audit(cfg)
	r2 := Audit(cfg)
	if len(r1.Findings) != len(r2.Findings) {
		t.Fatal("finding count differs between runs")
	}
	for i := range r1.Findings {
		if r1.Findings[i] != r2.Findings[i] {
			t.Fatalf("finding %d differs between runs: %+v vs %+v", i, r1.Findings[i], r2.Findings[i])
		}
	}
	// Errors must sort before warnings before infos.
	for i := 1; i < len(r1.Findings); i++ {
		if severityRank(r1.Findings[i].Severity) < severityRank(r1.Findings[i-1].Severity) {
			t.Fatalf("findings not sorted by severity: %+v", r1.Findings)
		}
	}
}

func TestAuditDecisionProviderPlaintext(t *testing.T) {
	cfg := baseConfig()
	cfg.DecisionProviders = []config.DecisionProviderConfig{{
		ID: "jev1", Type: "jev", APIKey: canarySecret, BaseURL: "https://jev.example.com",
	}}
	rep := Audit(cfg)
	found := false
	for _, f := range rep.Findings {
		if f.Code == "PLAINTEXT_SECRET" && f.Scope == "decision_provider:jev1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected decision provider plaintext finding, got %+v", rep.Findings)
	}
	blob, _ := json.Marshal(rep)
	if strings.Contains(string(blob), canarySecret) {
		t.Fatal("report must never contain the decision provider secret")
	}
}
