package httpapi

// Issue #437: reproducible local load-profile harness.
//
// Two layers live here:
//
//  1. TestSlowDashboardSubscribersDoNotBlockInference is an always-on,
//     deterministic contract test. It proves the acceptance-criteria
//     shape "a slow UI must not block inference" without any wall-clock
//     threshold: inference status distribution and bounded counters are
//     asserted, never latency.
//
//  2. TestLoadProfileMixedWorkload is a gated (NEXAROUTE_LOAD=1)
//     reproducible profile. It drives concurrent short/streaming calls,
//     a request failover chain, provider 429s, a client-timeout category
//     and multiple dashboard subscribers against loopback httptest
//     upstreams. Gateway overhead is reported separately from the
//     configured upstream delay. It asserts only bounded-state and
//     liveness invariants (every request resolves, no in-flight leaks,
//     circuits do not grow); it never asserts a wall-clock threshold.
//
// Local-only: httptest loopback servers, AuthMode none, no secrets and
// no external provider calls. See scripts/load.sh and
// docs/reports/ISSUE437_LOAD_PROFILE.md for the commands of record.

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

const (
	// Fixed upstream delays let the harness subtract provider latency
	// from observed latency to report local gateway overhead.
	loadShortDelay  = 3 * time.Millisecond
	loadStreamDelay = 2 * time.Millisecond
	loadSlowDelay   = 400 * time.Millisecond
	// loadSlowTimeout is the client-side deadline for the timeout
	// category; it is deliberately far below loadSlowDelay.
	loadSlowTimeout = 60 * time.Millisecond
)

type loadCategory int

const (
	catShort loadCategory = iota
	catStreamCoding
	catFailover
	catRateLimit
	catSlowTimeout
)

// loadPick maps a seeded RNG draw to a category with fixed weights so a
// given seed produces the identical operation sequence on every machine.
func loadPick(rng *rand.Rand) loadCategory {
	switch n := rng.Intn(100); {
	case n < 40:
		return catShort
	case n < 70:
		return catStreamCoding
	case n < 85:
		return catFailover
	case n < 95:
		return catRateLimit
	default:
		return catSlowTimeout
	}
}

func loadEnvInt(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func loadNonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func loadPercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func loadShortUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(loadShortDelay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"short","object":"chat.completion","model":"short-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
}

func loadStreamUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		time.Sleep(loadStreamDelay)
		_, _ = io.WriteString(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(loadStreamDelay)
		_, _ = io.WriteString(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

// loadFailPrimaryUpstream returns 409 Conflict. A 409 is failover-eligible
// without quarantining the deployment or signalling a provider incident, so
// the failover chain is exercised on every request for the whole profile run.
func loadFailPrimaryUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"message":"transition conflict","type":"server_error"}}`)
	}))
}

func loadFallbackUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fb","object":"chat.completion","model":"fallback-model","choices":[{"index":0,"message":{"role":"assistant","content":"FALLBACK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
}

func loadRateLimitUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
}

func loadSlowUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-time.After(loadSlowDelay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"slow","object":"chat.completion","model":"slow-model","choices":[{"index":0,"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}],"usage":{}}`)
	}))
}

type loadUpstreams struct {
	short    *httptest.Server
	stream   *httptest.Server
	fail     *httptest.Server
	fallback *httptest.Server
	rate     *httptest.Server
	slow     *httptest.Server

	shortCalls, streamCalls, failCalls, fallbackCalls, rateCalls, slowCalls atomic.Int64
}

func newLoadUpstreams() *loadUpstreams {
	u := &loadUpstreams{}
	u.short = loadShortUpstream(&u.shortCalls)
	u.stream = loadStreamUpstream(&u.streamCalls)
	u.fail = loadFailPrimaryUpstream(&u.failCalls)
	u.fallback = loadFallbackUpstream(&u.fallbackCalls)
	u.rate = loadRateLimitUpstream(&u.rateCalls)
	u.slow = loadSlowUpstream(&u.slowCalls)
	return u
}

func (u *loadUpstreams) close() {
	for _, s := range []*httptest.Server{u.short, u.stream, u.fail, u.fallback, u.rate, u.slow} {
		s.Close()
	}
}

// loadGatewayConfig builds a deterministic priority-strategy gateway. The
// coding alias is served by a configurable pool of deployments so pool-scale
// routing overhead can be measured end to end.
func loadGatewayConfig(pool int, u *loadUpstreams) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.MaxRetryAfterSeconds = 1
	cfg.Routing.RequestTimeoutMS = 5000
	cfg.Routing.MaxInflightRequests = 256
	cfg.Routing.SanitizeEnabled = false
	// This harness measures load, not circuit liveness. High thresholds keep
	// one failure category from quarantining a deployment and silently
	// removing it from the workload mid-run; the cooldown/circuit lifecycle is
	// covered by the failure-injection and soak suites.
	cfg.Routing.FailureThreshold = 100000
	cfg.Routing.ProviderFailureThreshold = 100000
	cfg.Routing.CooldownSeconds = 1
	cfg.Routing.ProviderCooldownSeconds = 1

	providers := []config.ProviderConfig{
		{
			ID: "short", Name: "Short", Type: "openai_compatible", BaseURL: u.short.URL,
			AuthMode: "none", Enabled: true, MaxConcurrency: 256,
			Models: []config.ModelConfig{{ID: "m", Model: "short-model", Aliases: []string{"short"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
		},
		{
			ID: "fail", Name: "Fail", Type: "openai_compatible", BaseURL: u.fail.URL,
			AuthMode: "none", Enabled: true, MaxConcurrency: 256,
			Models: []config.ModelConfig{{ID: "m", Model: "fail-model", Aliases: []string{"failover"}, Enabled: true, Weight: 1, Priority: 0}},
		},
		{
			ID: "fallback", Name: "Fallback", Type: "openai_compatible", BaseURL: u.fallback.URL,
			AuthMode: "none", Enabled: true, MaxConcurrency: 256,
			Models: []config.ModelConfig{{ID: "m", Model: "fallback-model", Aliases: []string{"failover"}, Enabled: true, Weight: 1, Priority: 1}},
		},
		{
			ID: "rate", Name: "Rate", Type: "openai_compatible", BaseURL: u.rate.URL,
			AuthMode: "none", Enabled: true, MaxConcurrency: 256,
			Models: []config.ModelConfig{{ID: "m", Model: "rate-model", Aliases: []string{"ratelimit"}, Enabled: true, Weight: 1}},
		},
		{
			ID: "slow", Name: "Slow", Type: "openai_compatible", BaseURL: u.slow.URL,
			AuthMode: "none", Enabled: true, MaxConcurrency: 256,
			Models: []config.ModelConfig{{ID: "m", Model: "slow-model", Aliases: []string{"slow"}, Enabled: true, Weight: 1}},
		},
	}
	poolProvider := config.ProviderConfig{
		ID: "pool", Name: "Pool", Type: "openai_compatible", BaseURL: u.stream.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 256,
	}
	for i := 0; i < pool; i++ {
		poolProvider.Models = append(poolProvider.Models, config.ModelConfig{
			ID: fmt.Sprintf("cm%04d", i), Model: fmt.Sprintf("coding-model-%04d", i),
			Aliases: []string{"coding"}, Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true},
		})
	}
	providers = append(providers, poolProvider)
	cfg.Providers = providers
	return cfg
}

// startSlowDashboardSubscribers opens n admin event streams that read slowly.
// It returns a wait function, plus one readiness result per subscriber. A nil
// readiness result means the HTTP stream is established before the caller starts
// its inference workload. Producers must never block on these subscribers.
func startSlowDashboardSubscribers(ctx context.Context, baseURL string, n int) (func(), <-chan error) {
	var wg sync.WaitGroup
	ready := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/admin/api/events/stream?limit=16", nil)
			if err != nil {
				ready <- err
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				ready <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				ready <- fmt.Errorf("events stream returned HTTP %d", resp.StatusCode)
				return
			}
			ready <- nil
			reader := newSSEReader(resp.Body)
			for {
				if _, done, err := reader.Next(); done || err != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()
	}
	return wg.Wait, ready
}

// waitForSlowDashboardSubscribers prevents the inference workload from starting
// until every requested dashboard stream is established. Cleanup cancels and
// joins the subscriber goroutines even when readiness fails.
func waitForSlowDashboardSubscribers(t *testing.T, ready <-chan error, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := <-ready; err != nil {
			t.Fatalf("dashboard subscriber %d failed to become ready: %v", i+1, err)
		}
	}
}

// TestSlowDashboardSubscribersDoNotBlockInference proves, deterministically
// and with no wall-clock threshold, that a saturated set of slow dashboard
// subscribers cannot stall or fail inference: every request still returns 200
// and bounded admission/in-flight counters return to zero.
func TestSlowDashboardSubscribersDoNotBlockInference(t *testing.T) {
	var calls atomic.Int64
	up := loadShortUpstream(&calls)
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 64,
		Models: []config.ModelConfig{{ID: "m", Model: "short-model", Aliases: []string{"short"}, Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()

	const slowSubscribers = 32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitSubscribers, subscribersReady := startSlowDashboardSubscribers(ctx, gw.URL, slowSubscribers)
	t.Cleanup(func() {
		cancel()
		waitSubscribers()
	})
	waitForSlowDashboardSubscribers(t, subscribersReady, slowSubscribers)

	client := &http.Client{Timeout: 15 * time.Second}
	var failed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 16; i++ {
				resp, err := client.Post(gw.URL+"/v1/chat/completions", "application/json",
					strings.NewReader(`{"model":"short","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					failed.Add(1)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					failed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// Tear down the slow subscribers after inference has finished so their
	// cancellation cannot be mistaken for an inference failure.
	cancel()
	waitSubscribers()

	if failed.Load() != 0 {
		t.Fatalf("inference failed while slow dashboard subscribers were attached: failures=%d", failed.Load())
	}
	if got := s.inflight.Load(); got != 0 {
		t.Fatalf("global in-flight admission leaked: %d", got)
	}
	for _, stat := range s.reg.Stats() {
		if stat.ActiveRequests != 0 || stat.WaitingRequests != 0 {
			t.Fatalf("provider capacity leaked after slow-subscriber run: %+v", stat)
		}
	}
}

type loadResult struct {
	shortOverheads []time.Duration
	streamTTFTs    []time.Duration
}

// runLoadWorkers executes the deterministic operation sequence with a fixed
// worker pool and returns per-category gateway-overhead samples.
func runLoadWorkers(ctx context.Context, gwURL string, ops []loadCategory, workers int, failures *atomic.Int64) *loadResult {
	client := &http.Client{}
	var idx atomic.Int64
	res := &loadResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	doShort := func() {
		start := time.Now()
		resp, err := client.Post(gwURL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"short","messages":[{"role":"user","content":"hi"}]}`))
		elapsed := time.Since(start)
		if err != nil {
			failures.Add(1)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			failures.Add(1)
			return
		}
		mu.Lock()
		res.shortOverheads = append(res.shortOverheads, loadNonNegative(elapsed-loadShortDelay))
		mu.Unlock()
	}

	doStream := func() {
		start := time.Now()
		resp, err := client.Post(gwURL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"code"}]}`))
		if err != nil {
			failures.Add(1)
			return
		}
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		ttft := time.Since(start)
		rest, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		body := string(buf[:n]) + string(rest)
		if resp.StatusCode != http.StatusOK || n == 0 || !strings.Contains(body, "[DONE]") {
			failures.Add(1)
			return
		}
		mu.Lock()
		res.streamTTFTs = append(res.streamTTFTs, loadNonNegative(ttft-loadStreamDelay))
		mu.Unlock()
	}

	doFailover := func() {
		resp, err := client.Post(gwURL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"failover","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			failures.Add(1)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "FALLBACK") {
			failures.Add(1)
		}
	}

	doRateLimit := func() {
		resp, err := client.Post(gwURL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"ratelimit","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			failures.Add(1)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// 429 is the provider signal; 502/503 are the bounded gateway
		// shed/overload envelopes once the deployment is cooling down.
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		default:
			failures.Add(1)
		}
	}

	doSlow := func() {
		rctx, rcancel := context.WithTimeout(ctx, loadSlowTimeout)
		defer rcancel()
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, gwURL+"/v1/chat/completions",
			strings.NewReader(`{"model":"slow","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			failures.Add(1)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		// Any bounded outcome (client timeout, gateway 4xx/5xx, or a late
		// 200) resolves; only a hang would trip the test's deadline guard.
		_ = err
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(idx.Add(1)) - 1
				if i >= len(ops) {
					return
				}
				switch ops[i] {
				case catShort:
					doShort()
				case catStreamCoding:
					doStream()
				case catFailover:
					doFailover()
				case catRateLimit:
					doRateLimit()
				case catSlowTimeout:
					doSlow()
				}
			}
		}()
	}
	wg.Wait()
	return res
}

// TestLoadProfileMixedWorkload is the gated reproducible profile. Run it with
// NEXAROUTE_LOAD=1; see scripts/load.sh. It reports measurements and asserts
// only bounded-state/liveness invariants.
func TestLoadProfileMixedWorkload(t *testing.T) {
	if os.Getenv("NEXAROUTE_LOAD") != "1" {
		t.Skip("set NEXAROUTE_LOAD=1 to run the local load-profile harness")
	}

	pool := loadEnvInt("NEXAROUTE_LOAD_POOL", 100)
	totalOps := loadEnvInt("NEXAROUTE_LOAD_OPS", 1000)
	workers := loadEnvInt("NEXAROUTE_LOAD_WORKERS", 16)
	seed := int64(loadEnvInt("NEXAROUTE_LOAD_SEED", 1))
	subscribers := loadEnvInt("NEXAROUTE_LOAD_SUBSCRIBERS", 2)

	u := newLoadUpstreams()
	defer u.close()

	cfg := loadGatewayConfig(pool, u)
	s := testGateway(t, cfg)
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	beforeGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitSubscribers, subscribersReady := startSlowDashboardSubscribers(ctx, gw.URL, subscribers)
	t.Cleanup(func() {
		cancel()
		waitSubscribers()
	})
	waitForSlowDashboardSubscribers(t, subscribersReady, subscribers)

	rng := rand.New(rand.NewSource(seed))
	ops := make([]loadCategory, totalOps)
	for i := range ops {
		ops[i] = loadPick(rng)
	}

	var failures atomic.Int64
	start := time.Now()
	res := runLoadWorkers(ctx, gw.URL, ops, workers, &failures)
	elapsed := time.Since(start)

	cancel()
	waitSubscribers()

	// Let upstream cancellation and handler unwind settle before sampling.
	drainDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(drainDeadline) {
		if s.inflight.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Idle keep-alive handler goroutines are legitimate steady state; give
	// them a bounded moment to park before the leak sample.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	afterGoroutines := runtime.NumGoroutine()

	if failures.Load() != 0 {
		t.Fatalf("load profile failures=%d of %d ops", failures.Load(), totalOps)
	}
	if got := s.inflight.Load(); got != 0 {
		t.Fatalf("global in-flight admission leaked after profile: %d", got)
	}
	for _, stat := range s.reg.Stats() {
		if stat.ActiveRequests != 0 || stat.WaitingRequests != 0 {
			t.Fatalf("provider capacity leaked after profile: %+v", stat)
		}
	}
	if u.failCalls.Load() == 0 || u.fallbackCalls.Load() == 0 {
		t.Fatalf("failover chain was not exercised: fail=%d fallback=%d", u.failCalls.Load(), u.fallbackCalls.Load())
	}
	if u.rateCalls.Load() == 0 {
		t.Fatal("provider rate-limit path was not exercised")
	}

	sort.Slice(res.shortOverheads, func(i, j int) bool { return res.shortOverheads[i] < res.shortOverheads[j] })
	sort.Slice(res.streamTTFTs, func(i, j int) bool { return res.streamTTFTs[i] < res.streamTTFTs[j] })

	var b strings.Builder
	fmt.Fprintf(&b, "issue437 load profile\n")
	fmt.Fprintf(&b, "seed=%d pool=%d ops=%d workers=%d subscribers=%d\n", seed, pool, totalOps, workers, subscribers)
	fmt.Fprintf(&b, "elapsed=%s\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(&b, "upstream_calls short=%d stream=%d fail=%d fallback=%d rate=%d slow=%d\n",
		u.shortCalls.Load(), u.streamCalls.Load(), u.failCalls.Load(), u.fallbackCalls.Load(), u.rateCalls.Load(), u.slowCalls.Load())
	fmt.Fprintf(&b, "gateway_overhead short p50=%s p95=%s n=%d\n",
		loadPercentile(res.shortOverheads, 0.50).Round(time.Microsecond),
		loadPercentile(res.shortOverheads, 0.95).Round(time.Microsecond), len(res.shortOverheads))
	fmt.Fprintf(&b, "gateway_ttft stream p50=%s p95=%s n=%d\n",
		loadPercentile(res.streamTTFTs, 0.50).Round(time.Microsecond),
		loadPercentile(res.streamTTFTs, 0.95).Round(time.Microsecond), len(res.streamTTFTs))
	fmt.Fprintf(&b, "goroutines before=%d after=%d\n", beforeGoroutines, afterGoroutines)
	fmt.Fprintf(&b, "heap_alloc_mb before=%.2f after=%.2f\n",
		float64(before.HeapAlloc)/(1024*1024), float64(after.HeapAlloc)/(1024*1024))
	fmt.Fprintf(&b, "sessions=%d\n", s.rt.SessionCount())

	report := b.String()
	t.Logf("\n%s", report)
	if out := strings.TrimSpace(os.Getenv("NEXAROUTE_LOAD_OUT")); out != "" {
		if err := os.WriteFile(out, []byte(report), 0o644); err != nil {
			t.Fatalf("write load profile output %q: %v", out, err)
		}
	}

	// Goroutine growth is asserted with a generous, worker-proportional
	// tolerance only in the gated profile, and only after a drain loop: this
	// is a leak gate, not a latency/threshold gate. Idle keep-alive handler
	// goroutines legitimately scale with concurrency, so the bound scales too.
	if delta := afterGoroutines - beforeGoroutines; delta > 4*workers+subscribers+128 {
		t.Fatalf("goroutine growth %d exceeds bounded profile design (before=%d after=%d)", delta, beforeGoroutines, afterGoroutines)
	}
}
