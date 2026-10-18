package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// #435: the key list carries LIVE budget state for budgeted keys —
// settled (committed), in-flight reserved, and remaining — read
// from the same ledger admission enforces against. Remaining is
// emitted only when a configured limit makes it calculable;
// unbudgeted keys carry none of the state fields.
func TestAdminClientKeysExposeLiveBudgetState(t *testing.T) {
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
	now := time.Now()
	id, ok := s.budgets.Reserve("vk_budget", 250.5, 40, now)
	if !ok {
		t.Fatal("reserve for settle failed")
	}
	s.budgets.Settle("vk_budget", id, 32.5) // committed 32.5
	if _, ok := s.budgets.Reserve("vk_budget", 250.5, 10, now); !ok {
		t.Fatal("open reserve failed")
	} // reserved 10

	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/client-keys", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var decoded struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, k := range decoded.Keys {
		byID[k["id"].(string)] = k
	}
	b := byID["vk_budget"]
	if got := b["budget_committed_usd"]; got != 32.5 {
		t.Fatalf("committed = %v, want 32.5", got)
	}
	if got := b["budget_reserved_usd"]; got != 10.0 {
		t.Fatalf("reserved = %v, want 10", got)
	}
	if got := b["budget_remaining_usd"]; got != 208.0 {
		t.Fatalf("remaining = %v, want 208 (250.5 - 32.5 - 10)", got)
	}
	p := byID["vk_plain"]
	for _, field := range []string{"budget_committed_usd", "budget_reserved_usd", "budget_remaining_usd"} {
		if _, present := p[field]; present {
			t.Fatalf("unbudgeted key carries %s — remaining must not be invented without a limit", field)
		}
	}
}
