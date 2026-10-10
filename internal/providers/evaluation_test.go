package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestEvaluationTwinLiveCompletionIsIsolatedAndUsesProviderAdapter(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected live request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer live-evaluation-secret" {
			t.Errorf("provider auth missing: %q", got)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request["model"] != "unit-model" || request["stream"] != false || request["max_tokens"] != float64(64) {
			t.Errorf("unexpected request body: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"evaluated"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer server.Close()

	production, err := NewAdapter(config.ProviderConfig{ID: "eval", Type: "openai_compatible", BaseURL: server.URL, APIKey: "live-evaluation-secret", AuthMode: "bearer", Enabled: true, MaxConcurrency: 2}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if LiveCapable(production) != nil {
		t.Fatal("production adapter was incorrectly accepted for live evaluation")
	}
	if _, ok := EvaluationTwin(nil); ok {
		t.Fatal("nil adapter produced an evaluation twin")
	}
	twin, ok := EvaluationTwin(production)
	if !ok || LiveCapable(twin) == nil {
		t.Fatal("provider adapter did not produce an evaluation-only twin")
	}
	before := production.Stats()
	result, err := LiveComplete(context.Background(), twin, "unit-model", "hello", 64)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !result.UpstreamAttempt || result.StatusCode != 200 || result.Output != "evaluated" || result.PromptTokens != 7 || result.OutputTokens != 3 || result.ErrorType != LiveErrNone {
		t.Fatalf("unexpected live result/call count: %+v calls=%d", result, calls)
	}
	if after := production.Stats(); after.ActiveRequests != before.ActiveRequests || after.WaitingRequests != before.WaitingRequests {
		t.Fatalf("live evaluation changed production concurrency state: before=%+v after=%+v", before, after)
	}
	if _, err := LiveComplete(context.Background(), production, "unit-model", "hello", 64); err == nil {
		t.Fatal("live evaluation was allowed on the production adapter")
	}
	if calls != 1 {
		t.Fatalf("refused production adapter still sent upstream traffic: %d", calls)
	}
}

func TestLiveCompletionRejectsInvalidInputsAndCapturesBoundedFailures(t *testing.T) {
	if _, err := LiveComplete(context.Background(), nil, "m", "prompt", 1); err == nil {
		t.Fatal("nil adapter accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"upstream failed with live-secret"}`)
	}))
	defer server.Close()
	prod, err := NewAdapter(config.ProviderConfig{ID: "eval", Type: "openai_compatible", BaseURL: server.URL, APIKey: "live-secret", AuthMode: "bearer", Enabled: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	twin, _ := EvaluationTwin(prod)
	if _, err = LiveComplete(context.Background(), twin, "m", "  ", 1); err == nil {
		t.Fatal("empty prompt accepted")
	}
	result, err := LiveComplete(context.Background(), twin, "m", "prompt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ErrorType != LiveErrHTTP || result.StatusCode != http.StatusBadGateway || strings.Contains(result.Message, "live-secret") {
		t.Fatalf("failure result unsafe or misclassified: %+v", result)
	}
}

func TestLiveRequestNativeShapesAndResponseDecoders(t *testing.T) {
	for _, tc := range []struct {
		kind, wantPath, wantField string
	}{
		{"openai_compatible", "/chat", "messages"},
		{"anthropic_compatible", "/messages", "messages"},
		{"openai_responses", "/responses", "input"},
		{"gemini", "", "contents"},
	} {
		a := &httpAdapter{p: config.ProviderConfig{Type: tc.kind, ChatPath: "/chat", MessagesPath: "/messages", ResponsesPath: "/responses", BaseURL: "https://example.test"}}
		body, path, err := a.liveCompletionRequest("m", "prompt", 9)
		if err != nil {
			t.Fatalf("%s request: %v", tc.kind, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded[tc.wantField] == nil {
			t.Errorf("%s request missing %s: %s", tc.kind, tc.wantField, body)
		}
		if tc.wantPath != "" && path != tc.wantPath {
			t.Errorf("%s path=%q want=%q", tc.kind, path, tc.wantPath)
		}
		if tc.kind == "gemini" && !strings.Contains(path, "generateContent") {
			t.Errorf("Gemini request path=%q", path)
		}
	}

	cases := []struct {
		kind, body, text string
		input, output    int
	}{
		{"openai_compatible", `{"choices":[{"message":{"content":"chat"}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`, "chat", 2, 3},
		{"anthropic_compatible", `{"content":[{"type":"text","text":"a"},{"type":"tool_use","text":"skip"},{"type":"text","text":"b"}],"usage":{"input_tokens":4,"output_tokens":5}}`, "ab", 4, 5},
		{"openai_responses", `{"output_text":"response","output":[{"content":[{"type":"output_text","text":"!"}]}],"usage":{"input_tokens":6,"output_tokens":7}}`, "response!", 6, 7},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"gemini"},{"text":"!"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":9}}`, "gemini!", 8, 9},
		{"unknown", `{"choices":[{"message":{"content":"default"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, "default", 1, 2},
	}
	for _, tc := range cases {
		text, input, output := decodeLiveCompletion(tc.kind, []byte(tc.body))
		if text != tc.text || input != tc.input || output != tc.output {
			t.Errorf("decode %s = %q %d %d", tc.kind, text, input, output)
		}
	}
	if text, input, output := decodeLiveCompletion("gemini", []byte("bad json")); text != "" || input != 0 || output != 0 {
		t.Fatalf("invalid response body decoded: %q %d %d", text, input, output)
	}
}

func TestLiveTextTruncationAndProviderSecretRedaction(t *testing.T) {
	if got := truncateLiveText("short"); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	long := strings.Repeat("x", maxLiveOutputTextBytes+20)
	if got := truncateLiveText(long); len(got) != maxLiveOutputTextBytes {
		t.Fatalf("truncated length=%d", len(got))
	}
	a, err := newHTTPAdapter(config.ProviderConfig{ID: "redact", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "secret-one", Credentials: []config.CredentialConfig{{Name: "second", APIKey: "secret-two", Enabled: true}}, AuthMode: "bearer", Enabled: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := a.redactString("provider said secret-one and secret-two")
	if strings.Contains(got, "secret-one") || strings.Contains(got, "secret-two") {
		t.Fatalf("redaction leaked credential: %q", got)
	}
}
