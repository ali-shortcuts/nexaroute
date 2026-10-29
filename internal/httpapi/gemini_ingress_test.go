package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Gemini CLI ingress proof tests (FIXTURE): Gemini-protocol requests
// through the gateway stack against loopback fake upstreams. Zero live
// calls, zero real credentials.

type geminiFakeUpstream struct {
	kind string // "openai_chat" or "gemini"

	mu     sync.Mutex
	calls  int
	bodies []string
}

func (u *geminiFakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls++
	u.bodies = append(u.bodies, string(body))
	call := u.calls
	u.mu.Unlock()
	stream := strings.Contains(string(body), `"stream":true`)
	switch u.kind {
	case "gemini":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"gemini hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":3,"totalTokenCount":14}}`)
	case "openai_chat":
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			write := func(s string) {
				_, _ = io.WriteString(w, s)
				if fl != nil {
					fl.Flush()
				}
			}
			write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"stream \"},\"finish_reason\":null}]}\n\n")
			write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
			write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n")
			write("data: [DONE]\n\n")
			return
		}
		if call == 1 && strings.Contains(string(body), "lookup") && strings.Contains(string(body), "tools") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"chat hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	}
}

func geminiTestGateway(t *testing.T, kind, providerType string, auth bool) (*Server, *geminiFakeUpstream) {
	t.Helper()
	up := &geminiFakeUpstream{kind: kind}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	if auth {
		cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"test-client-key"}}
	}
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "Gemini test upstream", Type: providerType, BaseURL: srv.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s, up
}

func geminiPost(t *testing.T, s *Server, url, body string, keyHeader, keyValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if keyHeader != "" {
		req.Header.Set(keyHeader, keyValue)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestGeminiGenerateContentOverChatUpstream(t *testing.T) {
	s, up := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	body := `{"systemInstruction":{"parts":[{"text":"Be brief."}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	cands, _ := out["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates = %v", out)
	}
	cand, _ := cands[0].(map[string]any)
	if cand["finishReason"] != "STOP" {
		t.Fatalf("finishReason = %v", cand["finishReason"])
	}
	content, _ := cand["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "chat hello" {
		t.Fatalf("parts = %v", parts)
	}
	usage, _ := out["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != float64(7) || usage["candidatesTokenCount"] != float64(5) {
		t.Fatalf("usageMetadata = %v", usage)
	}
	up.mu.Lock()
	seen := strings.Join(up.bodies, "\n")
	up.mu.Unlock()
	if !strings.Contains(seen, "Be brief.") || !strings.Contains(seen, `"role":"system"`) {
		t.Fatalf("chat upstream did not receive system instruction: %s", seen)
	}
}

func TestGeminiStreamGenerateContentOverChatUpstream(t *testing.T) {
	s, _ := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:streamGenerateContent?alt=sse", body, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("Gemini stream must not use the [DONE] sentinel: %s", out)
	}
	for _, want := range []string{`"text":"stream "`, `"text":"hello"`, `"finishReason":"STOP"`, `"promptTokenCount":7`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var frame map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				t.Fatalf("non-JSON Gemini frame: %v (%s)", err, line)
			}
		}
	}
}

func TestGeminiFunctionCallRoundTrip(t *testing.T) {
	s, _ := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	tools := `"tools":[{"functionDeclarations":[{"name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}]`
	first := `{"contents":[{"role":"user","parts":[{"text":"run lookup"}]}],` + tools + `}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", first, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("first turn status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"functionCall"`) || !strings.Contains(rr.Body.String(), `"name":"lookup"`) {
		t.Fatalf("first turn missing functionCall: %s", rr.Body.String())
	}
	second := `{"contents":[
		{"role":"user","parts":[{"text":"run lookup"}]},
		{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},
		{"role":"function","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}
	]}`
	rr2 := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", second, "", "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("second turn status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "chat hello") {
		t.Fatalf("second turn missing text answer: %s", rr2.Body.String())
	}
}

func TestGeminiAuthForms(t *testing.T) {
	s, _ := geminiTestGateway(t, "openai_chat", "openai_compatible", true)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	forms := []struct {
		name     string
		url      string
		header   string
		value    string
		wantCode int
	}{
		{"x-goog-api-key", "http://gateway/v1beta/models/m:generateContent", "x-goog-api-key", "test-client-key", http.StatusOK},
		{"query key", "http://gateway/v1beta/models/m:generateContent?key=test-client-key", "", "", http.StatusOK},
		{"bearer", "http://gateway/v1beta/models/m:generateContent", "Authorization", "Bearer test-client-key", http.StatusOK},
		{"x-api-key", "http://gateway/v1beta/models/m:generateContent", "x-api-key", "test-client-key", http.StatusOK},
		{"wrong key", "http://gateway/v1beta/models/m:generateContent", "x-goog-api-key", "wrong-key", http.StatusUnauthorized},
		{"no key", "http://gateway/v1beta/models/m:generateContent", "", "", http.StatusUnauthorized},
	}
	for _, f := range forms {
		rr := geminiPost(t, s, f.url, body, f.header, f.value)
		if rr.Code != f.wantCode {
			t.Fatalf("%s: status=%d want %d body=%s", f.name, rr.Code, f.wantCode, rr.Body.String())
		}
		if f.wantCode == http.StatusUnauthorized {
			var env map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("%s: auth failure is not JSON: %v", f.name, err)
			}
			errObj, _ := env["error"].(map[string]any)
			if errObj["code"] != float64(401) || errObj["status"] != "UNAUTHENTICATED" {
				t.Fatalf("%s: Gemini error envelope = %v", f.name, errObj)
			}
		}
	}
}

func TestGeminiRejects(t *testing.T) {
	s, up := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	cases := []struct {
		name string
		url  string
		body string
		code int
	}{
		{"malformed JSON", "http://gateway/v1beta/models/m:generateContent", `{bad`, http.StatusBadRequest},
		{"empty contents", "http://gateway/v1beta/models/m:generateContent", `{"contents":[]}`, http.StatusBadRequest},
		{"candidateCount", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`, http.StatusBadRequest},
		{"safetySettings", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT"}]}`, http.StatusBadRequest},
		{"unknown action", "http://gateway/v1beta/models/m:countTokens", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, http.StatusNotFound},
		{"bad method", "http://gateway/v1beta/models/m:generateContent", ``, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		var rr *httptest.ResponseRecorder
		if tc.name == "bad method" {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			rr = httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
		} else {
			rr = geminiPost(t, s, tc.url, tc.body, "", "")
		}
		if rr.Code != tc.code {
			t.Fatalf("%s: status=%d want %d body=%s", tc.name, rr.Code, tc.code, rr.Body.String())
		}
		var env map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: error is not JSON: %v", tc.name, err)
		}
		if _, ok := env["error"].(map[string]any); !ok {
			t.Fatalf("%s: missing Gemini error envelope: %s", tc.name, rr.Body.String())
		}
	}
	up.mu.Lock()
	calls := up.calls
	up.mu.Unlock()
	if calls != 0 {
		t.Fatalf("rejected requests must not reach upstream, got %d calls", calls)
	}
}

// TestGeminiUnknownModelStrict documents the gateway-wide routing rule:
// with FallbackOnUnknownModel disabled, an unknown model is rejected
// (503, Gemini envelope) before any upstream call. With the default
// (fallback enabled), the same request routes to eligible deployments;
// that fallback is routing behavior shared by every ingress, not a
// Gemini-specific gap.
func TestGeminiUnknownModelStrict(t *testing.T) {
	up := &geminiFakeUpstream{kind: "openai_chat"}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.FallbackOnUnknownModel = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "Gemini test upstream", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := geminiPost(t, s, "http://gateway/v1beta/models/nope:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, "", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env["error"].(map[string]any); !ok {
		t.Fatalf("missing Gemini error envelope: %s", rr.Body.String())
	}
	up.mu.Lock()
	calls := up.calls
	up.mu.Unlock()
	if calls != 0 {
		t.Fatalf("rejected unknown model reached upstream %d times", calls)
	}
}

func TestGeminiModelsList(t *testing.T) {
	s, _ := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/v1beta/models", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	models, _ := out["models"].([]any)
	found := false
	for _, m := range models {
		if mm, _ := m.(map[string]any); mm["name"] == "models/upstream-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("models list missing models/upstream-model: %s", rr.Body.String())
	}
}

func TestGeminiGenerateContentOverGeminiUpstream(t *testing.T) {
	s, _ := geminiTestGateway(t, "gemini", "gemini", false)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "gemini hello") || !strings.Contains(rr.Body.String(), `"promptTokenCount":11`) {
		t.Fatalf("native Gemini response not preserved: %s", rr.Body.String())
	}
}
