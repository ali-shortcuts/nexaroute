package privacyaudit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// --- Finding 1: virtual-key NO_TRAINING_PARTIAL ---

// keyConfig builds providers from a trains_on_data map (provider id ->
// class), each with one model "m", plus a no_training virtual key.
func keyConfig(trains map[string]string) *config.Config {
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
	cfg.Egress.DenyHosts = []string{"x.example.com"}
	return &cfg
}

func keyFindings(rep Report, keyID string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Scope == "virtual_key:"+keyID {
			out = append(out, f)
		}
	}
	return out
}

func TestVirtualKeyPartialUnrestricted(t *testing.T) {
	cfg := keyConfig(map[string]string{"a": "no", "b": "yes"})
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{ID: "k1", RequirePrivacy: "no_training"}}
	kf := keyFindings(Audit(cfg), "k1")
	if len(kf) != 1 || kf[0].Code != "NO_TRAINING_PARTIAL" || kf[0].Severity != SeverityWarning {
		t.Fatalf("unrestricted mixed key must yield exactly one PARTIAL warning, got %+v", kf)
	}
	if !strings.Contains(kf[0].Message, "1 of 2") {
		t.Fatalf("partial message must count the full set (1 of 2), got: %s", kf[0].Message)
	}
}

func TestVirtualKeyPartialAllowedRoutes(t *testing.T) {
	cfg := keyConfig(map[string]string{"a": "no", "b": "yes"})
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m", "b/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "k1", RequirePrivacy: "no_training", AllowedRoutes: []string{"r1"},
	}}
	kf := keyFindings(Audit(cfg), "k1")
	if len(kf) != 1 || kf[0].Code != "NO_TRAINING_PARTIAL" || kf[0].Severity != SeverityWarning {
		t.Fatalf("route-restricted mixed key must yield PARTIAL warning, got %+v", kf)
	}
	if !strings.Contains(kf[0].Message, "1 of 2") {
		t.Fatalf("partial message must count 1 of 2, got: %s", kf[0].Message)
	}
}

func TestVirtualKeyAllSatisfyNoFinding(t *testing.T) {
	cfg := keyConfig(map[string]string{"a": "no", "b": "no"})
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{ID: "k1", RequirePrivacy: "no_training"}}
	if kf := keyFindings(Audit(cfg), "k1"); len(kf) != 0 {
		t.Fatalf("fully satisfying key must produce no findings, got %+v", kf)
	}
}

func TestVirtualKeyNoneSatisfyViaRoutes(t *testing.T) {
	cfg := keyConfig(map[string]string{"a": "yes", "b": "yes"})
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "k1", RequirePrivacy: "no_training", AllowedRoutes: []string{"r1"},
	}}
	kf := keyFindings(Audit(cfg), "k1")
	if len(kf) != 1 || kf[0].Code != "NO_TRAINING_UNSATISFIABLE" || kf[0].Severity != SeverityError {
		t.Fatalf("expected one UNSATISFIABLE error, got %+v", kf)
	}
}

func TestVirtualKeyRevokedMixedNoFindings(t *testing.T) {
	cfg := keyConfig(map[string]string{"a": "no", "b": "yes"})
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "k1", RequirePrivacy: "no_training", Revoked: true,
	}}
	if kf := keyFindings(Audit(cfg), "k1"); len(kf) != 0 {
		t.Fatalf("revoked key must produce no findings, got %+v", kf)
	}
}

func TestVirtualKeyOverlappingRoutesDedup(t *testing.T) {
	cfg := keyConfig(map[string]string{"a": "no", "b": "yes"})
	// r1 reaches {a, b}; r2 reaches {a}. Union is {a, b}: 1 of 2, not 1 of 3.
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "poolAB", Mode: "explicit", Deployments: []string{"a/m", "b/m"}},
		{ID: "poolA", Mode: "explicit", Deployments: []string{"a/m"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "r1", CandidatePool: "poolAB"},
		{ID: "r2", CandidatePool: "poolA"},
	}
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "k1", RequirePrivacy: "no_training", AllowedRoutes: []string{"r1", "r2"},
	}}
	kf := keyFindings(Audit(cfg), "k1")
	if len(kf) != 1 || kf[0].Code != "NO_TRAINING_PARTIAL" {
		t.Fatalf("expected one PARTIAL finding, got %+v", kf)
	}
	if !strings.Contains(kf[0].Message, "1 of 2") {
		t.Fatalf("overlapping routes must be deduplicated (want '1 of 2'), got: %s", kf[0].Message)
	}
}

func TestVirtualKeyUnknownRouteReported(t *testing.T) {
	cfg := keyConfig(map[string]string{"a": "no"})
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "k1", RequirePrivacy: "no_training", AllowedRoutes: []string{"ghost-route"},
	}}
	kf := keyFindings(Audit(cfg), "k1")
	found := false
	for _, f := range kf {
		if f.Code == "VIRTUAL_KEY_ROUTE_UNKNOWN" && f.Severity == SeverityError {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown allowed route must be reported, got %+v", kf)
	}
}

// --- Finding 2: header/storage-encryption contract ---

func TestCustomHeaderPlaintextDetected(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].Headers = map[string]string{"X-Access-Token": canarySecret}
	rep := Audit(cfg)
	if codesOf(rep)["PLAINTEXT_SECRET"] != SeverityError {
		t.Fatalf("custom header with plaintext value must be flagged, got %+v", rep.Findings)
	}
	blob, _ := json.Marshal(rep)
	if strings.Contains(string(blob), canarySecret) {
		t.Fatal("report must never contain the header value")
	}
}

func TestCustomHeaderEncryptedNotFlagged(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].Headers = map[string]string{"X-Access-Token": "nxs1:encryptedblob"}
	rep := Audit(cfg)
	if _, ok := codesOf(rep)["PLAINTEXT_SECRET"]; ok {
		t.Fatalf("encrypted header value must not be flagged: %+v", rep.Findings)
	}
}

func TestStoredSecretFieldsContract(t *testing.T) {
	const proxyCanary = "proxy-user:proxy-pass-canary"
	cfg := baseConfig()
	cfg.Providers[0].ProxyURL = "http://" + proxyCanary + "@proxy.example.com:8080"
	cfg.Admin.APIKey = canarySecret
	cfg.ClientAuth.Keys = []string{canarySecret}
	rep := Audit(cfg)
	scopes := map[string]bool{}
	for _, f := range rep.Findings {
		if f.Code == "PLAINTEXT_SECRET" {
			scopes[f.Scope] = true
		}
		if f.Code == "CLEARTEXT_ENDPOINT" {
			t.Fatalf("proxy_url must not raise CLEARTEXT_ENDPOINT (http proxies are accepted): %+v", f)
		}
	}
	for _, want := range []string{"provider:p1", "admin", "client_auth"} {
		if !scopes[want] {
			t.Fatalf("expected PLAINTEXT_SECRET for scope %s, got %+v", want, rep.Findings)
		}
	}
	blob, _ := json.Marshal(rep)
	if strings.Contains(string(blob), canarySecret) || strings.Contains(string(blob), "proxy-pass-canary") {
		t.Fatal("report must never contain stored secret values")
	}
}

// --- Finding 3: disabled decision providers ---

func boolPtr(b bool) *bool { return &b }

func TestDisabledDecisionProviderPlaintextStillAudited(t *testing.T) {
	cfg := baseConfig()
	cfg.DecisionProviders = []config.DecisionProviderConfig{{
		ID: "jev-off", Type: "jev", Enabled: boolPtr(false),
		APIKey: canarySecret, BaseURL: "https://user:passw0rd-canary@jev.example.com",
	}}
	rep := Audit(cfg)
	foundSecret, foundURL := false, false
	for _, f := range rep.Findings {
		if f.Scope != "decision_provider:jev-off" {
			continue
		}
		if f.Code == "PLAINTEXT_SECRET" {
			foundSecret = true
		}
		if f.Code == "URL_CREDENTIALS" {
			foundURL = true
		}
	}
	if !foundSecret {
		t.Fatalf("disabled decision provider plaintext key must be flagged, got %+v", rep.Findings)
	}
	if !foundURL {
		t.Fatalf("disabled decision provider URL credentials must be flagged, got %+v", rep.Findings)
	}
	blob, _ := json.Marshal(rep)
	if strings.Contains(string(blob), canarySecret) || strings.Contains(string(blob), "passw0rd-canary") {
		t.Fatal("report must never contain decision provider secrets")
	}
}
