package httpapi

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

func TestMission95ResponsesHandlerEarlyBranches(t *testing.T) {
	s := &Server{}
	// Wrong method: 405 in the Responses envelope.
	rec := httptest.NewRecorder()
	s.openAIResponses(rec, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "method not allowed") {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Invalid JSON: 400.
	rec = httptest.NewRecorder()
	s.openAIResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{bad`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON") {
		t.Fatalf("bad json code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Missing model / empty input: 400.
	for _, body := range []string{
		`{"input":[{"type":"message","role":"user","content":"hi"}]}`,
		`{"model":"m1"}`,
		`{"model":"  ","input":[{"type":"message","role":"user","content":"hi"}]}`,
	} {
		rec = httptest.NewRecorder()
		s.openAIResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model and input are required") {
			t.Fatalf("body %s: code=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
	// previous_response_id is refused closed, not silently severed.
	rec = httptest.NewRecorder()
	s.openAIResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m1","previous_response_id":"resp_123","input":[{"type":"message","role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "previous_response_id is not supported") {
		t.Fatalf("previous_response_id code=%d body=%s", rec.Code, rec.Body.String())
	}
	// Auth-enabled server without a key: 401 in the Responses envelope.
	authServer := &Server{}
	authServer.cfg.ClientAuth.Enabled = true
	rec = httptest.NewRecorder()
	authServer.openAIResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m1","input":[{"type":"message","role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("keyless responses code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func mission95BudgetServer(t *testing.T, spend float64) (*Server, chan BudgetAlertPayload) {
	t.Helper()
	now := time.Now()
	ledger := usage.NewBudgetLedger()
	if id, ok := ledger.Reserve("key1", 10, spend, now); ok {
		ledger.Settle("key1", id, spend)
	} else {
		t.Fatalf("reserve %v failed", spend)
	}
	cfg := config.Config{}
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{
		ID: "key1", Name: "Key One", BudgetUSD: 10,
		BudgetWebhookURL: "http://127.0.0.1:9/hook", // never actually dialed: deliver stub below
	}}
	alerter := newBudgetAlerter(cfg, log.New(io.Discard, "", 0))
	delivered := make(chan BudgetAlertPayload, 10)
	alerter.deliver = func(ctx context.Context, payload BudgetAlertPayload, webhookURL string, timeout time.Duration, egressCfg config.EgressConfig) error {
		delivered <- payload
		return nil
	}
	alerter.now = func() time.Time { return now }
	s := &Server{cfg: cfg, budgets: ledger, budgetAlerter: alerter}
	return s, delivered
}

func TestMission95CheckBudgetAlertsLive(t *testing.T) {
	// Spend 8.50 of a 10.00 budget crosses the 50 and 80 rungs, not 100.
	s, delivered := mission95BudgetServer(t, 8.50)
	s.checkBudgetAlerts("key1")
	seen := map[float64]bool{}
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case p := <-delivered:
			seen[p.ThresholdPercent] = true
			if p.BudgetID != "key1" || p.LimitUSD != 10 || p.SpendUSD != 8.5 || p.Currency != "USD" || p.Period != "lifetime:10" {
				t.Errorf("payload = %+v", p)
			}
		case <-timeout:
			t.Fatalf("only %d deliveries arrived, seen=%v", len(seen), seen)
		}
	}
	if !seen[50] || !seen[80] || seen[100] {
		t.Errorf("fired thresholds = %v", seen)
	}
	// Exactly-once: a second evaluation in the same period fires nothing.
	s.checkBudgetAlerts("key1")
	select {
	case p := <-delivered:
		t.Errorf("duplicate delivery: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
	// Unknown key, zero budget, and missing webhook are all silent.
	s.checkBudgetAlerts("ghost")
	zeroCfg := config.Config{}
	zeroCfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{ID: "key1", BudgetUSD: 0, BudgetWebhookURL: "http://127.0.0.1:9/hook"}}
	zeroServer := &Server{cfg: zeroCfg, budgets: usage.NewBudgetLedger(), budgetAlerter: s.budgetAlerter}
	zeroServer.checkBudgetAlerts("key1")
	nowebCfg := config.Config{}
	nowebCfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{ID: "key1", BudgetUSD: 10}}
	nowebServer := &Server{cfg: nowebCfg, budgets: usage.NewBudgetLedger(), budgetAlerter: s.budgetAlerter}
	nowebServer.checkBudgetAlerts("key1")
	select {
	case p := <-delivered:
		t.Errorf("silent case delivered: %+v", p)
	default:
	}
	// Nil budgets ledger and nil alerter never panic.
	(&Server{cfg: s.cfg}).checkBudgetAlerts("key1")
	noLedger := &Server{cfg: s.cfg, budgetAlerter: s.budgetAlerter}
	noLedger.checkBudgetAlerts("key1")
	// Monthly period label + global webhook fallback.
	monthlyCfg := config.Config{}
	monthlyCfg.BudgetAlerts.WebhookURL = "http://127.0.0.1:9/global"
	monthlyCfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{{ID: "key1", BudgetUSD: 10, BudgetPeriod: "monthly"}}
	ledger := usage.NewBudgetLedger()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if id, ok := ledger.Reserve("key1", 10, 6, now); ok {
		ledger.Settle("key1", id, 6)
	}
	mAlerter := newBudgetAlerter(monthlyCfg, nil)
	mAlerter.deliver = func(ctx context.Context, payload BudgetAlertPayload, webhookURL string, timeout time.Duration, egressCfg config.EgressConfig) error {
		delivered <- payload
		return nil
	}
	mAlerter.now = func() time.Time { return now }
	mServer := &Server{cfg: monthlyCfg, budgets: ledger, budgetAlerter: mAlerter}
	mServer.checkBudgetAlerts("key1")
	select {
	case p := <-delivered:
		if p.Period != "2026-10" || p.ThresholdPercent != 50 {
			t.Errorf("monthly payload = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("monthly delivery never arrived")
	}
}

func TestMission95BudgetAlertsSnapshotAndStoreWiring(t *testing.T) {
	s, _ := mission95BudgetServer(t, 8.50)
	s.checkBudgetAlerts("key1")
	snap := s.budgetAlertsSnapshot()
	if snap["enabled"] != true || snap["durable"] != "memory" || snap["webhook_configured"] != false {
		t.Fatalf("snapshot head = %v", snap)
	}
	budgets, _ := snap["budgets"].([]map[string]any)
	if len(budgets) != 1 || budgets[0]["id"] != "key1" || budgets[0]["spend_usd"] != 8.5 ||
		budgets[0]["limit_usd"] != 10.0 || budgets[0]["webhook_configured"] != true {
		t.Fatalf("snapshot budgets = %v", budgets)
	}
	fired, _ := budgets[0]["fired"].([]float64)
	if len(fired) != 2 || fired[0] != 50 || fired[1] != 80 {
		t.Errorf("snapshot fired = %v", fired)
	}
	// Key-level thresholds override the global ladder in the snapshot.
	s.cfg.ClientAuth.VirtualKeys[0].BudgetAlertThresholds = []float64{90}
	snap = s.budgetAlertsSnapshot()
	budgets, _ = snap["budgets"].([]map[string]any)
	thresholds, _ := budgets[0]["thresholds"].([]float64)
	if len(thresholds) != 1 || thresholds[0] != 90 {
		t.Errorf("key thresholds = %v", budgets[0]["thresholds"])
	}
	// Snapshot without an alerter still renders (durable=memory, no fired).
	bare := &Server{cfg: s.cfg, budgets: s.budgets}
	if snap := bare.budgetAlertsSnapshot(); snap["durable"] != "memory" {
		t.Errorf("bare snapshot durable = %v", snap["durable"])
	}
	// Snapshot skips zero-budget keys entirely.
	s.cfg.ClientAuth.VirtualKeys = append(s.cfg.ClientAuth.VirtualKeys, config.VirtualKeyConfig{ID: "free", BudgetUSD: 0})
	if snap := s.budgetAlertsSnapshot(); len(snap["budgets"].([]map[string]any)) != 1 {
		t.Errorf("zero-budget key leaked into snapshot: %v", snap["budgets"])
	}
	// SetBudgetAlertStore merges durable marks and reports the controlplane
	// durable kind; a failing store counts a failure; nil reverts.
	fake := &fakeBudgetAlertIO{}
	seedAt := time.Now().UnixNano()
	if _, err := NewIOBudgetAlertStore(fake).Save(map[string]int64{"durable-mark": seedAt}, 0); err != nil {
		t.Fatal(err)
	}
	s.SetBudgetAlertStore(NewIOBudgetAlertStore(fake))
	if s.budgetAlerter.fired["durable-mark"] != seedAt {
		t.Errorf("store marks not merged: %v", s.budgetAlerter.fired)
	}
	if snap := s.budgetAlertsSnapshot(); snap["durable"] != "controlplane" {
		t.Errorf("store snapshot durable = %v", snap["durable"])
	}
	before := s.budgetAlerter.stateFailures
	broken := &fakeBudgetAlertIO{readErr: errMission95StoreDown}
	s.SetBudgetAlertStore(NewIOBudgetAlertStore(broken))
	if s.budgetAlerter.stateFailures != before+1 {
		t.Errorf("store failures = %d, want %d", s.budgetAlerter.stateFailures, before+1)
	}
	s.SetBudgetAlertStore(nil)
	if s.budgetAlerter.store != nil {
		t.Error("nil store did not revert")
	}
	// A nil alerter snapshot on a nil server path is safe.
	(&Server{}).SetBudgetAlertStore(nil)
	// persistBestEffort writes the file store synchronously.
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.json")
	fileAlerter := newBudgetAlerter(config.Config{}, nil)
	fileAlerter.fileStore = &fileBudgetAlertStore{path: path}
	fileAlerter.markFired("persisted-mark")
	fileAlerter.persistBestEffort()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "persisted-mark") {
		t.Fatalf("persisted file = %s, %v", data, err)
	}
	// A failing file store counts a failure instead of panicking.
	fileAlerter.fileStore = &fileBudgetAlertStore{path: filepath.Join(path, "child.json")}
	before = fileAlerter.stateFailures
	fileAlerter.persistBestEffort()
	if fileAlerter.stateFailures != before+1 {
		t.Errorf("file persist failures = %d", fileAlerter.stateFailures)
	}
}

var errMission95StoreDown = errMission95()

func errMission95() error { return &mission95Error{"store down"} }

type mission95Error struct{ msg string }

func (e *mission95Error) Error() string { return e.msg }
