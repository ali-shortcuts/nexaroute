package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Contract under test (browser line 877 + Prompt 1):
//
//   - Replacing the primary credential with replace_key=true persists the
//     new primary and reports credential_configured=true with
//     secret_source = the PRIMARY source ("literal"), even when a
//     supplementary credential pool is preserved alongside it.
//   - An explicitly empty provider.credentials list ([]) on a primary
//     replacement is the UI placeholder for "pool untouched": the pool
//     survives. A nil (omitted/null) list clears the pool (whole-set
//     replacement semantics), and a non-empty list replaces it.
//   - An explicit clear (replace_key=true with no primary credential at
//     all) clears primary AND pool: nothing remains configured.
//   - Invalid or disabled credential indices are rejected, never
//     silently enabled or ignored.
//   - No secret value and no full email ever appears in a read, save,
//     or pool-metadata response.
//
// All canaries below are test-only strings; none is a real credential.
func TestProviderReplaceKeyWithPoolPreserved(t *testing.T) {
	const (
		oldPrimary = "POOLTEST_OLD_PRIMARY_111"
		newPrimary = "POOLTEST_NEW_PRIMARY_222"
		poolLit    = "POOLTEST_POOL_LITERAL_333"
		poolEmail  = "operator-pool@example.com"
	)

	newServer := func() *Server {
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Providers = []config.ProviderConfig{{
			ID: "pooltest", Name: "Pool Test", Type: "openai_compatible",
			BaseURL: "http://127.0.0.1:9/v1", APIKey: oldPrimary,
			APIKeyEnv: "POOLTEST_UNSET_ENV_XYZ", AuthMode: "bearer", Enabled: false,
			Models: []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1}},
			Credentials: []config.CredentialConfig{
				{Name: "literal-pool", APIKey: poolLit, Email: poolEmail, Enabled: true},
				{Name: "env-pool", APIKeyEnv: "POOLTEST_POOL_UNSET_XYZ", Enabled: true},
			},
		}}
		return testGateway(t, cfg)
	}
	serve := func(s *Server, method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, "http://localhost"+path, nil)
		} else {
			r = httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.RemoteAddr = "127.0.0.1:12345"
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		return rr
	}
	type readOut struct {
		Provider struct {
			APIKey      string `json:"api_key"`
			Credentials []struct {
				APIKey string `json:"api_key"`
			} `json:"credentials"`
		} `json:"provider"`
		CredentialConfigured bool   `json:"credential_configured"`
		SecretSource         string `json:"secret_source"`
		CredentialCount      int    `json:"credential_count"`
		HasSecret            bool   `json:"has_secret"`
		CredentialsMeta      []struct {
			Name        string `json:"name"`
			EmailMasked string `json:"email_masked"`
		} `json:"credentials_meta"`
	}
	read := func(s *Server) readOut {
		rr := serve(s, http.MethodGet, "/admin/api/providers/pooltest", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
		}
		for _, secret := range []string{oldPrimary, newPrimary, poolLit, poolEmail} {
			if strings.Contains(rr.Body.String(), secret) {
				t.Fatalf("GET leaked secret value: body contains %q", secret)
			}
		}
		var got readOut
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET decode: %v", err)
		}
		if got.Provider.APIKey != "" {
			t.Fatalf("GET provider.api_key must be redacted, got %q", got.Provider.APIKey)
		}
		for _, c := range got.Provider.Credentials {
			if c.APIKey != "" {
				t.Fatalf("GET pool api_key must be redacted, got %q", c.APIKey)
			}
		}
		return got
	}
	models := `"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}]`

	t.Run("replace primary with empty placeholder preserves pool and reports literal", func(t *testing.T) {
		s := newServer()
		// Exactly what app.js sends on a #p-key replacement: new primary,
		// credentials=[], replace_key=true, preserve_secret=false.
		put := `{"provider":{"id":"pooltest","name":"Pool Test","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","api_key":"` + newPrimary + `","api_key_env":"","enabled":false,` + models + `,"credentials":[]},"preserve_secret":false,"preserve_headers":true,"preserve_proxy":true,"replace_key":true}`
		rr := serve(s, http.MethodPut, "/admin/api/providers/pooltest", put)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT status=%d body=%s", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), newPrimary) || strings.Contains(rr.Body.String(), oldPrimary) || strings.Contains(rr.Body.String(), poolLit) {
			t.Fatalf("PUT leaked a secret: %s", rr.Body.String())
		}
		live := s.currentConfig().Providers[0]
		if live.APIKey != newPrimary {
			t.Fatalf("replacement did not persist: primary changed=%v", live.APIKey != oldPrimary)
		}
		if len(live.Credentials) != 2 || live.Credentials[0].Name != "literal-pool" || live.Credentials[1].Name != "env-pool" {
			t.Fatalf("empty placeholder must preserve the pool, got %d entries", len(live.Credentials))
		}
		if live.Credentials[0].APIKey != poolLit {
			t.Fatalf("pool entry secret must survive a primary replacement")
		}
		got := read(s)
		if !got.CredentialConfigured || got.SecretSource != "literal" {
			t.Fatalf("browser contract: configured=%v source=%q, want true/literal", got.CredentialConfigured, got.SecretSource)
		}
		if got.CredentialCount != 2 {
			t.Fatalf("credential_count=%d, want 2 (primary + pool entry, env entry unresolved)", got.CredentialCount)
		}
		if len(got.CredentialsMeta) != 2 || got.CredentialsMeta[0].EmailMasked == poolEmail || !strings.Contains(got.CredentialsMeta[0].EmailMasked, "example.com") {
			t.Fatalf("credentials_meta must mask the email, got %+v", got.CredentialsMeta)
		}
	})

	t.Run("explicit clear clears primary and pool", func(t *testing.T) {
		s := newServer()
		put := `{"provider":{"id":"pooltest","name":"Pool Test","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","api_key":"","api_key_env":"","enabled":false,` + models + `,"credentials":[]},"preserve_secret":false,"preserve_headers":true,"preserve_proxy":true,"replace_key":true}`
		rr := serve(s, http.MethodPut, "/admin/api/providers/pooltest", put)
		if rr.Code != http.StatusOK {
			t.Fatalf("clear PUT status=%d body=%s", rr.Code, rr.Body.String())
		}
		live := s.currentConfig().Providers[0]
		if live.APIKey != "" || len(live.Credentials) != 0 {
			t.Fatalf("clear must leave no credential: primary set=%v pool=%d", live.APIKey != "", len(live.Credentials))
		}
		got := read(s)
		if got.CredentialConfigured || got.HasSecret || got.SecretSource != "none" {
			t.Fatalf("clear read: configured=%v has_secret=%v source=%q, want false/false/none", got.CredentialConfigured, got.HasSecret, got.SecretSource)
		}
	})

	t.Run("nil credentials on replace clears pool", func(t *testing.T) {
		s := newServer()
		// credentials field omitted entirely (nil after decode).
		put := `{"provider":{"id":"pooltest","name":"Pool Test","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","api_key":"` + newPrimary + `","api_key_env":"","enabled":false,` + models + `},"preserve_secret":false,"preserve_headers":true,"preserve_proxy":true,"replace_key":true}`
		rr := serve(s, http.MethodPut, "/admin/api/providers/pooltest", put)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT status=%d body=%s", rr.Code, rr.Body.String())
		}
		if live := s.currentConfig().Providers[0]; len(live.Credentials) != 0 {
			t.Fatalf("nil credentials must clear the pool, got %d entries", len(live.Credentials))
		}
	})

	t.Run("pool update rejects invalid index", func(t *testing.T) {
		s := newServer()
		put := `{"provider":{"id":"pooltest","name":"Pool Test","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":false,` + models + `},"preserve_secret":false,"replace_key":false,"credential_pool":{"replace":true,"entries":[{"index":7,"name":"bogus","api_key":"POOLTEST_BOGUS_444"}]}}`
		rr := serve(s, http.MethodPut, "/admin/api/providers/pooltest", put)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid pool index must be 400, got %d body=%s", rr.Code, rr.Body.String())
		}
		if live := s.currentConfig().Providers[0]; len(live.Credentials) != 2 {
			t.Fatalf("rejected pool update must not mutate the pool, got %d entries", len(live.Credentials))
		}
	})

	t.Run("single credential selection rejects out of range and disabled", func(t *testing.T) {
		p := config.ProviderConfig{Credentials: []config.CredentialConfig{
			{Name: "a", APIKey: "POOLTEST_A_555", Enabled: true},
			{Name: "b", APIKey: "POOLTEST_B_666", Enabled: false},
		}}
		if _, _, err := singleCredentialProvider(p, intPtr(2)); err == nil {
			t.Fatal("out-of-range credential index must be rejected")
		}
		if _, _, err := singleCredentialProvider(p, intPtr(1)); err == nil {
			t.Fatal("disabled credential index must be rejected, never silently enabled")
		}
		if _, name, err := singleCredentialProvider(p, intPtr(0)); err != nil || name != "a" {
			t.Fatalf("enabled index 0 must select entry a, got name=%q err=%v", name, err)
		}
	})
}
