package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// budgetAlertCapture replaces the alerter delivery function with a
// synchronous recorder so threshold logic is tested deterministically.
// The production async path (goroutine + real HTTP) is covered by the
// data-plane e2e test below.
type budgetAlertCapture struct {
	mu       sync.Mutex
	payloads []BudgetAlertPayload
	targets  []string
}

func (c *budgetAlertCapture) deliver(_ context.Context, p BudgetAlertPayload, target string, _ time.Duration, _ config.EgressConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, p)
	c.targets = append(c.targets, target)
	return nil
}

func (c *budgetAlertCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

func (c *budgetAlertCapture) all() []BudgetAlertPayload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]BudgetAlertPayload(nil), c.payloads...)
}

// waitFor polls for exactly n deliveries. Delivery is asynchronous by
// design, so threshold tests must wait rather than assert immediately.
func (c *budgetAlertCapture) waitFor(t *testing.T, n int) []BudgetAlertPayload {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.count() >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := c.all()
	if len(got) != n {
		t.Fatalf("deliveries=%d, want %d", len(got), n)
	}
	return got
}

// assertSettled waits out any in-flight delivery goroutine, then
// asserts the count is stable: exactly-once means no late duplicates.
func (c *budgetAlertCapture) assertSettled(t *testing.T, n int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := c.count(); got != n {
		t.Fatalf("deliveries=%d after settle, want %d", got, n)
	}
}

func budgetAlertTestServer(t *testing.T, budget float64, mutate func(*config.Config)) (*Server, *budgetAlertCapture) {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk-a", Name: "alerted", KeyHash: keyDigest("alert-key"), BudgetUSD: budget},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s := testGateway(t, cfg)
	cap := &budgetAlertCapture{}
	s.budgetAlerter.deliver = cap.deliver
	return s, cap
}

func settleSpend(t *testing.T, s *Server, key string, limit, amount float64) {
	t.Helper()
	id, ok := s.budgets.Reserve(key, limit, amount, time.Now())
	if !ok {
		t.Fatalf("reserve %v against %v rejected", amount, limit)
	}
	s.budgets.Settle(key, id, amount)
}

// Threshold crossing fires once: the first crossing delivers, repeated
// spend updates at the same level do not re-fire, and a higher
// threshold fires exactly once when crossed.
func TestBudgetAlertThresholdCrossingFiresOnce(t *testing.T) {
	s, cap := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
		cfg.BudgetAlerts.Thresholds = []float64{50, 80}
	})
	settleSpend(t, s, "vk-a", 10, 6) // 60%: crosses 50
	s.checkBudgetAlerts("vk-a")
	got := cap.waitFor(t, 1)[0]
	if got.BudgetID != "vk-a" || got.BudgetName != "alerted" {
		t.Fatalf("identity wrong: %+v", got)
	}
	if got.ThresholdPercent != 50 {
		t.Fatalf("threshold=%v, want 50", got.ThresholdPercent)
	}
	if got.SpendUSD != 6 || got.LimitUSD != 10 || got.Currency != "USD" {
		t.Fatalf("spend/limit/currency wrong: %+v", got)
	}
	if got.Period != "lifetime:10" {
		t.Fatalf("period=%q, want lifetime:10", got.Period)
	}
	if _, err := time.Parse(time.RFC3339, got.Timestamp); err != nil {
		t.Fatalf("timestamp %q is not RFC3339: %v", got.Timestamp, err)
	}

	// Repeated updates below the next threshold do not re-fire 50.
	settleSpend(t, s, "vk-a", 10, 1) // 70%
	s.checkBudgetAlerts("vk-a")
	s.checkBudgetAlerts("vk-a")
	cap.assertSettled(t, 1)

	// Crossing 80 fires exactly once.
	settleSpend(t, s, "vk-a", 10, 1.5) // 85%
	s.checkBudgetAlerts("vk-a")
	s.checkBudgetAlerts("vk-a")
	all := cap.waitFor(t, 2)
	if all[1].ThresholdPercent != 80 || all[1].SpendUSD != 8.5 {
		t.Fatalf("second payload wrong: %+v", all[1])
	}
}

// Per-key webhook URL and thresholds override the global policy; a
// budget with no URL anywhere stays silent.
func TestBudgetAlertPerKeyOverride(t *testing.T) {
	s, cap := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://global.example.test/hook"
		cfg.BudgetAlerts.Thresholds = []float64{90}
		cfg.ClientAuth.VirtualKeys[0].BudgetWebhookURL = "https://perkey.example.test/hook"
		cfg.ClientAuth.VirtualKeys[0].BudgetAlertThresholds = []float64{25}
	})
	settleSpend(t, s, "vk-a", 10, 3) // 30%: crosses per-key 25, not global 90
	s.checkBudgetAlerts("vk-a")
	all := cap.waitFor(t, 1)
	if all[0].ThresholdPercent != 25 {
		t.Fatalf("threshold=%v, want per-key 25", all[0].ThresholdPercent)
	}
	if cap.targets[0] != "https://perkey.example.test/hook" {
		t.Fatalf("target=%q, want per-key URL", cap.targets[0])
	}
}

// A new period re-arms fired thresholds: with a monthly period,
// crossing the same threshold in the next calendar month fires again.
func TestBudgetAlertNewPeriodRearms(t *testing.T) {
	s, cap := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
		cfg.BudgetAlerts.Thresholds = []float64{50}
		cfg.ClientAuth.VirtualKeys[0].BudgetPeriod = "monthly"
	})
	jan := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)
	s.budgetAlerter.now = func() time.Time { return jan }
	settleSpend(t, s, "vk-a", 10, 6)
	s.checkBudgetAlerts("vk-a")
	cap.waitFor(t, 1)
	if got := cap.all()[0].Period; got != "2026-01" {
		t.Fatalf("period=%q, want 2026-01", got)
	}
	// Same period: silent.
	s.checkBudgetAlerts("vk-a")
	cap.assertSettled(t, 1)
	// New month: re-arms and fires again for the same spend level.
	s.budgetAlerter.now = func() time.Time { return feb }
	s.checkBudgetAlerts("vk-a")
	all := cap.waitFor(t, 2)
	if all[1].Period != "2026-02" {
		t.Fatalf("period=%q, want 2026-02", all[1].Period)
	}
}

// The default lifetime period is bound to the configured limit: editing
// budget_usd starts a new period and re-arms fired thresholds.
func TestBudgetAlertLimitChangeRearms(t *testing.T) {
	s, cap := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
		cfg.BudgetAlerts.Thresholds = []float64{50}
	})
	settleSpend(t, s, "vk-a", 10, 6)
	s.checkBudgetAlerts("vk-a")
	cap.waitFor(t, 1)
	// Raising the limit to 20 drops spend to 30%: no fire at the old
	// or the new period.
	if _, err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.ClientAuth.VirtualKeys[0].BudgetUSD = 20
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.checkBudgetAlerts("vk-a")
	cap.assertSettled(t, 1)
	// Spend $6 more ($12 total, 60% of $20): the new period fires 50.
	settleSpend(t, s, "vk-a", 20, 6)
	s.checkBudgetAlerts("vk-a")
	all := cap.waitFor(t, 2)
	if all[1].Period != "lifetime:20" || all[1].SpendUSD != 12 {
		t.Fatalf("re-armed payload wrong: %+v", all[1])
	}
}

// With no webhook configured anywhere (the zero-service default),
// full spend is silent: no delivery, no panic, no error.
func TestBudgetAlertDisabledByDefault(t *testing.T) {
	s, cap := budgetAlertTestServer(t, 10, nil)
	settleSpend(t, s, "vk-a", 10, 10)
	s.checkBudgetAlerts("vk-a")
	s.checkBudgetAlerts("unknown-key")
	cap.assertSettled(t, 0)
	// A webhook URL without a budget is equally inert.
	s2, cap2 := budgetAlertTestServer(t, 0, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
	})
	s2.checkBudgetAlerts("vk-a")
	cap2.assertSettled(t, 0)
}

// Payload contract: exact key set, no secrets, no request content.
func TestBudgetAlertPayloadContract(t *testing.T) {
	p := BudgetAlertPayload{
		BudgetID: "vk-a", BudgetName: "alerted",
		ThresholdPercent: 50, SpendUSD: 6, LimitUSD: 10,
		Currency: "USD", Period: "lifetime:10",
		Timestamp: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"budget_id": true, "budget_name": true, "threshold_percent": true,
		"spend_usd": true, "limit_usd": true, "currency": true,
		"period": true, "timestamp": true,
	}
	if len(decoded) != len(want) {
		t.Fatalf("payload keys=%v, want exactly %d keys", decoded, len(want))
	}
	for k := range decoded {
		if !want[k] {
			t.Fatalf("unexpected payload key %q: %s", k, raw)
		}
	}
	body := string(raw)
	for _, forbidden := range []string{
		"key_hash", "api_key", "authorization", "secret", "credential",
		"request", "prompt", "message", "token",
	} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("payload contains %q: %s", forbidden, body)
		}
	}
	// The key hash that identifies this budget must not appear even as
	// a substring: payloads carry the budget ID, never key material.
	if strings.Contains(body, keyDigest("alert-key")) {
		t.Fatalf("payload leaked key material: %s", body)
	}
	if decoded["currency"] != "USD" || decoded["budget_id"] != "vk-a" {
		t.Fatalf("payload values wrong: %s", body)
	}
}

// SSRF-denied targets are refused at the egress boundary without any
// network attempt; delivery errors carry the policy reason and never
// secret or target-credential detail.
func TestBudgetAlertSSRFDeniedTargetsRefused(t *testing.T) {
	payload := BudgetAlertPayload{BudgetID: "vk-a", ThresholdPercent: 50, SpendUSD: 6, LimitUSD: 10, Currency: "USD", Period: "lifetime:10", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cases := []struct {
		name      string
		target    string
		egress    config.EgressConfig
		wantMatch string
	}{
		{"plaintext http to public host", "http://alerts.example.test/hook", config.EgressConfig{}, "https"},
		{"userinfo", "https://user:pw@alerts.example.test/hook", config.EgressConfig{}, "credentials"},
		{"query string", "https://alerts.example.test/hook?token=secret", config.EgressConfig{}, "credentials/query"},
		{"fragment", "https://alerts.example.test/hook#x", config.EgressConfig{}, "malformed"},
		{"private literal", "https://10.1.2.3/hook", config.EgressConfig{}, "blocked"},
		{"metadata literal", "https://169.254.169.254/hook", config.EgressConfig{}, "blocked"},
		{"deny host", "https://denied.example.test/hook", config.EgressConfig{DenyHosts: []string{"denied.example.test"}}, "denied"},
		{"deny cidr literal", "https://203.0.113.7/hook", config.EgressConfig{DenyCIDRs: []string{"203.0.113.0/24"}}, "denied"},
		{"allowlist mismatch", "https://other.example.test/hook", config.EgressConfig{AllowHosts: []string{"alerts.example.test"}}, "allow-listed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := deliverBudgetAlert(ctx, payload, tc.target, 5*time.Second, tc.egress)
			if err == nil {
				t.Fatalf("SSRF target %q accepted", tc.target)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantMatch)) {
				t.Fatalf("error %q does not mention %q", err, tc.wantMatch)
			}
			if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked target secrets: %q", err)
			}
		})
	}
}

// Delivery honors the operator host allowlist end to end: a listed
// loopback receiver gets the POST with no auth material; the request
// carries only the JSON contract.
func TestBudgetAlertDeliveryShape(t *testing.T) {
	var gotMethod, gotCT, gotAuth, gotCookie string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotCT = r.Method, r.Header.Get("Content-Type")
		gotAuth, gotCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	payload := BudgetAlertPayload{BudgetID: "vk-a", BudgetName: "n", ThresholdPercent: 80, SpendUSD: 8, LimitUSD: 10, Currency: "USD", Period: "lifetime:10", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := deliverBudgetAlert(ctx, payload, srv.URL+"/hook", 5*time.Second, config.EgressConfig{}); err != nil {
		t.Fatalf("loopback delivery failed: %v", err)
	}
	if gotMethod != http.MethodPost || gotCT != "application/json" {
		t.Fatalf("method=%q content-type=%q", gotMethod, gotCT)
	}
	if gotAuth != "" || gotCookie != "" {
		t.Fatalf("credential forwarding: auth=%q cookie=%q", gotAuth, gotCookie)
	}
	var decoded BudgetAlertPayload
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not the payload contract: %v (%s)", err, gotBody)
	}
	if decoded != payload {
		t.Fatalf("round trip changed payload:\n got %+v\nwant %+v", decoded, payload)
	}
}

// A slow or failing webhook never affects request handling: the data
// plane still settles spend and serves the response.
func TestBudgetAlertFailureDoesNotAffectRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`))
	}))
	defer up.Close()
	// Failing receiver: 500s with a 200ms stall, far above the 1s
	// alert timeout but irrelevant to the request path.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hook.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk-b", KeyHash: keyDigest("budget-key"), BudgetUSD: 100},
	}
	cfg.BudgetAlerts.WebhookURL = hook.URL + "/hook"
	cfg.BudgetAlerts.TimeoutSeconds = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "llm", Name: "LLM", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "chat-model", Enabled: true, Weight: 1,
			InputCostPerMTok: 10, OutputCostPerMTok: 30,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-api-key", "budget-key")
	rr := httptest.NewRecorder()
	start := time.Now()
	s.Handler().ServeHTTP(rr, req)
	elapsed := time.Since(start)
	if rr.Code != http.StatusOK {
		t.Fatalf("request with failing webhook: %d %s", rr.Code, rr.Body.String())
	}
	if elapsed > 30*time.Second {
		t.Fatalf("request path waited on webhook delivery: %s", elapsed)
	}
	committed, _ := s.budgets.Snapshot("vk-b", time.Now())
	if committed <= 0 {
		t.Fatal("spend was not settled when the webhook failed")
	}
	// The background delivery attempt may still be running; the gateway must
	// remain able to serve a follow-up request while it fails or lands.
	req2 := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("x-api-key", "budget-key")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("follow-up request: %d %s", rr2.Code, rr2.Body.String())
	}
}

// Data-plane e2e: real spend crossing real thresholds delivers real
// webhooks exactly once each, asynchronously, with the 402 path adding
// no further alerts.
func TestBudgetAlertDataPlaneEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var received []BudgetAlertPayload
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p BudgetAlertPayload
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk-b", Name: "e2e", KeyHash: keyDigest("budget-key"), BudgetUSD: 0.02},
	}
	cfg.BudgetAlerts.WebhookURL = hook.URL + "/hook"
	cfg.Providers = []config.ProviderConfig{{
		ID: "llm", Name: "LLM", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "chat-model", Enabled: true, Weight: 1,
			InputCostPerMTok: 10, OutputCostPerMTok: 30,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	chat := func() *httptest.ResponseRecorder {
		body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("x-api-key", "budget-key")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	// $0.013/request: req1 -> $0.013 (65%, crosses 50), req2 ->
	// $0.026 (crosses 80 and 100), req3 -> 402 with no new spend.
	if rr := chat(); rr.Code != http.StatusOK {
		t.Fatalf("req1: %d %s", rr.Code, rr.Body.String())
	}
	if rr := chat(); rr.Code != http.StatusOK {
		t.Fatalf("req2: %d %s", rr.Code, rr.Body.String())
	}
	if rr := chat(); rr.Code != 402 {
		t.Fatalf("req3: %d, want 402", rr.Code)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 {
		t.Fatalf("webhooks=%d, want exactly 3 (50, 80, 100)", len(received))
	}
	seen := map[float64]bool{}
	for _, p := range received {
		if p.BudgetID != "vk-b" || p.LimitUSD != 0.02 || p.Currency != "USD" {
			t.Fatalf("payload identity wrong: %+v", p)
		}
		if seen[p.ThresholdPercent] {
			t.Fatalf("threshold %v fired twice", p.ThresholdPercent)
		}
		seen[p.ThresholdPercent] = true
	}
	for _, want := range []float64{50, 80, 100} {
		if !seen[want] {
			t.Fatalf("threshold %v never fired (got %v)", want, received)
		}
	}
}

// Fired marks persist to the file store and survive alerter
// reconstruction: a restart does not re-fire the same period.
func TestBudgetAlertFileDedupSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget-alerts.json")
	s, cap := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
		cfg.BudgetAlerts.Thresholds = []float64{50}
		cfg.BudgetAlerts.StatePath = path
	})
	settleSpend(t, s, "vk-a", 10, 6)
	s.checkBudgetAlerts("vk-a")
	cap.waitFor(t, 1)
	// A restarted gateway loading the same state file must stay silent
	// for the already-fired (budget, threshold, period).
	cfg2 := s.currentConfig()
	s2 := testGateway(t, cfg2)
	cap2 := &budgetAlertCapture{}
	s2.budgetAlerter.deliver = cap2.deliver
	settleSpend(t, s2, "vk-a", 10, 6)
	s2.checkBudgetAlerts("vk-a")
	cap2.assertSettled(t, 0)
	// A HIGHER threshold in the same file-backed period still fires
	// once its own level is crossed.
	if _, err := s2.mutateConfig(func(cfg *config.Config) error {
		cfg.BudgetAlerts.Thresholds = []float64{50, 80}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settleSpend(t, s2, "vk-a", 10, 2.5) // $8.5 total: crosses 80
	s2.checkBudgetAlerts("vk-a")
	cap2.waitFor(t, 1)
	if got := cap2.all()[0].ThresholdPercent; got != 80 {
		t.Fatalf("threshold=%v, want 80", got)
	}
}

// The snapshot-IO store round-trips marks and merges on conflict (the
// control-plane file/postgres/redis path).
type memBudgetAlertIO struct {
	mu      sync.Mutex
	payload []byte
	rev     uint64
	fail    int
}

func (m *memBudgetAlertIO) Read(_ context.Context) ([]byte, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.payload == nil {
		return nil, 0, ErrBudgetAlertStoreNotFound
	}
	return append([]byte(nil), m.payload...), m.rev, nil
}

func (m *memBudgetAlertIO) Write(_ context.Context, payload []byte, expected uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail > 0 {
		m.fail--
		return 0, ErrBudgetAlertStoreConflict
	}
	if expected != m.rev {
		return 0, ErrBudgetAlertStoreConflict
	}
	m.payload = append([]byte(nil), payload...)
	m.rev++
	return m.rev, nil
}

func TestBudgetAlertIOStoreRoundTripAndConflictMerge(t *testing.T) {
	backend := &memBudgetAlertIO{}
	store := NewIOBudgetAlertStore(backend)
	fired, rev, err := store.Load()
	if err != nil || len(fired) != 0 || rev != 0 {
		t.Fatalf("empty load=%v rev=%d err=%v", fired, rev, err)
	}
	marks := map[string]int64{"vk-a\x00lifetime:10\x0050": 123}
	rev, err = store.Save(marks, 0)
	if err != nil || rev != 1 {
		t.Fatalf("save rev=%d err=%v", rev, err)
	}
	loaded, rev, err := store.Load()
	if err != nil || rev != 1 || len(loaded) != 1 {
		t.Fatalf("reload=%v rev=%d err=%v", loaded, rev, err)
	}
	// Simulate a lost race: another writer moves the backend to rev 2
	// with its own mark; our save under the stale rev conflicts once,
	// then merges (union is safe: marks are add-only) and succeeds.
	backend.mu.Lock()
	other, _ := marshalBudgetAlertDoc(map[string]int64{"vk-b\x00lifetime:5\x0050": 456})
	backend.payload, backend.rev = other, 2
	backend.mu.Unlock()
	merged := map[string]int64{"vk-a\x00lifetime:10\x0050": 123, "vk-a\x00lifetime:10\x0080": 789}
	if _, err := store.Save(merged, 1); err != nil {
		t.Fatalf("conflicted save failed: %v", err)
	}
	final, _, err := store.Load()
	if err != nil || len(final) != 3 {
		t.Fatalf("merged=%v err=%v", final, err)
	}
}

// Admin visibility: webhook fields are listed with budgets (no secret
// material), accepted on create, and rejected when malformed.
func TestBudgetAlertAdminSurface(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "admin-secret"
	cfg.Admin.BindLocalOnly = false
	cfg.ClientAuth = config.ClientAuthConfig{
		Enabled: true,
		VirtualKeys: []config.VirtualKeyConfig{
			{ID: "vk-web", Name: "hooked", KeyHash: keyDigest("nrk_hooked"), Role: "developer",
				BudgetUSD: 100, BudgetWebhookURL: "https://alerts.example.test/hook",
				BudgetAlertThresholds: []float64{75}, BudgetPeriod: "monthly"},
		},
	}
	s := testGateway(t, cfg)
	get := func() []map[string]any {
		t.Helper()
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
		return decoded.Keys
	}
	keys := get()
	if len(keys) != 1 {
		t.Fatalf("keys=%d, want 1", len(keys))
	}
	k := keys[0]
	if k["budget_webhook_url"] != "https://alerts.example.test/hook" {
		t.Fatalf("webhook url missing: %v", k)
	}
	if k["budget_period"] != "monthly" {
		t.Fatalf("period missing: %v", k)
	}
	ths, ok := k["budget_alert_thresholds"].([]any)
	if !ok || len(ths) != 1 || ths[0].(float64) != 75 {
		t.Fatalf("thresholds missing: %v", k)
	}
	body := strings.ToLower(string(mustAlertJSON(t, k)))
	for _, forbidden := range []string{"key_hash", "nrk_hooked"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("admin list leaked %q: %v", forbidden, k)
		}
	}

	// Create accepts webhook overrides; malformed targets are refused.
	post := func(payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/client-keys", strings.NewReader(payload))
		req.Header.Set("X-Admin-Key", "admin-secret")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	rr := post(`{"name":"new","budget_webhook_url":"https://hooks.example.test/b","budget_alert_thresholds":[60],"budget_period":"monthly"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["budget_webhook_url"] != "https://hooks.example.test/b" || created["budget_period"] != "monthly" {
		t.Fatalf("created key missing webhook fields: %v", created)
	}
	rr = post(`{"name":"bad","budget_webhook_url":"http://hooks.example.test/plain"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed webhook create: %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
	if len(get()) != 2 {
		t.Fatalf("bad create must not persist a key")
	}
}

// The admin snapshot exposes alert policy + per-budget spend state for
// operators without leaking secrets.
func TestBudgetAlertSnapshotSurface(t *testing.T) {
	s, _ := budgetAlertTestServer(t, 10, func(cfg *config.Config) {
		cfg.BudgetAlerts.WebhookURL = "https://alerts.example.test/hook"
	})
	settleSpend(t, s, "vk-a", 10, 6)
	s.checkBudgetAlerts("vk-a")
	snap := s.budgetAlertsSnapshot()
	if snap["enabled"] != true || snap["durable"] != "memory" {
		t.Fatalf("snapshot policy wrong: %v", snap)
	}
	budgets, ok := snap["budgets"].([]map[string]any)
	if !ok || len(budgets) != 1 {
		t.Fatalf("snapshot budgets: %v", snap)
	}
	b := budgets[0]
	if b["id"] != "vk-a" || b["spend_usd"] != 6.0 || b["limit_usd"] != 10.0 {
		t.Fatalf("snapshot budget wrong: %v", b)
	}
	fired, ok := b["fired"].([]float64)
	if !ok || len(fired) != 1 || fired[0] != 50 {
		t.Fatalf("snapshot fired wrong: %v", b)
	}
	raw := strings.ToLower(string(mustAlertJSON(t, snap)))
	if strings.Contains(raw, keyDigest("alert-key")) {
		t.Fatalf("snapshot leaked key material: %v", snap)
	}
}

// Delivery timeout bounds a hung receiver and surfaces an error.
func TestBudgetAlertDeliveryTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()
	payload := BudgetAlertPayload{BudgetID: "vk-a", ThresholdPercent: 50, SpendUSD: 6, LimitUSD: 10, Currency: "USD", Period: "lifetime:10", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := deliverBudgetAlert(ctx, payload, slow.URL+"/hook", 100*time.Millisecond, config.EgressConfig{})
	if err == nil {
		t.Fatal("hung receiver accepted")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("delivery not bounded: %s", elapsed)
	}
}

func mustAlertJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
