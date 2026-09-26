package httpapi

import (
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
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
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
