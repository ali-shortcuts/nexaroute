package httpapi

import (
	"context"
	"fmt"
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
	return config.ProviderConfig{ID: id, Name: id, Type: "openai_compatible", BaseURL: baseURL, AuthMode: "none", Enabled: true, MaxConcurrency: maxConcurrency, Models: []config.ModelConfig{{ID: "m", Model: "upstream", Aliases: []string{"client"}, Enabled: true, Priority: priority, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}
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
	cfg.Routing.AdmissionQueueTimeoutMS = 0
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

// timeout=0 must wait until a slot frees (as long as the client is still
// connected) instead of 503'ing a live Claude Code / sub-agent call.
func TestAdmissionQueueZeroWaitsUntilClientGone(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, burstOpenAIResponse)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 1
	cfg.Routing.AdmissionQueueTimeoutMS = 0
	cfg.Providers = []config.ProviderConfig{burstProvider("p", up.URL, 4, 0)}
	s := testGateway(t, cfg)

	firstDone := make(chan int, 1)
	go func() { firstDone <- burstPost(t, s, "zero-first").Code }()
	deadline := time.Now().Add(2 * time.Second)
	for s.inflight.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.inflight.Load() < 1 {
		t.Fatal("first request never took the slot")
	}

	secondDone := make(chan int, 1)
	go func() { secondDone <- burstPost(t, s, "zero-second").Code }()
	select {
	case code := <-secondDone:
		t.Fatalf("second request returned %d before the slot freed; 0 must wait", code)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case code := <-secondDone:
		if code != 200 {
			t.Fatalf("queued sub-agent status=%d want 200", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued sub-agent never drained")
	}
	if code := <-firstDone; code != 200 {
		t.Fatalf("first request status=%d want 200", code)
	}
}

// A single provider at max_concurrency must queue the extra sub-agent until a
// slot frees (timeout 0) instead of returning 503 all-saturated.
func TestProviderQueueZeroWaitsOnSingleProvider(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, burstOpenAIResponse)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 16
	cfg.Routing.AdmissionQueueTimeoutMS = 0
	cfg.Routing.ProviderQueueTimeoutMS = 0
	cfg.Providers = []config.ProviderConfig{burstProvider("p", up.URL, 1, 0)}
	s := testGateway(t, cfg)

	firstDone := make(chan int, 1)
	go func() { firstDone <- burstPost(t, s, "prov-first").Code }()
	deadline := time.Now().Add(2 * time.Second)
	for s.inflight.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	secondDone := make(chan int, 1)
	go func() { secondDone <- burstPost(t, s, "prov-second").Code }()
	select {
	case code := <-secondDone:
		t.Fatalf("second request returned %d while the only slot was busy; 0 must wait", code)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case code := <-secondDone:
		if code != 200 {
			t.Fatalf("queued provider wait status=%d want 200", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued provider wait never drained")
	}
	if code := <-firstDone; code != 200 {
		t.Fatalf("first request status=%d want 200", code)
	}
	if got := burstKinds(s, "gateway_overloaded"); got != 0 {
		t.Fatalf("gateway_overloaded events=%d want 0", got)
	}
}

const burstStreamBody = `{"model":"client","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`

func tokenBurstUpstream(chunks int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok-%d-xxxxxxxx\"}}]}\n\n", i)
			if fl != nil {
				fl.Flush()
			}
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// Parallel streaming sub-agents must all complete with 200 — more tokens and
// more concurrent requests must not 503 or truncate.
func TestParallelSubAgentStreamsAllComplete(t *testing.T) {
	up := tokenBurstUpstream(80)
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 4
	cfg.Routing.AdmissionQueueTimeoutMS = 0
	cfg.Routing.ProviderQueueTimeoutMS = 0
	cfg.Providers = []config.ProviderConfig{burstProvider("p", up.URL, 2, 0)}
	s := testGateway(t, cfg)

	const n = 12
	var wg sync.WaitGroup
	var ok, other atomic.Int32
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(burstStreamBody))
			req.Header.Set("content-type", "application/json")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code == 200 && strings.Contains(rr.Body.String(), "tok-79") && strings.Contains(rr.Body.String(), "[DONE]") {
				ok.Add(1)
				return
			}
			other.Add(1)
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != n {
		t.Fatalf("parallel streams ok=%d other=%d want %d all 200 with full token bodies", ok.Load(), other.Load(), n)
	}
	if got := s.inflight.Load(); got != 0 {
		t.Fatalf("inflight leaked: %d", got)
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
