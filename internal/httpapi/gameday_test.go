package httpapi

// Phase 18 resilience game-day suite (issue #418).
//
// Each scenario runs against hermetic fake providers and asserts a named
// invariant. A scenario only counts if its assertion actually executes: the
// runner records a pass/fail per scenario and writes a scorecard under
// .artifacts/issue418/GAMEDAY_SCORECARD.md. The scorecard is evidence, not
// documentation — it is regenerated from the real run.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type gameDayResult struct {
	Name      string
	Invariant string
	Passed    bool
	Detail    string
}

type gameDayScenario struct {
	name      string
	invariant string
	run       func(t *testing.T) error
}

func TestResilienceGameDay(t *testing.T) {
	scenarios := []gameDayScenario{
		{
			name:      "storm_429_5xx_all_candidates",
			invariant: "no duplicate provider submissions on ambiguous retries",
			run:       gameDayStorm,
		},
		{
			name:      "primary_killed_midstream",
			invariant: "streams terminate honestly; no mid-stream failover after bytes are committed",
			run:       gameDayMidstreamKill,
		},
		{
			name:      "slow_loris_upstream",
			invariant: "a slow upstream times out and the gateway stays responsive for health probes",
			run:       gameDaySlowLoris,
		},
		{
			name:      "egress_dns_failure",
			invariant: "a dead/egress-failed upstream degrades to a normalized error with no leak",
			run:       gameDayEgressFailure,
		},
	}

	results := make([]gameDayResult, 0, len(scenarios))
	for _, sc := range scenarios {
		var runErr error
		t.Run(sc.name, func(t *testing.T) {
			runErr = sc.run(t)
			if runErr != nil {
				t.Error(runErr)
			}
		})
		detail := "assertions passed"
		if runErr != nil {
			detail = runErr.Error()
		}
		results = append(results, gameDayResult{
			Name:      sc.name,
			Invariant: sc.invariant,
			Passed:    runErr == nil,
			Detail:    detail,
		})
	}
	writeGameDayScorecard(t, results)
}

func gameDayFakeServer(t *testing.T, ctr *atomic.Int64, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctr.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gameDayPost(s *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func gameDayStorm(t *testing.T) error {
	var a, b, c atomic.Int64
	upA := gameDayFakeServer(t, &a, http.StatusTooManyRequests, `{"error":{"message":"A_PRIVATE_BODY","type":"rate_limit_error"}}`)
	upB := gameDayFakeServer(t, &b, http.StatusInternalServerError, `{"error":{"message":"B_PRIVATE_BODY"}}`)
	upC := gameDayFakeServer(t, &c, http.StatusBadGateway, `{"error":{"message":"C_PRIVATE_BODY"}}`)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Type: "openai_compatible", BaseURL: upA.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "a", Aliases: []string{"storm"}, Enabled: true, Weight: 1, Priority: 0}}},
		{ID: "B", Type: "openai_compatible", BaseURL: upB.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "b", Aliases: []string{"storm"}, Enabled: true, Weight: 1, Priority: 1}}},
		{ID: "C", Type: "openai_compatible", BaseURL: upC.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "c", Aliases: []string{"storm"}, Enabled: true, Weight: 1, Priority: 2}}},
	}
	s := testGateway(t, cfg)
	rr := gameDayPost(s, "/v1/chat/completions", `{"model":"storm","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusServiceUnavailable {
		return fmt.Errorf("status=%d want 503 body=%s", rr.Code, rr.Body.String())
	}
	for _, leaked := range []string{"A_PRIVATE_BODY", "B_PRIVATE_BODY", "C_PRIVATE_BODY"} {
		if strings.Contains(rr.Body.String(), leaked) {
			return fmt.Errorf("provider body leaked to client: %q", leaked)
		}
	}
	// Each candidate may be attempted at most once: an ambiguous retry must not
	// submit the same request twice to the same provider.
	gotA, gotB, gotC := a.Load(), b.Load(), c.Load()
	if gotA > 1 || gotB > 1 || gotC > 1 {
		return fmt.Errorf("duplicate provider submissions A=%d B=%d C=%d", gotA, gotB, gotC)
	}
	if gotA+gotB+gotC == 0 {
		return fmt.Errorf("no upstream attempt was made")
	}
	return nil
}

func gameDayMidstreamKill(t *testing.T) error {
	var aHits, bHits atomic.Int64
	// A accepts, streams one committed chunk, then drops the connection.
	killer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
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
	t.Cleanup(killer.Close)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"B\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(healthy.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		{ID: "A", Type: "openai_compatible", BaseURL: killer.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "a", Aliases: []string{"midstream"}, Enabled: true, Weight: 1, Priority: 0, Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "B", Type: "openai_compatible", BaseURL: healthy.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "b", Aliases: []string{"midstream"}, Enabled: true, Weight: 1, Priority: 1, Capabilities: config.Capabilities{Streaming: true}}}},
	}
	s := testGateway(t, cfg)
	rr := gameDayPost(s, "/v1/chat/completions", `{"model":"midstream","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if aHits.Load() == 0 {
		return fmt.Errorf("primary was never attempted: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// The primary committed bytes before dropping. NexaRoute must terminate the
	// stream honestly instead of silently failing over to B (which would corrupt
	// the client's stream).
	if bHits.Load() != 0 {
		return fmt.Errorf("mid-stream failover occurred after commit: B was attempted %d times", bHits.Load())
	}
	if strings.Count(rr.Body.String(), `"content":"partial"`) != 1 {
		return fmt.Errorf("committed chunk duplicated or missing: %s", rr.Body.String())
	}
	return nil
}

func gameDaySlowLoris(t *testing.T) error {
	var hits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(slow.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.RequestTimeoutMS = 150
	cfg.Providers = []config.ProviderConfig{
		{ID: "slow", Type: "openai_compatible", BaseURL: slow.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "slow", Aliases: []string{"slow"}, Enabled: true, Weight: 1, Priority: 0}}},
	}
	s := testGateway(t, cfg)
	codeCh := make(chan int, 1)
	go func() {
		rr := gameDayPost(s, "/v1/chat/completions", `{"model":"slow","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		codeCh <- rr.Code
	}()
	// While the slow request is in flight, health must stay responsive.
	time.Sleep(30 * time.Millisecond)
	hz := httptest.NewRecorder()
	hzReq := httptest.NewRequest(http.MethodGet, "http://gateway/healthz", nil)
	s.Handler().ServeHTTP(hz, hzReq)
	if hz.Code != http.StatusOK {
		return fmt.Errorf("gateway health unresponsive during slow upstream: status=%d", hz.Code)
	}
	select {
	case code := <-codeCh:
		if code != http.StatusServiceUnavailable && code != http.StatusGatewayTimeout {
			return fmt.Errorf("slow-loris request status=%d want 503/504", code)
		}
	case <-time.After(3 * time.Second):
		return fmt.Errorf("slow-loris request never completed; timeout was not enforced")
	}
	return nil
}

func gameDayEgressFailure(t *testing.T) error {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.RetryBackoffMS = 0
	cfg.Providers = []config.ProviderConfig{
		// Closed loopback port: connection refused, no DNS dependency, no
		// outbound egress.
		{ID: "dead", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "dead", Aliases: []string{"dead"}, Enabled: true, Weight: 1, Priority: 0}}},
	}
	s := testGateway(t, cfg)
	rr := gameDayPost(s, "/v1/chat/completions", `{"model":"dead","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusServiceUnavailable && rr.Code != http.StatusBadGateway {
		return fmt.Errorf("status=%d want 502/503 body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "all candidate deployments failed") {
		return fmt.Errorf("normalized terminal message missing: %s", rr.Body.String())
	}
	// Admin/health remain responsive after an egress failure.
	hz := httptest.NewRecorder()
	s.Handler().ServeHTTP(hz, httptest.NewRequest(http.MethodGet, "http://gateway/healthz", nil))
	if hz.Code != http.StatusOK {
		return fmt.Errorf("health status=%d after egress failure", hz.Code)
	}
	return nil
}

// writeGameDayScorecard emits the scenario × invariant × pass/fail matrix. It
// never fails the test: the assertions above are the evidence; this is the
// published record.
func writeGameDayScorecard(t *testing.T, results []gameDayResult) {
	t.Helper()
	root := repoRootForTest(t)
	if root == "" {
		t.Logf("gameday: repo root not found; skipping scorecard file")
		return
	}
	dir := filepath.Join(root, ".artifacts", "issue418")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("gameday: cannot create scorecard dir: %v", err)
		return
	}
	var b strings.Builder
	b.WriteString("# Phase 18 resilience game-day scorecard\n\n")
	b.WriteString("Generated by `go test ./internal/httpapi -run TestResilienceGameDay -v`.\n")
	b.WriteString("Each row maps a scenario to the invariant it proves. A scenario without an assertion does not appear.\n\n")
	b.WriteString("| Scenario | Invariant proven | Result |\n")
	b.WriteString("| --- | --- | --- |\n")
	passed := 0
	for _, r := range results {
		res := "FAIL"
		if r.Passed {
			res = "PASS"
			passed++
		}
		b.WriteString(fmt.Sprintf("| `%s` | %s | %s |\n", r.Name, r.Invariant, res))
	}
	b.WriteString(fmt.Sprintf("\n**%d/%d scenarios passed.**\n", passed, len(results)))
	b.WriteString("\nAll scenarios run against hermetic fake providers; no external services are contacted.\n")
	path := filepath.Join(dir, "GAMEDAY_SCORECARD.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Logf("gameday: cannot write scorecard: %v", err)
		return
	}
	t.Logf("gameday scorecard written to %s", path)
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
