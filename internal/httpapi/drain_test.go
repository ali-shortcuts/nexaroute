package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestGracefulDrainRejectsDataPlaneButKeepsHealth(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "drain-admin"
	cfg.Admin.BindLocalOnly = false
	s := testGateway(t, cfg)
	set := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/drain", strings.NewReader(`{"draining":true}`))
	set.Header.Set("X-Admin-Key", "drain-admin")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, set)
	if rr.Code != http.StatusOK {
		t.Fatalf("set drain status=%d body=%s", rr.Code, rr.Body.String())
	}
	data := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	dr := httptest.NewRecorder()
	s.Handler().ServeHTTP(dr, data)
	if dr.Code != http.StatusServiceUnavailable || !strings.Contains(dr.Body.String(), "draining") {
		t.Fatalf("data status=%d body=%s", dr.Code, dr.Body.String())
	}
	health := httptest.NewRequest(http.MethodGet, "http://gateway/healthz", nil)
	hr := httptest.NewRecorder()
	s.Handler().ServeHTTP(hr, health)
	if hr.Code != http.StatusOK {
		t.Fatalf("health status=%d", hr.Code)
	}
	ready := httptest.NewRequest(http.MethodGet, "http://gateway/readyz", nil)
	rd := httptest.NewRecorder()
	s.Handler().ServeHTTP(rd, ready)
	if rd.Code != http.StatusServiceUnavailable || !strings.Contains(rd.Body.String(), "draining") {
		t.Fatalf("ready status=%d body=%s", rd.Code, rd.Body.String())
	}
}
