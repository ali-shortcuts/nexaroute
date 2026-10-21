package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
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
		TenantID:      "acme",
		ProjectID:     "platform",
		TeamID:        "engineering",
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
			ID:            "platform",
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

func TestAdminClientKeyRevokeContractAndIdempotency(t *testing.T) {
	const keyHash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	key := config.VirtualKeyConfig{
		ID: "vk_revoke", Name: "ci", KeyHash: keyHash, TenantID: "acme", ProjectID: "platform",
		TeamID: "engineering", Role: "developer", AllowedModels: []string{"team-model"},
		AllowedRoutes: []string{"/v1/chat/completions"}, ExpiresAt: "2030-01-01T00:00:00Z", RPM: 10, TPM: 20,
	}
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-secret"
	cfg.Admin.BindLocalOnly = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{key}
	s := testGateway(t, cfg)

	remove := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "http://gateway/admin/api/client-keys/"+id, nil)
		req.Header.Set("X-Admin-Key", "admin-secret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	for attempt := 0; attempt < 2; attempt++ {
		rr := remove(key.ID)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("revoke attempt %d status=%d body=%s", attempt+1, rr.Code, rr.Body.String())
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("revoke attempt %d returned a body: %q", attempt+1, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), keyHash) {
			t.Fatal("revoke response leaked the key hash")
		}
		got := s.currentConfig().ClientAuth.VirtualKeys
		if len(got) != 1 {
			t.Fatalf("revoke attempt %d changed key count: %d", attempt+1, len(got))
		}
		want := key
		want.Revoked = true
		if !reflect.DeepEqual(got[0], want) {
			t.Fatalf("revoke attempt %d changed metadata:\n got %+v\nwant %+v", attempt+1, got[0], want)
		}
	}

	missing := remove("vk_missing")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown key status=%d body=%s", missing.Code, missing.Body.String())
	}
	if !strings.Contains(missing.Body.String(), `client key \"vk_missing\" not found`) {
		t.Fatalf("unknown key body=%s", missing.Body.String())
	}
}

func TestAdminClientKeyRevokeRequiresManageKeysRole(t *testing.T) {
	const keyHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	path := "/admin/api/client-keys/vk_role"
	for _, tc := range []struct {
		role authz.Role
		want int
	}{
		{authz.RoleAdmin, http.StatusNoContent},
		{authz.RoleOperator, http.StatusForbidden},
		{authz.RoleViewer, http.StatusForbidden},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			cfg := config.Default()
			cfg.Probe.Enabled = false
			cfg.ClientAuth.Enabled = true
			cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
				ID: "vk_role", KeyHash: keyHash, TenantID: "acme", ProjectID: "platform", TeamID: "engineering",
				Role: "developer", AllowedModels: []string{"m"}, AllowedRoutes: []string{"/v1/chat/completions"},
			}}
			srv := testGateway(t, cfg)
			session := installTestOIDCSession(t, srv, tc.role)

			req := httptest.NewRequest(http.MethodDelete, "http://gateway"+path, nil)
			req.RemoteAddr = "127.0.0.1:10101"
			req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: session})
			const csrf = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			req.Header.Set("Origin", "http://gateway")
			req.Header.Set("X-NexaRoute-CSRF", csrf)
			req.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: csrf})
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != tc.want {
				t.Fatalf("status=%d body=%s want %d", rr.Code, rr.Body.String(), tc.want)
			}
			revoked := srv.currentConfig().ClientAuth.VirtualKeys[0].Revoked
			if revoked != (tc.want == http.StatusNoContent) {
				t.Fatalf("revoked=%t after status %d", revoked, rr.Code)
			}

			events, err := srv.securityDB.AuditRecords(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == http.StatusNoContent {
				authorized, completed := -1, -1
				for i, event := range events {
					if event.Target != path {
						continue
					}
					switch event.Action {
					case "admin.operation.authorized":
						authorized = i
					case "admin.operation.completed":
						completed = i
						if event.Outcome != "success" || event.Status != http.StatusNoContent {
							t.Fatalf("completion event=%+v", event)
						}
					}
				}
				if authorized == -1 || completed == -1 {
					t.Fatalf("missing audit pair: %+v", events)
				}
				if completed > authorized {
					t.Fatalf("completion must be recorded after authorization: %+v", events)
				}
			} else {
				foundDenied := false
				for _, event := range events {
					if event.Target == path && event.Action == "admin.access.denied" && event.Status == http.StatusForbidden {
						foundDenied = true
					}
					if event.Target == path && (event.Action == "admin.operation.authorized" || event.Action == "admin.operation.completed") {
						t.Fatalf("denied role produced a mutation audit event: %+v", event)
					}
				}
				if !foundDenied {
					t.Fatalf("missing denied audit event: %+v", events)
				}
			}
		})
	}
}

func TestAdminClientKeyRevokePersistenceFailureLeavesKeyActive(t *testing.T) {
	const keyHash = "1111111111111111111111111111111111111111111111111111111111111111"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-secret"
	cfg.Admin.BindLocalOnly = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{ID: "vk_persist", KeyHash: keyHash, Role: "developer"}}
	s := testGateway(t, cfg)
	readOnly := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	s.configPath = filepath.Join(readOnly, "config.json")

	req := httptest.NewRequest(http.MethodDelete, "http://gateway/admin/api/client-keys/vk_persist", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("persistence failure status=%d body=%s, want existing 404 mapping", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), keyHash) {
		t.Fatal("persistence failure response leaked the key hash")
	}
	keys := s.currentConfig().ClientAuth.VirtualKeys
	if len(keys) != 1 || keys[0].Revoked {
		t.Fatalf("persistence failure changed runtime config: %+v", keys)
	}
}
