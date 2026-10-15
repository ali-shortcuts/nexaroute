package httpapi

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M5-6 budget + record-time costing e2e. The fake upstream always
// reports 1000 prompt / 100 completion tokens. Model prices:
// $10/M in, $30/M out -> actual cost per request $0.013. Requests
// carry max_tokens=100 so the admission estimate is
// ~22 input tokens + 100 output tokens ~= $0.0032.

func budgetGateway(t *testing.T, budget float64, withBook bool) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`))
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk-b", KeyHash: keyDigest("budget-key"), BudgetUSD: budget},
	}
	cfg.Providers = []config.ProviderConfig{{
		ID: "llm", Name: "LLM", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "chat-model", Enabled: true, Weight: 1,
			InputCostPerMTok: 10, OutputCostPerMTok: 30,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	if withBook {
		// The book halves the input price; record-time costing must
		// use it and attribute the cost to this version.
		cfg.Pricing.Versions = []config.PriceVersionConfig{{
			Version: "2026-q4", EffectiveFrom: "2020-01-01T00:00:00Z",
			Prices: map[string]config.PriceEntryConfig{
				"llm/m": {InputPerMTok: 5, OutputPerMTok: 30},
			},
		}}
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func budgetChat(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-api-key", "budget-key")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestBudgetAdmissionSettleAndReject(t *testing.T) {
	s := budgetGateway(t, 0.02, false)
	if rr := budgetChat(t, s); rr.Code != http.StatusOK {
		t.Fatalf("request 1: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := budgetChat(t, s); rr.Code != http.StatusOK {
		t.Fatalf("request 2: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Committed is now 2 x $0.013 = $0.026 > the $0.02 budget: the
	// third request is rejected at admission with 402.
	rr := budgetChat(t, s)
	if rr.Code != 402 {
		t.Fatalf("request 3: status=%d, want 402 (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "client budget exceeded") {
		t.Fatalf("request 3 body: %s", rr.Body.String())
	}
	committed, reserved := s.budgets.Snapshot("vk-b", time.Now())
	if math.Abs(committed-0.026) > 1e-9 || reserved != 0 {
		t.Fatalf("ledger = (%v, %v), want (0.026, 0)", committed, reserved)
	}
	// Record-time costing: the usage snapshot's total is the sum of
	// recorded costs under the "config" fallback version.
	snap := s.usageSnapshotWithPrices(s.currentConfig())
	if math.Abs(snap.TotalEstimatedCostUSD-0.026) > 1e-9 {
		t.Fatalf("snapshot cost = %v, want 0.026", snap.TotalEstimatedCostUSD)
	}
	if len(snap.ByDeployment) != 1 || snap.ByDeployment[0].CostByVersion["config"] != snap.ByDeployment[0].RecordedCostUSD {
		t.Fatalf("version attribution wrong: %+v", snap.ByDeployment)
	}
}

func TestBudgetUnlimitedWhenZero(t *testing.T) {
	s := budgetGateway(t, 0, false)
	for i := 0; i < 3; i++ {
		if rr := budgetChat(t, s); rr.Code != http.StatusOK {
			t.Fatalf("request %d with unlimited budget: %d", i, rr.Code)
		}
	}
}

func TestRecordTimeCostingUsesActiveBookVersion(t *testing.T) {
	s := budgetGateway(t, 0, true)
	if rr := budgetChat(t, s); rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Book price: (1000 x $5 + 100 x $30)/1e6 = $0.008, attributed
	// to version "2026-q4" — NOT the model-config $0.013.
	snap := s.usageSnapshotWithPrices(s.currentConfig())
	if len(snap.ByDeployment) != 1 {
		t.Fatalf("rows: %+v", snap.ByDeployment)
	}
	row := snap.ByDeployment[0]
	if math.Abs(row.RecordedCostUSD-0.008) > 1e-9 {
		t.Fatalf("recorded cost = %v, want 0.008 (book price)", row.RecordedCostUSD)
	}
	if row.CostByVersion["2026-q4"] != row.RecordedCostUSD {
		t.Fatalf("version attribution: %+v", row.CostByVersion)
	}
}
