package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func testGateway(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	if cfg.Routing.Strategy == "ready_queue" {
		for _, d := range rt.All() {
			hm.RecordSuccess(d.ID, time.Millisecond)
		}
	}
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	return New(cfg, t.TempDir()+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
}

func TestAnthropicNativePreservesUnknownFieldsAndBetaHeader(t *testing.T) {
	var got map[string]any
	var beta string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("anthropic-beta")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"upstream-model","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Aliases: []string{"client-model"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}}}}
	s := testGateway(t, cfg)
	body := `{"model":"client-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8}}`
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	req.Header.Set("anthropic-beta", "test-beta")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	if got["model"] != "upstream-model" {
		t.Fatalf("model not patched %#v", got["model"])
	}
	if _, ok := got["thinking"]; !ok {
		t.Fatalf("unknown thinking field was dropped: %#v", got)
	}
	if beta != "test-beta" {
		t.Fatalf("beta not forwarded %q", beta)
	}
}

func TestCountTokensUsesNativeAnthropicEndpoint(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		calls++
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		if obj["model"] != "upstream" {
			t.Fatalf("model=%v", obj["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":42}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream", Aliases: []string{"client"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Tools: true, Vision: true}}}}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/messages/count_tokens", strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hello"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "42") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestOpenAINativePreservesUnknownFields(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "o", Name: "O", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "upstream", Aliases: []string{"client"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := got["response_format"]; !ok {
		t.Fatalf("unknown field dropped %#v", got)
	}
}

func TestAnthropicStreamToOpenAIIncludesToolArguments(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"shell","input":{}}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n")
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}
	rr := httptest.NewRecorder()
	if err := streamAnthropicToOpenAI(rr, resp, "client"); err != nil {
		t.Fatal(err)
	}
	out := rr.Body.String()
	if !strings.Contains(out, "arguments") || !strings.Contains(out, "cmd") || !strings.Contains(out, "ls") {
		t.Fatalf("tool arguments missing: %s", out)
	}
}

func TestOpenAIStreamToAnthropicParallelTools(t *testing.T) {
	chunks := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"one","arguments":"{\"x\":"}},{"index":1,"id":"b","type":"function","function":{"name":"two","arguments":"{\"y\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, "",
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Join(chunks, "\n\n")))}
	rr := httptest.NewRecorder()
	if err := streamOpenAIToAnthropic(rr, resp, "client"); err != nil {
		t.Fatal(err)
	}
	out := rr.Body.String()
	if strings.Count(out, "content_block_start") < 2 || !strings.Contains(out, "\"stop_reason\":\"tool_use\"") {
		t.Fatalf("parallel tools not translated: %s", out)
	}
}

func TestReadyAndMetrics(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	for _, path := range []string{"/readyz", "/metrics"} {
		req := httptest.NewRequest("GET", "http://gateway"+path, nil)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestContextCancellationWhileProviderConcurrencyBlocked(t *testing.T) {
	gate := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-gate; w.WriteHeader(200) }))
	defer up.Close()
	p := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 1}
	a, _ := providers.NewAdapter(p, 2*time.Second)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() {
		resp, _ := a.Do(ctx1, []byte(`{}`), false, nil)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	_, err := a.Do(ctx2, []byte(`{}`), false, nil)
	if err == nil {
		t.Fatal("expected cancellation")
	}
	close(gate)
}

func TestAdminEditPreservesSecretsAndAdvancedFields(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "Old", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9999", APIKey: "secret-one", Credentials: []config.CredentialConfig{{Name: "second", APIKey: "secret-two", Enabled: true}}, AuthMode: "bearer", ProxyURL: "http://127.0.0.1:8888", MaxConcurrency: 7, StreamIdleTimeoutSeconds: 222, Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	body := `{"provider":{"id":"p","name":"New","type":"openai_compatible","base_url":"http://127.0.0.1:9998","auth_mode":"bearer","proxy_url":"http://127.0.0.1:7777","max_concurrency":9,"stream_idle_timeout_seconds":333,"enabled":true,"models":[{"id":"m","model":"m","enabled":true,"weight":1,"capabilities":{}}]},"preserve_secret":true}`
	req := httptest.NewRequest("PUT", "http://gateway/admin/api/providers/p", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	got := s.currentConfig().Providers[0]
	if got.APIKey != "secret-one" || len(got.Credentials) != 1 || got.Credentials[0].APIKey != "secret-two" {
		t.Fatalf("secrets not preserved %#v", got)
	}
	if got.BaseURL != "http://127.0.0.1:9998" || got.ProxyURL != "http://127.0.0.1:7777" || got.MaxConcurrency != 9 || got.StreamIdleTimeoutSeconds != 333 {
		t.Fatalf("advanced fields not saved %#v", got)
	}
}

func TestConfiguredForwardHeaderPassesThroughButClientAuthDoesNot(t *testing.T) {
	var custom, auth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		custom = r.Header.Get("x-provider-feature")
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"up","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "anthropic_compatible", BaseURL: up.URL, APIKey: "upstream-secret", AuthMode: "bearer", ForwardHeaders: []string{"x-provider-feature", "authorization"}, Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "up", Aliases: []string{"client"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"client","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-provider-feature", "feature-on")
	req.Header.Set("Authorization", "Bearer client-secret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if custom != "feature-on" {
		t.Fatalf("custom header not forwarded: %q", custom)
	}
	if auth != "Bearer upstream-secret" {
		t.Fatalf("client auth leaked or upstream auth missing: %q", auth)
	}
}

func TestUpstreamErrorRedactsProviderCredential(t *testing.T) {
	const secret = "super-secret-provider-key"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("upstream auth=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"credential super-secret-provider-key rejected"}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		APIKey: secret, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up", Aliases: []string{"client"}, Enabled: true, Weight: 1}},
	}}
	srv := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"client","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("provider credential leaked in upstream error: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "[REDACTED]") {
		t.Fatalf("expected redaction marker, body=%s", rr.Body.String())
	}
}

func TestNativeProxyStripsSensitiveAndHopByHopResponseHeaders(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type":       []string{"application/json"},
			"Set-Cookie":         []string{"session=upstream-secret"},
			"Authorization":      []string{"Bearer upstream-secret"},
			"X-Api-Key":          []string{"upstream-secret"},
			"Connection":         []string{"keep-alive"},
			"Keep-Alive":         []string{"timeout=5"},
			"Proxy-Authenticate": []string{"Basic realm=upstream"},
			"X-Safe-Upstream":    []string{"ok"},
		},
		Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
	rr := httptest.NewRecorder()
	if err := proxyResponse(rr, resp); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Set-Cookie", "Authorization", "X-Api-Key", "Connection", "Keep-Alive", "Proxy-Authenticate"} {
		if got := rr.Header().Get(h); got != "" {
			t.Fatalf("sensitive/hop-by-hop header %s leaked: %q", h, got)
		}
	}
	if got := rr.Header().Get("X-Safe-Upstream"); got != "ok" {
		t.Fatalf("safe upstream header missing: %q", got)
	}
}

func TestNativeSSEProxyFlushes(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "Content-Length": []string{"12"}}, Body: io.NopCloser(strings.NewReader("data: one\n\n"))}
	rr := httptest.NewRecorder()
	if err := proxyResponse(rr, resp); err != nil {
		t.Fatal(err)
	}
	if !rr.Flushed {
		t.Fatal("expected SSE response to flush")
	}
	if rr.Header().Get("Content-Length") != "" {
		t.Fatalf("SSE content-length should be removed: %q", rr.Header().Get("Content-Length"))
	}
}

func TestAnthropicIngressUsesAnthropicErrorShape(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"x","messages":[]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "error" {
		t.Fatalf("missing anthropic top-level type: %#v", got)
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("wrong error type: %#v", got)
	}
}

func TestAnthropicRequestFailsOverAfterUpstream401(t *testing.T) {
	badCalls := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad upstream key"}}`))
	}))
	defer bad.Close()
	goodCalls := 0
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c2","object":"chat.completion","model":"good-model","choices":[{"index":0,"message":{"role":"assistant","content":"fallback-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer good.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{
		{ID: "bad", Name: "Bad", Type: "openai_compatible", BaseURL: bad.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "bad-model", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}}},
		{ID: "good", Name: "Good", Type: "openai_compatible", BaseURL: good.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "good-model", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"coding","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "fallback-ok") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if badCalls != 1 || goodCalls != 1 {
		t.Fatalf("badCalls=%d goodCalls=%d", badCalls, goodCalls)
	}
	if st := s.hm.Get("bad/m"); st.Status != health.Cooldown {
		t.Fatalf("bad deployment not cooled: %+v", st)
	}
	if got := rr.Header().Get("X-Gateway-Provider"); got != "good" {
		t.Fatalf("route header provider=%q want good", got)
	}
}

func TestClaudeCodeLikeToolRoundTripThroughOpenAIProvider(t *testing.T) {
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
			w.WriteHeader(400)
			return
		}
		if calls == 1 {
			if _, ok := got["tools"]; !ok {
				t.Errorf("first request lost tools: %#v", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		msgs, _ := got["messages"].([]any)
		if len(msgs) < 3 {
			t.Errorf("second request messages too short: %#v", got["messages"])
		} else {
			assistant, _ := msgs[len(msgs)-2].(map[string]any)
			toolMsg, _ := msgs[len(msgs)-1].(map[string]any)
			if assistant["role"] != "assistant" || assistant["tool_calls"] == nil {
				t.Errorf("assistant tool_use not translated: %#v", assistant)
			}
			if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
				t.Errorf("tool_result not translated: %#v", toolMsg)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c2","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"file says hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "o", Name: "OpenAI-ish", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "up", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	s := testGateway(t, cfg)

	first := `{"model":"coding","max_tokens":32,"stream":true,"tools":[{"name":"read_file","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"messages":[{"role":"user","content":"read a.txt"}]}`
	r1 := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(first))
	rr1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr1, r1)
	if rr1.Code != 200 {
		t.Fatalf("first status=%d body=%s", rr1.Code, rr1.Body.String())
	}
	out1 := rr1.Body.String()
	if !strings.Contains(out1, `"type":"tool_use"`) || !strings.Contains(out1, `"stop_reason":"tool_use"`) || !strings.Contains(out1, `read_file`) {
		t.Fatalf("first anthropic stream missing tool flow: %s", out1)
	}

	second := `{"model":"coding","max_tokens":32,"messages":[{"role":"user","content":"read a.txt"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"a.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"hello"}]}]}`
	r2 := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(second))
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, r2)
	if rr2.Code != 200 || !strings.Contains(rr2.Body.String(), "file says hello") {
		t.Fatalf("second status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2", calls)
	}
}

func TestParseModelListCommonShapes(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{`{"data":[{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{`{"models":[{"name":"models/gemini-x"},"plain"]}`, []string{"gemini-x", "plain"}},
		{`[{"model":"x"},{"id":"y"}]`, []string{"x", "y"}},
	}
	for _, tc := range cases {
		got := parseModelList([]byte(tc.body))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("body=%s got=%v want=%v", tc.body, got, tc.want)
		}
	}
}

func TestConcurrentRequestsDuringHotReload(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "up", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}}}}
	s := testGateway(t, cfg)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"coding","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
				rr := httptest.NewRecorder()
				s.Handler().ServeHTTP(rr, req)
				if rr.Code != 200 {
					t.Errorf("request status=%d body=%s", rr.Code, rr.Body.String())
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		next := s.currentConfig()
		next.Providers[0].Name = fmt.Sprintf("P-%d", i)
		if err := s.applyConfig(next); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	wg.Wait()
}

func TestReadyQueueEjectsFailedPrimaryAndUsesNextHealthyModel(t *testing.T) {
	var primaryCalls atomic.Int32
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"primary down"}`))
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","object":"chat.completion","model":"fallback","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer fallback.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "primary", Name: "Primary", Type: "openai_compatible", BaseURL: primary.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "primary", Aliases: []string{"auto"}, Enabled: true, Priority: 0, Weight: 2, Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "fallback", Name: "Fallback", Type: "openai_compatible", BaseURL: fallback.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "fallback", Aliases: []string{"auto"}, Enabled: true, Priority: 10, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}},
	}
	srv := testGateway(t, cfg)

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}

	if rr := request(); rr.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", rr.Code, rr.Body.String())
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("first request calls primary=%d fallback=%d", primaryCalls.Load(), fallbackCalls.Load())
	}
	if st := srv.hm.Get("primary/m"); st.Status != health.Degraded {
		t.Fatalf("failed primary must be quarantined immediately: %+v", st)
	}

	if rr := request(); rr.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", rr.Code, rr.Body.String())
	}
	if primaryCalls.Load() != 1 {
		t.Fatalf("quarantined primary was retried by Claude traffic: calls=%d", primaryCalls.Load())
	}
	if fallbackCalls.Load() != 2 {
		t.Fatalf("fallback calls=%d want 2", fallbackCalls.Load())
	}
}

func TestClientCancellationDoesNotQuarantineHealthyReadyModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}}}
	srv := testGateway(t, cfg)
	if st := srv.hm.Get("p/m"); st.Status != health.Healthy {
		t.Fatalf("fixture not ready: %+v", st)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if st := srv.hm.Get("p/m"); st.Status != health.Healthy {
		t.Fatalf("client cancellation incorrectly damaged provider health: %+v", st)
	}
	if got := srv.bus.Counts()["client_disconnect"]; got != 1 {
		t.Fatalf("client_disconnect events=%d want 1", got)
	}
}

func TestRequestTimeoutIsTotalFailoverBudget(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"late","choices":[]}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"late2","choices":[]}`))
	}))
	defer second.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RequestTimeoutMS = 120
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", Name: "A", Type: "openai_compatible", BaseURL: first.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "a", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 2}}},
		{ID: "b", Name: "B", Type: "openai_compatible", BaseURL: second.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "b", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1}}},
	}
	srv := testGateway(t, cfg)
	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("timeout budget restarted across candidates: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("request exceeded total failover budget by too much: %s", elapsed)
	}
}

func TestHotReloadInvalidatesChangedProviderHealthProof(t *testing.T) {
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"a","choices":[]}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"b","choices":[]}`))
	}))
	defer up2.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up1.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
	srv := testGateway(t, cfg)
	if st := srv.hm.Get("p/m"); st.Status != health.Healthy {
		t.Fatalf("fixture not healthy: %+v", st)
	}

	next := srv.currentConfig()
	next.Providers[0].BaseURL = up2.URL
	if err := srv.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	if st := srv.hm.Get("p/m"); st.Status != health.Unknown {
		t.Fatalf("changed provider kept stale ready proof: %+v", st)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/readyz", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status=%d want 503 after health invalidation, body=%s", rr.Code, rr.Body.String())
	}
}

func TestTranslatedStreamsRequireTerminalSignal(t *testing.T) {
	openAIResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")),
	}
	if err := streamOpenAIToAnthropic(httptest.NewRecorder(), openAIResp, "m"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("openai translated stream error=%v want unexpected EOF", err)
	}

	anthResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")),
	}
	if err := streamAnthropicToOpenAI(httptest.NewRecorder(), anthResp, "m"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("anthropic translated stream error=%v want unexpected EOF", err)
	}
}
