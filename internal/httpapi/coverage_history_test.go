package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestCoverageBehaviorConfigHistoryRevisionGuard(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "test-admin"
	s := testGateway(t, cfg)
	if _, err := s.mutateConfig(func(c *config.Config) error { c.Routing.PublicModel = "first"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.mutateConfig(func(c *config.Config) error { c.Routing.PublicModel = "second"; return nil }); err != nil {
		t.Fatal(err)
	}
	request := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/admin/api/config/history", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Admin-Key", "test-admin")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	get := request(http.MethodGet, "")
	if get.Code != http.StatusOK {
		t.Fatalf("history GET status=%d body=%s", get.Code, get.Body.String())
	}
	var history struct {
		Revision  uint64 `json:"revision"`
		Available int    `json:"available_rollbacks"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if history.Available != 2 {
		t.Fatalf("available rollbacks=%d want 2", history.Available)
	}
	stale := request(http.MethodPost, `{"steps":1,"expected_revision":999999}`)
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale rollback status=%d body=%s", stale.Code, stale.Body.String())
	}
	rolled := request(http.MethodPost, `{"steps":0}`)
	if rolled.Code != http.StatusOK || !strings.Contains(rolled.Body.String(), `"rolled_back":true`) {
		t.Fatalf("rollback status=%d body=%s", rolled.Code, rolled.Body.String())
	}
	if s.baseCfg.Routing.PublicModel != "first" {
		t.Fatalf("rollback restored %q want first", s.baseCfg.Routing.PublicModel)
	}
	if method := request(http.MethodPut, "{}"); method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method status=%d", method.Code)
	}
}
