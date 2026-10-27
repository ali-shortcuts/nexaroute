package httpapi

// Retry-safety failure-injection suite: the gateway's retry policy
// pinned at the observable boundary (upstream hit counts, client
// status/body, bus events). It proves, against faulting upstreams:
//
//   - timeout before send (connection refused) fails over once and
//     is NOT recorded as ambiguous: nothing reached a provider.
//   - timeout after send (gateway deadline while the upstream holds
//     the request) is terminal 504, never replays to a second
//     candidate, and IS recorded as ambiguous_outcome.
//   - a partial stream never replays: the secondary is never called
//     and no [DONE] is fabricated.
//   - client cancellation stops failover: the secondary is never called.
//   - identical bodies are not duplicates, and a client idempotency
//     key is by default never forwarded upstream; only an explicit
//     operator forward_headers listing forwards it, as an operator
//     assertion, not a gateway-verified mechanism.
//   - a static provider idempotency header is rejected at config
//     validation.
//
// Telemetry/redaction: the ambiguous_outcome event carries a fixed
// metadata-only message. No test here (or in production) puts a
// prompt, response body, or credential into that event; the
// timeout-after-send test asserts the client body is a gateway
// timeout envelope, not upstream content.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type retrySafetyCapture struct {
	hits   atomic.Int64
	keys   []string
	bodies []string
	mu     sync.Mutex
}

func (c *retrySafetyCapture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.keys = append(c.keys, r.Header.Get("Idempotency-Key"))
	c.bodies = append(c.bodies, string(body))
	c.mu.Unlock()
	c.hits.Add(1)
}

func retrySafetyOKServer(t *testing.T, cap *retrySafetyCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, failureOKBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func retrySafetyChatRequest(t *testing.T, s *Server, body, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func retrySafetyConfig(providers ...config.ProviderConfig) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Providers = providers
	return cfg
}

func retrySafetyHasKind(s *Server, kind string) bool {
	for _, e := range s.bus.SnapshotLimit(200) {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func retrySafetyEventMessagesContain(s *Server, kind, needle string) bool {
	for _, e := range s.bus.SnapshotLimit(200) {
		if e.Kind == kind && strings.Contains(e.Message, needle) {
			return true
		}
	}
	return false
}

// Default forwarding: a client idempotency key never reaches the
// upstream, and identical bodies are still distinct requests.
// Three requests (same key with two different bodies, then the first
// body again without a key) must produce three upstream hits.
func TestRetrySafetyIdempotencyKeyNotForwardedAndBodiesAreNotDuplicates(t *testing.T) {
	var cap retrySafetyCapture
	up := retrySafetyOKServer(t, &cap)
	s := failureGateway(t, retrySafetyConfig(failureProvider("only", up.URL, 100)))

	rr := retrySafetyChatRequest(t, s, failureChatBody, "key-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", rr.Code, rr.Body.String())
	}
	otherBody := `{"model":"m","messages":[{"role":"user","content":"bye"}]}`
	rr = retrySafetyChatRequest(t, s, otherBody, "key-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("same-key different-body request status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = retrySafetyChatRequest(t, s, failureChatBody, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("same-body no-key request status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := cap.hits.Load(); got != 3 {
		t.Fatalf("upstream hits=%d, want 3: identical bodies and shared keys must not dedupe", got)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for i, k := range cap.keys {
		if k != "" {
			t.Fatalf("upstream request %d carried Idempotency-Key %q; default forwarding must be none", i, k)
		}
	}
	if len(cap.bodies) != 3 || !strings.Contains(cap.bodies[0], `"hi"`) || !strings.Contains(cap.bodies[1], `"bye"`) || !strings.Contains(cap.bodies[2], `"hi"`) {
		t.Fatalf("upstream bodies do not show three distinct requests: %#v", cap.bodies)
	}
}

// Operator-explicit forwarding is the only forwarding path: when the
// operator lists the header in forward_headers, the client's key is
// forwarded verbatim. This is the operator's assertion about their
// provider, not a gateway-verified idempotency mechanism.
func TestRetrySafetyOperatorForwardHeadersForwardsKey(t *testing.T) {
	var cap retrySafetyCapture
	up := retrySafetyOKServer(t, &cap)
	p := failureProvider("only", up.URL, 100)
	p.ForwardHeaders = []string{"Idempotency-Key"}
	s := failureGateway(t, retrySafetyConfig(p))

	rr := retrySafetyChatRequest(t, s, failureChatBody, "operator-key-9")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := cap.hits.Load(); got != 1 {
		t.Fatalf("upstream hits=%d, want 1", got)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.keys) != 1 || cap.keys[0] != "operator-key-9" {
		t.Fatalf("operator-listed forwarding: upstream keys=%#v, want [operator-key-9]", cap.keys)
	}
}

// Anthropic ingress: the client key is likewise never forwarded by
// default, here to an Anthropic-compatible upstream.
func TestRetrySafetyAnthropicIdempotencyKeyNotForwarded(t *testing.T) {
	var cap retrySafetyCapture
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"up-model","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	p := config.ProviderConfig{
		ID: "p", Name: "p", Type: "anthropic_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1, Priority: 100}},
	}
	s := failureGateway(t, retrySafetyConfig(p))
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "anth-key-1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.keys) != 1 || cap.keys[0] != "" {
		t.Fatalf("anthropic upstream keys=%#v, want one request with no key", cap.keys)
	}
}

// Timeout before send: the primary refuses the connection, so
// nothing reached a provider. Failover succeeds, the secondary is
// hit exactly once, and no ambiguous_outcome is recorded.
func TestRetrySafetyTimeoutBeforeSendFailsOverOnceAndIsNotAmbiguous(t *testing.T) {
	var cap retrySafetyCapture
	up := retrySafetyOKServer(t, &cap)
	s := failureGateway(t, retrySafetyConfig(
		failureProvider("dead", deadURL(t), 0),
		failureProvider("healthy", up.URL, 100),
	))
	rr := retrySafetyChatRequest(t, s, failureChatBody, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"content":"ok"`) {
		t.Fatalf("status=%d body=%s, want failover success", rr.Code, rr.Body.String())
	}
	if got := cap.hits.Load(); got != 1 {
		t.Fatalf("secondary hits=%d, want 1", got)
	}
	if retrySafetyHasKind(s, "ambiguous_outcome") {
		t.Fatal("connection-refused failover must not record ambiguous_outcome: the request was never sent")
	}
	if !retrySafetyHasKind(s, "failover") {
		t.Fatal("expected a failover event for the refused primary")
	}
}

// Timeout after send: the primary holds the request past the
// gateway deadline. The gateway answers 504, never submits the
// request to the secondary, and records the outcome as ambiguous.
func TestRetrySafetyTimeoutAfterSendIsTerminalAndAmbiguous(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, failureOKBody)
	}))
	defer secondary.Close()
	cfg := retrySafetyConfig(
		failureProvider("slow", primary.URL, 0),
		failureProvider("healthy", secondary.URL, 100),
	)
	cfg.Routing.RequestTimeoutMS = 500
	s := failureGateway(t, cfg)

	start := time.Now()
	rr := retrySafetyChatRequest(t, s, failureChatBody, "")
	elapsed := time.Since(start)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s, want 504 gateway timeout", rr.Code, rr.Body.String())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("gateway timeout took %s; the bound must fire promptly", elapsed)
	}
	if got := secondaryHits.Load(); got != 0 {
		t.Fatalf("secondary hits=%d, want 0: a timed-out request must never be replayed", got)
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits=%d, want 1", got)
	}
	if !retrySafetyHasKind(s, "ambiguous_outcome") {
		t.Fatal("timeout after send must record ambiguous_outcome")
	}
	if strings.Contains(rr.Body.String(), `"content":"ok"`) {
		t.Fatal("504 response must not contain a fabricated success")
	}
}

// Partial stream: the primary commits one chunk then drops the
// connection. The client keeps the partial content, the secondary
// is never called, and no [DONE] is fabricated.
func TestRetrySafetyPartialStreamNeverReplays(t *testing.T) {
	var primaryHits, secondaryHits atomic.Int64
	killer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"))
		_, _ = conn.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))
	}))
	defer killer.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"SECONDARY\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer healthy.Close()
	s := failureGateway(t, retrySafetyConfig(
		failureProvider("killer", killer.URL, 0),
		failureProvider("healthy", healthy.URL, 100),
	))
	// Streaming capability is required for the candidates to serve a
	// stream request; failureProvider already sets Streaming: true.
	rr := retrySafetyChatRequest(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "")
	if primaryHits.Load() != 1 {
		t.Fatalf("primary hits=%d, want 1", primaryHits.Load())
	}
	if got := secondaryHits.Load(); got != 0 {
		t.Fatalf("secondary hits=%d, want 0: a committed stream must never be replayed", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("client must keep the committed partial content, got: %s", body)
	}
	if strings.Contains(body, "SECONDARY") {
		t.Fatalf("secondary content leaked into a committed stream: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("gateway must not fabricate [DONE] for a dropped stream: %s", body)
	}
}

// Cancellation: the client cancels while the primary holds the
// request. The handler must finish promptly and the secondary must
// never be called.
func TestRetrySafetyCancellationStopsFailover(t *testing.T) {
	hit := make(chan struct{}, 1)
	var secondaryHits atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, failureOKBody)
	}))
	defer secondary.Close()
	s := failureGateway(t, retrySafetyConfig(
		failureProvider("blocking", primary.URL, 0),
		failureProvider("healthy", secondary.URL, 100),
	))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(failureChatBody))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rr, req)
	}()
	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("primary was never attempted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish after client cancellation")
	}
	if got := secondaryHits.Load(); got != 0 {
		t.Fatalf("secondary hits=%d, want 0: a cancelled request must never be replayed", got)
	}
}

// Config validation: a static idempotency header is always a
// misconfiguration (one key for every request), while an explicit
// forward_headers listing is the operator's own assertion and is
// not rejected here.
func TestRetrySafetyConfigRejectsStaticIdempotencyHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	base := func() config.Config {
		cfg := config.Default()
		cfg.ApplyDefaults()
		return cfg
	}
	static := base()
	static.Providers = []config.ProviderConfig{{
		ID: "p", Name: "p", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 256, StreamIdleTimeoutSeconds: 180,
		Headers: map[string]string{"Idempotency-Key": "fixed-key"},
		Models:  []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	if err := static.Validate(); err == nil || !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("static idempotency header must be rejected, got err=%v", err)
	}
	forwarded := base()
	forwarded.Providers = []config.ProviderConfig{{
		ID: "p", Name: "p", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 256, StreamIdleTimeoutSeconds: 180,
		ForwardHeaders: []string{"Idempotency-Key"},
		Models:         []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	if err := forwarded.Validate(); err != nil {
		t.Fatalf("operator forward_headers listing must not be rejected by this rule, got err=%v", err)
	}
}
