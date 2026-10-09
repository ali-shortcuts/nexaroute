package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestVirtualKeyPolicyExpiryAndRevocation(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.ClientAuth = config.ClientAuthConfig{
		Enabled:     true,
		Tenants:     []config.TenantConfig{{ID: "acme", Projects: []config.ProjectConfig{{ID: "platform", AllowedModels: []string{"allowed"}}}}},
		VirtualKeys: []config.VirtualKeyConfig{{ID: "vk1", KeyHash: keyDigest("nrk_allowed"), TenantID: "acme", ProjectID: "platform", AllowedModels: []string{"allowed"}, Role: "developer"}},
	}
	cfg.Providers[0].Models[0].Aliases = []string{"allowed", "blocked"}
	s := testGateway(t, cfg)
	for _, tc := range []struct {
		name  string
		model string
		want  int
	}{
		{"allowed", "allowed", http.StatusOK},
		{"blocked", "blocked", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer nrk_allowed")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s status=%d body=%s want %d", tc.name, rr.Code, rr.Body.String(), tc.want)
		}
	}

	// Revoked keys stop working without deleting historical metadata.
	_, err := s.mutateConfig(func(c *config.Config) error { c.ClientAuth.VirtualKeys[0].Revoked = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"allowed","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "nrk_allowed")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status=%d want 401", rr.Code)
	}

	_, err = s.mutateConfig(func(c *config.Config) error {
		c.ClientAuth.VirtualKeys[0].Revoked = false
		c.ClientAuth.VirtualKeys[0].ExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired status=%d want 401", rr.Code)
	}
}

func TestAdminClientKeyCreateRotateListNeverReturnsHash(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-secret"
	cfg.Admin.BindLocalOnly = false
	s := testGateway(t, cfg)
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
		r.Header.Set("X-Admin-Key", "admin-secret")
		r.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		return rr
	}
	created := post("/admin/api/client-keys", `{"name":"ci","tenant_id":"acme","role":"developer","allowed_models":["m"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var first clientKeyResponse
	if err := json.Unmarshal(created.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Key == "" || !first.KeyShownOnce {
		t.Fatalf("create must return one-time key: %+v", first)
	}
	if first.ID == "" {
		t.Fatal("missing key id")
	}
	if strings.Contains(created.Body.String(), "key_hash") {
		t.Fatal("create response leaked key_hash")
	}

	get := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/client-keys", nil)
	get.Header.Set("X-Admin-Key", "admin-secret")
	gr := httptest.NewRecorder()
	s.Handler().ServeHTTP(gr, get)
	if gr.Code != http.StatusOK {
		t.Fatalf("list status=%d", gr.Code)
	}
	if strings.Contains(gr.Body.String(), first.Key) || strings.Contains(gr.Body.String(), "key_hash") {
		t.Fatalf("list leaked secret/hash: %s", gr.Body.String())
	}

	rotated := post("/admin/api/client-keys/"+first.ID+"/rotate", "{}")
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", rotated.Code, rotated.Body.String())
	}
	var second clientKeyResponse
	if err := json.Unmarshal(rotated.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Key == "" || second.Key == first.Key || second.ID == first.ID {
		t.Fatalf("rotation did not create a new one-time key: %+v", second)
	}
}


func TestVirtualKeyScopesIntersectInsteadOfWidening(t *testing.T) {
	cfg := config.Default()
	cfg.ClientAuth.Tenants = []config.TenantConfig{{
		ID: "acme",
		Projects: []config.ProjectConfig{{
			ID:            "platform",
			AllowedModels: []string{"safe", "team-model"},
			AllowedRoutes: []string{"/v1/chat/completions"},
			Teams: []config.TeamConfig{{
				ID:            "engineering",
				AllowedModels: []string{"team-model"},
				AllowedRoutes: []string{"/v1/chat/completions"},
			}},
		}},
	}}
	id := clientIdentity{ID: "vk1", TenantID: "acme", ProjectID: "platform", TeamID: "engineering", Role: "developer", Virtual: true}
	key := config.VirtualKeyConfig{
		TenantID:     "acme",
		ProjectID:    "platform",
		TeamID:       "engineering",
		AllowedModels: []string{"*"},
		AllowedRoutes: []string{"*"},
	}
	for _, tc := range []struct {
		name  string
		path  string
		model string
		want  bool
	}{
		{"allowed by every scope", "/v1/chat/completions", "team-model", true},
		{"project denies despite key wildcard", "/v1/chat/completions", "other-model", false},
		{"team denies despite key wildcard", "/v1/chat/completions", "safe", false},
		{"project route denies despite key wildcard", "/v1/messages", "team-model", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := identityPolicyAllows(cfg, id, key, tc.path, tc.model); got != tc.want {
				t.Fatalf("identityPolicyAllows(%q, %q) = %t, want %t", tc.path, tc.model, got, tc.want)
			}
		})
	}
}

func TestVirtualKeyProjectReferencesFailClosed(t *testing.T) {
	cfg := config.Default()
	cfg.ClientAuth.Tenants = []config.TenantConfig{{
		ID: "acme",
		Projects: []config.ProjectConfig{{
			ID: "platform",
			AllowedModels: []string{"safe"},
		}},
	}}
	base := clientIdentity{ID: "vk1", Role: "developer", Virtual: true}
	cases := []struct {
		name string
		key  config.VirtualKeyConfig
	}{
		{"unknown tenant", config.VirtualKeyConfig{TenantID: "missing", ProjectID: "platform", AllowedModels: []string{"*"}}},
		{"unknown project", config.VirtualKeyConfig{TenantID: "acme", ProjectID: "missing", AllowedModels: []string{"*"}}},
		{"team without project", config.VirtualKeyConfig{TenantID: "acme", TeamID: "engineering", AllowedModels: []string{"*"}}},
		{"unknown team", config.VirtualKeyConfig{TenantID: "acme", ProjectID: "platform", TeamID: "missing", AllowedModels: []string{"*"}}},
		{"project without tenant", config.VirtualKeyConfig{ProjectID: "platform", AllowedModels: []string{"*"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if identityPolicyAllows(cfg, base, tc.key, "/v1/chat/completions", "safe") {
				t.Fatal("misconfigured scope unexpectedly authorized request")
			}
		})
	}
}
