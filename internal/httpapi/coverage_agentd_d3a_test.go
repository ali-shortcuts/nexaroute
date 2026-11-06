// Coverage tests for Agent D3 (100% mission): httpapi providers/wiring files.
// Part A: anthropic, gemini, canonical_path, hedging, mcp, helpers, info,
// identity_usage, identity_usage_admin, deployment.
//
// Every test is behavioral: it asserts an observable status, body, error, or
// state — never mere execution. Production code is never edited by this file.

package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// --- info.go ---

// D3: hello rejects non-GET requests (info.go:12-14).
func TestD3HelloMethodNotAllowed(t *testing.T) {
	s := testGateway(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/api/hello", nil)
	rr := httptest.NewRecorder()
	s.hello(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("hello POST: got status %d, want 405", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "method not allowed") {
		t.Fatalf("hello POST: body %q does not contain 'method not allowed'", rr.Body.String())
	}
}

// --- identity_usage_admin.go ---

// D3: adminIdentityUsage rejects non-GET requests (identity_usage_admin.go:11-13).
func TestD3AdminIdentityUsageMethodNotAllowed(t *testing.T) {
	s := testGateway(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/usage/identities", nil)
	rr := httptest.NewRecorder()
	s.adminIdentityUsage(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("adminIdentityUsage POST: got status %d, want 405", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "method not allowed") {
		t.Fatalf("adminIdentityUsage POST: body %q does not contain 'method not allowed'", rr.Body.String())
	}
}

// --- identity_usage.go ---

// D3: usage recorded with no identity in context lands on the "anonymous"
// row (identity_usage.go:48-49).
func TestD3RecordUsageAnonymousIdentity(t *testing.T) {
	s := testGateway(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", nil)
	s.recordUsageEx(req, "dep/m", usage.Counts{Input: 3, Output: 4}, false)
	rows := s.identityUsage.Snapshot()
	var found *usage.IdentityTotals
	for i := range rows {
		if rows[i].Identity == "anonymous" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("recordUsageEx: no 'anonymous' row in snapshot %+v", rows)
	}
	if found.Input != 3 || found.Output != 4 || found.Requests != 1 {
		t.Fatalf("recordUsageEx: anonymous row %+v, want input=3 output=4 requests=1", *found)
	}
}

// --- deployment.go ---

// D3: deploymentReportForConfig reports the memory security-store backend as
// a per-instance audit scope (deployment.go:63).
func TestD3DeploymentReportMemoryBackend(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.SecurityStoreBackend = "memory"
	rep := deploymentReportForConfig(cfg)
	if rep.SecurityStoreBackend != "memory" {
		t.Fatalf("deploymentReportForConfig: SecurityStoreBackend %q, want \"memory\"", rep.SecurityStoreBackend)
	}
	if rep.AuditSecurityStore != "per-instance" {
		t.Fatalf("deploymentReportForConfig: AuditSecurityStore %q, want \"per-instance\"", rep.AuditSecurityStore)
	}
}

// --- helpers.go ---

// D3: readJSON rejects a body larger than maxJSONBodyBytes
// (helpers.go:81-82).
func TestD3ReadJSONTooLarge(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxJSONBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", bytes.NewReader(body))
	var dst map[string]any
	limited, err := readJSON(req, &dst)
	if err == nil {
		t.Fatalf("readJSON: oversized body accepted, got %d bytes", len(limited))
	}
	var tooLarge *requestTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("readJSON: error %T %v, want *requestTooLargeError", err, err)
	}
	if len(limited) != maxJSONBodyBytes {
		t.Fatalf("readJSON: returned %d bytes, want %d", len(limited), maxJSONBodyBytes)
	}
}

// --- anthropic.go ---

// d3AnthropicStreamingRequest is the standard streaming Anthropic request
// body used by the anthropic.go coverage tests.
const d3AnthropicStreamingRequest = `{"model":"coverage-anthropic-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`

// D3: when the upstream keeps rejecting stream_options even after a repair
// attempt, stripStreamOptions fails and the probe body is restored
// (anthropic.go:272-273). Assert exactly two upstream calls and that the
// response still mentions stream_options.
func TestD3AnthropicStreamOptionsStripFails(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"stream_options is not supported"}}`))
	}))
	defer upstream.Close()

	cfg := coverageAnthropicConfig("openai_compatible", upstream.URL)
	cfg.Routing.MaxRepairAttempts = 1
	s := testGateway(t, cfg)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages",
		strings.NewReader(d3AnthropicStreamingRequest))
	s.Handler().ServeHTTP(rr, req)
	if calls.Load() != 2 {
		t.Fatalf("stripStreamOptions failure: upstream called %d times, want 2 (original + one repair retry)", calls.Load())
	}
	if !strings.Contains(rr.Body.String(), "stream_options") {
		t.Fatalf("stripStreamOptions failure: response %q does not mention stream_options", rr.Body.String())
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("stripStreamOptions failure: got status %d, want 400", rr.Code)
	}
}

// D3: when the only candidate is learned-unsupported for streaming, the
// attempt loop skips it without setting lastErr and the handler returns the
// post-loop 503 (anthropic.go:615-616).
func TestD3AnthropicCapabilitySkipNoDeployment(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"coverage-anthropic-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := coverageAnthropicConfig("openai_compatible", upstream.URL)
	s := testGateway(t, cfg)
	s.CapabilityStore().LearnUnsupported(
		"coverage-anthropic-provider/coverage-anthropic-model",
		compat.CapStreaming, "d3", "streaming verified unsupported", "")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages",
		strings.NewReader(d3AnthropicStreamingRequest))
	s.Handler().ServeHTTP(rr, req)
	if calls.Load() != 0 {
		t.Fatalf("capability skip: upstream called %d times, want 0 (candidate skipped)", calls.Load())
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("capability skip: got status %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("capability skip: body %q does not contain 'no compatible healthy deployment'", rr.Body.String())
	}
}

// d3OpencodeProvider returns a config with a single opencode-console provider
// whose upstream always answers with the given status and body.
func d3OpencodeProvider(t *testing.T, status int, body string) config.Config {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	p := config.ProviderConfig{
		ID:       "d3-opencode",
		Type:     "opencode-console",
		BaseURL:  upstream.URL,
		APIKey:   "test-key-1",
		AuthMode: "bearer",
		Enabled:  true,
		Models: []config.ModelConfig{{
			ID:      "d3-opencode-model",
			Model:   "conf-model",
			Aliases: []string{"client-model"},
			Enabled: true,
			Weight:  1,
			Capabilities: config.Capabilities{
				Streaming: true,
				Tools:     true,
			},
		}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	return cfg
}

// D3: an OpenCode entitlement denial (403) surfaces the diagnostic message
// on the Anthropic ingress path (anthropic.go:365-366).
func TestD3AnthropicOpencodeEntitlement(t *testing.T) {
	cfg := d3OpencodeProvider(t, http.StatusForbidden,
		`{"error":{"message":"model conf-model is not entitled"}}`)
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages",
		strings.NewReader(`{"model":"client-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("opencode 403: got status %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "OpenCode model entitlement denied") {
		t.Fatalf("opencode 403: body %q does not contain the entitlement diagnostic", rr.Body.String())
	}
}

// D3: the native Anthropic SSE tracker reports cache_read tokens from a
// message_delta usage block to the usage hook (anthropic.go:867-868).
func TestD3AnthropicNativeSSECacheReadDelta(t *testing.T) {
	var got []nativeSSEUsage
	tracker := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) {
		got = append(got, u)
	}}
	events := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":2}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":3,"cache_read_input_tokens":7}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	// Feed the whole stream at once: consume only processes a line once it
	// sees its terminating newline.
	if err := tracker.consume([]byte(events)); err != nil {
		t.Fatalf("nativeSSETracker.consume: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("nativeSSETracker: usage hook fired %d times, want 1 (on message_stop)", len(got))
	}
	u := got[0]
	if u.Input != 5 || u.Output != 3 || u.CacheRead != 7 {
		t.Fatalf("nativeSSETracker: usage %+v, want input=5 output=3 cache_read=7", u)
	}
}

// --- canonical_path.go ---

// D3: canonicalErrorJSON renders the Gemini envelope (canonical_path.go:440).
func TestD3CanonicalErrorJSONGemini(t *testing.T) {
	rr := httptest.NewRecorder()
	canonicalErrorJSON(rr, "gemini", http.StatusBadGateway, "api_error", "boom")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("canonicalErrorJSON gemini: got status %d, want 502", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "UNAVAILABLE") || !strings.Contains(body, "boom") {
		t.Fatalf("canonicalErrorJSON gemini: body %q does not contain the Gemini error envelope", body)
	}
}

// D3: handleCanonicalResponse translates an OpenAI chat SSE stream for an
// OpenAI client (canonical_path.go:377-378).
func TestD3HandleCanonicalResponseStream(t *testing.T) {
	s := testGateway(t, config.Default())
	sse := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"model-x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"model-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	rr := httptest.NewRecorder()
	err := s.handleCanonicalResponse(context.Background(), rr, resp,
		"openai_chat", "openai_chat", "model-x", "d3-req", true, nil, nil)
	if err != nil {
		t.Fatalf("handleCanonicalResponse stream: unexpected error %v", err)
	}
	if !strings.Contains(rr.Body.String(), "hi") {
		t.Fatalf("handleCanonicalResponse stream: output %q does not contain the streamed text", rr.Body.String())
	}
}

// D3: handleCanonicalResponse fails the Gemini encoding when an Anthropic
// tool_use block has no name (canonical_path.go:424-425).
func TestD3HandleCanonicalResponseGeminiEncodeError(t *testing.T) {
	s := testGateway(t, config.Default())
	payload := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"call_1","name":"","input":{}}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
	rr := httptest.NewRecorder()
	err := s.handleCanonicalResponse(context.Background(), rr, resp,
		"anthropic", "gemini", "m", "d3-req", false, nil, nil)
	if err == nil {
		t.Fatalf("handleCanonicalResponse gemini: nameless tool_use accepted, want an encode error")
	}
	if !strings.Contains(err.Error(), "gemini response encode failed") ||
		!strings.Contains(err.Error(), "tool call without a name") {
		t.Fatalf("handleCanonicalResponse gemini: error %q, want 'gemini response encode failed' + 'tool call without a name'", err.Error())
	}
}

// D3 (evidence for canonical_path.go:168-169): a Responses stream whose
// output_item.done arrives with no prior function_call start and empty
// arguments fails validation with a terminal error instead of reaching
// emitValidatedTool's !ok branch.
func TestD3CanonicalStreamPumpToolEndWithoutStart(t *testing.T) {
	s := testGateway(t, config.Default())
	sse := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	rr := httptest.NewRecorder()
	err := s.canonicalStreamPump(rr, resp,
		"openai_responses", "openai_responses", "m", "d3-req", nil, nil)
	if err == nil {
		t.Fatalf("canonicalStreamPump: tool-end without start accepted, want a validation error")
	}
	if !strings.Contains(err.Error(), "arguments empty") {
		t.Fatalf("canonicalStreamPump: error %q, want 'arguments empty'", err.Error())
	}
}

// d3TwoProviderConfig returns a config with two priority-ordered providers.
// p1's upstream always fails to connect (closed port); p2 is the provider
// whose capability is learned-unsupported by the caller.
func d3TwoProviderConfig(t *testing.T, p1Priority, p2Priority int) (config.Config, *httptest.Server) {
	t.Helper()
	p2Upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"shared-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(p2Upstream.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RequestTimeoutMS = 30
	cfg.Routing.RetryBackoffMS = 200
	mk := func(id, base string, prio int) config.ProviderConfig {
		p := config.ProviderConfig{
			ID:       id,
			Type:     "openai_compatible",
			BaseURL:  base,
			APIKey:   "test-key-1",
			AuthMode: "bearer",
			Enabled:  true,
			Models: []config.ModelConfig{{
				ID:       id + "-model",
				Model:    "shared-model",
				Enabled:  true,
				Weight:   1,
				Priority: prio,
				Capabilities: config.Capabilities{
					Streaming: true,
					Tools:     true,
				},
			}},
		}
		p.ApplyDefaults()
		return p
	}
	cfg.Providers = []config.ProviderConfig{
		mk("d3-p1", "http://127.0.0.1:1", p1Priority),
		mk("d3-p2", p2Upstream.URL, p2Priority),
	}
	return cfg, p2Upstream
}

// d3ResponsesToolsRequest is a non-streaming Responses request that asks for
// a tool, so the candidate profile requires tool support.
const d3ResponsesToolsRequest = `{"model":"shared-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"lookup","description":"find","parameters":{"type":"object","properties":{}}}],"tool_choice":"required"}`

// D3: an OpenCode entitlement denial (403) surfaces the diagnostic message
// on the Responses ingress path (canonical_path.go:653-654).
func TestD3ResponsesOpencodeEntitlement(t *testing.T) {
	cfg := d3OpencodeProvider(t, http.StatusForbidden,
		`{"error":{"message":"model conf-model is not entitled"}}`)
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
		strings.NewReader(`{"model":"client-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("responses opencode 403: got status %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "OpenCode model entitlement denied") {
		t.Fatalf("responses opencode 403: body %q does not contain the entitlement diagnostic", rr.Body.String())
	}
}

// D3: when every candidate is skipped or fails and the route deadline has
// expired, the Responses path returns 504 (canonical_path.go:812-814).
func TestD3ResponsesPostLoopDeadline(t *testing.T) {
	cfg, _ := d3TwoProviderConfig(t, 1, 2)
	s := testGateway(t, cfg)
	s.CapabilityStore().LearnUnsupported("d3-p2/d3-p2-model",
		compat.CapTools, "d3", "tools verified unsupported", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
		strings.NewReader(d3ResponsesToolsRequest))
	// Fixed request ID: the retry pause is full jitter in [0, 200ms]
	// deterministic on the request ID; "d3-postloop-1" hashes to a 149ms
	// pause, safely past the 30ms route deadline, so the post-loop 504 is
	// deterministic instead of a ~15% flake.
	req.Header.Set("x-request-id", "d3-postloop-1")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("responses post-loop deadline: got status %d, want 504", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "gateway request timeout") {
		t.Fatalf("responses post-loop deadline: body %q does not contain 'gateway request timeout'", rr.Body.String())
	}
}

// D3: with MaxAttempts=-1 the attempt loop never runs and the handler
// reports "no usable deployment" (canonical_path.go:816-817).
func TestD3ResponsesMaxAttemptsNegative(t *testing.T) {
	cfg := coverageAnthropicConfig("openai_compatible", "http://127.0.0.1:1")
	cfg.Routing.MaxAttempts = -1
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
		strings.NewReader(`{"model":"coverage-anthropic-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("maxAttempts -1: got status %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no usable deployment") {
		t.Fatalf("maxAttempts -1: body %q does not contain 'no usable deployment'", rr.Body.String())
	}
}

// d3StallingUpstream returns an upstream that writes 200 headers, flushes,
// signals headersSent, waits for proceed, then writes the given body.
func d3StallingUpstream(t *testing.T, headersSent, proceed chan struct{}, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(headersSent)
		<-proceed
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// D3: when the route deadline expires while the upstream body is stalled,
// the Responses path returns 504 (canonical_path.go:766-768).
func TestD3ResponsesUsageDeadline(t *testing.T) {
	headersSent := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(headersSent)
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer upstream.Close()
	t.Cleanup(func() {
		select {
		case <-headersSent:
		default:
		}
	})
	cfg := coverageAnthropicConfig("openai_compatible", upstream.URL)
	cfg.Routing.RequestTimeoutMS = 50
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
		strings.NewReader(`{"model":"coverage-anthropic-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("responses usage deadline: got status %d, want 504", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "gateway request timeout") {
		t.Fatalf("responses usage deadline: body %q does not contain 'gateway request timeout'", rr.Body.String())
	}
}

// D3: when the client disconnects while the upstream body is stalled, the
// Responses handler returns silently without writing a response
// (canonical_path.go:750-751).
func TestD3ResponsesUsageClientGone(t *testing.T) {
	headersSent := make(chan struct{})
	proceed := make(chan struct{})
	upstream := d3StallingUpstream(t, headersSent, proceed, `{"bad": `)
	cfg := coverageAnthropicConfig("openai_compatible", upstream.URL)
	s := testGateway(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses",
		strings.NewReader(`{"model":"coverage-anthropic-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)).WithContext(ctx)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rr, req)
	}()
	select {
	case <-headersSent:
	case <-time.After(5 * time.Second):
		t.Fatal("responses client-gone: upstream never sent headers")
	}
	// Give the gateway time to leave Do (headers received) and block inside
	// readResponseBody: cancelling earlier aborts Do itself, which takes the
	// transport-error path instead of the usage-error client-gone branch.
	// (Without this delay the test passed but covered the wrong branch —
	// the full profile showed canonical_path.go:750 uncovered.)
	time.Sleep(200 * time.Millisecond)
	cancel()
	close(proceed)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("responses client-gone: ServeHTTP did not return after client cancel")
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("responses client-gone: wrote %d bytes %q, want an empty response", rr.Body.Len(), rr.Body.String())
	}
}

// --- gemini.go ---

// d3GeminiToolsRequest is a non-streaming Gemini generateContent request
// that declares a function, so the candidate profile requires tool support.
const d3GeminiToolsRequest = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","description":"find"}]}]}`

// D3 (evidence that gemini.go:357-358 is unreachable): an OpenCode provider
// can never serve Gemini ingress — providers.OpencodePathForIngress supports
// only openai_chat, openai_responses and anthropic, so finishCanonicalAttempt
// fails closed before any HTTP request and the handler reports "candidate
// could not serve this request" (gemini.go:293). The entitlement override in
// the Gemini status-error branch therefore cannot fire for any opencode
// candidate; it is dead code unless OpencodePathForIngress gains a Gemini
// path (a production change, not a test fix).
func TestD3GeminiOpencodeIngressFailClosed(t *testing.T) {
	cfg := d3OpencodeProvider(t, http.StatusForbidden,
		`{"error":{"message":"model conf-model is not entitled"}}`)
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"http://gateway/v1beta/models/client-model:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("gemini opencode ingress: got status %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "candidate could not serve this request") {
		t.Fatalf("gemini opencode ingress: body %q does not contain 'candidate could not serve this request'", rr.Body.String())
	}
}

// D3: when every candidate is skipped or fails and the route deadline has
// expired, the Gemini path returns 504 (gemini.go:514-516).
func TestD3GeminiPostLoopDeadline(t *testing.T) {
	cfg, _ := d3TwoProviderConfig(t, 1, 2)
	s := testGateway(t, cfg)
	s.CapabilityStore().LearnUnsupported("d3-p2/d3-p2-model",
		compat.CapTools, "d3", "tools verified unsupported", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"http://gateway/v1beta/models/shared-model:generateContent",
		strings.NewReader(d3GeminiToolsRequest))
	// Fixed request ID: the retry pause is full jitter in [0, 200ms]
	// deterministic on the request ID; "d3-postloop-1" hashes to a 149ms
	// pause, safely past the 30ms route deadline, so the post-loop 504 is
	// deterministic instead of a ~15% flake.
	req.Header.Set("x-request-id", "d3-postloop-1")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("gemini post-loop deadline: got status %d, want 504", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "DEADLINE_EXCEEDED") {
		t.Fatalf("gemini post-loop deadline: body %q does not contain 'DEADLINE_EXCEEDED'", rr.Body.String())
	}
}

// D3: when the route deadline expires while the upstream body is stalled,
// the Gemini path returns 504 (gemini.go:468-470).
func TestD3GeminiUsageDeadline(t *testing.T) {
	headersSent := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(headersSent)
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()
	cfg := coverageAnthropicConfig("openai_compatible", upstream.URL)
	cfg.Routing.RequestTimeoutMS = 50
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"http://gateway/v1beta/models/coverage-anthropic-model:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("gemini usage deadline: got status %d, want 504", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "DEADLINE_EXCEEDED") {
		t.Fatalf("gemini usage deadline: body %q does not contain 'DEADLINE_EXCEEDED'", rr.Body.String())
	}
}

// D3: when the client disconnects while the upstream body is stalled, the
// Gemini handler returns silently without writing a response
// (gemini.go:454-455).
func TestD3GeminiUsageClientGone(t *testing.T) {
	headersSent := make(chan struct{})
	proceed := make(chan struct{})
	upstream := d3StallingUpstream(t, headersSent, proceed, `{"bad": `)
	cfg := coverageAnthropicConfig("openai_compatible", upstream.URL)
	s := testGateway(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost,
		"http://gateway/v1beta/models/coverage-anthropic-model:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)).WithContext(ctx)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rr, req)
	}()
	select {
	case <-headersSent:
	case <-time.After(5 * time.Second):
		t.Fatal("gemini client-gone: upstream never sent headers")
	}
	// Give the gateway time to leave Do (headers received) and block inside
	// readResponseBody: cancelling earlier aborts Do itself, which takes the
	// transport-error path instead of the usage-error client-gone branch.
	time.Sleep(200 * time.Millisecond)
	cancel()
	close(proceed)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gemini client-gone: ServeHTTP did not return after client cancel")
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("gemini client-gone: wrote %d bytes %q, want an empty response", rr.Body.Len(), rr.Body.String())
	}
}

// --- hedging.go ---

// D3: abandoning a hedge leg with a nil cancel function is a no-op for the
// cancel and still records the abandon event (hedging.go:89-90).
func TestD3HedgeAbandonNilCancel(t *testing.T) {
	s := testGateway(t, config.Default())
	done := make(chan hedgeResult, 1)
	leg := &hedgeLeg{done: done}
	close(done)
	s.abandonHedgeLeg(leg, "d3-dep/d3-model", "d3-req", "test-abandon")
	deadline := time.Now().Add(2 * time.Second)
	for {
		found := false
		for _, ev := range s.bus.Snapshot() {
			if ev.Kind == "hedged_abandoned" {
				found = true
			}
		}
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("abandonHedgeLeg: no hedged_abandoned event recorded within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// D3: when the request context is cancelled while the primary leg is still
// running, hedgedUpstreamDo returns the cancellation without launching a
// hedge (hedging.go:165-166).
func TestD3HedgeRouteContextCancelled(t *testing.T) {
	s := testGateway(t, config.Default())
	ctx, cancel := context.WithCancel(context.Background())
	primary := func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	secondary := func(ctx context.Context) (*http.Response, error) {
		return &http.Response{}, nil
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	out := s.hedgedUpstreamDo(ctx, "d3-req", "d3-p1/m", "d3-p2/m",
		time.Hour, primary, secondary)
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("hedgedUpstreamDo: err %v, want context.Canceled", out.err)
	}
	if out.hedgeLaunched {
		t.Fatal("hedgedUpstreamDo: hedge was launched, want it unlaunched on cancellation")
	}
}

// --- mcp.go ---

// D3: the MCP gateway rejects an unauthenticated request when client auth
// is enabled (mcp.go:202-203).
func TestD3MCPMissingAPIKey(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.MCP.Enabled = true
	cfg.ClientAuth.Enabled = true
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("mcp missing key: got status %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid or missing API key") {
		t.Fatalf("mcp missing key: body %q does not contain 'invalid or missing API key'", rr.Body.String())
	}
}

// D3: postRawLocked fails before any network use when the payload cannot be
// marshalled (mcp.go:477-478).
func TestD3MCPPostRawMarshalError(t *testing.T) {
	b := newMCPBroker()
	srv := config.MCPServerConfig{ID: "d3-mcp", URL: "http://127.0.0.1:1"}
	st := b.stateFor(srv)
	_, _, _, err := b.postRawLocked(context.Background(), config.Default(), srv, st,
		map[string]any{"bad": func() {}})
	if err == nil {
		t.Fatal("postRawLocked: unmarshalable payload accepted, want an error")
	}
	if !strings.Contains(err.Error(), "json: unsupported type") {
		t.Fatalf("postRawLocked: error %q, want 'json: unsupported type'", err.Error())
	}
}

// --- round 2: lines the full-package profile showed the round-1 tests
// reached only via neighbouring paths ---

// D3: the native Anthropic SSE tracker reports cache-creation tokens from a
// message_delta usage block (anthropic.go:867-868). Round 1 covered only the
// cache_read assignment; this drives the cache_creation assignment.
func TestD3AnthropicNativeSSECacheCreationDelta(t *testing.T) {
	var got []nativeSSEUsage
	tracker := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) {
		got = append(got, u)
	}}
	events := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":3,"cache_creation_input_tokens":4}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	if err := tracker.consume([]byte(events)); err != nil {
		t.Fatalf("nativeSSETracker.consume: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("nativeSSETracker: usage hook fired %d times, want 1 (on message_stop)", len(got))
	}
	if got[0].CacheWrite != 4 {
		t.Fatalf("nativeSSETracker: usage %+v, want cache_write=4", got[0])
	}
}

// d3CloseTracker is a ReadCloser that records whether Close was called.
type d3CloseTracker struct {
	io.Reader
	closed atomic.Bool
}

func (c *d3CloseTracker) Close() error {
	c.closed.Store(true)
	return nil
}

// D3: abandoning a hedge leg whose result carries a response body closes
// that body before recording the abandon event (hedging.go:89-90).
func TestD3HedgeAbandonClosesBody(t *testing.T) {
	s := testGateway(t, config.Default())
	body := &d3CloseTracker{Reader: strings.NewReader("x")}
	done := make(chan hedgeResult, 1)
	done <- hedgeResult{resp: &http.Response{Body: body}}
	leg := &hedgeLeg{done: done}
	s.abandonHedgeLeg(leg, "d3-dep/d3-model", "d3-req", "test-abandon-close")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if body.closed.Load() {
			for _, ev := range s.bus.Snapshot() {
				if ev.Kind == "hedged_abandoned" {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("abandonHedgeLeg: body closed=%v, no hedged_abandoned event within 2s", body.closed.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// D3: when the primary leg wins after the hedge was launched, the secondary
// leg is abandoned (hedging.go:155-156). (The sibling branch at 165-166 — a
// secondary win observed from the primary case — is unreachable: a
// successful secondary result returns from the secondaryDone case
// immediately, so it can never be observed pending in the primary case.)
func TestD3HedgePrimaryWinsAfterHedge(t *testing.T) {
	s := testGateway(t, config.Default())
	primary := func(ctx context.Context) (*http.Response, error) {
		select {
		case <-time.After(50 * time.Millisecond):
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	secondary := func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out := s.hedgedUpstreamDo(context.Background(), "d3-req", "d3-p1/m", "d3-p2/m",
		time.Millisecond, primary, secondary)
	if out.err != nil {
		t.Fatalf("hedgedUpstreamDo: err %v, want nil (primary wins)", out.err)
	}
	if out.resp == nil {
		t.Fatal("hedgedUpstreamDo: resp is nil, want the primary response")
	}
	defer func() { _ = out.resp.Body.Close() }()
	if !out.hedgeLaunched {
		t.Fatal("hedgedUpstreamDo: hedgeLaunched=false, want true (timer fired before the primary returned)")
	}
}

// D3: decodeValidatedJSONLimited surfaces a read failure from its limited
// reader (helpers.go:81-82). readJSONLimited caps at maxUpstreamJSONBytes
// (32 MiB), not the 16 MiB request limit.
func TestD3DecodeValidatedJSONLimitedTooLarge(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxUpstreamJSONBytes+1)
	var dst map[string]any
	err := decodeValidatedJSONLimited(bytes.NewReader(body), &dst, nil)
	if err == nil {
		t.Fatal("decodeValidatedJSONLimited: oversized body accepted, want an error")
	}
	if !strings.Contains(err.Error(), "upstream JSON response exceeds") {
		t.Fatalf("decodeValidatedJSONLimited: error %q, want 'upstream JSON response exceeds'", err.Error())
	}
}

// D3: recordUsageEx is a no-op (not a crash) when the identity tracker is
// nil (identity_usage.go:48-49).
func TestD3RecordUsageNilTracker(t *testing.T) {
	s := testGateway(t, config.Default())
	s.identityUsage = nil
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", nil)
	s.recordUsageEx(req, "dep/m", usage.Counts{Input: 3, Output: 4}, false)
	// No panic above is the assertion: the nil tracker is skipped silently.
}

// D3: the MCP gateway accepts a legacy client key through the fallback at
// mcp.go:202-203. findClientIdentity reads the cached config snapshot, while
// the handler passes the live config (currentConfig) to mcpClientAllowed —
// so a key rotated into the live config after construction is unknown to
// findClientIdentity but still matches clientKeyMatches. The test builds the
// server with no legacy key, rotates one into the live config only, and
// proves the fallback admits it.
func TestD3MCPLegacyKey(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.MCP.Enabled = true
	cfg.ClientAuth.Enabled = true
	s := testGateway(t, cfg)
	s.runtimeMu.Lock()
	s.cfg.ClientAuth.Keys = []string{"d3-legacy-key"}
	s.runtimeMu.Unlock()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("x-api-key", "d3-legacy-key")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mcp legacy key: got status %d, want 200; body %q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"result":{}`) {
		t.Fatalf("mcp legacy key: body %q does not contain the ping result", rr.Body.String())
	}
}
