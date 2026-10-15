package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M5-4 semantic cache e2e. The embeddings fake maps prompt keywords
// to a 2-dim vector space: prompts about France land on [1,0],
// about Italy on [0,1] — paraphrases of the same question produce
// the same vector, different questions are orthogonal.

func semanticFixture(t *testing.T, mutate func(*config.Config)) (*Server, *atomic.Int64) {
	t.Helper()
	var chatCalls atomic.Int64
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Paris."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	t.Cleanup(chat.Close)
	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vec := "[0.0,0.0]"
		lower := strings.ToLower(string(body))
		switch {
		case strings.Contains(lower, "france"), strings.Contains(lower, "french"), strings.Contains(lower, "paris"):
			vec = "[1.0,0.0]"
		case strings.Contains(lower, "italy"), strings.Contains(lower, "italian"), strings.Contains(lower, "rome"):
			vec = "[0.0,1.0]"
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
	cfg.Providers = []config.ProviderConfig{
		{ID: "llm", Name: "LLM", Type: "openai_compatible", BaseURL: chat.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "chat-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "emb", Name: "Emb", Type: "openai_compatible", BaseURL: emb.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "e1", Model: "emb-model", Enabled: true, Weight: 1}}},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s, &chatCalls
}

func semanticChat(t *testing.T, s *Server, prompt, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"m","messages":[{"role":"user","content":"` + prompt + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestSemanticCacheParaphraseHitAndMiss(t *testing.T) {
	s, calls := semanticFixture(t, nil)

	rr := semanticChat(t, s, "What is the capital of France?", "")
	if rr.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("first request: status=%d calls=%d", rr.Code, calls.Load())
	}
	// A paraphrase (different body -> exact miss) with the same
	// embedding must be served semantically without an upstream call.
	rr = semanticChat(t, s, "Tell me the French capital city", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("paraphrase: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-NexaRoute-Cache"); got != "SEMANTIC-HIT" {
		t.Fatalf("expected SEMANTIC-HIT, got %q (calls=%d, body=%s)", got, calls.Load(), rr.Body.String())
	}
	if prov := rr.Header().Get("X-Gateway-Cache-Provenance"); !strings.Contains(prov, "deployment=llm/m") {
		t.Fatalf("provenance header missing/wrong: %q", prov)
	}
	if calls.Load() != 1 {
		t.Fatalf("semantic hit still called upstream: %d", calls.Load())
	}
	// An orthogonal question misses and goes upstream.
	rr = semanticChat(t, s, "What is the capital of Italy?", "")
	if rr.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("italy: status=%d calls=%d", rr.Code, calls.Load())
	}
	if got := rr.Header().Get("X-NexaRoute-Cache"); got == "SEMANTIC-HIT" {
		t.Fatal("orthogonal question served from semantic cache")
	}
}

func TestSemanticCacheClientIsolation(t *testing.T) {
	s, calls := semanticFixture(t, func(cfg *config.Config) {
		cfg.ClientAuth.Enabled = true
		cfg.ClientAuth.Keys = []string{"key-one", "key-two"}
	})
	if rr := semanticChat(t, s, "What is the capital of France?", "key-one"); rr.Code != http.StatusOK {
		t.Fatalf("key-one first: %d", rr.Code)
	}
	// Same paraphrase, DIFFERENT client key: different scope -> miss.
	rr := semanticChat(t, s, "Tell me the French capital city", "key-two")
	if got := rr.Header().Get("X-NexaRoute-Cache"); got == "SEMANTIC-HIT" {
		t.Fatal("semantic hit crossed client isolation scopes")
	}
	if calls.Load() != 2 {
		t.Fatalf("isolated client should have gone upstream: calls=%d", calls.Load())
	}
	// Same client again -> hit.
	rr = semanticChat(t, s, "Tell me the French capital city", "key-one")
	if got := rr.Header().Get("X-NexaRoute-Cache"); got != "SEMANTIC-HIT" {
		t.Fatalf("same-client paraphrase should hit, got %q", got)
	}
}

func TestSemanticCachePrivacyGateEndToEnd(t *testing.T) {
	// The storing request runs WITHOUT a privacy requirement
	// against a provider that trains on data; a later no_training
	// request (virtual key) must NOT be served that entry even with
	// an identical prompt — it goes upstream to the eligible
	// no-training deployment instead.
	s, _ := semanticFixture(t, func(cfg *config.Config) {
		cfg.Providers[0].DataHandling = config.DataHandlingConfig{TrainsOnData: "yes"}
		cfg.ClientAuth.Enabled = true
		cfg.ClientAuth.Keys = []string{"plain-key"}
		cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
			{ID: "vk-private", KeyHash: keyDigest("private-key"), RequirePrivacy: "no_training"},
		}
	})
	rr := semanticChat(t, s, "What is the capital of France?", "plain-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("store request: %d %s", rr.Code, rr.Body.String())
	}
	rr = semanticChat(t, s, "Tell me the French capital city", "private-key")
	if got := rr.Header().Get("X-NexaRoute-Cache"); got == "SEMANTIC-HIT" {
		t.Fatal("no_training request served an entry lacking no_training provenance")
	}
}

func TestSemanticCacheDisabledByDefault(t *testing.T) {
	s, calls := semanticFixture(t, func(cfg *config.Config) {
		cfg.Cache.Semantic.Enabled = false
	})
	_ = semanticChat(t, s, "What is the capital of France?", "")
	rr := semanticChat(t, s, "Tell me the French capital city", "")
	if got := rr.Header().Get("X-NexaRoute-Cache"); got == "SEMANTIC-HIT" {
		t.Fatal("semantic hit while disabled")
	}
	if calls.Load() != 2 {
		t.Fatalf("disabled semantic cache should not dedupe: calls=%d", calls.Load())
	}
}
