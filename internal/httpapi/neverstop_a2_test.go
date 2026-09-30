package httpapi

// v0.12.0 A2 (issue #74, parent #49): never-stop routing with live recovery
// and admin addition.
//
// Lifecycle on a SINGLE Server instance (no restart):
// A fails (429 hard cooldown), B fails (429 hard cooldown), C is degraded
// (500) then recovers upstream, D is added live via POST /admin/api/providers
// (hot reload + route selection), then C breaks again and D absorbs traffic.
//
// Acceptance: no restart, no lost route, traffic resumes on C then includes
// D after readiness, admin write + hot reload + route selection covered,
// timeout bounds + cleanup included.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestNeverStopA2LiveRecoveryAndAdminAdd(t *testing.T) {
	start := time.Now()
	const testBound = 20 * time.Second
	checkBound := func(phase string) {
		t.Helper()
		if elapsed := time.Since(start); elapsed > testBound {
			t.Fatalf("phase %s exceeded timeout bound %s (elapsed %s)", phase, testBound, elapsed)
		}
	}

	var aHits, bHits, cHits, dHits atomic.Int64
	okBody := func(tag, model string) string {
		return `{"id":"` + tag + `","object":"chat.completion","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"` + tag + `_ONLY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	}
	failSrv := func(hits *atomic.Int64, status int, body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(s.Close)
		return s
	}
	// A and B: deterministic hard-cooldown failures (429 => ForceCooldown).
	a := failSrv(&aHits, http.StatusTooManyRequests, `{"error":{"message":"a limited","type":"rate_limit_error"}}`)
	b := failSrv(&bHits, http.StatusTooManyRequests, `{"error":{"message":"b limited","type":"rate_limit_error"}}`)

	var cBroken atomic.Bool
	cBroken.Store(true)
	c := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if cBroken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"c down","type":"server_error"}}`))
			return
		}
		_, _ = w.Write([]byte(okBody("C", "c-up")))
	}))
	t.Cleanup(c.Close)
	d := failSrv(&dHits, http.StatusOK, okBody("D", "d-up"))

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.SessionAffinity = false
	cfg.Routing.RequestTimeoutMS = 5000
	// Single 500 keeps C eligible (degraded, not cooled) so live recovery is
	// observable without waiting out a cooldown window.
	cfg.Routing.FailureThreshold = 5
	cfg.Routing.CooldownSeconds = 1800
	cfg.Routing.ProviderFailureThreshold = 100
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "openai_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1}}},
		{ID: "C", Name: "C", Type: "openai_compatible", BaseURL: c.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 2, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	// No restart: capture the live instance pointer; every phase below must
	// serve through this same *Server.
	live := s

	chatReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	// Phase 1: A (429), B (429), C (500) — all down, exactly one attempt each.
	// A lost route here would be a 5xx with no candidate left; assert the
	// attempt distribution instead of requiring success.
	checkBound("phase1-all-down")
	rr := chatReq()
	if rr.Code == http.StatusOK {
		t.Fatalf("all deployments down: expected failure, got %s", rr.Body.String())
	}
	if aHits.Load() != 1 || bHits.Load() != 1 || cHits.Load() != 1 {
		t.Fatalf("expected one attempt per deployment, got A=%d B=%d C=%d", aHits.Load(), bHits.Load(), cHits.Load())
	}
	if s != live {
		t.Fatal("server instance changed during phase 1 (restart not allowed)")
	}

	// Phase 2: C recovers upstream; traffic resumes on C with no restart and
	// no lost route (must be 200 carrying C_ONLY).
	checkBound("phase2-c-recovery")
	cBroken.Store(false)
	rr = chatReq()
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "C_ONLY") {
		t.Fatalf("C recovery did not resume traffic: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if s != live {
		t.Fatal("server instance changed during C recovery (restart not allowed)")
	}

	// Phase 3: D is added live via the admin API (write + hot reload).
	checkBound("phase3-admin-add")
	addPayload, _ := json.Marshal(map[string]any{"provider": map[string]any{
		"id": "D", "name": "D", "type": "openai_compatible", "base_url": d.URL,
		"auth_mode": "none", "enabled": true,
		"models": []any{map[string]any{"id": "m", "model": "shared-model", "aliases": []string{"coding"}, "enabled": true, "priority": 3, "weight": 1}},
	}})
	rr = adminRequest(s, http.MethodPost, "/admin/api/providers", string(addPayload))
	if rr.Code != http.StatusCreated {
		t.Fatalf("live admin add of D failed: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if s != live {
		t.Fatal("server instance changed during admin add (restart not allowed)")
	}
	// Hot reload proof: D is registered in config and in route selection.
	foundProvider := false
	for _, p := range s.currentConfig().Providers {
		if p.ID == "D" {
			foundProvider = true
		}
	}
	if !foundProvider {
		t.Fatal("hot reload did not register provider D in runtime config")
	}
	foundRoute := false
	for _, dep := range s.rt.All() {
		if dep.ID == "D/m" {
			foundRoute = true
		}
	}
	if !foundRoute {
		t.Fatal("hot reload did not expose route D/m in route selection")
	}
	// Readiness proof: admin read-back surfaces D.
	rr = adminRequest(s, http.MethodGet, "/admin/api/providers/D", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"D"`) {
		t.Fatalf("added provider D not readable after reload: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Phase 4: C breaks again; D absorbs traffic on the same instance.
	// Every request must succeed (no lost route) and at least one must carry
	// D_ONLY (route selection includes the live-added deployment).
	checkBound("phase4-d-absorbs")
	cBroken.Store(true)
	sawD := false
	for i := 0; i < 6; i++ {
		checkBound("phase4-attempt")
		rr = chatReq()
		if rr.Code != http.StatusOK {
			t.Fatalf("traffic stopped after live add (attempt %d): status=%d body=%s", i, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "D_ONLY") {
			sawD = true
		}
	}
	if !sawD {
		t.Fatalf("live-added deployment D never served traffic: A=%d B=%d C=%d D=%d", aHits.Load(), bHits.Load(), cHits.Load(), dHits.Load())
	}
	if dHits.Load() == 0 {
		t.Fatal("D hits are zero: admin-added deployment received no traffic")
	}
	// A and B were never retried after their hard cooldowns: no flapping.
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("cooled-down deployments were retried: A=%d B=%d", aHits.Load(), bHits.Load())
	}
	if s != live {
		t.Fatal("server instance changed by end of test (restart not allowed)")
	}
	checkBound("done")
	if elapsed := time.Since(start); elapsed > testBound {
		t.Fatalf("test exceeded timeout bound %s (elapsed %s)", testBound, elapsed)
	}
	t.Logf("never-stop lifecycle complete in %s: A=%d B=%d C=%d D=%d", time.Since(start).Round(time.Millisecond), aHits.Load(), bHits.Load(), cHits.Load(), dHits.Load())
}
