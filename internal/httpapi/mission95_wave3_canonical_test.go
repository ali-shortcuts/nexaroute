package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Wave-3 coverage tests: canonical ingress handlers (anthropic, responses,
// chat) plus count_tokens. Helpers are wave3-prefixed.

func wave3Provider(id, typ, baseURL, modelID, upstreamModel string, aliases []string) config.ProviderConfig {
	return config.ProviderConfig{
		ID: id, Name: id, Type: typ, BaseURL: baseURL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: modelID, Model: upstreamModel, Aliases: aliases, Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true},
		}},
	}
}

func wave3Gateway(t *testing.T, providers []config.ProviderConfig, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = providers
	if mutate != nil {
		mutate(&cfg)
	}
	return testGateway(t, cfg)
}

func wave3RequestCtx(s *Server, ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, "http://gateway"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

const wave3AnthropicOK = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"upstream-model","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`

const wave3ChatOK = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"model":"upstream-model","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`

const wave3AnthropicSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"upstream-model\",\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

const wave3ChatSSE = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}],\"model\":\"upstream-model\"}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

func wave3AnthropicReq(model string) string {
	return `{"model":"` + model + `","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
}

func wave3AnthropicStreamReq(model string) string {
	return `{"model":"` + model + `","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
}

func wave3ChatReq(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

func wave3ResponsesReq(model string) string {
	return `{"model":"` + model + `","input":"hi"}`
}

// wave3JSONServer serves body with the given status and counts calls.
func wave3JSONServer(t *testing.T, status int, body string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func TestWave3AnthropicTooLarge(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3AnthropicOK, nil)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, nil)
	body := `{"model":"cm","max_tokens":32,"messages":[{"role":"user","content":"` + strings.Repeat("x", (16<<20)+1) + `"}]}`
	rr := wave3Request(s, "POST", "/v1/messages", body)
	if rr.Code != 413 {
		t.Fatalf("oversize anthropic status=%d want 413", rr.Code)
	}
}

func TestWave3AnthropicTooComplex(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3AnthropicOK, nil)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, nil)
	var b strings.Builder
	b.WriteString(`{"model":"cm","max_tokens":32,"messages":[`)
	for i := 0; i < 100001; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"role":"user","content":"hi"}`)
	}
	b.WriteString(`]}`)
	rr := wave3Request(s, "POST", "/v1/messages", b.String())
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "too complex") {
		t.Fatalf("over-complex anthropic status=%d body=%s want 400 too complex", rr.Code, rr.Body.String())
	}
}

func TestWave3AnthropicVirtualEndpointGuards(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3AnthropicOK, nil)
	defer up.Close()
	mk := func(ve config.VirtualEndpointConfig) *Server {
		return wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
			cfg.RouteProfiles = []config.RouteProfileConfig{{
				ID:            "rp",
				CandidatePool: "pool",
			}}
			cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
			cfg.VirtualEndpoints = []config.VirtualEndpointConfig{ve}
		})
	}
	disabled := func() *bool { b := false; return &b }()
	enabled := func() *bool { b := true; return &b }()
	s := mk(config.VirtualEndpointConfig{ID: "ve", PublicModel: "ve-model", RouteProfile: "rp", Enabled: disabled})
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("ve-model"))
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatalf("disabled VE status=%d body=%s want 404 disabled", rr.Code, rr.Body.String())
	}
	s = mk(config.VirtualEndpointConfig{ID: "ve", PublicModel: "ve-model", RouteProfile: "rp", Enabled: enabled, Protocols: []string{"openai"}})
	rr = wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("ve-model"))
	if rr.Code != 400 {
		t.Fatalf("protocol-restricted VE status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
}

func TestWave3AnthropicCapabilitySkip(t *testing.T) {
	var calls atomic.Int32
	up := wave3JSONServer(t, 200, wave3AnthropicOK, &calls)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, nil)
	s.capStore.LearnUnsupported("a/m", compat.CapTools, compat.SourceRuntime, "wave3 test", "wave3-key")
	body := `{"model":"cm","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"}}]}`
	rr := wave3Request(s, "POST", "/v1/messages", body)
	if rr.Code != 503 {
		t.Fatalf("capability-skipped anthropic status=%d body=%s want 503", rr.Code, rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("capability-skipped anthropic upstream calls=%d want 0", calls.Load())
	}
	if !wave3BusHasKindDeployment(s, "capability_skip", "a/m") {
		t.Fatalf("capability_skip event missing for a/m")
	}
}

func TestWave3AnthropicCapabilitySkipClientGone(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3AnthropicOK, nil)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, nil)
	s.capStore.LearnUnsupported("a/m", compat.CapTools, compat.SourceRuntime, "wave3 test", "wave3-key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := `{"model":"cm","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"}}]}`
	rr := wave3RequestCtx(s, ctx, "POST", "/v1/messages", body)
	if rr.Body.Len() != 0 {
		t.Fatalf("cancelled capability-skipped request body=%q want empty", rr.Body.String())
	}
}

func TestWave3AnthropicCircuitSkip(t *testing.T) {
	var calls1 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
	}))
	defer up1.Close()
	// One provider, two deployments sharing the provider circuit. The
	// threshold is 2 and one failure is pre-seeded from a third deployment,
	// so the first candidate's 401 trips the circuit and the second
	// candidate's re-check fails without ever being called. Both
	// deployments share the provider adapter, so a second attempt would
	// show up as a second call on the same upstream.
	s := wave3Gateway(t, nil, func(cfg *config.Config) {
		cfg.Routing.ProviderFailureThreshold = 2
		cfg.Providers = []config.ProviderConfig{{
			ID: "a", Name: "a", Type: "anthropic_compatible", BaseURL: up1.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{
				{ID: "m1", Model: "upstream-model", Aliases: []string{"cm"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}},
				{ID: "m2", Model: "upstream-model", Aliases: []string{"cm"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}},
			},
		}}
	})
	_ = up1
	s.hm.RecordProviderFailure("a", "a/other", "wave3 seed")
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("cm"))
	if rr.Code != 401 {
		t.Fatalf("circuit-skipped anthropic status=%d body=%s want 401 raw terminal", rr.Code, rr.Body.String())
	}
	if calls1.Load() != 1 {
		t.Fatalf("upstream calls=%d want 1 (second candidate must never be attempted)", calls1.Load())
	}
}

func TestWave3AnthropicRepairRetryTransportFail(t *testing.T) {
	var calls atomic.Int32
	var bodies [][]byte
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
	})
	body := `{"model":"cm","max_tokens":32,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
	rr := wave3Request(s, "POST", "/v1/messages", body)
	if rr.Code != 502 {
		t.Fatalf("repair-retry transport status=%d body=%s want 502", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("repair-retry calls=%d want 2", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d want 2", len(bodies))
	}
	if !strings.Contains(string(bodies[0]), "temperature") {
		t.Fatalf("first payload missing temperature: %s", bodies[0])
	}
	if strings.Contains(string(bodies[1]), "temperature") {
		t.Fatalf("repaired payload still has temperature: %s", bodies[1])
	}
	if !wave3BusHasKind(s, "compat_repair") {
		t.Fatalf("compat_repair event missing")
	}
}

func TestWave3AnthropicStreamOptionsStrip(t *testing.T) {
	var calls atomic.Int32
	var bodies [][]byte
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"request contains stream_options which this endpoint rejects","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wave3ChatSSE)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
	})
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicStreamReq("cm"))
	if rr.Code != 200 {
		t.Fatalf("stream-options strip status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("stream-options strip calls=%d want 2", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(bodies[0]), "stream_options") {
		t.Fatalf("first payload missing injected stream_options: %s", bodies[0])
	}
	if strings.Contains(string(bodies[1]), "stream_options") {
		t.Fatalf("stripped payload still has stream_options: %s", bodies[1])
	}
	if !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("stripped retry body missing translated text: %s", rr.Body.String())
	}
}

func TestWave3AnthropicStreamOptionsNoMention(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request, nope","type":"invalid_request_error"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
	})
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicStreamReq("cm"))
	if rr.Code != 400 {
		t.Fatalf("unstripped 400 status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("unstripped 400 calls=%d want 1 (no strip retry)", calls.Load())
	}
}

func TestWave3AnthropicTransportFailover(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	var goodCalls atomic.Int32
	good := wave3JSONServer(t, 200, wave3AnthropicOK, &goodCalls)
	defer good.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "anthropic_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "anthropic_compatible", good.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("cm"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"text":"ok"`) {
		t.Fatalf("transport failover status=%d body=%s want 200 ok", rr.Code, rr.Body.String())
	}
	if goodCalls.Load() != 1 {
		t.Fatalf("good candidate calls=%d want 1", goodCalls.Load())
	}
	if !wave3BusHasKind(s, "failover") {
		t.Fatalf("failover event missing")
	}
}

func TestWave3AnthropicReadyQuarantineTransport(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "anthropic_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "ready_queue" })
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("cm"))
	if rr.Code != 502 {
		t.Fatalf("ready transport status=%d body=%s want 502", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "route_fail", "a/m") {
		t.Fatalf("route_fail event missing for a/m")
	}
}

func TestWave3AnthropicCapabilityLabel(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"temperature is not supported by this model"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
	})
	body := `{"model":"cm","max_tokens":32,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
	rr := wave3Request(s, "POST", "/v1/messages", body)
	if rr.Code != 400 {
		t.Fatalf("capability label status=%d body=%s want 400 raw passthrough", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("capability label calls=%d want 2 (repair retry, then terminal)", calls.Load())
	}
	contract := s.capStore.Get("a/m")
	if contract.Capabilities.Temperature != compat.Unsupported {
		t.Fatalf("temperature contract=%v want unsupported after surviving repair", contract.Capabilities.Temperature)
	}
}

func TestWave3AnthropicNativeStreamingUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wave3AnthropicSSE)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
	})
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicStreamReq("cm"))
	if rr.Code != 200 {
		t.Fatalf("native streaming status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"text":"ok"`) || !strings.Contains(rr.Body.String(), `"output_tokens":5`) {
		t.Fatalf("native streaming body missing text/usage: %s", rr.Body.String())
	}
}

func TestWave3AnthropicDecodeClientGone(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- wave3RequestCtx(s, ctx, "POST", "/v1/messages", wave3AnthropicReq("cm"))
	}()
	<-entered
	cancel()
	select {
	case rr := <-done:
		if rr.Body.Len() != 0 {
			t.Fatalf("cancelled decode body=%q want empty", rr.Body.String())
		}
		if !wave3BusHasKind(s, "client_disconnect") {
			t.Fatalf("client_disconnect event missing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled decode handler did not return")
	}
}

func TestWave3AnthropicDecodeFailures(t *testing.T) {
	mk := func(strategy string, stream bool) (*Server, *httptest.ResponseRecorder) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stream {
				// A streaming request whose upstream answers 200 with a
				// non-SSE content type fails inside the native SSE proxy
				// before any byte is committed: the precommit path.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, `{"not":"sse"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "{not json")
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
			cfg.Routing.Strategy = strategy
		})
		body := wave3AnthropicReq("cm")
		if stream {
			body = wave3AnthropicStreamReq("cm")
		}
		return s, wave3Request(s, "POST", "/v1/messages", body)
	}
	// Non-stream + ready strategy: quarantine branch.
	s, rr := mk("ready_queue", false)
	if rr.Code != 502 || !strings.Contains(rr.Body.String(), "upstream returned an invalid response") {
		t.Fatalf("ready decode-fail status=%d body=%s want 502 invalid response", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "response_decode_fail", "a/m") {
		t.Fatalf("response_decode_fail event missing (ready)")
	}
	// Non-stream + default strategy: record-failure branch.
	s, rr = mk("priority", false)
	if rr.Code != 502 {
		t.Fatalf("default decode-fail status=%d want 502", rr.Code)
	}
	if !wave3BusHasKindDeployment(s, "response_decode_fail", "a/m") {
		t.Fatalf("response_decode_fail event missing (default)")
	}
	// Streaming + ready_mesh: scope-failure branch (precommit).
	s, rr = mk("ready_mesh", true)
	if rr.Code != 502 {
		t.Fatalf("ready_mesh stream decode-fail status=%d want 502", rr.Code)
	}
	if !wave3BusHasKind(s, "stream_fail_precommit") {
		t.Fatalf("stream_fail_precommit event missing (ready_mesh)")
	}
	// Streaming + default strategy, non-SSE upstream body: precommit kind.
	s, rr = mk("priority", true)
	if rr.Code != 502 {
		t.Fatalf("default stream decode-fail status=%d want 502", rr.Code)
	}
	if !wave3BusHasKind(s, "stream_fail_precommit") {
		t.Fatalf("stream_fail_precommit event missing")
	}
}

func TestWave3AnthropicStreamCommittedFailures(t *testing.T) {
	mk := func(strategy string) (*Server, *httptest.ResponseRecorder) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {garbage\n\n")
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
			cfg.Routing.Strategy = strategy
		})
		return s, wave3Request(s, "POST", "/v1/messages", wave3AnthropicStreamReq("cm"))
	}
	s, rr := mk("priority")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("committed stream-fail status=%d body=%s want 200 with partial text", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "stream_fail", "a/m") {
		t.Fatalf("stream_fail event missing (default)")
	}
	s, rr = mk("ready_queue")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("ready committed stream-fail status=%d body=%s want 200 with partial text", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "stream_fail", "a/m") {
		t.Fatalf("stream_fail event missing (ready)")
	}
	s, rr = mk("ready_mesh")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("ready_mesh committed stream-fail status=%d body=%s want 200 with partial text", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "stream_fail", "a/m") {
		t.Fatalf("stream_fail event missing (ready_mesh)")
	}
}

func TestWave3AnthropicRetryAfterTerminal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"})}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
	})
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("cm"))
	if rr.Code != 429 {
		t.Fatalf("retry-after terminal status=%d body=%s want 429", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != "7" {
		t.Fatalf("Retry-After header=%q want 7", rr.Header().Get("Retry-After"))
	}
	if !strings.Contains(rr.Body.String(), "slow down") {
		t.Fatalf("retry-after terminal body not raw upstream: %s", rr.Body.String())
	}
}

func TestWave3AnthropicHedgeBothFail(t *testing.T) {
	release := make(chan struct{})
	entered1 := make(chan struct{})
	entered2 := make(chan struct{})
	var once1, once2 sync.Once
	var calls1, calls2 atomic.Int32
	hijackClose := func(entered chan struct{}, once *sync.Once, calls *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			once.Do(func() { close(entered) })
			<-release
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}
	up1 := httptest.NewServer(hijackClose(entered1, &once1, &calls1))
	defer up1.Close()
	up2 := httptest.NewServer(hijackClose(entered2, &once2, &calls2))
	defer up2.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up1.URL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "openai_compatible", up2.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Routing.HedgingEnabled = true
		cfg.Routing.HedgingDelayMS = 50
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("cm"))
	}()
	// Both legs in flight: the primary immediately, the partner once the
	// hedge delay fires. Channel receipt is the synchronization.
	select {
	case <-entered1:
	case <-time.After(10 * time.Second):
		t.Fatal("primary hedge leg did not start")
	}
	select {
	case <-entered2:
	case <-time.After(10 * time.Second):
		t.Fatal("hedge partner leg did not start")
	}
	close(release)
	select {
	case rr := <-done:
		if rr.Code != 502 {
			t.Fatalf("hedge both-fail status=%d body=%s want 502", rr.Code, rr.Body.String())
		}
		if calls1.Load() != 1 || calls2.Load() != 1 {
			t.Fatalf("hedge legs calls=%d/%d want 1/1", calls1.Load(), calls2.Load())
		}
		if !wave3BusHasKind(s, "hedge_launch") {
			t.Fatalf("hedge_launch event missing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hedge both-fail handler did not return")
	}
}

func TestWave3AnthropicHedgeSecondaryWins(t *testing.T) {
	release := make(chan struct{})
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up1.Close()
	var calls2 atomic.Int32
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls2.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wave3ChatOK)
	}))
	defer up2.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up1.URL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "openai_compatible", up2.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Routing.HedgingEnabled = true
		cfg.Routing.HedgingDelayMS = 50
	})
	rr := wave3Request(s, "POST", "/v1/messages", wave3AnthropicReq("cm"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"text":"ok"`) {
		t.Fatalf("hedge secondary-wins status=%d body=%s want 200 ok", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Gateway-Deployment") != "b/m" {
		t.Fatalf("winner deployment header=%q want b/m", rr.Header().Get("X-Gateway-Deployment"))
	}
	if calls2.Load() != 1 {
		t.Fatalf("partner calls=%d want 1", calls2.Load())
	}
	if !wave3BusHasKind(s, "hedge_launch") {
		t.Fatalf("hedge_launch event missing")
	}
	// The abandoned primary leg's server-side handler does not observe
	// the leg cancellation promptly in this sandbox: neither the
	// gateway's leg-cancel nor an explicit CloseClientConnections()
	// unblocks it, so test cleanup would hang on up1.Close().
	// Releasing the handler directly after all assertions have passed
	// keeps cleanup deterministic without weakening any assertion
	// (the secondary-won outcome is already fully proven above).
	// Observation for the parent: abandoned hedge legs appear to
	// retain their upstream connection past leg-cancel in this
	// environment; not a production-code change in this task.
	close(release)
}

func TestWave3AnthropicSemanticHit(t *testing.T) {
	var chatCalls, embCalls atomic.Int32
	chat := wave3JSONServer(t, 200, wave3AnthropicOK, &chatCalls)
	defer chat.Close()
	emb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"embedding":[1,0,0]}]}`)
	}))
	defer emb.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "anthropic_compatible", chat.URL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("emb", "openai_compatible", emb.URL, "emb-model", "text-embedding-3-small", nil),
	}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Cache.Semantic.Enabled = true
		cfg.Cache.Semantic.EmbeddingDeployment = "emb/emb-model"
		cfg.Cache.Semantic.EmbeddingPath = "/v1/embeddings"
		cfg.Cache.Semantic.Threshold = 0.95
	})
	body := wave3AnthropicReq("cm")
	rr1 := wave3Request(s, "POST", "/v1/messages", body)
	if rr1.Code != 200 {
		t.Fatalf("first semantic request status=%d body=%s want 200", rr1.Code, rr1.Body.String())
	}
	if rr1.Header().Get("X-NexaRoute-Cache") == "SEMANTIC-HIT" {
		t.Fatalf("first request unexpectedly served from semantic cache")
	}
	rr2 := wave3Request(s, "POST", "/v1/messages", body)
	if rr2.Code != 200 {
		t.Fatalf("second semantic request status=%d body=%s want 200", rr2.Code, rr2.Body.String())
	}
	if rr2.Header().Get("X-NexaRoute-Cache") != "SEMANTIC-HIT" {
		t.Fatalf("second request cache header=%q want SEMANTIC-HIT body=%s", rr2.Header().Get("X-NexaRoute-Cache"), rr2.Body.String())
	}
	if chatCalls.Load() != 1 {
		t.Fatalf("chat upstream calls=%d want 1 (second request must not hit upstream)", chatCalls.Load())
	}
	if embCalls.Load() != 2 {
		t.Fatalf("embedding calls=%d want 2 (one per request for lookup)", embCalls.Load())
	}
	if !wave3BusHasKind(s, "semantic_cache_hit") {
		t.Fatalf("semantic_cache_hit event missing")
	}
}

// ---- openAIResponses (/v1/responses) ----

func TestWave3ResponsesTransportFailover(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	var goodCalls atomic.Int32
	good := wave3JSONServer(t, 200, wave3ChatOK, &goodCalls)
	defer good.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "openai_compatible", good.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("responses transport failover status=%d body=%s want 200 ok", rr.Code, rr.Body.String())
	}
	if goodCalls.Load() != 1 {
		t.Fatalf("good candidate calls=%d want 1", goodCalls.Load())
	}
	if !wave3BusHasKindDeployment(s, "route_changed", "b/m") {
		t.Fatalf("route_changed event missing for b/m")
	}
}

func TestWave3ResponsesTransportError502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	if rr.Code != 502 || !strings.Contains(rr.Body.String(), "all candidate deployments failed") {
		t.Fatalf("responses transport status=%d body=%s want 502 all candidate deployments failed", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "model_failed", "a/m") {
		t.Fatalf("model_failed event missing for a/m")
	}
}

func TestWave3ResponsesReadyStrategyTransport(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "ready_queue" })
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	if rr.Code != 502 {
		t.Fatalf("responses ready transport status=%d body=%s want 502", rr.Code, rr.Body.String())
	}
}

func TestWave3ResponsesDeadline504(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	// Register cleanup after up.Close so a failing assertion still releases
	// the deliberately blocked upstream before httptest waits for connections.
	defer releaseUpstream()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Routing.RequestTimeoutMS = 1000
	})
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	// Unblock the fake provider before asserting so failure output cannot be
	// obscured by httptest.Server.Close waiting on the intentionally held connection.
	releaseUpstream()
	if rr.Code != 504 || !strings.Contains(rr.Body.String(), "gateway request timeout") {
		t.Fatalf("responses deadline status=%d body=%s want 504 gateway request timeout", rr.Code, rr.Body.String())
	}
}

func TestWave3ResponsesClientGoneOnTransportError(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- wave3RequestCtx(s, ctx, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	}()
	<-entered
	cancel()
	select {
	case rr := <-done:
		if rr.Body.Len() != 0 {
			t.Fatalf("cancelled responses body=%q want empty", rr.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled responses handler did not return")
	}
	// Sandbox observation (see TestWave3AnthropicHedgeSecondaryWins):
	// an upstream handler blocked before writing any bytes does not
	// observe cancellation promptly, so release it after the
	// empty-body assertion instead of hanging in up.Close().
	close(release)
}

func TestWave3ResponsesRepairThenTransportError(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	body := `{"model":"cm","input":"hi","temperature":0.7}`
	rr := wave3Request(s, "POST", "/v1/responses", body)
	if rr.Code != 502 {
		t.Fatalf("responses repair-retry transport status=%d body=%s want 502", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("responses repair-retry calls=%d want 2", calls.Load())
	}
	if !wave3BusHasKind(s, "compat_repair") {
		t.Fatalf("compat_repair event missing")
	}
}

func TestWave3ResponsesLifecycleErrors(t *testing.T) {
	mk := func(code string) *httptest.ResponseRecorder {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"model gone","type":"invalid_request_error"}}`)
		}))
		t.Cleanup(up.Close)
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		return wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	}
	rr := mk("model_retired")
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("retired responses status=%d body=%s want 503 candidate_exhausted", rr.Code, rr.Body.String())
	}
	rr = mk("model_unavailable")
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("unavailable responses status=%d body=%s want 503 candidate_exhausted", rr.Code, rr.Body.String())
	}
}

func TestWave3ResponsesRateLimitCooldown(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	if rr.Code != 429 {
		t.Fatalf("responses rate-limit status=%d body=%s want 429", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "provider_rate_limited", "a/m") {
		t.Fatalf("provider_rate_limited event missing")
	}
	if !wave3BusHasKindDeployment(s, "model_cooldown", "a/m") {
		t.Fatalf("model_cooldown event missing")
	}
}

func TestWave3ResponsesNativeRawError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":"plain failure"}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_responses", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	if rr.Code != 400 || rr.Body.String() != `{"error":"plain failure"}` {
		t.Fatalf("native raw error status=%d body=%s want 400 verbatim body", rr.Code, rr.Body.String())
	}
}

func TestWave3ResponsesMalformedFailover(t *testing.T) {
	var calls1, calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{bad json`)
	}))
	defer up1.Close()
	up2 := wave3JSONServer(t, 200, wave3ChatOK, &calls2)
	defer up2.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up1.URL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "openai_compatible", up2.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("malformed failover status=%d body=%s want 200 ok", rr.Code, rr.Body.String())
	}
	if calls1.Load() != 1 || calls2.Load() != 1 {
		t.Fatalf("malformed failover calls=%d/%d want 1/1", calls1.Load(), calls2.Load())
	}
}

func TestWave3ResponsesUsageErrorClientGone(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- wave3RequestCtx(s, ctx, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	}()
	<-entered
	cancel()
	select {
	case rr := <-done:
		if rr.Body.Len() != 0 {
			t.Fatalf("cancelled usage-error body=%q want empty", rr.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled usage-error handler did not return")
	}
}

func TestWave3ResponsesStreamFailAfterCommit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {garbage\n\n")
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	body := `{"model":"cm","input":"hi","stream":true}`
	rr := wave3Request(s, "POST", "/v1/responses", body)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("responses stream-fail status=%d body=%s want 200 with partial text", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "stream_fail", "a/m") {
		t.Fatalf("stream_fail event missing")
	}
}

func TestWave3ResponsesBuildFailCircuit(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"authentication_error"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, nil, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Routing.ProviderFailureThreshold = 2
		cfg.Providers = []config.ProviderConfig{{
			ID: "a", Name: "a", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{
				{ID: "m1", Model: "upstream-model", Aliases: []string{"cm"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}},
				{ID: "m2", Model: "upstream-model", Aliases: []string{"cm"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}},
			},
		}}
	})
	s.hm.RecordProviderFailure("a", "a/other", "wave3 seed")
	rr := wave3Request(s, "POST", "/v1/responses", wave3ResponsesReq("cm"))
	if rr.Code != 502 || !strings.Contains(rr.Body.String(), "candidate could not serve this request") {
		t.Fatalf("responses circuit build-fail status=%d body=%s want 502 candidate could not serve", rr.Code, rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("responses circuit calls=%d want 1", calls.Load())
	}
}

// ---- openAIChat (/v1/chat/completions) ----

func TestWave3OpenAIToolChoiceString(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wave3ChatOK)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	body := `{"model":"cm","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"tool_choice":"required"}`
	rr := wave3Request(s, "POST", "/v1/chat/completions", body)
	if rr.Code != 200 {
		t.Fatalf("tool_choice string status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	if got["tool_choice"] != "required" {
		t.Fatalf("upstream tool_choice=%v want required", got["tool_choice"])
	}
}

func TestWave3OpenAITooComplex(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3ChatOK, nil)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	var b strings.Builder
	b.WriteString(`{"model":"cm","messages":[`)
	for i := 0; i < 100001; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"role":"user","content":"hi"}`)
	}
	b.WriteString(`]}`)
	rr := wave3Request(s, "POST", "/v1/chat/completions", b.String())
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "too complex") {
		t.Fatalf("over-complex chat status=%d body=%s want 400 too complex", rr.Code, rr.Body.String())
	}
}

func TestWave3OpenAIReasoningRelaxed(t *testing.T) {
	var calls atomic.Int32
	up := wave3JSONServer(t, 200, wave3AnthropicOK, &calls)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	body := `{"model":"cm","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	rr := wave3Request(s, "POST", "/v1/chat/completions", body)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("reasoning relaxed status=%d body=%s want 200 translated ok", rr.Code, rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("reasoning relaxed calls=%d want 1", calls.Load())
	}
}

func TestWave3OpenAIRepairRetryTransportFail(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	body := `{"model":"cm","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`
	rr := wave3Request(s, "POST", "/v1/chat/completions", body)
	if rr.Code != 502 {
		t.Fatalf("chat repair-retry transport status=%d body=%s want 502", rr.Code, rr.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("chat repair-retry calls=%d want 2", calls.Load())
	}
}

func TestWave3OpenAITransportFailover(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	var goodCalls atomic.Int32
	good := wave3JSONServer(t, 200, wave3ChatOK, &goodCalls)
	defer good.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "openai_compatible", good.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/chat/completions", wave3ChatReq("cm"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("chat transport failover status=%d body=%s want 200 ok", rr.Code, rr.Body.String())
	}
	if goodCalls.Load() != 1 {
		t.Fatalf("good candidate calls=%d want 1", goodCalls.Load())
	}
}

func TestWave3OpenAITemporarilyUnavailable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"code":"model_unavailable","message":"model down","type":"invalid_request_error"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/chat/completions", wave3ChatReq("cm"))
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("chat unavailable status=%d body=%s want 503 candidate_exhausted", rr.Code, rr.Body.String())
	}
}

func TestWave3OpenAIReadyQuarantineCommitted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {garbage\n\n")
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "ready_queue" })
	body := `{"model":"cm","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rr := wave3Request(s, "POST", "/v1/chat/completions", body)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("chat ready committed status=%d body=%s want 200 partial", rr.Code, rr.Body.String())
	}
	if !wave3BusHasKindDeployment(s, "stream_fail", "a/m") {
		t.Fatalf("stream_fail event missing (chat ready)")
	}
}

func TestWave3OpenAIClientGonePostLoop(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3ChatOK, nil)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	s.capStore.LearnUnsupported("a/m", compat.CapTools, compat.SourceRuntime, "wave3 test", "wave3-key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := `{"model":"cm","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	rr := wave3RequestCtx(s, ctx, "POST", "/v1/chat/completions", body)
	if rr.Body.Len() != 0 {
		t.Fatalf("cancelled chat post-loop body=%q want empty", rr.Body.String())
	}
}

func TestWave3OpenAICircuitSkip(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"authentication_error"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, nil, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Routing.ProviderFailureThreshold = 2
		cfg.Providers = []config.ProviderConfig{{
			ID: "a", Name: "a", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{
				{ID: "m1", Model: "upstream-model", Aliases: []string{"cm"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}},
				{ID: "m2", Model: "upstream-model", Aliases: []string{"cm"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}},
			},
		}}
	})
	s.hm.RecordProviderFailure("a", "a/other", "wave3 seed")
	rr := wave3Request(s, "POST", "/v1/chat/completions", wave3ChatReq("cm"))
	if rr.Code != 401 {
		t.Fatalf("chat circuit skip status=%d body=%s want 401 raw terminal", rr.Code, rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("chat circuit calls=%d want 1", calls.Load())
	}
}

func TestWave3OpenAIRetryAfterTerminal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	}))
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	rr := wave3Request(s, "POST", "/v1/chat/completions", wave3ChatReq("cm"))
	if rr.Code != 429 {
		t.Fatalf("chat retry-after status=%d body=%s want 429", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != "7" {
		t.Fatalf("chat Retry-After header=%q want 7", rr.Header().Get("Retry-After"))
	}
	if !strings.Contains(rr.Body.String(), "slow down") {
		t.Fatalf("chat retry-after body not raw upstream: %s", rr.Body.String())
	}
}

func TestWave3OpenAIAttemptsZero(t *testing.T) {
	up := wave3JSONServer(t, 200, wave3ChatOK, nil)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	s.capStore.LearnUnsupported("a/m", compat.CapTools, compat.SourceRuntime, "wave3 test", "wave3-key")
	body := `{"model":"cm","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	rr := wave3Request(s, "POST", "/v1/chat/completions", body)
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("chat attempts-zero status=%d body=%s want 503 no compatible healthy deployment", rr.Code, rr.Body.String())
	}
}

func TestWave3OpenAIHedgeBothFail(t *testing.T) {
	release := make(chan struct{})
	entered1 := make(chan struct{})
	entered2 := make(chan struct{})
	var once1, once2 sync.Once
	var calls1, calls2 atomic.Int32
	hijackClose := func(entered chan struct{}, once *sync.Once, calls *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			once.Do(func() { close(entered) })
			<-release
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}
	up1 := httptest.NewServer(hijackClose(entered1, &once1, &calls1))
	defer up1.Close()
	up2 := httptest.NewServer(hijackClose(entered2, &once2, &calls2))
	defer up2.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "openai_compatible", up1.URL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "openai_compatible", up2.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) {
		cfg.Routing.Strategy = "priority"
		cfg.Routing.HedgingEnabled = true
		cfg.Routing.HedgingDelayMS = 50
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- wave3Request(s, "POST", "/v1/chat/completions", wave3ChatReq("cm"))
	}()
	select {
	case <-entered1:
	case <-time.After(10 * time.Second):
		t.Fatal("chat primary hedge leg did not start")
	}
	select {
	case <-entered2:
	case <-time.After(10 * time.Second):
		t.Fatal("chat hedge partner leg did not start")
	}
	close(release)
	select {
	case rr := <-done:
		if rr.Code != 502 {
			t.Fatalf("chat hedge both-fail status=%d body=%s want 502", rr.Code, rr.Body.String())
		}
		if calls1.Load() != 1 || calls2.Load() != 1 {
			t.Fatalf("chat hedge legs calls=%d/%d want 1/1", calls1.Load(), calls2.Load())
		}
		if !wave3BusHasKind(s, "hedge_launch") {
			t.Fatalf("hedge_launch event missing (chat)")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("chat hedge both-fail handler did not return")
	}
}

// ---- countTokens (/v1/messages/count_tokens) ----

func wave3CountReq(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

func wave3CountTokens(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rr.Code != 200 {
		t.Fatalf("count_tokens status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	return wave3JSON(t, rr)
}

func TestWave3CountTokensGuards(t *testing.T) {
	up := wave3JSONServer(t, 200, `{"input_tokens":42}`, nil)
	defer up.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	if rr := wave3Request(s, "GET", "/v1/messages/count_tokens", ""); rr.Code != 405 {
		t.Fatalf("GET count_tokens status=%d want 405", rr.Code)
	}
	if rr := wave3Request(s, "POST", "/v1/messages/count_tokens", "{bad"); rr.Code != 400 {
		t.Fatalf("invalid JSON count_tokens status=%d want 400", rr.Code)
	}
	big := `{"model":"cm","messages":[{"role":"user","content":"` + strings.Repeat("x", (16<<20)+1) + `"}]}`
	if rr := wave3Request(s, "POST", "/v1/messages/count_tokens", big); rr.Code != 413 {
		t.Fatalf("oversize count_tokens status=%d want 413", rr.Code)
	}
	var b strings.Builder
	b.WriteString(`{"model":"cm","messages":[`)
	for i := 0; i < 100001; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"role":"user","content":"hi"}`)
	}
	b.WriteString(`]}`)
	if rr := wave3Request(s, "POST", "/v1/messages/count_tokens", b.String()); rr.Code != 400 || !strings.Contains(rr.Body.String(), "too complex") {
		t.Fatalf("over-complex count_tokens status=%d body=%s want 400 too complex", rr.Code, rr.Body.String())
	}
}

func TestWave3CountTokensVirtualEndpointGuards(t *testing.T) {
	up := wave3JSONServer(t, 200, `{"input_tokens":42}`, nil)
	defer up.Close()
	mk := func(ve config.VirtualEndpointConfig) *Server {
		return wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) {
			cfg.Routing.Strategy = "priority"
			cfg.RouteProfiles = []config.RouteProfileConfig{{
				ID:            "rp",
				CandidatePool: "pool",
			}}
			cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m"}}}
			cfg.VirtualEndpoints = []config.VirtualEndpointConfig{ve}
		})
	}
	disabled := func() *bool { b := false; return &b }()
	enabled := func() *bool { b := true; return &b }()
	s := mk(config.VirtualEndpointConfig{ID: "ve", PublicModel: "ve-model", RouteProfile: "rp", Enabled: disabled})
	rr := wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("ve-model"))
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatalf("disabled VE count_tokens status=%d body=%s want 404 disabled", rr.Code, rr.Body.String())
	}
	s = mk(config.VirtualEndpointConfig{ID: "ve", PublicModel: "ve-model", RouteProfile: "rp", Enabled: enabled, Protocols: []string{"openai"}})
	rr = wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("ve-model"))
	if rr.Code != 400 {
		t.Fatalf("protocol-restricted VE count_tokens status=%d body=%s want 400", rr.Code, rr.Body.String())
	}
}

func TestWave3CountTokensNativePaths(t *testing.T) {
	// Native success.
	{
		up := wave3JSONServer(t, 200, `{"input_tokens":42}`, nil)
		defer up.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("cm")))
		if out["input_tokens"] != float64(42) {
			t.Fatalf("native count_tokens=%v want 42", out["input_tokens"])
		}
		if _, estimated := out["estimated"]; estimated {
			t.Fatalf("native count_tokens unexpectedly estimated: %v", out)
		}
	}
	// Non-anthropic candidate skipped: estimate path.
	{
		up := wave3JSONServer(t, 200, wave3ChatOK, nil)
		defer up.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "openai_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("cm")))
		if out["estimated"] != true {
			t.Fatalf("skipped-candidate count_tokens not estimated: %v", out)
		}
	}
	// Native transport error: estimate path.
	{
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadURL := dead.URL
		dead.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "anthropic_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("cm")))
		if out["estimated"] != true {
			t.Fatalf("transport-error count_tokens not estimated: %v", out)
		}
	}
	// Native 2xx invalid JSON / missing / negative input_tokens: estimate.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid-json", `{bad json`},
		{"missing", `{"something":1}`},
		{"negative", `{"input_tokens":-5}`},
	} {
		up := wave3JSONServer(t, 200, tc.body, nil)
		defer up.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("cm")))
		if out["estimated"] != true {
			t.Fatalf("%s count_tokens not estimated: %v", tc.name, out)
		}
	}
	// Native 400 (not failover-eligible): estimate, no failover.
	{
		var calls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"bad"}`)
		}))
		defer up.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("cm")))
		if out["estimated"] != true {
			t.Fatalf("400 count_tokens not estimated: %v", out)
		}
		if calls.Load() != 1 {
			t.Fatalf("400 count_tokens calls=%d want 1", calls.Load())
		}
	}
	// Native 429 then success: failover to the second candidate.
	{
		var calls1, calls2 atomic.Int32
		up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls1.Add(1)
			w.WriteHeader(429)
		}))
		defer up1.Close()
		up2 := wave3JSONServer(t, 200, `{"input_tokens":77}`, &calls2)
		defer up2.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "anthropic_compatible", up1.URL, "m", "upstream-model", []string{"cm"}),
			wave3Provider("b", "anthropic_compatible", up2.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("cm")))
		if out["input_tokens"] != float64(77) {
			t.Fatalf("failover count_tokens=%v want 77", out["input_tokens"])
		}
		if calls1.Load() != 1 || calls2.Load() != 1 {
			t.Fatalf("failover count_tokens calls=%d/%d want 1/1", calls1.Load(), calls2.Load())
		}
	}
	// Native oversize body (>2 MiB): estimate.
	{
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":1`+strings.Repeat(" ", 3<<20)+`}`)
		}))
		defer up.Close()
		s := wave3Gateway(t, []config.ProviderConfig{
			wave3Provider("a", "anthropic_compatible", up.URL, "m", "upstream-model", []string{"cm"}),
		}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", wave3CountReq("cm")))
		if out["estimated"] != true {
			t.Fatalf("oversize count_tokens not estimated: %v", out)
		}
	}
	// Pure estimate math: compacted raw `{"model":"","messages":[]}` is 26
	// runes, so (26+3)/4 = 7 tokens.
	{
		s := wave3Gateway(t, nil, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", `{"model":"","messages":[]}`))
		if out["input_tokens"] != float64(7) || out["estimated"] != true {
			t.Fatalf("pure estimate=%v want input_tokens=7 estimated=true", out)
		}
	}
	// Unicode estimate: `{"model":"","messages":[{"role":"user","content":"éé"}]}`
	// compacts to 56 runes, so (56+3)/4 = 14 tokens.
	{
		s := wave3Gateway(t, nil, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
		out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", `{"model":"","messages":[{"role":"user","content":"éé"}]}`))
		if out["input_tokens"] != float64(14) || out["estimated"] != true {
			t.Fatalf("unicode estimate=%v want input_tokens=14 estimated=true", out)
		}
	}
}

func TestWave3CountTokensReasoningPinsAnthropic(t *testing.T) {
	var anthCalls, openaiCalls atomic.Int32
	anth := wave3JSONServer(t, 200, `{"input_tokens":42}`, &anthCalls)
	defer anth.Close()
	oa := wave3JSONServer(t, 200, wave3ChatOK, &openaiCalls)
	defer oa.Close()
	s := wave3Gateway(t, []config.ProviderConfig{
		wave3Provider("a", "anthropic_compatible", anth.URL, "m", "upstream-model", []string{"cm"}),
		wave3Provider("b", "openai_compatible", oa.URL, "m", "upstream-model", []string{"cm"}),
	}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })
	body := `{"model":"cm","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8}}`
	out := wave3CountTokens(t, wave3Request(s, "POST", "/v1/messages/count_tokens", body))
	if out["input_tokens"] != float64(42) {
		t.Fatalf("reasoning count_tokens=%v want 42 from anthropic", out["input_tokens"])
	}
	if anthCalls.Load() != 1 || openaiCalls.Load() != 0 {
		t.Fatalf("reasoning count_tokens calls anthropic=%d openai=%d want 1/0", anthCalls.Load(), openaiCalls.Load())
	}
}
