package httpapi

// Wave C-4 (issue #405): resource-leak invariant matrix.
//
// For every gateway serve path that can reserve a budget hold, the
// ledger must return to a provably terminal state after the request
// completes. Three tiers:
//
//  1. Synchronous settle/release — reserved == 0 the moment the
//     request completes (normal/streaming completion, cache hits,
//     count_tokens, hedged winner, failover chain, admission
//     rejection).
//  2. Gateway-side rejection — zero upstream spend is provable, so
//     the admission hold must be RELEASED, never left to expire into
//     a committed charge (the #359 class; these are the Wave C-4
//     production fixes in openai.go, anthropic.go,
//     canonical_path.go, media.go).
//  3. Documented TTL behavior — upstream failures and client
//     disconnects keep the ledger's documented expire-into-committed
//     backstop ("a transport failure cannot make spend vanish"); the
//     invariant is that the TTL path completes exactly once: no
//     indefinite open reservation, no double commit.
//
// All tests are hermetic (httptest fakes, no external services) and
// run in the default pass and under -race.

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

// leakGateway builds a gateway with one budgeted virtual key
// ("lk-key" -> "vk-lk") and one OpenAI-compatible deployment.
// mutate may adjust the config before the server is built.
func leakGateway(t *testing.T, budget float64, up http.Handler, mutate func(*config.Config)) *Server {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
		{ID: "vk-lk", KeyHash: keyDigest("lk-key"), BudgetUSD: budget},
	}
	cfg.Providers = []config.ProviderConfig{{
		ID: "llm", Name: "LLM", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "chat-model", Enabled: true, Weight: 1,
			InputCostPerMTok: 10, OutputCostPerMTok: 30,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	if mutate != nil {
		mutate(&cfg)
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func leakDo(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(body))
	req.Header.Set("x-api-key", "lk-key")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

// leakCommittedReserved returns (committed, reserved) for the test key.
func leakCommittedReserved(s *Server) (float64, float64) {
	return s.budgets.Snapshot("vk-lk", time.Now())
}

// leakAssertSettled: the reservation closed synchronously.
func leakAssertSettled(t *testing.T, s *Server, where string) {
	t.Helper()
	_, reserved := leakCommittedReserved(s)
	if reserved != 0 {
		t.Fatalf("%s: leaked budget hold: reserved=%v (would commit at estimate after the TTL)", where, reserved)
	}
	_, open, _ := s.budgets.Stats()
	if open != 0 {
		t.Fatalf("%s: %d budget reservations still open", where, open)
	}
}

// leakAssertReleased: zero-spend path — no hold, no commit, and the
// ledger row itself must be evicted (a zero-spend row left behind
// leaks rows across rotated/ephemeral key IDs).
func leakAssertReleased(t *testing.T, s *Server, where string, committedBefore float64) {
	t.Helper()
	leakAssertSettled(t, s, where)
	committed, _ := leakCommittedReserved(s)
	if math.Abs(committed-committedBefore) > 1e-9 {
		t.Fatalf("%s: zero-spend path changed committed spend: %v -> %v", where, committedBefore, committed)
	}
	rows, _, _ := s.budgets.Stats()
	if rows != 0 && committedBefore == 0 {
		t.Fatalf("%s: ledger rows=%d after a zero-spend request, want 0 (row not evicted)", where, rows)
	}
}

// leakAssertTTLExactlyOnce: the documented backstop — the hold may be
// open right after the request, but advancing past the TTL must
// close it with exactly one committed estimate and zero open
// reservations.
func leakAssertTTLExactlyOnce(t *testing.T, s *Server, where string) {
	t.Helper()
	_, reserved := leakCommittedReserved(s)
	if reserved <= 0 {
		t.Fatalf("%s: expected an open hold pending TTL expiry, reserved=%v", where, reserved)
	}
	estimate := reserved
	committed, reservedAfter := s.budgets.Snapshot("vk-lk", time.Now().Add(16*time.Minute))
	if reservedAfter != 0 {
		t.Fatalf("%s: TTL did not clear the hold: reserved=%v", where, reservedAfter)
	}
	if math.Abs(committed-estimate) > 1e-9 {
		t.Fatalf("%s: TTL committed %v, want exactly the estimate %v (exactly-once)", where, committed, estimate)
	}
	_, open, _ := s.budgets.Stats()
	if open != 0 {
		t.Fatalf("%s: %d reservations still open after TTL", where, open)
	}
}

func leakOKChat() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	})
}

const leakChatBody = `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

// Tier 1: normal completion settles the admission hold.
func TestLeakMatrix_NormalCompletionSettles(t *testing.T) {
	s := leakGateway(t, 5, leakOKChat(), nil)
	rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rr.Code, rr.Body.String())
	}
	committed, _ := leakCommittedReserved(s)
	if committed <= 0 {
		t.Fatalf("normal completion committed nothing: %v", committed)
	}
	leakAssertSettled(t, s, "normal completion")
}

// Tier 1: streaming completion settles the admission hold.
func TestLeakMatrix_StreamCompletionSettles(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":100}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	s := leakGateway(t, 5, up, nil)
	rr := leakDo(t, s, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("stream chat: %d %s", rr.Code, rr.Body.String())
	}
	committed, _ := leakCommittedReserved(s)
	if committed <= 0 {
		t.Fatalf("stream completion committed nothing: %v", committed)
	}
	leakAssertSettled(t, s, "stream completion")
}

// Tier 1: exact-cache hit releases the admission hold without
// touching committed spend.
func TestLeakMatrix_ExactCacheHitReleases(t *testing.T) {
	var calls atomic.Int64
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	})
	s := leakGateway(t, 5, up, func(cfg *config.Config) { cfg.Cache.Enabled = true })
	if rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody); rr.Code != http.StatusOK {
		t.Fatalf("first: %d", rr.Code)
	}
	c1, _ := leakCommittedReserved(s)
	rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody)
	if rr.Code != http.StatusOK || rr.Header().Get("X-NexaRoute-Cache") != "HIT" {
		t.Fatalf("second must be an exact cache hit: code=%d cache=%q", rr.Code, rr.Header().Get("X-NexaRoute-Cache"))
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}
	leakAssertReleased(t, s, "exact cache hit", c1)
}

// Tier 1: a hedged request settles exactly once — the winner's usage
// wins, the cancelled loser records nothing, one reservation closes.
func TestLeakMatrix_HedgedWinnerSettlesOnce(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/slow/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"slow"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	}))
	mux.Handle("/fast/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fast"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	}))
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)
	mkModel := func() config.ModelConfig {
		return config.ModelConfig{ID: "m", Model: "chat-model", Enabled: true, Weight: 1,
			InputCostPerMTok: 10, OutputCostPerMTok: 30, Capabilities: config.Capabilities{Streaming: true}}
	}
	s := leakGateway(t, 50, leakOKChat(), func(cfg *config.Config) {
		cfg.Routing.HedgingEnabled = true
		cfg.Routing.HedgingDelayMS = 5
		cfg.Providers = []config.ProviderConfig{
			{ID: "slow", Name: "Slow", Type: "openai_compatible", BaseURL: up.URL + "/slow", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{mkModel()}},
			{ID: "fast", Name: "Fast", Type: "openai_compatible", BaseURL: up.URL + "/fast", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{mkModel()}},
		}
	})
	rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("hedged chat: %d %s", rr.Code, rr.Body.String())
	}
	committed, _ := leakCommittedReserved(s)
	// Both deployments price identically ($0.013): exactly one actual
	// cost may be committed — never zero, never double.
	if committed <= 0 || committed > 0.013*1.5 {
		t.Fatalf("hedged request committed=%v, want exactly one actual cost (~0.013)", committed)
	}
	leakAssertSettled(t, s, "hedged request")
}

// Tier 1: a failover chain settles exactly once on the winning
// attempt; failed attempts record no usage and open no second hold.
func TestLeakMatrix_FailoverChainSettlesOnce(t *testing.T) {
	var badCalls atomic.Int64
	mux := http.NewServeMux()
	mux.Handle("/bad/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	mux.Handle("/good/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	}))
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)
	mkModel := func(w float64) config.ModelConfig {
		return config.ModelConfig{ID: "m", Model: "chat-model", Enabled: true, Weight: w,
			InputCostPerMTok: 10, OutputCostPerMTok: 30, Capabilities: config.Capabilities{Streaming: true}}
	}
	s := leakGateway(t, 50, leakOKChat(), func(cfg *config.Config) {
		cfg.Providers = []config.ProviderConfig{
			{ID: "bad", Name: "Bad", Type: "openai_compatible", BaseURL: up.URL + "/bad", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{mkModel(10)}},
			{ID: "good", Name: "Good", Type: "openai_compatible", BaseURL: up.URL + "/good", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{mkModel(1)}},
		}
	})
	rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("failover chat: %d %s", rr.Code, rr.Body.String())
	}
	if badCalls.Load() == 0 {
		t.Fatalf("expected the bad deployment to be tried first")
	}
	committed, _ := leakCommittedReserved(s)
	if committed <= 0 || committed > 0.013*1.5 {
		t.Fatalf("failover chain committed=%v, want exactly one actual cost", committed)
	}
	leakAssertSettled(t, s, "failover chain")
}

// Tier 1: admission rejection (402) takes no hold at all.
func TestLeakMatrix_AdmissionRejectionTakesNoHold(t *testing.T) {
	s := leakGateway(t, 0.000001, leakOKChat(), nil)
	rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody)
	if rr.Code != 402 {
		t.Fatalf("want 402 budget-exceeded, got %d %s", rr.Code, rr.Body.String())
	}
	leakAssertReleased(t, s, "admission rejection", 0)
}

// Tier 2: route-resolution failure is a gateway-side rejection with
// provably zero upstream spend — the admission hold must be
// released (Wave C-4 fix), not left to expire into a committed
// charge. Covers the chat, anthropic and responses handlers.
func TestLeakMatrix_RouteResolutionFailureReleases(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"openai_chat", "/v1/chat/completions", leakChatBody},
		{"anthropic_messages", "/v1/messages", `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
		{"openai_responses", "/v1/responses", `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := leakGateway(t, 5, leakOKChat(), nil)
			// Trip the only deployment: route resolution fails, but
			// admission already took a hold (budgetEstimateFor works
			// off config, not health).
			s.hm.Quarantine("llm/m", "test trip", time.Second)
			rr := leakDo(t, s, "POST", tc.path, tc.body)
			if rr.Code != 503 {
				t.Fatalf("guard: want 503 no-healthy-deployment, got %d %s", rr.Code, rr.Body.String())
			}
			leakAssertReleased(t, s, tc.name+" route-resolution failure", 0)
		})
	}
}

// Tier 2: media gateway-side rejections release the admission hold.
func TestLeakMatrix_MediaEarlyReturnReleases(t *testing.T) {
	s := leakGateway(t, 5, leakOKChat(), nil)
	s.hm.Quarantine("llm/m", "test trip", time.Second)
	rr := leakDo(t, s, "POST", "/v1/images/generations", `{"model":"m","prompt":"a cat"}`)
	if rr.Code != 503 {
		t.Fatalf("guard: want 503 no-healthy-deployment, got %d %s", rr.Code, rr.Body.String())
	}
	leakAssertReleased(t, s, "media early return", 0)
}

// Tier 2: when no media provider supports the kind, no upstream
// attempt is possible — the hold must be released.
func TestLeakMatrix_MediaUnsupportedProviderReleases(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(up.Close)
	s := leakGateway(t, 5, leakOKChat(), func(cfg *config.Config) {
		// Anthropic-compatible deployments do not support media kinds.
		cfg.Providers[0].Type = "anthropic_compatible"
		cfg.Providers[0].Models[0].Model = "claude-model"
	})
	rr := leakDo(t, s, "POST", "/v1/images/generations", `{"model":"m","prompt":"a cat"}`)
	if rr.Code != 400 {
		t.Fatalf("guard: want 400 unsupported-media, got %d %s", rr.Code, rr.Body.String())
	}
	leakAssertReleased(t, s, "media unsupported provider", 0)
}

// Tier 3: all upstream attempts fail — the documented TTL backstop
// applies: the hold stays open, then expires into exactly one
// committed estimate, never more, never an open reservation.
func TestLeakMatrix_UpstreamFailureExpiresViaTTL(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})
	s := leakGateway(t, 5, up, nil)
	rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody)
	if rr.Code < 500 {
		t.Fatalf("guard: want 5xx, got %d", rr.Code)
	}
	leakAssertTTLExactlyOnce(t, s, "upstream failure")
}

// Tier 3: transport failure (unreachable upstream) — same TTL
// backstop, exactly once.
func TestLeakMatrix_TransportFailureExpiresViaTTL(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unreachable upstream must not be hit")
	})
	s := leakGateway(t, 5, up, func(cfg *config.Config) {
		cfg.Providers[0].BaseURL = "http://127.0.0.1:1"
	})
	rr := leakDo(t, s, "POST", "/v1/chat/completions", leakChatBody)
	if rr.Code < 500 {
		t.Fatalf("guard: want 5xx, got %d", rr.Code)
	}
	leakAssertTTLExactlyOnce(t, s, "transport failure")
}

// Tier 3: media success without usage accounting (non-embeddings)
// keeps the documented conservative behavior: the admission
// estimate is committed at the TTL, exactly once. This test pins
// the behavior so any future change is deliberate.
func TestLeakMatrix_MediaSuccessExpiresViaTTL(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"http://example/img.png"}]}`))
	})
	s := leakGateway(t, 5, up, nil)
	rr := leakDo(t, s, "POST", "/v1/images/generations", `{"model":"m","prompt":"a cat"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("guard: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	leakAssertTTLExactlyOnce(t, s, "media success (untracked usage)")
}
