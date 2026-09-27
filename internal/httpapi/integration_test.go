package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	if router.IsReadyStrategy(cfg.Routing.Strategy) {
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
	var clientResponse map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &clientResponse); err != nil {
		t.Fatal(err)
	}
	if clientResponse["model"] != "client-model" {
		t.Fatalf("response exposed physical model instead of stable client model: %#v", clientResponse["model"])
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
	if err := streamAnthropicToOpenAI(rr, resp, "client", nil); err != nil {
		t.Fatal(err)
	}
	out := rr.Body.String()
	if !strings.Contains(out, "arguments") || !strings.Contains(out, "cmd") || !strings.Contains(out, "ls") {
		t.Fatalf("tool arguments missing: %s", out)
	}
}

func TestNativeAnthropicStreamRewritesPhysicalModelToPublicModel(t *testing.T) {
	sse := `event: message_start
 data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"model":"physical-model","usage":{"input_tokens":1,"output_tokens":0}}}

event: message_stop
 data: {"type":"message_stop"}

`
	// Remove the leading space before SSE data fields: parsers accept it, but
	// exact upstream semantics are easier to assert with standard framing.
	sse = strings.ReplaceAll(sse, "\n data:", "\ndata:")
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}
	rr := httptest.NewRecorder()
	if err := proxyNativeSSEWithModel(rr, resp, "anthropic", "public-route"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), `"model":"public-route"`) || strings.Contains(rr.Body.String(), `"model":"physical-model"`) {
		t.Fatalf("stream leaked physical model name: %s", rr.Body.String())
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
	if err := streamOpenAIToAnthropic(rr, resp, "client", nil); err != nil {
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
	req.Host = "127.0.0.1"
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
			"Connection":         []string{"keep-alive, X-Internal-Hop"},
			"Keep-Alive":         []string{"timeout=5"},
			"X-Internal-Hop":     []string{"must-not-leak"},
			"Proxy-Authenticate": []string{"Basic realm=upstream"},
			"X-Safe-Upstream":    []string{"ok"},
		},
		Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
	rr := httptest.NewRecorder()
	copyUpstreamResponseHeaders(rr, resp, false)
	for _, h := range []string{"Set-Cookie", "Authorization", "X-Api-Key", "Connection", "Keep-Alive", "X-Internal-Hop", "Proxy-Authenticate"} {
		if got := rr.Header().Get(h); got != "" {
			t.Fatalf("sensitive/hop-by-hop header %s leaked: %q", h, got)
		}
	}
	if got := rr.Header().Get("X-Safe-Upstream"); got != "ok" {
		t.Fatalf("safe upstream header missing: %q", got)
	}
}

func TestNativeSSEProxyFlushes(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "Content-Length": []string{"12"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" + "data: [DONE]\n\n"))}
	rr := httptest.NewRecorder()
	if err := proxyNativeSSE(rr, resp, "openai"); err != nil {
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

func TestClaudeMessagesABCDFailureSupervisorRegression(t *testing.T) {
	probeContext, cancelProbe := context.WithCancel(context.Background())
	defer cancelProbe()
	allowBProbe := make(chan struct{})
	bProbeStarted := make(chan struct{}, 1)
	var aRequests, aProbes atomic.Int32
	var bRequests, bProbes atomic.Int32
	var cRequests, dRequests atomic.Int32
	var orderMu sync.Mutex
	order := make([]string, 0, 4)
	appendAttempt := func(name string) {
		orderMu.Lock()
		order = append(order, name)
		orderMu.Unlock()
	}
	isProbeRequest := func(r *http.Request) bool {
		var body struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) == 0 {
			return false
		}
		if content, ok := body.Messages[len(body.Messages)-1].Content.(string); ok {
			return content == "OK"
		}
		return false
	}
	validOpenAIResponse := func(w http.ResponseWriter, model, text string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-d","object":"chat.completion","model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"`+text+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}
	newUpstream := func(handler http.HandlerFunc) *httptest.Server {
		up := httptest.NewServer(handler)
		t.Cleanup(up.Close)
		return up
	}
	a := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		if isProbeRequest(r) {
			aProbes.Add(1)
			validOpenAIResponse(w, "deepseek-ai/deepseek-v4-flash", "unexpected-A-probe")
			return
		}
		aRequests.Add(1)
		appendAttempt("A")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"error":{"code":"model_eol","type":"model_retired","message":"deepseek-ai/deepseek-v4-flash has reached end of life"}}`)
	})
	b := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		if isProbeRequest(r) {
			bProbes.Add(1)
			select {
			case bProbeStarted <- struct{}{}:
			default:
			}
			select {
			case <-allowBProbe:
				validOpenAIResponse(w, "deepseek-v4-flash", "B_RECOVERED")
			case <-probeContext.Done():
				return
			}
			return
		}
		bRequests.Add(1)
		appendAttempt("B")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"model_unavailable","type":"invalid_request_error","message":"deepseek-v4-flash is temporarily unavailable"}}`)
	})
	c := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		if isProbeRequest(r) {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		cRequests.Add(1)
		appendAttempt("C")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"C_RATE_PRIVATE"}}`)
	})
	d := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		if isProbeRequest(r) {
			validOpenAIResponse(w, "model-d", "unexpected-D-probe")
			return
		}
		dRequests.Add(1)
		appendAttempt("D")
		validOpenAIResponse(w, "model-d", "D_ONLY")
	})

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Probe.CapabilityProbes = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 1
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "openai_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "deepseek-ai/deepseek-v4-flash", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "deepseek-v4-flash", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 1, Weight: 1}}},
		{ID: "C", Name: "C", Type: "openai_compatible", BaseURL: c.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "model-c", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 2, Weight: 1}}},
		{ID: "D", Name: "D", Type: "openai_compatible", BaseURL: d.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "model-d", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 3, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	s.probe.Start(probeContext)
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"claude-auto","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid Anthropic response: %v: %s", err, rr.Body.String())
	}
	if response["model"] != "claude-auto" || !strings.Contains(rr.Body.String(), "D_ONLY") {
		t.Fatalf("client did not receive only D's response with public model identity: %s", rr.Body.String())
	}
	for _, private := range []string{"model_eol", "model_unavailable", "C_RATE_PRIVATE", "B_RECOVERED"} {
		if strings.Contains(rr.Body.String(), private) {
			t.Fatalf("intermediate provider details leaked to Claude: %s", rr.Body.String())
		}
	}
	orderMu.Lock()
	gotOrder := strings.Join(order, ",")
	orderMu.Unlock()
	if gotOrder != "A,B,C,D" || aRequests.Load() != 1 || bRequests.Load() != 1 || cRequests.Load() != 1 || dRequests.Load() != 1 {
		t.Fatalf("attempt order=%q calls A/B/C/D=%d/%d/%d/%d", gotOrder, aRequests.Load(), bRequests.Load(), cRequests.Load(), dRequests.Load())
	}
	if st := s.hm.Get("A/m"); st.Status != health.Retired || st.LastErrorClass != "model_retired" {
		t.Fatalf("A must remain permanently retired: %+v", st)
	}
	if aProbes.Load() != 0 {
		t.Fatalf("retired A was recovery-probed: probes=%d", aProbes.Load())
	}
	if st := s.hm.Get("C/m"); st.Status != health.Cooldown || st.LastErrorClass != "rate_limit" {
		t.Fatalf("C did not enter Retry-After cooldown: %+v", st)
	} else if remaining := time.Until(st.CooldownUntil); remaining < time.Second || remaining > 3*time.Second {
		t.Fatalf("C Retry-After: 2 cooldown was not honored within bounds: remaining=%s state=%+v", remaining, st)
	}
	select {
	case <-bProbeStarted:
	case <-time.After(time.Second):
		t.Fatal("B did not enter supervised recovery")
	}
	close(allowBProbe)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := s.hm.Get("B/m")
		if st.Status == health.Healthy && !st.Quarantined {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := s.hm.Get("B/m"); st.Status != health.Healthy || st.Quarantined || bProbes.Load() != 1 {
		t.Fatalf("B's first verified recovery did not restore ready state: state=%+v probes=%d", st, bProbes.Load())
	}
	var sawRetirement, sawCooldown, sawRecovery, sawModelUnavailable bool
	for _, ev := range s.bus.Snapshot() {
		if ev.Deployment == "A/m" && ev.SupervisorState == "retired" && ev.SupervisorTerminal {
			sawRetirement = true
		}
		if ev.Deployment == "C/m" && ev.SupervisorState == "cooldown" && ev.FailureClass == "rate_limit" {
			sawCooldown = true
		}
		if ev.Deployment == "B/m" && ev.Kind == "recovery_ready" {
			sawRecovery = true
		}
		if ev.Deployment == "B/m" && ev.SupervisorState == "model_unavailable" && ev.FailureClass == "model_temporarily_unavailable" {
			sawModelUnavailable = true
		}
	}
	if !sawRetirement || !sawCooldown || !sawRecovery || !sawModelUnavailable {
		t.Fatalf("missing structured lifecycle events retirement=%v model_unavailable=%v cooldown=%v recovery=%v", sawRetirement, sawModelUnavailable, sawCooldown, sawRecovery)
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

func TestRateLimitRetryAfterZeroUsesMinimumPositiveCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "A", Name: "A", Type: "anthropic_compatible", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "physical-a", Aliases: []string{"claude-auto"}, Enabled: true, Weight: 1}},
	}}
	gateway := testGateway(t, cfg)
	clock := time.Now()
	gateway.hm.SetNowFunc(func() time.Time { return clock })
	request := `{"model":"claude-auto","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	rr := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(request)))
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") != "0" {
		t.Fatalf("rate-limit terminal response status=%d Retry-After=%q body=%s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
	state := gateway.hm.Get("A/m")
	if state.Status != health.Cooldown || state.LastErrorClass != "rate_limit" || state.CooldownUntil.Sub(clock) != time.Second {
		t.Fatalf("Retry-After: 0 must use a one-second minimum, state=%+v", state)
	}
}

func TestClaudeCandidateExhaustionReturnsOneSafeTerminalError(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		aCalls.Add(1)
		w.Header().Set("X-Internal-Upstream", "A_PRIVATE_HEADER")
		w.Header().Set("Authorization", "Bearer A_PRIVATE_CREDENTIAL")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"A_PRIVATE_AUTH_BODY"}}`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bCalls.Add(1)
		w.Header().Set("X-Internal-Upstream", "B_PRIVATE_HEADER")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"B_PRIVATE_5XX_BODY"}}`)
	}))
	defer b.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "anthropic_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-a", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "anthropic_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-b", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 1, Weight: 1}}},
	}
	gateway := testGateway(t, cfg)
	request := `{"model":"claude-auto","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	rr := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(request)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhaustion status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
	var terminal map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &terminal); err != nil || terminal["type"] != "error" {
		t.Fatalf("terminal response is not one normalized Anthropic error: err=%v body=%s", err, rr.Body.String())
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatalf("exhaustion calls A/B=%d/%d, want 1/1", aCalls.Load(), bCalls.Load())
	}
	for _, private := range []string{"A_PRIVATE_AUTH_BODY", "B_PRIVATE_5XX_BODY", "A_PRIVATE_CREDENTIAL", "A_PRIVATE_HEADER", "B_PRIVATE_HEADER"} {
		if strings.Contains(rr.Body.String(), private) || strings.Contains(rr.Header().Get("X-Internal-Upstream"), private) {
			t.Fatalf("provider detail leaked in terminal response: %s", rr.Body.String())
		}
	}
	if rr.Header().Get("Authorization") != "" || rr.Header().Get("X-Internal-Upstream") != "" {
		t.Fatalf("provider response headers leaked: %v", rr.Header())
	}
	for _, event := range gateway.bus.Snapshot() {
		if strings.Contains(event.Message, "A_PRIVATE") || strings.Contains(event.Message, "B_PRIVATE") {
			t.Fatalf("provider detail leaked in supervisor event: %+v", event)
		}
	}
}

func TestClaudeStreamFailureFailoverStopsAtCommitBoundary(t *testing.T) {
	writeCompleteStream := func(w http.ResponseWriter, model, text string) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg","type":"message","role":"assistant","content":[],"model":%q,"usage":{"input_tokens":1,"output_tokens":0}}}

`, model)
		_, _ = io.WriteString(w, `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`)
		_, _ = fmt.Fprintf(w, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}

`, text)
		_, _ = io.WriteString(w, `event: content_block_stop
data: {"type":"content_block_stop","index":0}

`)
		_, _ = io.WriteString(w, `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

`)
		_, _ = io.WriteString(w, `event: message_stop
data: {"type":"message_stop"}

`)
	}

	t.Run("precommit failover", func(t *testing.T) {
		var aCalls, bCalls atomic.Int32
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			aCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `event: error
data: {"type":"error","error":{"type":"api_error","message":"A_PRIVATE_STREAM_FAILURE"}}

`)
		}))
		defer a.Close()
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			bCalls.Add(1)
			writeCompleteStream(w, "physical-b", "B_ONLY")
		}))
		defer b.Close()

		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Routing.Strategy = "priority"
		cfg.Routing.MaxAttempts = 2
		cfg.Routing.RetryBackoffMS = 0
		cfg.Providers = []config.ProviderConfig{
			{ID: "A", Name: "A", Type: "anthropic_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-a", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 0, Weight: 1}}},
			{ID: "B", Name: "B", Type: "anthropic_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-b", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 1, Weight: 1}}},
		}
		gateway := testGateway(t, cfg)
		request := `{"model":"claude-auto","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		rr := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(request)))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "B_ONLY") || strings.Contains(rr.Body.String(), "A_PRIVATE_STREAM_FAILURE") {
			t.Fatalf("precommit failure did not fail over to B cleanly: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if aCalls.Load() != 1 || bCalls.Load() != 1 {
			t.Fatalf("precommit attempts A/B=%d/%d, want 1/1", aCalls.Load(), bCalls.Load())
		}
		var eventFound bool
		for _, event := range gateway.bus.Snapshot() {
			if event.Kind == "stream_fail_precommit" && event.Deployment == "A/m" && event.StreamPhase == "precommit" {
				eventFound = true
			}
		}
		if !eventFound {
			t.Fatal("missing stream_fail_precommit event")
		}
	})

	t.Run("postcommit does not continue on B", func(t *testing.T) {
		var aCalls, bCalls atomic.Int32
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			aCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg-a","type":"message","role":"assistant","content":[],"model":"physical-a","usage":{"input_tokens":1}}}

`)
			_, _ = io.WriteString(w, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A_PARTIAL"}}

`)
		}))
		defer a.Close()
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			bCalls.Add(1)
			writeCompleteStream(w, "physical-b", "B_MUST_NOT_APPEAR")
		}))
		defer b.Close()

		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Routing.Strategy = "priority"
		cfg.Routing.MaxAttempts = 2
		cfg.Routing.RetryBackoffMS = 0
		cfg.Providers = []config.ProviderConfig{
			{ID: "A", Name: "A", Type: "anthropic_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-a", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 0, Weight: 1}}},
			{ID: "B", Name: "B", Type: "anthropic_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-b", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 1, Weight: 1}}},
		}
		gateway := testGateway(t, cfg)
		request := `{"model":"claude-auto","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		rr := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(request)))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "A_PARTIAL") || strings.Contains(rr.Body.String(), "B_MUST_NOT_APPEAR") {
			t.Fatalf("postcommit failure incorrectly continued on B: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if aCalls.Load() != 1 || bCalls.Load() != 0 {
			t.Fatalf("postcommit attempts A/B=%d/%d, want 1/0", aCalls.Load(), bCalls.Load())
		}
		var eventFound bool
		for _, event := range gateway.bus.Snapshot() {
			if event.Kind == "stream_fail_postcommit" && event.Deployment == "A/m" && event.StreamPhase == "postcommit" {
				eventFound = true
			}
		}
		if !eventFound {
			t.Fatal("missing stream_fail_postcommit event")
		}
	})
}

func TestAnthropicStreamInitialValidationFailsOverBeforeOpenAICommit(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		aCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg-a","type":"message","role":"assistant","content":[],"usage":{"input_tokens":-1}}}

`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chat-b","object":"chat.completion.chunk","model":"physical-b","choices":[{"index":0,"delta":{"role":"assistant","content":"B_ONLY"},"finish_reason":null}]}

`)
		_, _ = io.WriteString(w, `data: {"id":"chat-b","object":"chat.completion.chunk","model":"physical-b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer b.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "anthropic_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-a", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-b", Aliases: []string{"coding"}, Enabled: true, Priority: 1, Weight: 1}}},
	}
	gateway := testGateway(t, cfg)
	request := `{"model":"coding","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	rr := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(request)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "B_ONLY") {
		t.Fatalf("malformed initial Anthropic frame was not failed over before client commit: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatalf("initial-frame attempts A/B=%d/%d, want 1/1", aCalls.Load(), bCalls.Load())
	}
	var eventFound bool
	for _, event := range gateway.bus.Snapshot() {
		if event.Kind == "stream_fail_precommit" && event.Deployment == "A/m" && event.StreamPhase == "precommit" {
			eventFound = true
		}
	}
	if !eventFound {
		t.Fatal("missing precommit stream failure event for invalid initial usage")
	}
}

func TestClaudeUnsupportedForcedToolChoiceFailsOverWithoutRepair(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["tool_choice"] == nil {
			t.Errorf("forced tool_choice was silently removed before dispatch: %#v", payload)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"tool_choice","message":"A_PRIVATE_UNSUPPORTED tool_choice is not supported"}}`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-tool","object":"chat.completion","model":"physical-b","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"run","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer b.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.MaxRepairAttempts = 1
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Name: "A", Type: "openai_compatible", BaseURL: a.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-a", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}}},
		{ID: "B", Name: "B", Type: "openai_compatible", BaseURL: b.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "physical-b", Aliases: []string{"claude-auto"}, Enabled: true, Priority: 1, Weight: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}}},
	}
	s := testGateway(t, cfg)
	request := `{"model":"claude-auto","max_tokens":32,"messages":[{"role":"user","content":"run this"}],"tools":[{"name":"run","description":"run a command","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"tool","name":"run"}}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(request))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid Anthropic tool response: %v: %s", err, rr.Body.String())
	}
	content, _ := response["content"].([]any)
	if response["model"] != "claude-auto" || len(content) != 1 {
		t.Fatalf("tool fallback did not preserve public model/content: %s", rr.Body.String())
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "run" || strings.Contains(rr.Body.String(), "A_PRIVATE_UNSUPPORTED") {
		t.Fatalf("Claude did not receive only B's valid tool response: %s", rr.Body.String())
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatalf("calls A/B=%d/%d; expected one unrepaired A attempt and one B fallback", aCalls.Load(), bCalls.Load())
	}
	if state := s.hm.Get("A/m"); state.Quarantined || state.Status != health.Unknown {
		t.Fatalf("unsupported parameter damaged deployment health: %+v", state)
	}
	var sawFailover bool
	for _, event := range s.bus.Snapshot() {
		if event.Kind == "failover" && event.Deployment == "A/m" && event.FailureClass == "unsupported_parameter" {
			sawFailover = true
		}
		if strings.Contains(event.Message, "A_PRIVATE_UNSUPPORTED") {
			t.Fatalf("provider detail leaked into an event: %+v", event)
		}
	}
	if !sawFailover {
		t.Fatal("missing structured unsupported-parameter failover event")
	}

	// The forced choice is a hard semantic requirement. Once A's rejection is
	// learned, a later request must skip A rather than silently repairing by
	// removing tool_choice and changing the requested behavior.
	req2 := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(request))
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK || aCalls.Load() != 1 || bCalls.Load() != 2 {
		t.Fatalf("learned forced-tool incompatibility was not skipped: status=%d A/B=%d/%d body=%s", rr2.Code, aCalls.Load(), bCalls.Load(), rr2.Body.String())
	}
	var sawCapabilitySkip bool
	for _, event := range s.bus.Snapshot() {
		if event.Kind == "capability_skip" && event.Deployment == "A/m" {
			sawCapabilitySkip = true
		}
	}
	if !sawCapabilitySkip {
		t.Fatal("missing capability_skip event for known unsupported forced tool choice")
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
	if err := streamOpenAIToAnthropic(httptest.NewRecorder(), openAIResp, "m", nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("openai translated stream error=%v want unexpected EOF", err)
	}

	anthResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")),
	}
	if err := streamAnthropicToOpenAI(httptest.NewRecorder(), anthResp, "m", nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("anthropic translated stream error=%v want unexpected EOF", err)
	}
}

func TestCountTokensRejectsMalformedNativeSuccessAndFallsBackToEstimate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{bad")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		MessagesPath: "/v1/messages", CountTokensPath: "/v1/messages/count_tokens",
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages/count_tokens", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("fallback response is not valid JSON: %v body=%s", err, rr.Body.String())
	}
	if got["estimated"] != true {
		t.Fatalf("malformed native 2xx should fall back to estimate: %#v", got)
	}
}

func TestCountTokensRejectsOversizedNativeSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1,"padding":"`+strings.Repeat("x", (2<<20)+100)+`"}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		CountTokensPath: "/", MessagesPath: "/",
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages/count_tokens", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got["estimated"] != true {
		t.Fatalf("oversized native response should fall back to estimate: err=%v body=%s", err, rr.Body.String())
	}
}

func TestHotReloadInvalidatesHealthOnEnvironmentCredentialRotation(t *testing.T) {
	const envName = "NEXAROUTE_INTEGRATION_ROTATING_KEY"
	t.Setenv(envName, "key-one")
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		APIKeyEnv: envName, AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	if st := s.hm.Get("p/m"); st.Status != health.Healthy {
		t.Fatalf("fixture not healthy: %+v", st)
	}

	t.Setenv(envName, "key-two")
	if err := s.applyConfig(s.currentConfig()); err != nil {
		t.Fatal(err)
	}
	if st := s.hm.Get("p/m"); st.Status != health.Unknown {
		t.Fatalf("rotated environment credential kept stale health proof: %+v", st)
	}
}

func TestHotReloadInvalidatesHealthWhenCapabilitiesChange(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "m", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true},
		}},
	}}
	s := testGateway(t, cfg)
	if st := s.hm.Get("p/m"); st.Status != health.Healthy {
		t.Fatalf("fixture not healthy: %+v", st)
	}

	next := s.currentConfig()
	next.Providers[0].Models[0].Capabilities.Tools = true
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	if st := s.hm.Get("p/m"); st.Status != health.Unknown {
		t.Fatalf("capability identity change kept stale health proof: %+v", st)
	}
}

func TestOpenAINativeInvalid2xxFailsOverBeforeCommit(t *testing.T) {
	var badCalls, goodCalls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","object":"chat.completion","model":"good","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer good.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "bad", Name: "Bad", Type: "openai_compatible", BaseURL: bad.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "bad", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "good", Name: "Good", Type: "openai_compatible", BaseURL: good.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "good", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"content":"ok"`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if badCalls.Load() != 1 || goodCalls.Load() != 1 {
		t.Fatalf("unexpected calls bad=%d good=%d", badCalls.Load(), goodCalls.Load())
	}
	if st := s.hm.Get("bad/m"); st.Status == health.Healthy {
		t.Fatalf("invalid successful envelope remained healthy: %+v", st)
	}
}

func TestAnthropicNativeInvalid2xxFailsOverBeforeCommit(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":null}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"good","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer good.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "bad", Name: "Bad", Type: "anthropic_compatible", BaseURL: bad.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "bad", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "good", Name: "Good", Type: "anthropic_compatible", BaseURL: good.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "good", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"coding","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"text":"ok"`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if st := s.hm.Get("bad/m"); st.Status == health.Healthy {
		t.Fatalf("invalid Anthropic envelope remained healthy: %+v", st)
	}
}

func TestCrossProtocolInvalid2xxFailsOverBeforeCommit(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","role":"assistant","content":[{"type":"text","text":"fallback"}],"model":"good","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer good.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = []config.ProviderConfig{
		{ID: "bad", Name: "Bad", Type: "openai_compatible", BaseURL: bad.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "bad", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1}}},
		{ID: "good", Name: "Good", Type: "anthropic_compatible", BaseURL: good.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "good", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"coding","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"fallback"`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if st := s.hm.Get("bad/m"); st.Status == health.Healthy {
		t.Fatalf("invalid cross-protocol envelope remained healthy: %+v", st)
	}
}

func TestAnthropicReasoningStaysOnNativeProtocol(t *testing.T) {
	var openCalls, anthCalls atomic.Int32
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"bad","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"wrong"}}]}`)
	}))
	defer open.Close()
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","type":"message","role":"assistant","content":[{"type":"text","text":"native"}],"model":"a","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer anth.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{ID: "open", Name: "Open", Type: "openai_compatible", BaseURL: open.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "o", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Reasoning: true}}}},
		{ID: "anth", Name: "Anth", Type: "anthropic_compatible", BaseURL: anth.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "a", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1, Capabilities: config.Capabilities{Reasoning: true}}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"coding","max_tokens":8,"thinking":{"type":"enabled","budget_tokens":8},"messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "native") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if openCalls.Load() != 0 || anthCalls.Load() != 1 {
		t.Fatalf("reasoning crossed protocol: open=%d anth=%d", openCalls.Load(), anthCalls.Load())
	}
}

func TestOpenAIReasoningStaysOnNativeProtocol(t *testing.T) {
	var openCalls, anthCalls atomic.Int32
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"bad","type":"message","role":"assistant","content":[{"type":"text","text":"wrong"}]}`)
	}))
	defer anth.Close()
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"native"}}],"usage":{}}`)
	}))
	defer open.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{ID: "anth", Name: "Anth", Type: "anthropic_compatible", BaseURL: anth.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "a", Aliases: []string{"coding"}, Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Reasoning: true}}}},
		{ID: "open", Name: "Open", Type: "openai_compatible", BaseURL: open.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "o", Aliases: []string{"coding"}, Enabled: true, Priority: 10, Weight: 1, Capabilities: config.Capabilities{Reasoning: true}}}},
	}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"coding","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "native") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if anthCalls.Load() != 0 || openCalls.Load() != 1 {
		t.Fatalf("reasoning crossed protocol: anth=%d open=%d", anthCalls.Load(), openCalls.Load())
	}
}

func TestReasoningFallsBackToReasoningCapableOpenAIDeployment(t *testing.T) {
	var calls atomic.Int32
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "open", Name: "Open", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "o", Aliases: []string{"coding"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Reasoning: true}}},
	}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"coding","max_tokens":8,"thinking":{"type":"enabled","budget_tokens":9000},"messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rr.Code, rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("reasoning fallback should reach the capable deployment: %d", calls.Load())
	}
	// The thinking budget must be translated into reasoning_effort, and the
	// Anthropic-only thinking field must not leak to the OpenAI upstream.
	body := string(gotBody)
	if !strings.Contains(body, `"reasoning_effort":"medium"`) {
		t.Fatalf("thinking budget not translated: %s", body)
	}
	if strings.Contains(body, `"thinking"`) {
		t.Fatalf("anthropic thinking field leaked upstream: %s", body)
	}
	// The Anthropic client must receive a valid Anthropic-shaped response.
	if !strings.Contains(rr.Body.String(), `"type":"message"`) || !strings.Contains(rr.Body.String(), `"stop_reason"`) {
		t.Fatalf("response not Anthropic-shaped: %s", rr.Body.String())
	}
}

func TestHotReloadClosesIdlePoolOfRebuiltAdapter(t *testing.T) {
	var idleSeen atomic.Bool
	var closedCount atomic.Int32
	up1 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	up1.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			idleSeen.Store(true)
		case http.StateClosed:
			closedCount.Add(1)
		}
	}
	up1.Start()
	defer up1.Close()

	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer up2.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up1.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 2,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	oldAdapter, ok := s.reg.Get("p")
	if !ok {
		t.Fatal("missing provider adapter")
	}
	resp, err := oldAdapter.Do(context.Background(), []byte(`{"model":"m","messages":[]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	deadline := time.Now().Add(time.Second)
	for !idleSeen.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !idleSeen.Load() {
		t.Fatal("upstream connection never became idle")
	}

	next := s.currentConfig()
	next.Providers[0].BaseURL = up2.URL
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(time.Second)
	for closedCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if closedCount.Load() == 0 {
		t.Fatal("hot reload left the retired adapter idle connection open")
	}
	newAdapter, _ := s.reg.Get("p")
	if newAdapter == oldAdapter {
		t.Fatal("provider identity change reused the old adapter")
	}
}

func TestRoutingOnlyReloadKeepsExistingAdapterPool(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		AuthMode: "none", Enabled: true, MaxConcurrency: 2,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	before, _ := s.reg.Get("p")
	next := s.currentConfig()
	next.Routing.CapacityWeight += 1
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	after, _ := s.reg.Get("p")
	if before != after {
		t.Fatal("routing-only reload rebuilt provider adapter instead of reusing its pool")
	}
}

func TestRetryAfterCapReloadRebuildsProviderAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		AuthMode: "none", Enabled: true, MaxConcurrency: 2,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	before, _ := s.reg.Get("p")
	next := s.currentConfig()
	next.Routing.MaxRetryAfterSeconds++
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	after, _ := s.reg.Get("p")
	if before == after {
		t.Fatal("retry-after cap change reused adapter with stale retry policy")
	}
}

func TestSavedProviderCredentialsAreWriteOnly(t *testing.T) {
	t.Setenv("WRITE_ONLY_TEST_KEY", "environment-secret")
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.BindLocalOnly = false
	cfg.Providers = []config.ProviderConfig{{ID: "private", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1", APIKey: "primary-secret", APIKeyEnv: "WRITE_ONLY_TEST_KEY", AuthMode: "bearer", Headers: map[string]string{"X-Token": "header-secret"}, ProxyURL: "http://user:proxy-secret@127.0.0.1:9998", Credentials: []config.CredentialConfig{{Name: "extra", APIKey: "pool-secret", Enabled: true}}, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	for _, path := range []string{"/admin/api/providers/private", "/admin/api/providers/private?reveal=1", "/admin/api/providers/private?reveal=true", "/admin/api/providers", "/admin/api/snapshot"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://localhost"+path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		for _, secret := range []string{"primary-secret", "environment-secret", "pool-secret", "header-secret", "proxy-secret", "resolved_api_key"} {
			if strings.Contains(rr.Body.String(), secret) {
				t.Fatalf("%s leaked %s", path, secret)
			}
		}
	}
	// Redacting a read must not mutate live configuration or credential slices.
	got := s.currentConfig().Providers[0]
	if got.APIKey != "primary-secret" || got.Credentials[0].APIKey != "pool-secret" || got.Headers["X-Token"] != "header-secret" {
		t.Fatal("read mutated saved secrets")
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "http://localhost/admin/api/providers/private", strings.NewReader(`{"provider":{"id":"private","name":"Renamed","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":false,"models":[]},"preserve_secret":true,"preserve_headers":true,"preserve_proxy":true}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	got = s.currentConfig().Providers[0]
	if got.APIKey != "primary-secret" || got.Credentials[0].APIKey != "pool-secret" || got.Headers["X-Token"] != "header-secret" || !strings.Contains(got.ProxyURL, "proxy-secret") {
		t.Fatal("edit lost saved secrets")
	}
}
