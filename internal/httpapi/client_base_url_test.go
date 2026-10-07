package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestClientBaseURLSnapshotAndSettingsContract(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientBaseURL = "https://gateway.example.test/nexa"
	s := testGateway(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "http://localhost/admin/api/snapshot", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	var snap map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	access, ok := snap["client_access"].(map[string]any)
	if !ok || access["base_url"] != cfg.ClientBaseURL || access["source"] != "configured" {
		t.Fatalf("client access metadata=%#v", snap["client_access"])
	}
	if strings.Contains(rr.Body.String(), "api_key") && strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("snapshot appears to contain a secret")
	}

	req = httptest.NewRequest(http.MethodPut, "http://localhost/admin/api/settings", strings.NewReader(`{"client_base_url":"http://remote.example.test","routing":{},"probe":{}}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid remote HTTP URL status=%d body=%s", rr.Code, rr.Body.String())
	}
}
