package privacyaudit

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Agent-D1 coverage: provider credential plaintext, URL parse/loopback
// branches, disabled-provider data-handling skip, poolSatisfaction
// unknown-pool and unknown-deployment branches, and the virtual-key
// unknown-pool continue.

func TestAgentD1AuditProviderCredentialPlaintext(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].Credentials = []config.CredentialConfig{{Name: "c1", APIKey: "plaintext-credential-key"}}
	rep := Audit(cfg)
	found := false
	for _, f := range rep.Findings {
		if f.Code == "PLAINTEXT_SECRET" && f.Severity == SeverityError && f.Scope == "provider:p1" {
			found = true
			if !strings.Contains(f.Message, "credentials[0]") {
				t.Fatalf("finding message = %q, want it to name credentials[0]", f.Message)
			}
		}
		if strings.Contains(f.Message, "plaintext-credential-key") {
			t.Fatal("finding must never contain the secret value")
		}
	}
	if !found {
		t.Fatalf("expected a PLAINTEXT_SECRET error for provider:p1 credentials[0], got %+v", rep.Findings)
	}
	if !rep.HasErrors() {
		t.Fatal("report should have errors")
	}
}

func TestAgentD1AuditEmptyBaseURLSkipsURLCheck(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].BaseURL = ""
	rep := Audit(cfg)
	for _, f := range rep.Findings {
		if f.Code == "URL_CREDENTIALS" || f.Code == "CLEARTEXT_ENDPOINT" {
			t.Fatalf("empty base_url should produce no URL findings, got %+v", rep.Findings)
		}
	}
}

func TestAgentD1AuditMalformedBaseURLSkipsURLCheck(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].BaseURL = "http://[::1"
	rep := Audit(cfg)
	for _, f := range rep.Findings {
		if f.Code == "URL_CREDENTIALS" || f.Code == "CLEARTEXT_ENDPOINT" {
			t.Fatalf("malformed base_url should produce no URL findings, got %+v", rep.Findings)
		}
	}
	// A host-less URL is likewise unparseable for audit purposes.
	cfg.Providers[0].BaseURL = "/just/a/path"
	rep = Audit(cfg)
	for _, f := range rep.Findings {
		if f.Code == "URL_CREDENTIALS" || f.Code == "CLEARTEXT_ENDPOINT" {
			t.Fatalf("host-less base_url should produce no URL findings, got %+v", rep.Findings)
		}
	}
}

func TestAgentD1AuditLocalhostBaseURLIsLoopback(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers[0].BaseURL = "http://localhost:8080/v1"
	rep := Audit(cfg)
	for _, f := range rep.Findings {
		if f.Code == "CLEARTEXT_ENDPOINT" {
			t.Fatalf("http://localhost must not be flagged as a cleartext endpoint, got %+v", rep.Findings)
		}
	}
	// A loopback IP is equally trusted.
	cfg.Providers[0].BaseURL = "http://127.0.0.1:8080/v1"
	rep = Audit(cfg)
	for _, f := range rep.Findings {
		if f.Code == "CLEARTEXT_ENDPOINT" {
			t.Fatalf("http://127.0.0.1 must not be flagged as a cleartext endpoint, got %+v", rep.Findings)
		}
	}
}

func TestAgentD1AuditDisabledProviderSkipsDataHandling(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID: "p2", Name: "P2", Type: "openai_compatible",
		BaseURL: "https://api2.example.com/v1",
		Enabled: false,
		// TrainsOnData deliberately undeclared: a disabled provider
		// must not be reported.
	})
	rep := Audit(cfg)
	for _, f := range rep.Findings {
		if f.Scope == "provider:p2" {
			t.Fatalf("disabled provider p2 should produce no findings, got %+v", rep.Findings)
		}
	}
	// And an enabled provider with undeclared handling IS reported.
	cfg.Providers[1].Enabled = true
	rep = Audit(cfg)
	if codesOf(rep)["DATA_HANDLING_UNDECLARED"] != SeverityInfo {
		t.Fatalf("enabled p2 with undeclared handling should report DATA_HANDLING_UNDECLARED, got %+v", rep.Findings)
	}
}

func TestAgentD1PoolSatisfactionUnknownPool(t *testing.T) {
	resolver := route.NewResolver(config.Config{}, nil)
	total, satisfying, known := poolSatisfaction(resolver, map[string]router.Deployment{}, "no-such-pool")
	if known {
		t.Fatal("unknown pool should report known=false")
	}
	if total != 0 || satisfying != 0 {
		t.Fatalf("unknown pool = (%d, %d), want (0, 0)", total, satisfying)
	}
}

func TestAgentD1PoolSatisfactionSkipsUnknownDeployment(t *testing.T) {
	cfg := config.Config{
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "p1", Mode: "explicit", Deployments: []string{"prov-a/m1"}},
		},
	}
	cfg.ApplyDefaults()
	all := []router.Deployment{{ID: "prov-a/m1", ProviderID: "prov-a", Model: "model-a", TrainsOnData: "no"}}
	resolver := route.NewResolver(cfg, all)
	// byID deliberately omits prov-a/m1 even though the pool expands
	// to it: the deployment must be skipped, not counted.
	total, satisfying, known := poolSatisfaction(resolver, map[string]router.Deployment{}, "p1")
	if !known {
		t.Fatal("known pool should report known=true")
	}
	if total != 0 || satisfying != 0 {
		t.Fatalf("pool with only unknown deployments = (%d, %d), want (0, 0)", total, satisfying)
	}
	// With the deployment present and satisfying no_training, it counts.
	byID := map[string]router.Deployment{"prov-a/m1": all[0]}
	total, satisfying, known = poolSatisfaction(resolver, byID, "p1")
	if !known || total != 1 || satisfying != 1 {
		t.Fatalf("poolSatisfaction = (%d, %d, %v), want (1, 1, true)", total, satisfying, known)
	}
}

func TestAgentD1VirtualKeySkipsUnknownPoolProfile(t *testing.T) {
	cfg := baseConfig()
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "rp1", CandidatePool: "ghost-pool"},
	}
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk1", RequirePrivacy: "no_training", AllowedRoutes: []string{"rp1"}},
	}
	rep := Audit(cfg)
	if codesOf(rep)["PROFILE_POOL_UNKNOWN"] != SeverityError {
		t.Fatalf("expected PROFILE_POOL_UNKNOWN for the ghost pool, got %+v", rep.Findings)
	}
	// The unknown-pool route contributes no deployments to the key's
	// union (the continue), so the key's verdict is the total==0
	// unsatisfiable case — not a partial verdict over real pools.
	found := false
	for _, f := range rep.Findings {
		if f.Scope == "virtual_key:vk1" {
			if f.Code != "NO_TRAINING_UNSATISFIABLE" || !strings.Contains(f.Message, "can reach no deployments") {
				t.Fatalf("virtual_key:vk1 finding = %+v, want NO_TRAINING_UNSATISFIABLE over zero deployments", f)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a no-training verdict for virtual_key:vk1, got %+v", rep.Findings)
	}
}
