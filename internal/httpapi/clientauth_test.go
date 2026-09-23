package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func clientAuthGateway(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	if mutate != nil {
		mutate(&cfg)
	}
	return testGateway(t, cfg)
}

func dataPlanePost(t *testing.T, s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestClientAuthDisabledByDefault(t *testing.T) {
	s := clientAuthGateway(t, nil)
	if rr := dataPlanePost(t, s, "/v1/messages", nil); rr.Code != 503 { // no providers configured, but NOT 401
		t.Fatalf("expected data plane reachable without auth, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestClientAuthRequiredRejectsMissingInvalidAndDisabledKeys(t *testing.T) {
	s := clientAuthGateway(t, func(cfg *config.Config) {
		cfg.ClientAuth = config.ClientAuthConfig{Required: true, Keys: []config.ClientKey{
			{ID: "k1", Name: "laptop", Key: "nr-good", Enabled: true},
			{ID: "k2", Name: "old", Key: "nr-disabled", Enabled: false},
		}}
	})

	if rr := dataPlanePost(t, s, "/v1/messages", nil); rr.Code != 401 {
		t.Fatalf("missing key: expected 401, got %d", rr.Code)
	}
	if rr := dataPlanePost(t, s, "/v1/messages", map[string]string{"x-api-key": "nr-wrong"}); rr.Code != 401 {
		t.Fatalf("invalid key: expected 401, got %d", rr.Code)
	}
	if rr := dataPlanePost(t, s, "/v1/messages", map[string]string{"x-api-key": "nr-disabled"}); rr.Code != 401 {
		t.Fatalf("disabled key: expected 401, got %d", rr.Code)
	}
	// Anthropic ingress gets a native anthropic error shape.
	rr := dataPlanePost(t, s, "/v1/messages", map[string]string{"x-api-key": "nr-good"})
	if rr.Code == 401 {
		t.Fatalf("valid key rejected: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "gateway_error") && !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("unexpected pass-through body: %s", rr.Body.String())
	}
	// OpenAI ingress via Bearer header reaches the handler too.
	rr = dataPlanePost(t, s, "/v1/chat/completions", map[string]string{"Authorization": "Bearer nr-good"})
	if rr.Code == 401 {
		t.Fatalf("valid bearer key rejected: %s", rr.Body.String())
	}
	// Non-data-plane endpoints stay open (admin surface has its own auth).
	if rr := dataPlanePost(t, s, "/metrics", nil); rr.Code == 401 {
		t.Fatal("/metrics should not require a client key")
	}
}

func TestClientKeyAdminLifecycle(t *testing.T) {
	s := clientAuthGateway(t, func(cfg *config.Config) {
		cfg.ClientAuth = config.ClientAuthConfig{Required: true, Keys: []config.ClientKey{{ID: "k1", Name: "laptop", Key: "nr-good", Enabled: true}}}
	})
	adminReq := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(body))
		req.Header.Set("x-admin-key", "k")
		if body != "" {
			req.Header.Set("content-type", "application/json")
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	rr := adminReq(http.MethodGet, "/admin/api/client-keys", "")
	if rr.Code != 200 {
		t.Fatalf("list keys status %d", rr.Code)
	}
	var list struct {
		Required bool `json:"required"`
		Keys     []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if !list.Required || len(list.Keys) != 1 {
		t.Fatalf("unexpected list: %s", rr.Body.String())
	}

	// Generate a second key.
	rr = adminReq(http.MethodPost, "/admin/api/client-keys", `{"name":"ci-bot"}`)
	if rr.Code != 201 {
		t.Fatalf("create key status %d body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		Key clientKeyOut `json:"key"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key.Key, "nr-") || len(created.Key.Key) < 32 {
		t.Fatalf("generated key looks wrong: %q", created.Key.Key)
	}

	// Generated key authenticates immediately.
	if rr := dataPlanePost(t, s, "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + created.Key.Key}); rr.Code == 401 {
		t.Fatal("generated key should authenticate")
	}

	// Disable it → rejected, then re-enable.
	rr = adminReq(http.MethodPut, "/admin/api/client-keys/"+created.Key.ID, `{"enabled":false}`)
	if rr.Code != 200 {
		t.Fatalf("disable status %d", rr.Code)
	}
	if rr := dataPlanePost(t, s, "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + created.Key.Key}); rr.Code != 401 {
		t.Fatal("disabled generated key should be rejected")
	}

	// Required=true with zero enabled keys is refused by validation.
	if rr := adminReq(http.MethodPut, "/admin/api/client-keys/k1", `{"enabled":false}`); rr.Code != 400 {
		t.Fatalf("disabling the last enabled key while required must 400, got %d", rr.Code)
	}
	if rr := adminReq(http.MethodPut, "/admin/api/client-keys/"+created.Key.ID, `{"enabled":true}`); rr.Code != 200 {
		t.Fatalf("re-enable status %d", rr.Code)
	}

	// Delete.
	if rr := adminReq(http.MethodDelete, "/admin/api/client-keys/"+created.Key.ID, ""); rr.Code != 200 {
		t.Fatalf("delete status %d", rr.Code)
	}
	if rr := adminReq(http.MethodDelete, "/admin/api/client-keys/"+created.Key.ID, ""); rr.Code != 404 {
		t.Fatalf("double delete should 404, got %d", rr.Code)
	}

	// Duplicate key values are refused.
	if rr := adminReq(http.MethodPost, "/admin/api/client-keys", `{"name":"dup"}`); rr.Code != 201 {
		t.Fatalf("create dup status %d", rr.Code)
	}
}

func TestClientAuthRequiredNeedsEnabledKey(t *testing.T) {
	cfg := config.Default()
	cfg.ClientAuth = config.ClientAuthConfig{Required: true, Keys: []config.ClientKey{{ID: "k1", Key: "nr-x", Enabled: false}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("required=true with zero enabled keys must fail validation")
	}
}
