package httpapi

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Failure-injection suite (M4-8): the request path driven against
// faulting upstreams and locked stores. Each test pins the OBSERVABLE
// semantics documented in docs/FAILURE_SEMANTICS.md — status codes,
// error envelope, failover, bounded waits, partial-stream behavior,
// and fail-closed startup. Fixtures mirror coverage_openai_test.go.

const failureOKBody = `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

func failureGateway(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func failureProvider(id, baseURL string, priority int) config.ProviderConfig {
	return config.ProviderConfig{
		ID: id, Name: id, Type: "openai_compatible", BaseURL: baseURL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: id + "-model", Enabled: true, Weight: 1, Priority: priority,
			Capabilities: config.Capabilities{Streaming: true},
		}},
	}
}

func failureChatRequest(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

const failureChatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// deadURL returns the URL of a server that has already been closed:
// connections to it are refused.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// F1: the only upstream refuses connections. The client receives the
// documented upstream-error response (never a hang or a fabricated
// success), and the failure is visible in /metrics.
func TestFailureInjectionUpstreamRefused(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Providers = []config.ProviderConfig{failureProvider("dead", deadURL(t), 100)}
	s := failureGateway(t, cfg)

	start := time.Now()
	rr := failureChatRequest(s, failureChatBody)
	elapsed := time.Since(start)
	if rr.Code == http.StatusOK {
		t.Fatalf("refused upstream must not produce success: %s", rr.Body.String())
	}
	if rr.Code < 500 || rr.Code > 599 {
		t.Fatalf("refused upstream status=%d body=%s, want a 5xx upstream error", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"error"`) {
		t.Fatalf("refused upstream body lacks the error envelope: %s", rr.Body.String())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("refused upstream took %s; failure must be prompt", elapsed)
	}
	m := scrapeFailureMetrics(t, s)
	if !strings.Contains(m, `nexaroute_errors_total{error_type="provider_connection_failed"}`) {
		t.Fatalf("metrics do not reflect the refused upstream:\n%s", m)
	}
}

// F2: the primary refuses, a healthy secondary serves the model. The
// request fails over and succeeds.
func TestFailureInjectionFailoverFromRefusedPrimary(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, failureOKBody)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Providers = []config.ProviderConfig{
		failureProvider("dead", deadURL(t), 100),
		failureProvider("healthy", up.URL, 10),
	}
	s := failureGateway(t, cfg)
	rr := failureChatRequest(s, failureChatBody)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"content":"ok"`) {
		t.Fatalf("failover request status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// F3: the upstream accepts the connection and never responds. The
// gateway must give up within the configured request timeout and
// return an error — the client is never left hanging on the
// upstream's schedule.
func TestFailureInjectionUpstreamHangIsBounded(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		up.Close()
	}()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Routing.RequestTimeoutMS = 1200
	cfg.Providers = []config.ProviderConfig{failureProvider("hang", up.URL, 100)}
	s := failureGateway(t, cfg)

	start := time.Now()
	rr := failureChatRequest(s, failureChatBody)
	elapsed := time.Since(start)
	if rr.Code == http.StatusOK {
		t.Fatalf("hanging upstream must not produce success: %s", rr.Body.String())
	}
	if elapsed > 15*time.Second {
		t.Fatalf("hanging upstream held the request for %s; want the configured ~1.2s bound", elapsed)
	}
	t.Logf("hang bounded after %s with status %d", elapsed, rr.Code)
}

// F4: the upstream cuts the connection mid-stream. Semantics pinned:
// the client sees the partial content that was actually produced and
// the stream then terminates — the gateway does not fabricate the
// remainder or a successful completion it never received — and the
// gateway keeps serving afterwards.
func TestFailureInjectionMidStreamCut(t *testing.T) {
	var cutMode atomic.Bool
	cutMode.Store(true)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cutMode.Load() {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, failureOKBody)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream is not hijackable")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial-\"}}]}\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close() // abrupt cut: no terminating chunk, no [DONE]
		}
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Providers = []config.ProviderConfig{failureProvider("cut", up.URL, 100)}
	s := failureGateway(t, cfg)

	rr := failureChatRequest(s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rr.Body.String()
	t.Logf("mid-stream cut status=%d body=%q", rr.Code, body)
	if !strings.Contains(body, "partial-") {
		t.Fatalf("client lost the partial content that was produced: %q", body)
	}
	if strings.Count(body, `"finish_reason":"stop"`) > strings.Count(body, "partial-") {
		t.Fatalf("gateway fabricated a completion after the cut: %q", body)
	}
	// The gateway itself is undamaged: a follow-up request succeeds.
	cutMode.Store(false)
	rr2 := failureChatRequest(s, failureChatBody)
	if rr2.Code != http.StatusOK {
		t.Fatalf("gateway unhealthy after mid-stream cut: status=%d body=%s", rr2.Code, rr2.Body.String())
	}
}

// F5: the admin security store file is locked by another handle
// (e.g. a second gateway or a stuck process). Startup must surface
// the store error — bounded in time — instead of running without its
// audit/session store.
func TestFailureInjectionSecurityStoreLockedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "locked-security.db")
	first, err := securitystore.Open(path, 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.SecurityStorePath = path
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Built via New directly: testGateway refuses fixtures whose
	// security store fails, which is exactly the state under test.
	start := time.Now()
	s := New(cfg, configDir+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
	elapsed := time.Since(start)
	defer func() { _ = s.CloseSecurityStore() }()
	if err := s.SecurityStoreError(); err == nil {
		t.Fatal("gateway with a locked security store must surface SecurityStoreError")
	}
	if elapsed > 15*time.Second {
		t.Fatalf("locked store blocked startup for %s; the lock wait must be bounded", elapsed)
	}
}

// scrapeFailureMetrics reads /metrics with the admin API key.
func scrapeFailureMetrics(t *testing.T, s *Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/metrics", nil)
	req.Header.Set("x-admin-key", s.cfg.Admin.APIKey)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status=%d", rr.Code)
	}
	return rr.Body.String()
}

// F6: recovery semantics differ by strategy family, and both are
// pinned. Under a non-ready strategy (priority), a deployment whose
// cooldown has expired is re-admitted by a live trial request: the
// first post-cooldown request reaches the upstream and its success
// restores service — no probe engine required. (Under the default
// ready_mesh strategy, eligibility requires Healthy and recovery is
// probe-driven instead; see the flapping soak and
// docs/FAILURE_SEMANTICS.md.)
func TestFailureInjectionHalfOpenTrialRecovery(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, failureOKBody)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Routing.FailureThreshold = 1
	cfg.Routing.CooldownSeconds = 1
	cfg.Routing.ProviderFailureThreshold = 100 // isolate deployment-level behavior
	cfg.Providers = []config.ProviderConfig{failureProvider("trial", up.URL, 100)}
	s := failureGateway(t, cfg)

	if rr := failureChatRequest(s, failureChatBody); rr.Code == http.StatusOK {
		t.Fatalf("failing upstream must not succeed: %s", rr.Body.String())
	}
	fail.Store(false)
	// Inside the cooldown the gateway still refuses without a trial.
	if rr := failureChatRequest(s, failureChatBody); rr.Code == http.StatusOK {
		t.Fatal("request inside cooldown must not succeed")
	}
	// After the cooldown, a live trial request re-admits the
	// deployment and traffic flows again.
	time.Sleep(1200 * time.Millisecond)
	if rr := failureChatRequest(s, failureChatBody); rr.Code != http.StatusOK {
		t.Fatalf("half-open trial status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := failureChatRequest(s, failureChatBody); rr.Code != http.StatusOK {
		t.Fatalf("post-recovery request status=%d body=%s", rr.Code, rr.Body.String())
	}
}
