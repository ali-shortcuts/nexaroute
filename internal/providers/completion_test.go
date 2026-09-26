package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// completeTestAdapter builds a real httpAdapter (production transport/auth path)
// against a test upstream.
func completeTestAdapter(t *testing.T, p config.ProviderConfig) Adapter {
	t.Helper()
	p.Enabled = true
	a, err := NewAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	return a
}

func lastBodyPtr(dst *atomic.Value) func(r *http.Request) {
	return func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		dst.Store(string(b))
	}
}

func TestComplete_OpenAIChatPayloadAndDecode(t *testing.T) {
	var body atomic.Value
	var authz atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBodyPtr(&body)(r)
		authz.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"42","tool_calls":[{"type":"function","function":{"name":"search_files","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`)
	}))
	defer up.Close()

	p := config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, APIKey: "SECRET_EVAL_PROVIDER_KEY_3f42"}
	a, ok := completeTestAdapter(t, p).(Completer)
	if !ok {
		t.Fatal("httpAdapter must implement Completer")
	}
	c, err := a.Complete(context.Background(), CompletionRequest{Model: "model-a", System: "be terse", Prompt: "compute", MaxTokens: 64})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if c.StatusCode != 200 {
		t.Fatalf("status = %d", c.StatusCode)
	}
	if c.Text != "42" {
		t.Fatalf("text = %q", c.Text)
	}
	if c.InputTokens != 11 || c.OutputTokens != 5 {
		t.Fatalf("tokens = %d/%d", c.InputTokens, c.OutputTokens)
	}
	if c.Refusal {
		t.Fatal("unexpected refusal")
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "search_files" {
		t.Fatalf("tool calls = %#v", c.ToolCalls)
	}
	// The request went through the production transport + auth.
	if got, _ := authz.Load().(string); got != "Bearer SECRET_EVAL_PROVIDER_KEY_3f42" {
		t.Fatalf("authorization = %q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["model"] != "model-a" || payload["stream"] != false {
		t.Fatalf("payload model/stream wrong: %v", payload)
	}
	if payload["max_tokens"].(float64) != 64 {
		t.Fatalf("max_tokens wrong: %v", payload["max_tokens"])
	}
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "compute" {
		t.Fatalf("messages wrong: %v", msgs)
	}
}

func TestComplete_AnthropicPayloadAndDecode(t *testing.T) {
	var body atomic.Value
	var apiKey, version atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBodyPtr(&body)(r)
		apiKey.Store(r.Header.Get("x-api-key"))
		version.Store(r.Header.Get("anthropic-version"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m","type":"message","content":[{"type":"text","text":"carol"},{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"/a"}}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`)
	}))
	defer up.Close()

	p := config.ProviderConfig{ID: "p1", Type: "anthropic_compatible", BaseURL: up.URL, APIKey: "k-1"}
	a := completeTestAdapter(t, p).(Completer)
	c, err := a.Complete(context.Background(), CompletionRequest{Model: "claude-x", Prompt: "who?", MaxTokens: 32})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if c.Text != "carol" {
		t.Fatalf("text = %q", c.Text)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "read_file" || !strings.Contains(c.ToolCalls[0].Arguments, "/a") {
		t.Fatalf("tool calls = %#v", c.ToolCalls)
	}
	if c.InputTokens != 7 || c.OutputTokens != 3 {
		t.Fatalf("tokens = %d/%d", c.InputTokens, c.OutputTokens)
	}
	if got, _ := apiKey.Load().(string); got != "k-1" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got, _ := version.Load().(string); got == "" {
		t.Fatal("anthropic-version header missing")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["max_tokens"].(float64) != 32 {
		t.Fatalf("max_tokens wrong: %v", payload)
	}
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages wrong (user only, no injected system): %v", msgs)
	}
}

func TestComplete_GeminiPathAndDecode(t *testing.T) {
	var sawPath atomic.Value
	var key atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath.Store(r.URL.Path)
		key.Store(r.Header.Get("x-goog-api-key"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"NEEDLE-A"},{"functionCall":{"name":"lookup","args":{"id":1}}}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2}}`)
	}))
	defer up.Close()

	p := config.ProviderConfig{ID: "p1", Type: "gemini", BaseURL: up.URL, APIKey: "g-key"}
	a := completeTestAdapter(t, p).(Completer)
	c, err := a.Complete(context.Background(), CompletionRequest{Model: "gemini-x", Prompt: "find the needle"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got, _ := sawPath.Load().(string); !strings.HasSuffix(got, ":generateContent") || !strings.Contains(got, "gemini-x") {
		t.Fatalf("gemini path wrong: %v", got)
	}
	if got, _ := key.Load().(string); got != "g-key" {
		t.Fatalf("x-goog-api-key = %q", got)
	}
	if c.Text != "NEEDLE-A" {
		t.Fatalf("text = %q", c.Text)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool calls = %#v", c.ToolCalls)
	}
	if c.InputTokens != 9 || c.OutputTokens != 2 {
		t.Fatalf("tokens = %d/%d", c.InputTokens, c.OutputTokens)
	}
}

func TestComplete_OpenAIResponsesPayloadAndDecode(t *testing.T) {
	var body atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBodyPtr(&body)(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"concise"}]},{"type":"function_call","name":"apply","arguments":"{\"patch\":true}"}],"usage":{"input_tokens":4,"output_tokens":6}}`)
	}))
	defer up.Close()

	p := config.ProviderConfig{ID: "p1", Type: "openai_responses", BaseURL: up.URL, AuthMode: "none"}
	a := completeTestAdapter(t, p).(Completer)
	c, err := a.Complete(context.Background(), CompletionRequest{Model: "gpt-x", System: "no preamble", Prompt: "answer"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if c.Text != "concise" {
		t.Fatalf("text = %q", c.Text)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "apply" {
		t.Fatalf("tool calls = %#v", c.ToolCalls)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["input"] != "answer" || payload["instructions"] != "no preamble" {
		t.Fatalf("responses payload wrong: %v", payload)
	}
	if payload["max_output_tokens"].(float64) != DefaultCompletionMaxTokens {
		t.Fatalf("default max tokens wrong: %v", payload)
	}
}

func TestComplete_Non2xxRedactsCredentials(t *testing.T) {
	const key = "SECRET_EVAL_PROVIDER_KEY_3f42"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// The upstream echoes the credential in its error body (worst case).
		fmt.Fprintf(w, `{"error":{"message":"boom with key %s"}}`, key)
	}))
	defer up.Close()

	p := config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, APIKey: key}
	a := completeTestAdapter(t, p).(Completer)
	c, err := a.Complete(context.Background(), CompletionRequest{Model: "model-a", Prompt: "hi"})
	if err == nil {
		t.Fatal("500 must produce an error")
	}
	if c.StatusCode != 500 {
		t.Fatalf("status = %d, want 500 preserved for evidence mapping", c.StatusCode)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error leaked credential: %s", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error not redacted: %s", err)
	}
}

func TestComplete_ContextDeadlinePropagates(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"late"}}]}`)
	}))
	defer up.Close()

	p := config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none"}
	a := completeTestAdapter(t, p).(Completer)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := a.Complete(ctx, CompletionRequest{Model: "model-a", Prompt: "hi"})
	if err == nil {
		t.Fatal("deadline must fail the completion")
	}
}

func TestComplete_ValidateBounds(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer up.Close()
	a := completeTestAdapter(t, config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none"}).(Completer)
	if _, err := a.Complete(context.Background(), CompletionRequest{Prompt: "x"}); err == nil {
		t.Fatal("empty model must be rejected")
	}
	if _, err := a.Complete(context.Background(), CompletionRequest{Model: "m"}); err == nil {
		t.Fatal("empty prompt and system must be rejected")
	}
	if _, err := a.Complete(context.Background(), CompletionRequest{Model: "m", Prompt: strings.Repeat("x", MaxCompletionPromptBytes+1)}); err == nil {
		t.Fatal("oversized prompt must be rejected")
	}
}

func BenchmarkComplete(b *testing.B) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer up.Close()
	cfg := config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true}
	a, err := NewAdapter(cfg, 5*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	completer := a.(Completer)
	req := CompletionRequest{Model: "model-a", Prompt: "compute 40+2", MaxTokens: 16}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c, err := completer.Complete(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
		if c.Text != "42" {
			b.Fatalf("text = %q", c.Text)
		}
	}
}

// FuzzCompletionDecode asserts the four upstream response decoders never
// panic, always keep outputs bounded and never fabricate usage on garbage.
func FuzzCompletionDecode(f *testing.F) {
	f.Add([]byte(`{"choices":[{"message":{"content":"42","tool_calls":[{"function":{"name":"x","arguments":"{}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	f.Add([]byte(`{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"y","input":{"a":1}}],"stop_reason":"refusal","usage":{"input_tokens":2,"output_tokens":2}}`))
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"text":"q"},{"functionCall":{"name":"z","args":{}}}]}}],"usageMetadata":{"promptTokenCount":1}}`))
	f.Add([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"w"}]},{"type":"function_call","name":"f","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`{"choices":"nope"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, kind := range []string{"openai_compatible", "anthropic_compatible", "gemini", "openai_responses"} {
			c, err := decodeCompletion(kind, data, Completion{})
			if err != nil {
				continue
			}
			if len(c.Text) > maxCompletionTextBytes {
				t.Fatalf("%s text escaped bound: %d", kind, len(c.Text))
			}
			if len(c.ToolCalls) > maxCompletionToolCalls {
				t.Fatalf("%s tool calls escaped bound: %d", kind, len(c.ToolCalls))
			}
			for _, tc := range c.ToolCalls {
				if len(tc.Name) > 256 || len(tc.Arguments) > maxCompletionToolArgsBytes {
					t.Fatalf("%s tool call escaped bound: %#v", kind, tc)
				}
			}
			if c.InputTokens < 0 || c.OutputTokens < 0 {
				t.Fatalf("%s negative token count", kind)
			}
		}
	})
}
