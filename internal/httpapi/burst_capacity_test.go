package httpapi

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
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

const burstOpenAIResponse = `{"id":"c","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

const burstChatBody = `{"model":"client","messages":[{"role":"user","content":"hi"}],"stream":false}`

func burstProvider(id string, baseURL string, maxConcurrency int, priority int) config.ProviderConfig {
	return config.ProviderConfig{ID: id, Name: id, Type: "openai_compatible", BaseURL: baseURL, AuthMode: "none", Enabled: true, MaxConcurrency: maxConcurrency, Models: []config.ModelConfig{{ID: "m", Model: "upstream", Aliases: []string{"client"}, Enabled: true, Priority: priority, Weight: 1}}}
}

func burstPost(t *testing.T, s *Server, rid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(burstChatBody))
	req.Header.Set("x-request-id", rid)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func burstKinds(s *Server, kind string) int {
	n := 0
	for _, e := range s.bus.Snapshot() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// A burst bigger than the global admission limit must queue and drain — not
// fail with instant 503s — while the per-request wait stays bounded.
func TestAdmissionQueueAbsorbsBurst(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, burstOpenAIResponse)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 2
	cfg.Routing.AdmissionQueueTimeoutMS = 5000
	cfg.Providers = []config.ProviderConfig{burstProvider("p", up.URL, 8, 0)}
	s := testGateway(t, cfg)

	const n = 5
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := burstPost(t, s, "burst-q-"+string(rune('a'+i)))
			codes[i] = rr.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: status %d, want 200 (burst must queue, not reject)", i, c)
		}
	}
	if got := s.admissionWaits.Load(); got == 0 {
		t.Fatal("expected queued admissions to be counted")
	}
	if got := burstKinds(s, "gateway_overloaded"); got != 0 {
		t.Fatalf("gateway_overloaded events=%d want 0", got)
	}
}

// A tiny queue timeout must still reject fast with 503 + Retry-After instead
// of hanging until the route budget expires.
func TestAdmissionQueueTimeoutRejectsFast(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, burstOpenAIResponse)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 1
	cfg.Routing.AdmissionQueueTimeoutMS = 100
	cfg.Providers = []config.ProviderConfig{burstProvider("p", up.URL, 4, 0)}
	s := testGateway(t, cfg)

	first := make(chan int, 1)
	go func() { first <- burstPost(t, s, "burst-t-first").Code }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}
	start := time.Now()
	rr := burstPost(t, s, "burst-t-second")
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("overload reject took %v, want fast", elapsed)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("overload reject must carry Retry-After")
	}
	if got := burstKinds(s, "gateway_overloaded"); got != 1 {
		t.Fatalf("gateway_overloaded events=%d want 1", got)
	}
	if code := <-first; code != 200 {
		t.Fatalf("first request status=%d want 200", code)
	}
}

// When the first-choice provider is saturated, requests must spill over to
// the next candidate quickly without poisoning the busy provider's health.
func TestProviderSaturationSpillsToNextCandidate(t *testing.T) {
	enteredSlow := make(chan struct{})
	var once sync.Once
	var slowCalls atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowCalls.Add(1)
		once.Do(func() { close(enteredSlow) })
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, burstOpenAIResponse)
	}))
	defer slow.Close()
	var fastCalls atomic.Int64
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, burstOpenAIResponse)
	}))
	defer fast.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.ProviderQueueTimeoutMS = 100
	cfg.Providers = []config.ProviderConfig{
		burstProvider("pa", slow.URL, 1, 0),
		burstProvider("pb", fast.URL, 8, 1),
	}
	s := testGateway(t, cfg)

	first := make(chan int, 1)
	go func() { first <- burstPost(t, s, "burst-s-first").Code }()
	select {
	case <-enteredSlow:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the slow provider")
	}
	const spill = 2
	codes := make([]int, spill)
	var wg sync.WaitGroup
	for i := 0; i < spill; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = burstPost(t, s, "burst-s-spill").Code
		}(i)
	}
	wg.Wait()
	if code := <-first; code != 200 {
		t.Fatalf("first request status=%d want 200", code)
	}
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("spilled request %d: status %d, want 200", i, c)
		}
	}
	if got := slowCalls.Load(); got != 1 {
		t.Fatalf("slow provider calls=%d want exactly 1 (spill must avoid it)", got)
	}
	if got := fastCalls.Load(); got < spill {
		t.Fatalf("fast provider calls=%d want >= %d", got, spill)
	}
	if got := burstKinds(s, "provider_saturated"); got < spill {
		t.Fatalf("provider_saturated events=%d want >= %d", got, spill)
	}
	if got := s.saturatedSpills.Load(); got < spill {
		t.Fatalf("saturatedSpills=%d want >= %d", got, spill)
	}
	for _, st := range s.hm.Snapshot() {
		if st.Deployment == "pa/m" && st.Failures != 0 {
			t.Fatalf("saturated provider recorded failures=%d, want 0 (no health poison)", st.Failures)
		}
	}
}

// When every candidate is saturated, the gateway must fail fast with 503 +
// Retry-After (not 502, not a hung route budget) and still not poison health.
func TestAllProvidersSaturatedReturns503(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, burstOpenAIResponse)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.ProviderQueueTimeoutMS = 100
	cfg.Providers = []config.ProviderConfig{burstProvider("p", up.URL, 1, 0)}
	s := testGateway(t, cfg)

	first := make(chan int, 1)
	go func() { first <- burstPost(t, s, "burst-a-first").Code }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}
	start := time.Now()
	rr := burstPost(t, s, "burst-a-second")
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("saturated terminal took %v, want fast", elapsed)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("saturated terminal must carry Retry-After")
	}
	if !strings.Contains(rr.Body.String(), "saturated") {
		t.Fatalf("saturated terminal must say so: %s", rr.Body.String())
	}
	if got := burstKinds(s, "provider_saturated"); got != 1 {
		t.Fatalf("provider_saturated events=%d want 1", got)
	}
	for _, st := range s.hm.Snapshot() {
		if st.Deployment == "p/m" && st.Failures != 0 {
			t.Fatalf("saturated provider recorded failures=%d, want 0", st.Failures)
		}
	}
	if code := <-first; code != 200 {
		t.Fatalf("first request status=%d want 200", code)
	}
}

// A hedged loser that only gave up on a saturated slot carries no health
// information and must be left alone like a merely-slower loser.
func TestSaturatedHedgeLoserDoesNotPoisonHealth(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	s.recordHedgeLoser(
		hedgeCall{strategy: "ready_mesh", routeCtx: context.Background(), requestID: "burst-h"},
		router.Scored{Deployment: router.Deployment{ID: "p/m"}},
		hedgeResult{err: providers.ErrProviderSaturated, latency: time.Millisecond},
	)
	for _, st := range s.hm.Snapshot() {
		if st.Deployment == "p/m" {
			t.Fatalf("saturated hedge loser poisoned health: %+v", st)
		}
	}
	if got := burstKinds(s, "route_fail"); got != 0 {
		t.Fatalf("route_fail events=%d want 0", got)
	}
}
