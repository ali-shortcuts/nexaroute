package httpapi

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Regression (audit 2026-10-15): a semantic-cache hit is a
// zero-upstream-spend serve, but semanticServe never released the
// admission budget hold, so the reservation expired 15 minutes
// later into a committed charge at the full estimate. The
// exact-match cache path released correctly; the semantic path
// must too.
func TestSemanticCacheHitReleasesBudgetHold(t *testing.T) {
	var chatCalls atomic.Int64
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Paris."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	}))
	t.Cleanup(chat.Close)
	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vec := "[0.0,0.0]"
		if strings.Contains(strings.ToLower(string(body)), "france") || strings.Contains(strings.ToLower(string(body)), "french") {
			vec = "[1.0,0.0]"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":`+vec+`}]}`)
	}))
	t.Cleanup(emb.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Cache.Enabled = true
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "emb/e1"
	cfg.Cache.Semantic.Threshold = 0.9
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk-sem", KeyHash: keyDigest("sem-key"), BudgetUSD: 5},
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "llm", Name: "LLM", Type: "openai_compatible", BaseURL: chat.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "chat-model", Enabled: true, Weight: 1, InputCostPerMTok: 10, OutputCostPerMTok: 30, Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "emb", Name: "Emb", Type: "openai_compatible", BaseURL: emb.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "e1", Model: "emb-model", Enabled: true, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	do := func(prompt string) *httptest.ResponseRecorder {
		body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"` + prompt + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("x-api-key", "sem-key")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := do("What is the capital of France?"); rr.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", rr.Code, rr.Body.String())
	}
	committed1, reserved1 := s.budgets.Snapshot("vk-sem", time.Now())
	if committed1 <= 0 || reserved1 != 0 {
		t.Fatalf("after upstream serve: ledger=(%v,%v), want committed>0 reserved=0", committed1, reserved1)
	}
	rr := do("Tell me the French capital city.")
	if rr.Code != http.StatusOK || rr.Header().Get("X-NexaRoute-Cache") != "SEMANTIC-HIT" {
		t.Fatalf("second request must be a semantic hit: code=%d cache=%q body=%s", rr.Code, rr.Header().Get("X-NexaRoute-Cache"), rr.Body.String())
	}
	committed2, reserved2 := s.budgets.Snapshot("vk-sem", time.Now())
	if reserved2 != 0 {
		t.Fatalf("semantic hit leaked its budget hold: reserved=%v (would commit at estimate after the TTL)", reserved2)
	}
	if math.Abs(committed2-committed1) > 1e-9 {
		t.Fatalf("semantic hit changed committed spend: %v -> %v", committed1, committed2)
	}
	if chatCalls.Load() != 1 {
		t.Fatalf("chat upstream calls=%d, want 1 (second served from semantic cache)", chatCalls.Load())
	}
}

// Regression (audit 2026-10-15): count_tokens took an admission
// reservation in clientAuthAllowed and never settled or released
// it on any path, so token counting silently consumed budget at
// chat-completion estimates.
func TestCountTokensReleasesBudgetHold(t *testing.T) {
	s := budgetGateway(t, 5, false)
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("x-api-key", "budget-key")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("count_tokens: %d %s", rr.Code, rr.Body.String())
	}
	committed, reserved := s.budgets.Snapshot("vk-b", time.Now())
	if committed != 0 || reserved != 0 {
		t.Fatalf("count_tokens leaked its budget hold: ledger=(%v,%v), want (0,0)", committed, reserved)
	}
}
