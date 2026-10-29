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

// Claude Code ingress proof tests (FIXTURE): Anthropic Messages requests
// through the gateway stack against a loopback Anthropic fake. Zero live
// calls, zero real credentials.

type claudeFakeUpstream struct {
	mu    sync.Mutex
	calls int
}

func (u *claudeFakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.calls++
	call := u.calls
	u.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":42}`)
		return
	}
	stream := false
	body, _ := io.ReadAll(r.Body)
	stream = strings.Contains(string(body), `"stream":true`)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream-model\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if call == 1 && strings.Contains(string(body), "lookup") {
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}],"model":"upstream-model","stop_reason":"tool_use","usage":{"input_tokens":9,"output_tokens":4}}`)
		return
	}
	io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"model":"upstream-model","stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`)
}

func claudeTestGateway(t *testing.T, auth bool) *Server {
	t.Helper()
	up := httptest.NewServer(&claudeFakeUpstream{})
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	if auth {
		cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"test-client-key"}}
	}
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "Claude test upstream", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func claudePost(t *testing.T, s *Server, url, body, keyHeader, keyValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if keyHeader != "" {
		req.Header.Set(keyHeader, keyValue)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestClaudeCodeMessagesNonStreaming(t *testing.T) {
	s := claudeTestGateway(t, false)
	rr := claudePost(t, s, "http://gateway/v1/messages",
		`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content = %v", content)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(9) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestClaudeCodeMessagesStreaming(t *testing.T) {
	s := claudeTestGateway(t, false)
	rr := claudePost(t, s, "http://gateway/v1/messages",
		`{"model":"m","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	for _, want := range []string{"message_start", "content_block_delta", `"text":"Hi"`, "message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
}

func TestClaudeCodeToolRoundTrip(t *testing.T) {
	s := claudeTestGateway(t, false)
	tools := `"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]`
	first := `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"run lookup"}],` + tools + `}`
	rr := claudePost(t, s, "http://gateway/v1/messages", first, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("first turn status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"tool_use"`) || !strings.Contains(rr.Body.String(), `"name":"lookup"`) {
		t.Fatalf("first turn missing tool_use: %s", rr.Body.String())
	}
	second := `{"model":"m","max_tokens":32,"messages":[
		{"role":"user","content":"run lookup"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
	]}`
	rr2 := claudePost(t, s, "http://gateway/v1/messages", second, "", "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("second turn status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "hello") {
		t.Fatalf("second turn missing text answer: %s", rr2.Body.String())
	}
}

func TestClaudeCodeCountTokens(t *testing.T) {
	s := claudeTestGateway(t, false)
	rr := claudePost(t, s, "http://gateway/v1/messages/count_tokens",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["input_tokens"] != float64(42) {
		t.Fatalf("input_tokens = %v (want native 42)", out["input_tokens"])
	}
}

func TestClaudeCodeAuthForms(t *testing.T) {
	s := claudeTestGateway(t, true)
	body := `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	forms := []struct {
		name     string
		header   string
		value    string
		wantCode int
	}{
		{"bearer", "Authorization", "Bearer test-client-key", http.StatusOK},
		{"x-api-key", "x-api-key", "test-client-key", http.StatusOK},
		{"wrong key", "Authorization", "Bearer wrong-key", http.StatusUnauthorized},
		{"no key", "", "", http.StatusUnauthorized},
	}
	for _, f := range forms {
		rr := claudePost(t, s, "http://gateway/v1/messages", body, f.header, f.value)
		if rr.Code != f.wantCode {
			t.Fatalf("%s: status=%d want %d body=%s", f.name, rr.Code, f.wantCode, rr.Body.String())
		}
		if f.wantCode == http.StatusUnauthorized && !strings.Contains(rr.Body.String(), "invalid or missing API key") {
			t.Fatalf("%s: unexpected auth failure body: %s", f.name, rr.Body.String())
		}
	}
}
