package httpapi

// Wave-4 coverage tests for anthropicMessages (internal/httpapi/anthropic.go).
// Targets the stack-uncovered blocks that no earlier wave covered: native SSE
// proxy with a usage sink, the hedge skip, the hedge-partner capability gate,
// the nil-buildErr build failure, the tail Retry-After replay, and the client
// disconnect return. The stripStreamOptions failure and the lastErr-default
// tail are deliberately not forced here; see the final report for why they
// are unreachable through a validated request.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave4AnthropicConfig(providers ...config.ProviderConfig) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.MaxAttempts = 3
	cfg.Providers = providers
	return cfg
}

func wave4AnthropicProvider(id, url, model string, aliases []string, priority int) config.ProviderConfig {
	return config.ProviderConfig{ID: id, Name: id, Type: "openai_compatible", BaseURL: url, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
		{ID: "m", Model: model, Aliases: aliases, Enabled: true, Weight: 1, Priority: priority, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
	}}
}

func wave4AnthropicPost(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func wave4OpenAIChatOK(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"`+text+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
}

// A native Anthropic stream passes through untouched (anthropic.go 447).
// NOTE: the usage-sink closure at 448-450 is NOT exercised here, and cannot
// be: nativeSSETracker.observeAnthropicUsage unmarshals env["message"] into
// a struct expecting a nested "message" key (and env["usage"] into a struct
// expecting a nested "usage" key), so usageInput/usageOutput stay zero and
// the message_stop hook never fires. That is a production defect to fix
// outside wave-4 (which may not modify production code), not a test gap.
func TestWave4AnthropicNativeStreamUsageSink(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"upstream-model\",\"stop_reason\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer up.Close()
	cfg := wave4AnthropicConfig(config.ProviderConfig{ID: "a", Name: "A", Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
		{ID: "m", Model: "upstream-model", Aliases: []string{"client-model"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}},
	}})
	s := testGateway(t, cfg)
	rr := wave4AnthropicPost(t, s, `{"model":"client-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("native stream: status %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hello") {
		t.Fatalf("native stream: streamed delta text missing, body %s", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("native stream: content type %q, want event-stream", ct)
	}
}

// With hedging enabled, a hedge launch marks the partner as skipped; when the
// hedged attempt then fails over, the loop's skip check (anthropic.go 189)
// drops the partner as a primary. Primary blocks until the test releases it
// after the partner's arrival, so no timing is involved. Both legs answer
// 429, a failover class: a 500 is terminal and breaks via shieldTerminal
// before the loop ever reaches the skipped index. Three candidates are
// required: max is capped at len(candidates) (anthropic.go 170), and the
// hedge itself counts as an attempt, so with two candidates the hedged
// attempt exhausts the budget and the loop breaks instead of continuing.
// With three, the failover continues past the skipped partner to pC.
func TestWave4AnthropicHedgePartnerSkippedAfterHedge(t *testing.T) {
	var callsA, callsB, callsC atomic.Int32
	arrivedB := make(chan struct{})
	doneA := make(chan struct{})
	release := make(chan struct{})
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		<-release
		close(doneA)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"primary boom"}}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB.Add(1)
		select {
		case <-arrivedB:
		default:
			close(arrivedB)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"partner boom"}}`)
	}))
	defer upB.Close()
	upC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsC.Add(1)
		wave4OpenAIChatOK(w, "from-c")
	}))
	defer upC.Close()
	cfg := wave4AnthropicConfig(
		wave4AnthropicProvider("pA", upA.URL, "model-a", []string{"shared"}, 0),
		wave4AnthropicProvider("pB", upB.URL, "model-b", []string{"shared"}, 1),
		wave4AnthropicProvider("pC", upC.URL, "model-c", []string{"shared"}, 2),
	)
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 1
	s := testGateway(t, cfg)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"shared","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		done <- rr
	}()
	<-arrivedB // Hedge launched: the partner was dispatched before the primary finished.
	close(release)
	rr := <-done
	<-doneA // The primary leg completed; no goroutine is left blocked.
	if rr.Code != http.StatusOK {
		t.Fatalf("hedge skip: status %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "from-c") {
		t.Fatalf("hedge skip: expected the third candidate's answer, body %s", rr.Body.String())
	}
	if callsA.Load() != 1 || callsB.Load() != 1 || callsC.Load() != 1 {
		t.Fatalf("hedge skip: calls pA=%d pB=%d pC=%d, want 1/1/1 (partner only as hedge, never as primary)", callsA.Load(), callsB.Load(), callsC.Load())
	}
}

// A hedge partner whose streaming capability is recorded unsupported makes
// the hedge builder refuse it (anthropic.go 229-230); the primary then runs
// alone and the partner is never called.
func TestWave4AnthropicHedgePartnerIneligible(t *testing.T) {
	var callsA, callsB atomic.Int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upB.Close()
	cfg := wave4AnthropicConfig(
		wave4AnthropicProvider("pA", upA.URL, "model-a", []string{"shared"}, 0),
		wave4AnthropicProvider("pB", upB.URL, "model-b", []string{"shared"}, 1),
	)
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 1
	s := testGateway(t, cfg)
	s.capStore.LearnUnsupported("pB/m", compat.CapStreaming, "wave4", "wave4 test", "wave4")
	rr := wave4AnthropicPost(t, s, `{"model":"shared","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("hedge ineligible: status %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hi") {
		t.Fatalf("hedge ineligible: translated delta missing, body %s", rr.Body.String())
	}
	if callsB.Load() != 0 || callsA.Load() != 1 {
		t.Fatalf("hedge ineligible: calls pA=%d pB=%d, want 1/0", callsA.Load(), callsB.Load())
	}
}

// A request that decodes as Anthropic but cannot be encoded for Bedrock
// (unsupported image media type) fails the build with no buildErr, so the
// loop reports "attempt payload could not be built" (anthropic.go 207-208)
// and the tail answers 502.
func TestWave4AnthropicBuildFailureEmptyBundle(t *testing.T) {
	// The bedrock adapter requires the named env vars to hold values at
	// construction; the values are local dummies and are never sent
	// anywhere, because the build fails before any dispatch.
	t.Setenv("WAVE4_BEDROCK_KEY", "wave4-dummy-key")
	t.Setenv("WAVE4_BEDROCK_KEY_ID", "wave4-dummy-key-id")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("bedrock upstream must not be reached: the build fails first")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()
	cfg := wave4AnthropicConfig(config.ProviderConfig{ID: "b", Name: "B", Type: "bedrock", BaseURL: up.URL, AuthMode: "none", SecretKeyEnv: "WAVE4_BEDROCK_KEY", APIKeyEnv: "WAVE4_BEDROCK_KEY_ID", Enabled: true, Models: []config.ModelConfig{
		{ID: "m", Model: "bedrock-model", Aliases: []string{"m"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Vision: true}},
	}})
	s := testGateway(t, cfg)
	rr := wave4AnthropicPost(t, s, `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/tiff","data":"QUJD"}}]}]}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("build failure: status %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "attempt payload could not be built") {
		t.Fatalf("build failure: expected build-failure note, body %s", rr.Body.String())
	}
}

// Two rate-limited candidates: the first fails over, the second breaks the
// loop via shieldTerminal, and the tail replays its Retry-After
// (anthropic.go 580-581) on the 503.
func TestWave4AnthropicTailRetryAfter(t *testing.T) {
	mk := func(retry string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", retry)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
		}))
	}
	upA := mk("7")
	defer upA.Close()
	upB := mk("9")
	defer upB.Close()
	cfg := wave4AnthropicConfig(
		wave4AnthropicProvider("pA", upA.URL, "model-a", []string{"shared"}, 0),
		wave4AnthropicProvider("pB", upB.URL, "model-b", []string{"shared"}, 1),
	)
	s := testGateway(t, cfg)
	rr := wave4AnthropicPost(t, s, `{"model":"shared","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("tail retry-after: status %d, body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "9" {
		t.Fatalf("tail retry-after: Retry-After %q, want the last candidate's 9", got)
	}
	if !strings.Contains(rr.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("tail retry-after: expected shield-terminal note, body %s", rr.Body.String())
	}
}

// An attempt that fails while the client request is already gone returns
// without an error body and records a client_disconnect event. With a
// pre-cancelled context the transport branch (anthropic.go 280) fires; the
// post-processing branch (500-502) is covered by
// TestWave4AnthropicClientDisconnectDuringDecode below.
func TestWave4AnthropicClientDisconnect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wave4OpenAIChatOK(w, "hi")
	}))
	defer up.Close()
	cfg := wave4AnthropicConfig(wave4AnthropicProvider("pA", up.URL, "model-a", []string{"shared"}, 0))
	s := testGateway(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"shared","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	found := false
	for _, e := range s.bus.SnapshotLimit(200) {
		if e.Kind == "client_disconnect" {
			found = true
		}
	}
	if !found {
		t.Fatalf("client disconnect: no client_disconnect bus event; status %d body %s", rr.Code, rr.Body.String())
	}
}

// A client that disconnects mid-stream takes the post-processing disconnect
// branch (anthropic.go 500-502): the stream fails on the aborted upstream
// read while the client context is gone, and the handler returns without
// further output. Deterministic ordering, no timing: the test cancels only
// after the gateway demonstrably wrote the first translated chunk (the
// signal writer observes it), while the upstream is blocked before chunk 2,
// so Do has long completed and the failure can only be the stream read.
// (A non-stream variant is inherently racy here: the adapter buffers the
// body, so cancelling mid-body aborts Do itself and lands in the transport
// branch instead.) The 500 block's event carries the 200 status; the
// transport branch's event does not, which is how the two are distinguished.
type wave4SignalWriter struct {
	header http.Header
	body   strings.Builder
	wrote  chan struct{}
	once   bool
}

func (w *wave4SignalWriter) Header() http.Header { return w.header }
func (w *wave4SignalWriter) WriteHeader(int)     {}
func (w *wave4SignalWriter) Write(b []byte) (int, error) {
	if !w.once {
		w.once = true
		close(w.wrote)
	}
	return w.body.Write(b)
}
func (w *wave4SignalWriter) Flush() {}

func TestWave4AnthropicClientDisconnectDuringDecode(t *testing.T) {
	chunk1 := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		close(chunk1)
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	cfg := wave4AnthropicConfig(wave4AnthropicProvider("pA", up.URL, "model-a", []string{"shared"}, 0))
	s := testGateway(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 1)
	sw := &wave4SignalWriter{header: http.Header{}, wrote: make(chan struct{})}
	go func() {
		req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(`{"model":"shared","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		s.Handler().ServeHTTP(sw, req)
		done <- struct{}{}
	}()
	<-chunk1   // Upstream sent chunk 1; the gateway is translating it.
	<-sw.wrote // The gateway wrote the first translated chunk to the client.
	cancel()   // Only now, with Do complete and the stream blocked on chunk 2.
	<-done     // Cancel is the only unblocker: release stays closed, so no
	// terminating chunk can arrive first and turn this into route_ok.
	close(release)
	<-handlerDone
	found := false
	var kinds string
	for _, e := range s.bus.SnapshotLimit(200) {
		kinds += e.Kind + "/" + e.Deployment + "; "
		if e.Kind == "client_disconnect" && e.StatusCode == http.StatusOK {
			found = true
		}
	}
	if !found {
		t.Fatalf("decode disconnect: no client_disconnect event with status 200; bus: %s", kinds)
	}
}
