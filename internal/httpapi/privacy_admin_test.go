package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestPrivacyDataHandlingExposedAndSecretsNeverAppear(t *testing.T) {
	const canary = "PRIVACY_SECRET_CANARY_9d21"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		APIKey: canary, AuthMode: "bearer", Enabled: true,
		DataHandling: config.DataHandlingConfig{TrainsOnData: "no", Retention: "none", Note: "DPA"},
		Models:       []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)

	rr := adminRequest(s, http.MethodGet, "/admin/api/providers", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var list struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Providers) != 1 {
		t.Fatalf("providers=%d want 1", len(list.Providers))
	}
	dh, ok := list.Providers[0]["data_handling"].(map[string]any)
	if !ok {
		t.Fatalf("list response missing data_handling: %s", rr.Body.String())
	}
	if dh["trains_on_data"] != "no" || dh["retention"] != "none" {
		t.Fatalf("list data_handling wrong: %v", dh)
	}
	if strings.Contains(rr.Body.String(), canary) {
		t.Fatalf("list exposed secret canary: %s", rr.Body.String())
	}

	rr = adminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rr.Code, rr.Body.String())
	}
	var detail struct {
		Provider config.ProviderConfig `json:"provider"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Provider.DataHandling.TrainsOnData != "no" || detail.Provider.DataHandling.Retention != "none" {
		t.Fatalf("detail data_handling wrong: %+v", detail.Provider.DataHandling)
	}
	if strings.Contains(rr.Body.String(), canary) {
		t.Fatalf("detail exposed secret canary: %s", rr.Body.String())
	}
}
