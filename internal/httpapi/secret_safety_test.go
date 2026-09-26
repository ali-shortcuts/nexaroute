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

const secretCanary = "SECRET_CANARY_NEXAROUTE_9f3a7c21"

func TestProviderAPIKeyNeverReturnedAfterSave(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	body := `{
		"provider": {
			"id": "canary-p",
			"name": "Canary",
			"type": "openai_compatible",
			"base_url": "http://127.0.0.1:19999/v1",
			"api_key": "` + secretCanary + `",
			"auth_mode": "bearer",
			"enabled": false,
			"models": [{"id":"m","model":"m","enabled":true,"priority":1,"weight":1,"capabilities":{"streaming":true}}]
		},
		"preserve_secret": false
	}`
	adminReq := func(method, path, payload string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if payload != "" {
			req = httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
		}
		req.Host = "127.0.0.1"
		req.RemoteAddr = "127.0.0.1:12345"
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	rr := adminReq("POST", "/admin/api/providers", body)
	if rr.Code != 201 {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), secretCanary) {
		t.Fatal("create response leaked secret canary")
	}

	for _, path := range []string{
		"/admin/api/providers/canary-p",
		"/admin/api/providers/canary-p?reveal=1",
		"/admin/api/providers",
		"/admin/api/snapshot",
		"/metrics",
	} {
		got := adminReq("GET", path, "")
		if strings.Contains(got.Body.String(), secretCanary) {
			t.Fatalf("%s leaked secret canary: %s", path, got.Body.String())
		}
	}

	rr = adminReq("GET", "/admin/api/providers/canary-p", "")
	if rr.Code != 200 {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["has_secret"] != true {
		t.Fatalf("has_secret=%v want true", got["has_secret"])
	}
	if _, ok := got["resolved_api_key"]; ok {
		t.Fatal("resolved_api_key must not be present")
	}
	prov, _ := got["provider"].(map[string]any)
	if prov["api_key"] != nil && prov["api_key"] != "" {
		t.Fatalf("provider.api_key still present: %#v", prov["api_key"])
	}

	raw, err := os.ReadFile(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), secretCanary) {
		t.Fatal("secret should persist in 0600 config according to existing architecture")
	}
	st, err := os.Stat(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%o want 0600", st.Mode().Perm())
	}
}

func TestDashboardJSDoesNotRequestReveal(t *testing.T) {
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	if strings.Contains(js, "reveal=1") || strings.Contains(js, "resolved_api_key") {
		t.Fatal("dashboard still requests or consumes plaintext secrets")
	}
}
