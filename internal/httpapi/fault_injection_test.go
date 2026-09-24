package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// faultGateway wires two providers: a faulting primary and a healthy
// secondary. Caller-supplied handlers control each fake upstream.
func faultGateway(t *testing.T, primary, secondary *httptest.Server, tune func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue" // deterministic priority failover
	cfg.Routing.RequestTimeoutMS = 900
	cfg.Routing.FailureThreshold = 2
	cfg.Routing.CooldownSeconds = 60
	cfg.Providers = []config.ProviderConfig{
		{
			ID: "flaky", Name: "Flaky", Type: "openai_compatible", BaseURL: primary.URL,
			AuthMode: "bearer", APIKey: "sk-flaky", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "up-m1", Enabled: true, Priority: 1, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
		},
		{
			ID: "solid", Name: "Solid", Type: "openai_compatible", BaseURL: secondary.URL,
			AuthMode: "bearer", APIKey: "sk-solid", Enabled: true,
			Models: []config.ModelConfig{{ID: "m1", Model: "up-m1", Enabled: true, Priority: 5, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
		},
	}
	if tune != nil {
		tune(&cfg)
	}
	return testGateway(t, cfg)
}

func chatBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
}

func goodUpstream(tag string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-%s","object":"chat.completion","created":1,"model":"up-m1","choices":[{"index":0,"message":{"role":"assistant","content":"ok-%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, tag, tag)
	}))
}

func TestFaultTimeoutFailsOverToHealthyProvider(t *testing.T) {
	var primaryHits atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"late"}}]}`)
	}))
	defer slow.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, slow, solid, func(c *config.Config) { c.Routing.AttemptTimeoutMS = 400 })
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	start := time.Now()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("failover must produce 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("failover must not wait for the hung attempt: %v", elapsed)
	}
	if !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("response must come from the secondary: %s", rr.Body.String())
	}
	if primaryHits.Load() != 1 {
		t.Fatalf("slow primary should be attempted exactly once, got %d", primaryHits.Load())
	}
}

func TestFaultMidStreamCloseSurfacesTruncationWithoutCrash(t *testing.T) {
	abrupt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up-m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"par\"}}]}\n\n")
		flusher := w.(http.Flusher)
		flusher.Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		if conn != nil {
			conn.Close() // sever mid-stream, after commit
		}
	}))
	defer abrupt.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, abrupt, solid, nil)
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")[:len(chatBody("up-m1"))-1]+`, "stream":true}`))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("streaming commit returns 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"par"`) {
		t.Fatalf("client must receive the bytes that were delivered: %q", body)
	}
	// Gateway must stay healthy after the upstream vanished mid-stream.
	ready := httptest.NewRequest(http.MethodGet, "http://gw/readyz", nil)
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, ready)
	if rr2.Code != 200 {
		t.Fatalf("gateway must survive a mid-stream upstream close: %d %s", rr2.Code, rr2.Body.String())
	}
}

func TestFaultGarbage200FailsOverLikeAnError(t *testing.T) {
	var hits atomic.Int32
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices": [this is not json`)
	}))
	defer garbage.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, garbage, solid, nil)
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("garbage 200 must fail over pre-commit, got %d: %s", rr.Code, rr.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("garbage primary attempted once, got %d", hits.Load())
	}
}

func TestFaultRetryAfterHeaderIsCapped(t *testing.T) {
	refuseAll := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "999999")
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"overloaded","type":"rate_limit_error"}}`)
	}
	a := httptest.NewServer(http.HandlerFunc(refuseAll))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(refuseAll))
	defer b.Close()

	s := faultGateway(t, a, b, nil)
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 429 {
		t.Fatalf("all-429 must surface 429, got %d", rr.Code)
	}
	ra := rr.Header().Get("Retry-After")
	if ra == "" || ra == "999999" {
		t.Fatalf("Retry-After must be present and capped (<=60), got %q", ra)
	}
	if n := parseRetryAfterForTest(ra); n > 60 {
		t.Fatalf("Retry-After %d exceeds cap", n)
	}
}

func parseRetryAfterForTest(v string) int {
	var n int
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return -1
		}
		n = n*10 + int(v[i]-'0')
	}
	return n
}

func TestFaultConnectionRefusedFailsOverAndClassifies(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // guaranteed refused port

	solid := goodUpstream("solid")
	defer solid.Close()
	s := faultGateway(t, dead, solid, func(c *config.Config) {
		c.Providers[0].BaseURL = deadURL
	})
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("connection refused must fail over, got %d: %s", rr.Code, rr.Body.String())
	}
	class := classifyTransportError(fmt.Errorf("dial tcp %s: connect: connection refused", strings.TrimPrefix(deadURL, "http://")))
	if class != "provider_connection_failed" {
		t.Fatalf("connection refused must classify as provider_connection_failed, got %q", class)
	}
}

func TestFaultFlappingPrimaryEntersCooldownAndTrafficStopsHittingIt(t *testing.T) {
	var hits atomic.Int32
	flap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"boom","type":"api_error"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","created":1,"model":"up-m1","choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`)
	}))
	defer flap.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, flap, solid, nil)
	send := func() string {
		req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
		req.Header.Set("authorization", "Bearer nr-test")
		req.Header.Set("content-type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return fmt.Sprintf("%d %s", rr.Code, rr.Body.String())
	}
	// Two failures cross FailureThreshold=2 → primary quarantined.
	for i := 0; i < 2; i++ {
		out := send()
		if !strings.Contains(out, "ok-solid") {
			t.Fatalf("attempt %d must fail over: %s", i+1, out)
		}
	}
	before := hits.Load()
	// Cooldown window: primary is quarantined, traffic must not reach it.
	out := send()
	if !strings.Contains(out, "ok-solid") {
		t.Fatalf("steady-state request must use secondary: %s", out)
	}
	if got := hits.Load(); got != before {
		t.Fatalf("cooled-down primary must receive zero traffic (hits %d -> %d)", before, got)
	}
}

func TestFaultProviderSecretNeverLeaksIntoFailureResponses(t *testing.T) {
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"auth failed for sk-flaky at https://up.example/v1","type":"api_error"}}`)
	}))
	defer leak.Close()
	s := faultGateway(t, leak, leak, func(c *config.Config) { c.Routing.MaxAttempts = 1 })
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if body := rr.Body.String(); strings.Contains(body, "sk-flaky") {
		t.Fatalf("configured credential must never appear in failure body: %s", body)
	}
	if os.Getenv("NEXAROUTE_FAULT_DEBUG") != "" {
		t.Logf("failure body: %s", rr.Body.String())
	}
}

func TestFaultWithoutAttemptTimeoutWholeBudgetStillBounds(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"late"}}]}`)
	}))
	defer slow.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	// attempt_timeout_ms = 0 keeps the historical contract: the whole route
	// budget (request_timeout_ms) bounds the request and no failover happens.
	s := faultGateway(t, slow, solid, nil)
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	start := time.Now()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("no attempt timeout: whole-budget 504 expected, got %d", rr.Code)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("504 should fire at the route budget, took %v", elapsed)
	}
}

func TestFaultAttemptTimeoutCannotOutliveRouteBudget(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(200)
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer slow.Close()
	s := faultGateway(t, slow, slow, func(c *config.Config) {
		c.Routing.RequestTimeoutMS = 700
		c.Routing.AttemptTimeoutMS = 60000 // absurdly larger than the route budget
		c.Routing.MaxAttempts = 2
	})
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	start := time.Now()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("route budget must still bound the request, got %d", rr.Code)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("route budget fired late: %v", elapsed)
	}
}

func TestFaultRetryAfterSurvivesMixedFailuresPostLoop(t *testing.T) {
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	}))
	defer rateLimited.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	// Candidate order: 429 provider (tier 1) then dead provider (tier 5).
	// The loop ends with a transport error on the last candidate, so the
	// client-visible 429 comes from the post-loop write path and must still
	// carry the capped Retry-After from the earlier rate-limited attempt.
	s := faultGateway(t, rateLimited, rateLimited, func(c *config.Config) {
		c.Providers[1].BaseURL = deadURL
		c.Providers[1].ID = "solid-dead"
		c.Routing.AttemptTimeoutMS = 300
	})
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(chatBody("up-m1")))
	req.Header.Set("authorization", "Bearer nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 429 {
		t.Fatalf("expected final 429, got %d: %s", rr.Code, rr.Body.String())
	}
	ra := rr.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("post-loop 429 must surface the capped upstream Retry-After")
	}
	if n := parseRetryAfterForTest(ra); n < 1 || n > 60 {
		t.Fatalf("Retry-After %q outside cap/window", ra)
	}
}
