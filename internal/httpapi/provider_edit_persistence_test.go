package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestProviderEditPrefillsBaseURLAndModelsWithStructureOnly(t *testing.T) {
	const apiCanary = "EDIT_PERSIST_API_CANARY_9f31"
	const poolCanary = "EDIT_PERSIST_POOL_CANARY_4ab2"
	const headerCanary = "EDIT_PERSIST_HEADER_SECRET_c7d0"
	const proxyCanary = "EDIT_PERSIST_PROXY_SECRET_e5f6"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "editme", Name: "Edit Me", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9/v1",
		APIKey:  apiCanary, AuthMode: "bearer", Enabled: true,
		Headers:   map[string]string{"X-Custom-Auth": headerCanary, "X-Tenant": "plain-tenant"},
		ProxyURL:  "http://proxyuser:" + proxyCanary + "@127.0.0.1:9998",
		Credentials: []config.CredentialConfig{{Name: "extra", APIKey: poolCanary, Enabled: true}},
		Models: []config.ModelConfig{
			{ID: "m1", Model: "model-alpha", Enabled: true, Weight: 1},
			{ID: "m2", Model: "model-beta", Enabled: true, Weight: 1},
		},
	}}
	s := testGateway(t, cfg)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://localhost/admin/api/providers/editme", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET edit status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// 1. Base URL and model list must prefill on edit.
	var got struct {
		Provider struct {
			BaseURL string `json:"base_url"`
			Models  []struct {
				Model string `json:"model"`
			} `json:"models"`
		} `json:"provider"`
		HasSecret    bool     `json:"has_secret"`
		HasHeaders   bool     `json:"has_headers"`
		HeaderNames  []string `json:"header_names"`
		HasProxy     bool     `json:"has_proxy"`
		ProxyScheme  string   `json:"proxy_scheme"`
		ProxyHost    string   `json:"proxy_host"`
		ProxyDisplay string   `json:"proxy_display"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("base_url did not prefill: %q", got.Provider.BaseURL)
	}
	models := map[string]bool{}
	for _, m := range got.Provider.Models {
		models[m.Model] = true
	}
	if !models["model-alpha"] || !models["model-beta"] {
		t.Fatalf("model list did not prefill: %v", models)
	}
	// 2. Structure view present without secret values.
	if !got.HasSecret || !got.HasHeaders || !got.HasProxy {
		t.Fatalf("structure flags missing: has_secret=%v has_headers=%v has_proxy=%v", got.HasSecret, got.HasHeaders, got.HasProxy)
	}
	foundCustom, foundTenant := false, false
	for _, n := range got.HeaderNames {
		if n == "X-Custom-Auth" {
			foundCustom = true
		}
		if n == "X-Tenant" {
			foundTenant = true
		}
	}
	if !foundCustom || !foundTenant {
		t.Fatalf("header NAMES missing: %v", got.HeaderNames)
	}
	if got.ProxyScheme != "http" || got.ProxyHost != "127.0.0.1:9998" || got.ProxyDisplay != "http://127.0.0.1:9998" {
		t.Fatalf("proxy structure wrong: scheme=%q host=%q display=%q", got.ProxyScheme, got.ProxyHost, got.ProxyDisplay)
	}
	// 3. No secret value may ever appear in the read response.
	for _, secret := range []string{apiCanary, poolCanary, headerCanary, proxyCanary} {
		if strings.Contains(body, secret) {
			t.Fatalf("edit read leaked secret %q in %s", secret, body)
		}
	}
	if strings.Contains(body, "proxyuser") {
		t.Fatalf("edit read leaked proxy credentials: %s", body)
	}

	// 4. Save -> reopen with preserve flags keeps secrets server-side.
	put := `{"provider":{"id":"editme","name":"Edit Me v2","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-alpha","enabled":true,"weight":1,"capabilities":{}}]},"preserve_secret":true,"preserve_headers":true,"preserve_proxy":true}`
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPut, "http://localhost/admin/api/providers/editme", strings.NewReader(put))
	req2.RemoteAddr = "127.0.0.1:12345"
	req2.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if strings.Contains(rr2.Body.String(), apiCanary) || strings.Contains(rr2.Body.String(), headerCanary) || strings.Contains(rr2.Body.String(), proxyCanary) {
		t.Fatalf("save response leaked secret: %s", rr2.Body.String())
	}
	live := s.currentConfig().Providers[0]
	if live.APIKey != apiCanary || live.Headers["X-Custom-Auth"] != headerCanary || !strings.Contains(live.ProxyURL, proxyCanary) {
		t.Fatal("untouched write-only fields were not preserved across save")
	}
}

func TestProxyStructureNeverExposesCredentials(t *testing.T) {
	scheme, host, display := proxyStructure("http://user:s3cr3t@proxy.example:8080/some/path?q=1")
	if scheme != "http" || host != "proxy.example:8080" || display != "http://proxy.example:8080" {
		t.Fatalf("got %q %q %q", scheme, host, display)
	}
	if strings.Contains(display, "s3cr3t") || strings.Contains(display, "user") {
		t.Fatalf("proxy display leaked credentials: %q", display)
	}
	if s, h, d := proxyStructure(""); s != "" || h != "" || d != "" {
		t.Fatalf("empty proxy should yield empty structure, got %q %q %q", s, h, d)
	}
}

func TestProviderEditFrontendContracts(t *testing.T) {
	appJS, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(appJS)
	for _, want := range []string{
		"headerNames",
		"proxyDisplay",
		"headersStructure",
		"proxyStructure",
		"replaceKeyBtn",
		"********",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing edit-persistence contract %q", want)
		}
	}
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	for _, want := range []string{
		`id="headersStructure"`,
		`id="proxyStructure"`,
		`id="replaceKeyBtn"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing edit-persistence contract %q", want)
		}
	}
}
