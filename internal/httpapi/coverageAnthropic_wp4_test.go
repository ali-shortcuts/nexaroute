package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func coverageAnthropicRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func coverageAnthropicConfig(providerType, baseURL string) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Providers = []config.ProviderConfig{{
		ID: "coverage-anthropic-provider", Name: "coverage-anthropic-provider", Type: providerType,
		BaseURL: baseURL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "coverage-anthropic-model", Model: "upstream-model", Aliases: []string{"coverage-client-model"},
			Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true, Reasoning: true},
		}},
	}}
	return cfg
}

func TestCoverageAnthropicValidationAndStatusContracts(t *testing.T) {
	s := testGateway(t, config.Default())
	cases := []struct {
		name, method, body string
		status             int
		want               string
	}{
		{"method", http.MethodGet, "", http.StatusMethodNotAllowed, "method not allowed"},
		{"malformed json", http.MethodPost, "{", http.StatusBadRequest, "invalid JSON"},
		{"required fields", http.MethodPost, `{}`, http.StatusBadRequest, "model and messages are required"},
		{"no deployment", http.MethodPost, `{"model":"coverage-client-model","messages":[{"role":"user","content":"hi"}]}`, http.StatusServiceUnavailable, "no compatible healthy deployment"},
		// A string tool choice takes the fallback parsing branch in feature extraction.
		{"string tool choice", http.MethodPost, `{"model":"coverage-client-model","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`, http.StatusServiceUnavailable, "no compatible healthy deployment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := coverageAnthropicRequest(s, tc.method, "/v1/messages", tc.body)
			if rr.Code != tc.status || !strings.Contains(rr.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%q want status=%d containing %q", rr.Code, rr.Body.String(), tc.status, tc.want)
			}
		})
	}
}

func TestCoverageAnthropicOpenAIStreamConvertsToolsTokensAndRepairsStreamOptions(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if calls.Add(1) == 1 {
			if _, ok := request["stream_options"]; !ok {
				t.Errorf("stream_options was not injected: %#v", request)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"stream_options is not supported"}}`)
			return
		}
		if _, ok := request["stream_options"]; ok {
			t.Errorf("stream_options was not stripped on repair: %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":[{"type":"text","text":"hello"},{"type":"ignored","text":"no"}]}}]}`,
			"",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2}}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n"))
	}))
	defer upstream.Close()

	cfg := coverageAnthropicConfig("openai_compatible", upstream.URL)
	s := testGateway(t, cfg)
	body := `{"model":"coverage-client-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"find","input_schema":{"type":"object"}}],"tool_choice":"required"}`
	rr := coverageAnthropicRequest(s, http.MethodPost, "/v1/messages", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d want repair retry plus successful stream", calls.Load())
	}
	out := rr.Body.String()
	for _, want := range []string{"event: message_start", "hello", `"type":"tool_use"`, `"name":"lookup"`, `input_json_delta`, `"input_tokens":7`, `"output_tokens":4`, `"cache_read_input_tokens":2`, `"stop_reason":"tool_use"`, "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream omitted %q: %s", want, out)
		}
	}
}

func TestCoverageAnthropicMalformedOpenAIResponseReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"not-enough"}`)
	}))
	defer upstream.Close()

	s := testGateway(t, coverageAnthropicConfig("openai_compatible", upstream.URL))
	rr := coverageAnthropicRequest(s, http.MethodPost, "/v1/messages", `{"model":"coverage-client-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "upstream returned an invalid response") {
		t.Fatalf("unexpected error body=%s", rr.Body.String())
	}
}

func TestCoverageAnthropicMalformedStreamEmitsTerminalErrorFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {bad-json}\n\n")
	}))
	defer upstream.Close()

	s := testGateway(t, coverageAnthropicConfig("openai_compatible", upstream.URL))
	rr := coverageAnthropicRequest(s, http.MethodPost, "/v1/messages", `{"model":"coverage-client-model","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("committed stream status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"error"`) || !strings.Contains(rr.Body.String(), "invalid event") {
		t.Fatalf("missing terminal stream error frame: %s", rr.Body.String())
	}
}

func TestCoverageAnthropicCountTokensNegativeNativeFallsBackToEstimate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":-1}`)
	}))
	defer upstream.Close()

	cfg := coverageAnthropicConfig("anthropic_compatible", upstream.URL)
	cfg.Providers[0].CountTokensPath = "/v1/messages/count_tokens"
	s := testGateway(t, cfg)
	rr := coverageAnthropicRequest(s, http.MethodPost, "/v1/messages/count_tokens", `{"model":"coverage-client-model","messages":[{"role":"user","content":"hello"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	if got["estimated"] != true {
		t.Fatalf("negative native count should fall back to estimate: %#v", got)
	}
	if got["input_tokens"] == nil {
		t.Fatalf("estimate omitted input_tokens: %#v", got)
	}
}

func TestCoverageAnthropicNativeErrorPreservesProviderStatusAndBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer upstream.Close()

	s := testGateway(t, coverageAnthropicConfig("anthropic_compatible", upstream.URL))
	rr := coverageAnthropicRequest(s, http.MethodPost, "/v1/messages", `{"model":"coverage-client-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "slow down") {
		t.Fatalf("status=%d retry-after=%q body=%s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
}
