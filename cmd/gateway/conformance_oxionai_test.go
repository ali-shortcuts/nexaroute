package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConformanceCanaryFreeModelStyleOpenAICompatibleClaudeModel(t *testing.T) {
	var hits atomic.Int64
	const testKey = "fixture-freemodel-key-not-a-real-credential"
	const modelID = "claude-family-fixture-model"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request path=%q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization=%q, want Bearer fixture key", got)
		}
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Model != modelID {
			t.Errorf("request model=%q, want %q", request.Model, modelID)
		}
		if len(request.Messages) == 0 || request.Messages[0].Role != "user" {
			t.Errorf("request messages=%+v, want a user message", request.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture-1","object":"chat.completion","model":"` + modelID + `","choices":[{"index":0,"message":{"role":"assistant","content":"fixture response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	t.Setenv("NEXAROUTE_CANARY_API_KEY", testKey)
	out := filepath.Join(t.TempDir(), "freemodel-style-canary.json")
	var stdout, stderr strings.Builder
	code := runConformance([]string{
		"canary", "--acknowledge-cost", "--preset", "openai", "--model", modelID,
		"--base-url", server.URL + "/v1", "--auth-mode", "bearer",
		"--scenarios", "text", "--max-calls", "1", "--max-output-tokens", "32",
		"--input-cost-per-mtok", "0.1", "--output-cost-per-mtok", "0.2",
		"--max-cost-usd", "1", "--out", out,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", hits.Load())
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Outcome  string `json:"outcome"`
		Provider struct {
			Dialect string `json:"dialect"`
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "PASS" {
		t.Fatalf("outcome=%q, want PASS", report.Outcome)
	}
	if report.Provider.Dialect != "openai_compatible" {
		t.Fatalf("dialect=%q, want openai_compatible", report.Provider.Dialect)
	}
	if report.Provider.BaseURL != server.URL+"/v1" {
		t.Fatalf("base_url=%q, want %q", report.Provider.BaseURL, server.URL+"/v1")
	}
	if report.Provider.Model != modelID {
		t.Fatalf("model=%q, want %q", report.Provider.Model, modelID)
	}
}
