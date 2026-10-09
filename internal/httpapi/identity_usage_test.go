package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestIdentityUsageLedgerAndCSVExport(t *testing.T) {
	up := openAIUpstream(openAIOKBody, 0, nil)
	defer up.Close()
	cfg := singleProviderOpenAI(t, up.URL, "m", config.ModelConfig{})
	cfg.Admin.APIKey = "admin-secret"
	cfg.Admin.BindLocalOnly = false
	cfg.ClientAuth = config.ClientAuthConfig{
		Enabled: true,
		Tenants: []config.TenantConfig{{
			ID:       "acme",
			Projects: []config.ProjectConfig{{
				ID:    "p1",
				Teams: []config.TeamConfig{{
					ID: "t1",
					AllowedModels: []string{"m"},
				}},
			}},
		}},
		VirtualKeys: []config.VirtualKeyConfig{{
			ID: "vk-ledger", KeyHash: keyDigest("nrk_ledger"),
			TenantID: "acme", ProjectID: "p1", TeamID: "t1",
			AllowedModels: []string{"m"},
		}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer nrk_ledger")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("request status=%d body=%s", rr.Code, rr.Body.String())
	}
	admin := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/usage/identities", nil)
	admin.Header.Set("X-Admin-Key", "admin-secret")
	ar := httptest.NewRecorder()
	s.Handler().ServeHTTP(ar, admin)
	if ar.Code != http.StatusOK || !strings.Contains(ar.Body.String(), "vk-ledger") || !strings.Contains(ar.Body.String(), "acme") {
		t.Fatalf("identity report status=%d body=%s", ar.Code, ar.Body.String())
	}
	csvReq := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/usage/identities?format=csv", nil)
	csvReq.Header.Set("X-Admin-Key", "admin-secret")
	cr := httptest.NewRecorder()
	s.Handler().ServeHTTP(cr, csvReq)
	if cr.Code != http.StatusOK || !strings.Contains(cr.Body.String(), "identity,tenant_id") || !strings.Contains(cr.Body.String(), "vk-ledger,acme,p1,t1") {
		t.Fatalf("csv status=%d body=%s", cr.Code, cr.Body.String())
	}
}
