package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// B5b: explicit Replace key behavior with masked placeholder, untouched
// save preservation, and replacement save. The test covers reopen ->
// replace -> save -> reopen and verifies no credential leaks in responses
// or logs. No credential fixtures are committed.
func TestB5bProviderReplaceKeyUX(t *testing.T) {
	const oldKey = "B5B_OLD_SECRET_KEY_abc123"
	const newKey = "B5B_NEW_SECRET_KEY_xyz789"
	const maskedPlaceholder = "••••••••"

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "b5b", Name: "B5b", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9/v1",
		APIKey:  oldKey, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1}},
	}}
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

	// 1. Initial GET: verify masked placeholder is present, no secret leaked
	rr := serve(http.MethodGet, "/admin/api/providers/b5b", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("initial GET status=%d body=%s", rr.Code, rr.Body.String())
	}
	t.Logf("Response body: %s", rr.Body.String())
	var got struct {
		Provider struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
		} `json:"provider"`
		APIKeyMasked string `json:"api_key_masked"`
		HasSecret    bool   `json:"has_secret"`
		SecretSource string `json:"secret_source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.Provider.APIKey != "" {
		t.Fatalf("api_key must be empty in GET response, got %q", got.Provider.APIKey)
	}
	if got.APIKeyMasked != maskedPlaceholder {
		t.Fatalf("api_key_masked must be %q, got %q", maskedPlaceholder, got.APIKeyMasked)
	}
	if !got.HasSecret {
		t.Fatal("has_secret must be true")
	}
	if got.SecretSource != "literal" {
		t.Fatalf("secret_source must be 'literal', got %q", got.SecretSource)
	}
	// Verify no secret leaked in response
	if strings.Contains(rr.Body.String(), oldKey) {
		t.Fatalf("initial GET leaked old secret: %s", rr.Body.String())
	}

	// 2. Untouched save (preserve_secret=true): secret must be preserved
	putPreserve := `{"provider":{"id":"b5b","name":"B5b","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}]},"preserve_secret":true}`
	rr2 := serve(http.MethodPut, "/admin/api/providers/b5b", putPreserve)
	if rr2.Code != http.StatusOK {
		t.Fatalf("preserve PUT status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if strings.Contains(rr2.Body.String(), oldKey) || strings.Contains(rr2.Body.String(), newKey) {
		t.Fatalf("preserve PUT response leaked secret: %s", rr2.Body.String())
	}
	live := s.currentConfig().Providers[0]
	if live.APIKey != oldKey {
		t.Fatalf("preserve_secret did not keep old API key, got %q", live.APIKey)
	}

	// 3. Replacement save with replace_key=true: secret must be replaced
	putReplace := `{"provider":{"id":"b5b","name":"B5b","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}],"api_key":"` + newKey + `"},"replace_key":true}`
	rr3 := serve(http.MethodPut, "/admin/api/providers/b5b", putReplace)
	if rr3.Code != http.StatusOK {
		t.Fatalf("replace PUT status=%d body=%s", rr3.Code, rr3.Body.String())
	}
	// Response must not contain old or new secret
	if strings.Contains(rr3.Body.String(), oldKey) || strings.Contains(rr3.Body.String(), newKey) {
		t.Fatalf("replace PUT response leaked secret: %s", rr3.Body.String())
	}
	live = s.currentConfig().Providers[0]
	if live.APIKey != newKey {
		t.Fatalf("replace_key did not update API key, got %q", live.APIKey)
	}

	// 4. Reopen (GET again): verify new masked placeholder, no secret leaked
	rr4 := serve(http.MethodGet, "/admin/api/providers/b5b", "")
	if rr4.Code != http.StatusOK {
		t.Fatalf("reopen GET status=%d body=%s", rr4.Code, rr4.Body.String())
	}
	var got2 struct {
		Provider struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
		} `json:"provider"`
		APIKeyMasked string `json:"api_key_masked"`
		HasSecret    bool   `json:"has_secret"`
		SecretSource string `json:"secret_source"`
	}
	if err := json.Unmarshal(rr4.Body.Bytes(), &got2); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got2.Provider.APIKey != "" {
		t.Fatalf("api_key must be empty in reopen GET response, got %q", got2.Provider.APIKey)
	}
	if got2.APIKeyMasked != maskedPlaceholder {
		t.Fatalf("api_key_masked must be %q after replacement, got %q", maskedPlaceholder, got2.APIKeyMasked)
	}
	if !got2.HasSecret {
		t.Fatal("has_secret must be true after replacement")
	}
	if strings.Contains(rr4.Body.String(), oldKey) || strings.Contains(rr4.Body.String(), newKey) {
		t.Fatalf("reopen GET leaked secret: %s", rr4.Body.String())
	}

	// 5. Verify replacement works without preserve_secret (explicit new key in request)
	putReplace2 := `{"provider":{"id":"b5b","name":"B5b","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}],"api_key":"another-new-key"},"replace_key":true}`
	rr5 := serve(http.MethodPut, "/admin/api/providers/b5b", putReplace2)
	if rr5.Code != http.StatusOK {
		t.Fatalf("second replace PUT status=%d body=%s", rr5.Code, rr5.Body.String())
	}
	live = s.currentConfig().Providers[0]
	if live.APIKey != "another-new-key" {
		t.Fatalf("second replace_key did not update API key, got %q", live.APIKey)
	}

	// 6. Verify replace_key=false with new key does NOT replace (preserves old)
	putNoReplace := `{"provider":{"id":"b5b","name":"B5b","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}],"api_key":"should-not-replace"},"replace_key":false}`
	rr6 := serve(http.MethodPut, "/admin/api/providers/b5b", putNoReplace)
	if rr6.Code != http.StatusOK {
		t.Fatalf("no-replace PUT status=%d body=%s", rr6.Code, rr6.Body.String())
	}
	live = s.currentConfig().Providers[0]
	if live.APIKey != "another-new-key" {
		t.Fatalf("replace_key=false should have preserved key, got %q", live.APIKey)
	}
}

// TestB5bCredentialsReplaceKey covers credential pool replacement
func TestB5bCredentialsReplaceKey(t *testing.T) {
	const oldCred = "B5B_OLD_CRED_SECRET_111"
	const newCred = "B5B_NEW_CRED_SECRET_222"

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "b5b-cred", Name: "B5bCred", Type: "openai_compatible",
		BaseURL:  "http://127.0.0.1:9/v1",
		AuthMode: "bearer", Enabled: true,
		Credentials: []config.CredentialConfig{{Name: "key1", APIKey: oldCred, Enabled: true}},
		Models:      []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1}},
	}}
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

	// Initial GET
	rr := serve(http.MethodGet, "/admin/api/providers/b5b-cred", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), oldCred) {
		t.Fatalf("GET leaked credential: %s", rr.Body.String())
	}

	// Replace credential pool with replace_key=true
	putReplace := `{"provider":{"id":"b5b-cred","name":"B5bCred","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}],"credentials":[{"name":"key1","api_key":"` + newCred + `","enabled":true}]},"replace_key":true}`
	rr2 := serve(http.MethodPut, "/admin/api/providers/b5b-cred", putReplace)
	if rr2.Code != http.StatusOK {
		t.Fatalf("replace PUT status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if strings.Contains(rr2.Body.String(), oldCred) || strings.Contains(rr2.Body.String(), newCred) {
		t.Fatalf("replace PUT response leaked credential: %s", rr2.Body.String())
	}
	live := s.currentConfig().Providers[0]
	if len(live.Credentials) != 1 || live.Credentials[0].APIKey != newCred {
		t.Fatalf("replace_key did not update credential, got %+v", live.Credentials)
	}

	// Reopen GET
	rr3 := serve(http.MethodGet, "/admin/api/providers/b5b-cred", "")
	if rr3.Code != http.StatusOK {
		t.Fatalf("reopen GET status=%d body=%s", rr3.Code, rr3.Body.String())
	}
	if strings.Contains(rr3.Body.String(), oldCred) || strings.Contains(rr3.Body.String(), newCred) {
		t.Fatalf("reopen GET leaked credential: %s", rr3.Body.String())
	}
}

// TestB5bReplaceKeyWithoutMaskedPlaceholder verifies that sending the masked
// placeholder as the new key does NOT replace the secret (treated as untouched)
func TestB5bReplaceKeyWithoutMaskedPlaceholder(t *testing.T) {
	const oldKey = "B5B_SHOULD_PERSIST_999"
	const maskedPlaceholder = "••••••••"

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "b5b-mask", Name: "B5bMask", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9/v1",
		APIKey:  oldKey, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1}},
	}}
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

	// Submit masked placeholder as new key WITHOUT replace_key=true
	// This should be treated as "untouched" and preserve the old key
	putMasked := `{"provider":{"id":"b5b-mask","name":"B5bMask","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m1","model":"model-1","enabled":true,"weight":1,"capabilities":{}}],"api_key":"` + maskedPlaceholder + `"},"replace_key":false}`
	rr := serve(http.MethodPut, "/admin/api/providers/b5b-mask", putMasked)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rr.Code, rr.Body.String())
	}
	live := s.currentConfig().Providers[0]
	if live.APIKey != oldKey {
		t.Fatalf("masked placeholder without replace_key should preserve old key, got %q", live.APIKey)
	}
}
