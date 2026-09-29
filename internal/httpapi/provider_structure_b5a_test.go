package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// B5a: additive provider structure output exposes header NAMES only and
// proxy scheme+host only. Values, credentials, query strings and auth
// tokens must never leave the backend. Base URL and model list stay
// visible/editable. Write-only secret semantics and backwards
// compatibility are preserved. The test scans the COMPLETE response
// body for every sentinel secret value.
func TestB5aProviderStructureOnlyNoSecretValues(t *testing.T) {
	const apiCanary = "B5A_API_SENTINEL_7f2e9a"
	const poolCanary = "B5A_POOL_SENTINEL_3b8c1d"
	const headerCanary = "B5A_HEADER_VALUE_SENTINEL_5e4f6a"
	const proxyPassCanary = "B5A_PROXY_PASS_SENTINEL_9c1d2e"
	const proxyUserCanary = "B5A_PROXY_USER_4a7b3c"
	const queryCanary = "B5A_QUERY_SENTINEL_8d2e5f"
	const tokenCanary = "B5A_AUTH_TOKEN_SENTINEL_1a9b4c"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "b5a", Name: "B5a", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9/v1",
		APIKey:  apiCanary, AuthMode: "bearer", Enabled: true,
		Headers: map[string]string{
			"X-Custom-Auth": headerCanary,
			"Authorization": "Bearer " + tokenCanary,
			"X-Tenant":      "plain-tenant",
		},
		ProxyURL: "http:// 브랜드proxyuser:" + proxyPassCanary + "@127.0.0.1:9998/some/path?" + queryCanary + "=1#frag",
		Credentials: []config.CredentialConfig{
			{Name: "extra", APIKey: poolCanary, Enabled: true},
		},
		Models: []config.ModelConfig{
			{ID: "m1", Model: "model-alpha", Enabled: true, Weight: 1},
			{ID: "m2", Model: "model-beta", Enabled: true, Weight: 1},
		},
	}}
	// Fix proxy user to plain ASCII sentinel (non-ASCII above is intentional
	// to also prove userinfo never leaks even with odd bytes; reset to sentinel).
	cfg.Providers[0].ProxyURL = "http://" + proxyUserCanary + ":" + proxyPassCanary + "@127.0.0.1:9998/some/path?" + queryCanary + "=1#frag"
	s := testGateway(t, cfg)

	serve := func(method, path, body string) *httptest.ResponseRecorder {
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

	rr := serve(http.MethodGet, "/admin/api/providers/b5a", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
	}
	raw := rr.Body.String()

	// 1. Backwards-compatible envelope keys preserved.
	var envelope map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, k := range []string{"provider", "secret_source", "has_secret"} {
		if _, ok := envelope[k]; !ok {
			t.Fatalf("backwards-compat key %q missing in %v", k, envelope)
		}
	}
	// 2. Additive structure keys present.
	for _, k := range []string{"header_names", "header_count", "has_headers", "has_proxy", "proxy_scheme", "proxy_host", "proxy_display", "credential_count"} {
		if _, ok := envelope[k]; !ok {
			t.Fatalf("B5a additive key %q missing in %v", k, envelope)
		}
	}

	// 3. Base URL and model list remain visible/editable.
	var got struct {
		Provider struct {
			BaseURL string `json:"base_url"`
			Models  []struct {
				Model string `json:"model"`
			} `json:"models"`
		} `json:"provider"`
		HeaderNames  []string `json:"header_names"`
		ProxyScheme  string   `json:"proxy_scheme"`
		ProxyHost    string   `json:"proxy_host"`
		ProxyDisplay string   `json:"proxy_display"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("base_url must stay visible: %q", got.Provider.BaseURL)
	}
	if len(got.Provider.Models) != 2 {
		t.Fatalf("model list must stay visible: %+v", got.Provider.Models)
	}
	// Header NAMES only.
	foundCustom, foundTenant, foundAuth := false, false, false
	for _, n := range got.HeaderNames {
		if n == "X-Custom-Auth" {
			foundCustom = true
		}
		if n == "X-Tenant" {
			foundTenant = true
		}
		if n == "Authorization" {
			foundAuth = true
		}
	}
	if !foundCustom || !foundTenant || !foundAuth {
		t.Fatalf("header NAMES missing: %v", got.HeaderNames)
	}
	// Proxy scheme+host only.
	if got.ProxyScheme != "http" || got.ProxyHost != "127.0.0.1:9998" || got.ProxyDisplay != "http://127.0.0.1:9998" {
		t.Fatalf("proxy structure wrong: scheme=%q host=%q display=%q", got.ProxyScheme, got.ProxyHost, got.ProxyDisplay)
	}

	// 4. Scan the COMPLETE response for every sentinel: none may appear.
	sentinels := []string{apiCanary, poolCanary, headerCanary, proxyPassCanary, proxyUserCanary, queryCanary, tokenCanary}
	for _, path := range []string{
		"/admin/api/providers/b5a",
		"/admin/api/providers/b5a?reveal=1",
		"/admin/api/providers",
	} {
		r := serve(http.MethodGet, path, "")
		if r.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, r.Code, r.Body.String())
		}
		body := r.Body.String()
		for _, snt := range sentinels {
			if strings.Contains(body, snt) {
				t.Fatalf("GET %s leaked sentinel %q in: %s", path, snt, body)
			}
		}
		// Structured secrets must not appear anywhere either.
		for _, leak := range []string{"/some/path", "#frag"} {
			if strings.Contains(body, leak) {
				t.Fatalf("GET %s leaked proxy structure %q in: %s", path, leak, body)
			}
		}
	}
	// Full single-provider body re-scan (covers credential JSON shapes).
	for _, snt := range sentinels {
		if strings.Contains(raw, snt) {
			t.Fatalf("single-provider body leaked sentinel %q: %s", snt, raw)
		}
	}

	// 5. Write-only semantics: PUT with preserve flags keeps secrets server-side
	// and never echoes them.
	put := `{"provider":{"id":"b5a","name":"B5a v2","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-alpha","enabled":true,"weight":1,"capabilities":{}}]},"preserve_secret":true,"preserve_headers":true,"preserve_proxy":true}`
	rr2 := serve(http.MethodPut, "/admin/api/providers/b5a", put)
	if rr2.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	for _, snt := range sentinels {
		if strings.Contains(rr2.Body.String(), snt) {
			t.Fatalf("PUT response leaked sentinel %q: %s", snt, rr2.Body.String())
		}
	}
	live := s.currentConfig().Providers[0]
	if live.APIKey != apiCanary {
		t.Fatal("preserve_secret did not keep API key")
	}
	if live.Headers["X-Custom-Auth"] != headerCanary {
		t.Fatal("preserve_headers did not keep header values")
	}
	if !strings.Contains(live.ProxyURL, proxyPassCanary) {
		t.Fatal("preserve_proxy did not keep proxy URL")
	}
}

func TestB5aProxyStructureStripsSecrets(t *testing.T) {
	scheme, host, display := proxyStructure("http://user:s3cr3t@proxy.example:8080/some/path?q=1#frag")
	if scheme != "http" || host != "proxy.example:8080" || display != "http://proxy.example:8080" {
		t.Fatalf("got %q %q %q", scheme, host, display)
	}
	for _, leak := range []string{"s3cr3t", "user", "/some/path", "q=1", "frag"} {
		if strings.Contains(display, leak) || strings.Contains(host, leak) {
			t.Fatalf("proxy structure leaked %q in %q/%q", leak, host, display)
		}
	}
	if s, h, d := proxyStructure(""); s != "" || h != "" || d != "" {
		t.Fatalf("empty proxy should yield empty structure, got %q %q %q", s, h, d)
	}
	if s, h, d := proxyStructure("not-a-url"); s != "" || h != "" || d != "" {
		t.Fatalf("invalid proxy should yield empty structure, got %q %q %q", s, h, d)
	}
}

func TestB5aHeaderNamesNeverIncludeValues(t *testing.T) {
	names := providerHeaderNames(config.ProviderConfig{Headers: map[string]string{
		"X-Custom":      "SECRET_VALUE_XYZ",
		"X-Api-Key":     "TOKEN_ABC",
		"Authorization": "Bearer TOPSECRET",
	}})
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %v", names)
	}
	joined := strings.Join(names, ",")
	for _, leak := range []string{"SECRET_VALUE_XYZ", "TOKEN_ABC", "TOPSECRET"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("header names leaked value %q in %q", leak, joined)
		}
	}
	if got := providerHeaderNames(config.ProviderConfig{}); len(got) != 0 {
		t.Fatalf("empty headers should yield empty names, got %v", got)
	}
}
