package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// E4 regression: the Cost page reads budget_usd from
// GET /admin/api/client-keys, so the authorized response must
// preserve VirtualKeyConfig.BudgetUSD — a field the M5-6 budget
// backend added to the config model but this handler originally
// dropped. budget_usd is emitted for EVERY key (0 = no cap) so
// clients can distinguish a budget-aware backend from a legacy one;
// secret material (key hashes, plaintext) never appears.
func TestAdminClientKeysExposeBudgetUSD(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-secret"
	cfg.Admin.BindLocalOnly = false
	cfg.ClientAuth = config.ClientAuthConfig{
		Enabled: true,
		VirtualKeys: []config.VirtualKeyConfig{
			{ID: "vk_budget", Name: "budgeted", KeyHash: keyDigest("nrk_budgeted"), Role: "developer", BudgetUSD: 250.5},
			{ID: "vk_plain", Name: "unbudgeted", KeyHash: keyDigest("nrk_plain"), Role: "developer"},
		},
	}
	s := testGateway(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/client-keys", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "key_hash") || strings.Contains(body, keyDigest("nrk_budgeted")) || strings.Contains(body, "nrk_budgeted") {
		t.Fatalf("list leaked secret material: %s", body)
	}

	var decoded struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Keys) != 2 {
		t.Fatalf("keys=%d, want 2: %s", len(decoded.Keys), body)
	}
	byID := map[string]map[string]any{}
	for _, k := range decoded.Keys {
		id, _ := k["id"].(string)
		byID[id] = k
	}
	budgeted, ok := byID["vk_budget"]
	if !ok {
		t.Fatalf("vk_budget missing from response: %s", body)
	}
	raw, present := budgeted["budget_usd"]
	if !present {
		t.Fatalf("budget_usd missing for the budgeted key — the Cost page cannot list budgets: %s", body)
	}
	if got, _ := raw.(float64); got != 250.5 {
		t.Fatalf("budget_usd=%v, want 250.5", raw)
	}
	plain, ok := byID["vk_plain"]
	if !ok {
		t.Fatalf("vk_plain missing from response: %s", body)
	}
	rawPlain, present := plain["budget_usd"]
	if !present {
		t.Fatal("budget_usd must be emitted for every key (0 = no cap) so clients can detect a budget-aware backend")
	}
	if got, _ := rawPlain.(float64); got != 0 {
		t.Fatalf("unbudgeted key budget_usd=%v, want 0", rawPlain)
	}

	// The typed view must round-trip the same values.
	var typed struct {
		Keys []clientKeyResponse `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &typed); err != nil {
		t.Fatal(err)
	}
	for _, k := range typed.Keys {
		switch k.ID {
		case "vk_budget":
			if k.BudgetUSD != 250.5 {
				t.Fatalf("typed budget_usd=%v, want 250.5", k.BudgetUSD)
			}
		case "vk_plain":
			if k.BudgetUSD != 0 {
				t.Fatalf("typed budget_usd=%v, want 0", k.BudgetUSD)
			}
		}
	}
}
