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

func settingsRequest(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	return settingsRequestWithKey(t, s, method, body, "k")
}

func settingsRequestWithKey(t *testing.T, s *Server, method, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://gateway/admin/api/settings", strings.NewReader(body))
	req.Header.Set("x-admin-key", key)
	req.Header.Set("content-type", "application/json")
	if body == "" {
		req.Body = http.NoBody
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAdminSettingsGetReturnsAllSections(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false // httptest clients are not loopback
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	rr := settingsRequest(t, s, http.MethodGet, "")
	if rr.Code != 200 {
		t.Fatalf("GET settings status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var form settingsForm
	if err := json.Unmarshal(rr.Body.Bytes(), &form); err != nil {
		t.Fatal(err)
	}
	if form.Routing.Strategy == "" || form.Logging.MaxSizeMB == 0 || form.Listen == "" {
		t.Fatalf("settings GET returned incomplete sections: %+v", form)
	}
	if form.Admin.APIKey != "k" {
		t.Fatalf("settings GET should expose the stored admin key to an authorized admin, got %q", form.Admin.APIKey)
	}
}

func TestAdminSettingsPutPersistsLoggingAndAdmin(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false // httptest clients are not loopback
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	body := `{"routing":{"strategy":"ready_queue"},"probe":{"enabled":false},"logging":{"file":"off","max_size_mb":16,"max_backups":2,"access_mode":"errors","success_sample_every":50,"slow_request_ms":1500,"console_max_lines_per_minute":120},"admin":{"bind_local_only":false,"api_key":"rotated-key"}}`
	rr := settingsRequest(t, s, http.MethodPut, body)
	if rr.Code != 200 {
		t.Fatalf("PUT settings status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "rotated-key") {
		t.Fatal("PUT settings response must not echo the stored admin key")
	}

	// The rotation applies to the live server immediately: the old key is
	// rejected and the new key authorizes.
	rr = settingsRequestWithKey(t, s, http.MethodGet, "", "k")
	if rr.Code != 401 {
		t.Fatalf("old admin key should be rejected after rotation, got %d", rr.Code)
	}
	rr = settingsRequestWithKey(t, s, http.MethodGet, "", "rotated-key")
	if rr.Code != 200 {
		t.Fatalf("GET after rotation failed: %d", rr.Code)
	}

	// And the change is persisted atomically to the active config file.
	b, err := os.ReadFile(testConfigPath(t, s))
	if err != nil {
		t.Fatal(err)
	}
	var persisted config.Config
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Logging.AccessMode != "errors" || persisted.Logging.MaxSizeMB != 16 || persisted.Logging.ConsoleMaxLinesPerMinute != 120 {
		t.Fatalf("logging section not persisted: %+v", persisted.Logging)
	}
	if persisted.Admin.APIKey != "rotated-key" || persisted.Admin.BindLocalOnly {
		t.Fatalf("admin section not persisted: %+v", persisted.Admin)
	}
	if persisted.Routing.Strategy != "ready_queue" {
		t.Fatalf("routing section not persisted: %+v", persisted.Routing)
	}
}

func TestAdminSettingsPutRejectsOpenAdminWithoutKey(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false // httptest clients are not loopback
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	body := `{"routing":{"strategy":"ready_mesh"},"probe":{"enabled":false},"logging":{"file":"auto"},"admin":{"bind_local_only":false,"api_key":""}}`
	rr := settingsRequest(t, s, http.MethodPut, body)
	if rr.Code != 400 {
		t.Fatalf("expected 400 for bind_local_only=false without api_key, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// testConfigPath returns the config path a testGateway server persists to.
func testConfigPath(t *testing.T, s *Server) string {
	t.Helper()
	return s.configPath
}
