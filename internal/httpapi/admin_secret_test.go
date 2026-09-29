package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func adminRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:23456"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestProviderSecretsPersistButAreNeverReturnedByAdminSurfaces(t *testing.T) {
	const primary = "SECRET_PROVIDER_CANARY_PRIMARY_8fb3"
	const secondary = "SECRET_PROVIDER_CANARY_POOL_61ca"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKey: primary, AuthMode: "bearer", Enabled: true,
		Credentials: []config.CredentialConfig{{Name: "fallback", APIKey: secondary, Enabled: true}},
		Models:      []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)

	// Saving edits must preserve existing secret values server-side without
	// echoing them in the mutation response.
	put := `{"provider":{"id":"p","name":"P edited","type":"openai_compatible","base_url":"http://127.0.0.1:9","auth_mode":"bearer","enabled":true,"models":[{"id":"m","model":"m","enabled":true,"weight":1,"capabilities":{} }]},"preserve_secret":true}`
	if rr := adminRequest(s, http.MethodPut, "/admin/api/providers/p", put); rr.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rr.Code, rr.Body.String())
	} else if strings.Contains(rr.Body.String(), primary) || strings.Contains(rr.Body.String(), secondary) {
		t.Fatalf("save response exposed credentials: %s", rr.Body.String())
	}

	for _, path := range []string{
		"/admin/api/providers/p",
		"/admin/api/providers/p?reveal=1",
		"/admin/api/providers",
		"/admin/api/snapshot",
		"/metrics",
	} {
		rr := adminRequest(s, http.MethodGet, path, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), primary) || strings.Contains(rr.Body.String(), secondary) {
			t.Fatalf("%s exposed provider key canary: %s", path, rr.Body.String())
		}
	}

	cfgOnDisk, err := os.ReadFile(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfgOnDisk), primary) || !strings.Contains(string(cfgOnDisk), secondary) {
		t.Fatal("saved configuration did not preserve provider credentials")
	}
	info, err := os.Stat(s.configPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config file should be mode 0600: mode=%v err=%v", info, err)
	}
}

// TestProviderKeyAndBaseURLSurviveSaveReopen is the regression test for the
// "fields vanish after save/reopen" failure mode seen in gateway dashboards
// (cf. docs/COMPETITOR_COMPARISON addendum: claude-code-router audit). It
// simulates a full UI round-trip — reopen (GET, secrets write-only), save
// (PUT with preserve flags, as the embedded editor sends), reopen — and then
// proves functionally that the key still authenticates upstream and the base
// URL still routes there.
func TestProviderKeyAndBaseURLSurviveSaveReopen(t *testing.T) {
	const key = "ROUNDTRIP_KEY_CANARY_4d2a"
	var sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		APIKey: key, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)

	chat := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("chat status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	chat()
	if sawAuth != "Bearer "+key {
		t.Fatalf("upstream auth before round-trip=%q", sawAuth)
	}

	// Reopen: GET must return base_url but never the key.
	rr := adminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("reopen status=%d body=%s", rr.Code, rr.Body.String())
	}
	var opened struct {
		Provider   map[string]any `json:"provider"`
		HasSecret  bool           `json:"has_secret"`
		SecretSrc  string         `json:"secret_source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	if opened.Provider["base_url"] != up.URL {
		t.Fatalf("reopen lost base_url: %v", opened.Provider["base_url"])
	}
	if k, _ := opened.Provider["api_key"].(string); k != "" {
		t.Fatal("reopen exposed the API key despite write-only secrets")
	}
	if !opened.HasSecret {
		t.Fatal("reopen reports no secret while one is configured")
	}

	// Save: send the reopened payload back exactly as the embedded editor
	// does, with preserve flags (see web/app.js provider save path).
	providerJSON, _ := json.Marshal(opened.Provider)
	save := `{"provider":` + string(providerJSON) + `,"preserve_secret":true,"preserve_headers":true,"preserve_proxy":true}`
	if rr := adminRequest(s, http.MethodPut, "/admin/api/providers/p", save); rr.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Reopen again: base_url must still be there, key still write-only.
	rr = adminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("second reopen status=%d body=%s", rr.Code, rr.Body.String())
	}
	var reopened struct {
		Provider  map[string]any `json:"provider"`
		HasSecret bool           `json:"has_secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &reopened); err != nil {
		t.Fatal(err)
	}
	if reopened.Provider["base_url"] != up.URL {
		t.Fatalf("base_url vanished after save/reopen: %v", reopened.Provider["base_url"])
	}
	if !reopened.HasSecret {
		t.Fatal("API key vanished after save/reopen")
	}

	// Functional proof: the data plane still authenticates with the key.
	sawAuth = ""
	chat()
	if sawAuth != "Bearer "+key {
		t.Fatalf("upstream auth after save/reopen=%q, key did not survive", sawAuth)
	}
}
