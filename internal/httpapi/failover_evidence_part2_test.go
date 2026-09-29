package httpapi

// Part A2: never-stop evidence.
// A fails, B fails, C recovers, D is added live via the admin API; traffic
// continues with no restart (single Server instance serves throughout).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestFailoverEvidence_NeverStop_LiveAddViaAdmin(t *testing.T) {
	var aHits, bHits, cHits, dHits atomic.Int64
	fail := func(hits *atomic.Int64, status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	a := fail(&aHits, http.StatusTooManyRequests, `{"error":{"message":"a limited","type":"rate_limit_error"}}`)
	defer a.Close()
	b := fail(&bHits, http.StatusTooManyRequests, `{"error":{"message":"b limited","type":"rate_limit_error"}}`)
	defer b.Close()

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
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"c-up","choices":[{"index":0,"message":{"role":"assistant","content":"C_ONLY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer c.Close()
	d := fail(&dHits, http.StatusOK, `{"id":"d","object":"chat.completion","model":"d-up","choices":[{"index":0,"message":{"role":"assistant","content":"D_ONLY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	defer d.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.FailureThreshold = 5 // single 500 keeps C eligible (degraded)
	cfg.Routing.CooldownSeconds = 1800
	cfg.Routing.ProviderFailureThreshold = 100
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "openai_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1}}},
		{ID: "C", Name: "C", Type: "openai_compatible", BaseURL: c.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "shared-model", Aliases: []string{"coding"}, Enabled: true, Priority: 2, Weight: 1}}},
	}
	s := testGateway(t, cfg) // single instance: no restart allowed below
	doReq := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)))
		return rr
	}

	// Phase 1: A (429 hard cooldown), B (429 hard cooldown), C (500 degraded).
	if rr := doReq(); rr.Code == http.StatusOK {
		t.Fatalf("all deployments down: expected failure, got %s", rr.Body.String())
	}
	if aHits.Load() != 1 || bHits.Load() != 1 || cHits.Load() != 1 {
		t.Fatalf("expected one attempt per deployment, got A=%d B=%d C=%d", aHits.Load(), bHits.Load(), cHits.Load())
	}

	// Phase 2: C recovers upstream; traffic continues through C.
	cBroken.Store(false)
	if rr := doReq(); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "C_ONLY") {
		t.Fatalf("C recovery did not resume traffic: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Phase 3: D is added live via the admin API (no restart, same Server).
	addPayload, _ := json.Marshal(map[string]any{"provider": map[string]any{
		"id": "D", "name": "D", "type": "openai_compatible", "base_url": d.URL,
		"auth_mode": "none", "enabled": true,
		"models": []any{map[string]any{"id": "m", "model": "shared-model", "aliases": []string{"coding"}, "enabled": true, "priority": 3, "weight": 1}},
	}})
	if rr := adminRequest(s, http.MethodPost, "/admin/api/providers", string(addPayload)); rr.Code != http.StatusCreated {
		t.Fatalf("live admin add of D failed: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Phase 4: C breaks again; D absorbs traffic on the same instance.
	cBroken.Store(true)
	sawD := false
	for i := 0; i < 4; i++ {
		rr := doReq()
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
}
