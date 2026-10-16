package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

const (
	revealCanary     = "REVEAL_CANARY_literal_0123456789_abcd"
	revealPoolCanary = "POOL_CANARY_B_bbbbbbbbbbbbbbbbbbbb"
)

func revealTestConfig() config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKey: revealCanary, AuthMode: "bearer", Enabled: true,
		Credentials: []config.CredentialConfig{
			{Name: "a", APIKey: "POOL_CANARY_A_aaaaaaaaaaaaaaaaaaaa", Enabled: true},
			{Name: "b", APIKey: revealPoolCanary, Enabled: true},
		},
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	return cfg
}

func revealDecode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, rr.Body.String())
	}
	return out
}

func TestRevealSecretReturnsKeyAndGetStaysMasked(t *testing.T) {
	s := testGateway(t, revealTestConfig())

	rr := adminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), revealCanary) || strings.Contains(rr.Body.String(), revealPoolCanary) {
		t.Fatalf("GET exposed a full key: %s", rr.Body.String())
	}
	got := revealDecode(t, rr)
	if got["api_key_preview"] != maskedPlaceholder {
		t.Fatalf("api_key_preview = %v, want an opaque mask", got["api_key_preview"])
	}
	if got["reveal_allowed"] != true {
		t.Fatalf("reveal_allowed = %v, want true on a loopback listener", got["reveal_allowed"])
	}

	rr = adminRequest(s, http.MethodPost, "/admin/api/providers/p/reveal-secret", `{}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("reveal status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal response must be no-store, got %q", rr.Header().Get("Cache-Control"))
	}
	body := revealDecode(t, rr)
	if body["secret"] != revealCanary || body["source"] != "literal" {
		t.Fatalf("unexpected reveal body: %v", body)
	}

	audit := adminRequest(s, http.MethodGet, "/admin/api/audit?limit=100", "")
	if audit.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", audit.Code, audit.Body.String())
	}
	if !strings.Contains(audit.Body.String(), "provider.secret.revealed") {
		t.Fatalf("reveal was not audited: %s", audit.Body.String())
	}
	if strings.Contains(audit.Body.String(), revealCanary) {
		t.Fatalf("audit trail leaked the secret: %s", audit.Body.String())
	}
}

func TestRevealSecretPoolEntriesByIndex(t *testing.T) {
	s := testGateway(t, revealTestConfig())

	rr := adminRequest(s, http.MethodPost, "/admin/api/providers/p/reveal-secret", `{"credential_index":1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := revealDecode(t, rr)
	if body["secret"] != revealPoolCanary {
		t.Fatalf("pool index 1 returned %v", body["secret"])
	}
	for _, idx := range []string{"2", "-1", "99"} {
		rr = adminRequest(s, http.MethodPost, "/admin/api/providers/p/reveal-secret", `{"credential_index":`+idx+`}`)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("index %s status=%d, want 404; body=%s", idx, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), revealPoolCanary) {
			t.Fatalf("error response leaked a key: %s", rr.Body.String())
		}
	}

	get := adminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	meta, _ := revealDecode(t, get)["credentials_meta"].([]any)
	if len(meta) != 2 {
		t.Fatalf("credentials_meta has %d entries, want 2: %s", len(meta), get.Body.String())
	}
	second, _ := meta[1].(map[string]any)
	if second["preview"] != maskedPlaceholder || second["source"] != "literal" {
		t.Fatalf("unexpected pool metadata: %v", second)
	}
}

func TestRevealSecretNeverRevealsEnvSourcedKeys(t *testing.T) {
	const envName = "NR_REVEAL_TEST_ENV_KEY"
	const envValue = "ENV_CANARY_should_never_appear_9f3c"
	t.Setenv(envName, envValue)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "e", Name: "E", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKeyEnv: envName, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)

	rr := adminRequest(s, http.MethodPost, "/admin/api/providers/e/reveal-secret", `{}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), envValue) {
		t.Fatalf("env-sourced value leaked: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), envName) {
		t.Fatalf("response should name the env variable: %s", rr.Body.String())
	}
}

func TestRevealSecretDisabledByConfig(t *testing.T) {
	cfg := revealTestConfig()
	off := false
	cfg.Admin.AllowSecretReveal = &off
	s := testGateway(t, cfg)

	rr := adminRequest(s, http.MethodPost, "/admin/api/providers/p/reveal-secret", `{}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), revealCanary) {
		t.Fatalf("disabled reveal leaked the key: %s", rr.Body.String())
	}
	get := adminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	if revealDecode(t, get)["reveal_allowed"] != false {
		t.Fatalf("reveal_allowed should be false: %s", get.Body.String())
	}
}

func TestRevealSecretRateLimited(t *testing.T) {
	s := testGateway(t, revealTestConfig())
	for i := 0; i < revealRateMax; i++ {
		rr := adminRequest(s, http.MethodPost, "/admin/api/providers/p/reveal-secret", `{}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := adminRequest(s, http.MethodPost, "/admin/api/providers/p/reveal-secret", `{}`)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if strings.Contains(rr.Body.String(), revealCanary) {
		t.Fatalf("rate-limited response leaked the key: %s", rr.Body.String())
	}
}

func TestRevealSecretUnknownProviderAndWrongMethod(t *testing.T) {
	s := testGateway(t, revealTestConfig())
	if rr := adminRequest(s, http.MethodPost, "/admin/api/providers/nope/reveal-secret", `{}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown provider status=%d, want 404", rr.Code)
	}
	rr := adminRequest(s, http.MethodGet, "/admin/api/providers/p/reveal-secret", "")
	if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), revealCanary) {
		t.Fatalf("GET on reveal path must be denied, status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRevealSecretPermissionIsOwnerAdminOnly(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers/p/reveal-secret", nil)
	permission, mapped := adminPermissionForRequest(req)
	if !mapped || permission != authz.ManageKeys {
		t.Fatalf("permission=%q mapped=%v, want %q", permission, mapped, authz.ManageKeys)
	}
	for _, role := range []authz.Role{authz.RoleOperator, authz.RoleDeveloper, authz.RoleViewer} {
		identity := authz.Identity{Subject: "u", Roles: []authz.Role{role}}
		if identity.Authorize(permission) {
			t.Fatalf("role %s must not be able to reveal secrets", role)
		}
	}
	for _, role := range []authz.Role{authz.RoleOwner, authz.RoleAdmin} {
		identity := authz.Identity{Subject: "u", Roles: []authz.Role{role}}
		if !identity.Authorize(permission) {
			t.Fatalf("role %s should be able to reveal secrets", role)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := httptest.NewRequest(method, "/admin/api/providers/p/reveal-secret", nil)
		if _, mapped := adminPermissionForRequest(r); mapped {
			t.Fatalf("%s on the reveal path must be unmapped (denied)", method)
		}
	}
}

func TestSecretPreviewNeverExposesMostOfAShortKey(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"short":                             maskedPlaceholder,
		"elevenchars":                       maskedPlaceholder,
		"twelve_chars":                      maskedPlaceholder,
		"sk-abcdefghijklmnopqrstuvwxyz1234": maskedPlaceholder,
	}
	for in, want := range cases {
		if got := secretPreview(in); got != want {
			t.Errorf("secretPreview(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRevealablePoolOnlyProviderRequiresIndex(t *testing.T) {
	p := config.ProviderConfig{Credentials: []config.CredentialConfig{{Name: "a", APIKey: "k", Enabled: true}}}
	if _, _, status, _ := revealable(p, nil); status != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 when a pool-only provider is revealed without an index", status)
	}
	zero := 0
	if secret, _, status, _ := revealable(p, &zero); status != 0 || secret != "k" {
		t.Fatalf("index 0 -> secret=%q status=%d", secret, status)
	}
}

func TestRevealLimiterWindowResets(t *testing.T) {
	var l revealLimiter
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < revealRateMax; i++ {
		if !l.allow("alice", t0) {
			t.Fatalf("call %d should be allowed", i+1)
		}
	}
	if l.allow("alice", t0.Add(time.Second)) {
		t.Fatal("call over the limit must be rejected")
	}
	if !l.allow("bob", t0.Add(time.Second)) {
		t.Fatal("limits are per subject")
	}
	if !l.allow("alice", t0.Add(revealRateWindow+time.Second)) {
		t.Fatal("window should reset after it elapses")
	}
}
