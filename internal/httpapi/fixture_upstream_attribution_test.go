package httpapi

// Request-attributed loopback fixtures.
//
// A fake upstream that counts *every* HTTP request it receives cannot tell a
// duplicate dispatch of the request under test from a request that merely
// landed on the same loopback port. Linux reuses ephemeral ports, and this
// package leaves slow goroutines and keep-alive connections behind (the suite
// itself logs "httptest.Server blocked in Close ... connection in state
// active"), so a stray client can deliver a request to a port a later test has
// just bound. A bare counter reports that as the gateway dispatching twice.
//
// That is exactly how TestWave2GeminiHTTPFailover failed on PR #814 (CI run
// 41301006743: "good candidate calls=2 want 1") while the same request's own
// attempt ledger reported a single a/m then b/m dispatch.
//
// These helpers keep the assertion as strong as it was -- a genuine duplicate
// dispatch of the request under test still counts, and still fails -- while
// refusing to attribute somebody else's request to this test. Unrelated
// arrivals are reported through t.Logf, never silently swallowed.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// upstreamAttribution records requests that reached a fixture upstream but
// cannot belong to the request under test.
type upstreamAttribution struct {
	mu   sync.Mutex
	hits []string
}

func (a *upstreamAttribution) record(r *http.Request, body []byte) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hits = append(a.hits, fmt.Sprintf("%s %s bytes=%d remote=%s", r.Method, r.URL.Path, len(body), r.RemoteAddr))
}

func (a *upstreamAttribution) count() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.hits)
}

func (a *upstreamAttribution) logForeign(t *testing.T) {
	t.Helper()
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, hit := range a.hits {
		t.Logf("ignored unrelated request on the fixture upstream's loopback port (not this request): %s", hit)
	}
}

// fixtureMarker returns a per-call unique marker to embed in the prompt text of
// the request under test. The gateway forwards prompt text upstream, so the
// marker identifies this request's dispatches and nothing else.
func fixtureMarker(name string) string {
	return fmt.Sprintf("nexa-fixture-%s-%d", name, time.Now().UnixNano())
}

// markerCountingServer serves responseBody with status, and increments calls
// only for requests whose body carries marker. Requests without the marker are
// recorded as foreign traffic and answered normally, so a stray client can
// never wedge the test. bannedURL entries are refused as listener addresses so
// a dead-provider fixture and this one can never share a port.
//
// A real duplicate dispatch of the request under test carries the marker and is
// still counted, so "exactly one upstream call" keeps its full strength.
func markerCountingServer(t *testing.T, status int, responseBody, marker string, calls *atomic.Int32, foreign *upstreamAttribution, bannedURL ...string) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if !strings.Contains(string(body), marker) {
			foreign.record(r, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, responseBody)
			return
		}
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, responseBody)
	})
	for i := 0; i < 10; i++ {
		srv := httptest.NewServer(handler)
		banned := false
		for _, b := range bannedURL {
			if b != "" && srv.URL == b {
				banned = true
				break
			}
		}
		if !banned {
			return srv
		}
		srv.Close()
	}
	// Report, never Fatal: this constructor is also called from worker
	// goroutines, where testing.T.Fatalf (FailNow) is not allowed.
	t.Errorf("could not allocate a test server address distinct from %v", bannedURL)
	return nil
}

// mustMarkerCountingServer is the test-goroutine wrapper: a nil server from the
// constructor is fatal there, so every same-goroutine caller keeps failing
// immediately with a clear message.
func mustMarkerCountingServer(t *testing.T, status int, responseBody, marker string, calls *atomic.Int32, foreign *upstreamAttribution, bannedURL ...string) *httptest.Server {
	t.Helper()
	srv := markerCountingServer(t, status, responseBody, marker, calls, foreign, bannedURL...)
	if srv == nil {
		t.Fatalf("could not allocate a test server address distinct from %v", bannedURL)
	}
	return srv
}

// busEventCount counts gateway events of one kind for one deployment: the
// gateway's own attempt ledger, which no loopback-port reuse can perturb.
func busEventCount(s *Server, kind, deployment string) int {
	n := 0
	for _, e := range s.bus.Snapshot() {
		if e.Kind == kind && (deployment == "" || e.Deployment == deployment) {
			n++
		}
	}
	return n
}

// TestFixtureMarkerCountingServerSeparatesUnrelatedRequests pins the fixture
// behaviour the failover tests rely on: a request without this test's marker is
// recorded as foreign and does not increment the dispatch count, while a
// request carrying the marker always does (so a real duplicate dispatch is still
// detected).
func TestFixtureMarkerCountingServerSeparatesUnrelatedRequests(t *testing.T) {
	var calls atomic.Int32
	foreign := &upstreamAttribution{}
	marker := fixtureMarker("attribution-unit")
	srv := mustMarkerCountingServer(t, http.StatusOK, wave3ChatOK, marker, &calls, foreign)
	defer srv.Close()

	post := func(body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp
	}

	// Somebody else's request on this loopback port: not this test's dispatch.
	if resp := post(`{"model":"cm","messages":[{"role":"user","content":"unrelated"}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("foreign request status=%d want 200 (a stray client must not wedge the fixture)", resp.StatusCode)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("calls=%d after unrelated request, want 0 (attribution defect)", got)
	}
	if got := foreign.count(); got != 1 {
		t.Fatalf("foreign traffic recorded=%d want 1", got)
	}

	for i := 0; i < 2; i++ {
		post(`{"model":"cm","messages":[{"role":"user","content":` + strconv.Quote(marker) + `}]}`)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls=%d want 2 (marked dispatches must always count)", got)
	}
	if got := foreign.count(); got != 1 {
		t.Fatalf("foreign traffic recorded=%d want 1 after marked requests", got)
	}
}

// TestWave3ChatTransportFailoverRepeatedUnderConcurrency re-runs the chat
// failover scenario repeatedly and concurrently. It is the regression guard for
// the assertion shape: if a fixture ever counts foreign loopback traffic again,
// or if the gateway ever really dispatches the winning candidate twice, at least
// one of these iterations reports a count other than one.
func TestWave3ChatTransportFailoverRepeatedUnderConcurrency(t *testing.T) {
	const workers, rounds = 4, 3
	var wg sync.WaitGroup
	failures := make(chan string, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				deadURL := dead.URL
				dead.Close()

				var goodCalls atomic.Int32
				foreign := &upstreamAttribution{}
				marker := fixtureMarker(fmt.Sprintf("chat-failover-w%d-r%d", w, r))
				good := markerCountingServer(t, http.StatusOK, wave3ChatOK, marker, &goodCalls, foreign, deadURL)
				if good == nil {
					failures <- fmt.Sprintf("w=%d r=%d could not allocate a fixture server", w, r)
					continue
				}

				s := wave3Gateway(t, []config.ProviderConfig{
					wave3Provider("a", "openai_compatible", deadURL, "m", "upstream-model", []string{"cm"}),
					wave3Provider("b", "openai_compatible", good.URL, "m", "upstream-model", []string{"cm"}),
				}, func(cfg *config.Config) { cfg.Routing.Strategy = "priority" })

				rr := wave3Request(s, "POST", "/v1/chat/completions", wave3ChatMarkedReq("cm", marker))
				calls := goodCalls.Load()
				attempts := busEventCount(s, "route_attempt", "b/m")
				foreignCount := foreign.count()
				good.Close()

				if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ok") {
					failures <- fmt.Sprintf("w=%d r=%d status=%d body=%s", w, r, rr.Code, rr.Body.String())
					continue
				}
				if calls != 1 || attempts != 1 {
					failures <- fmt.Sprintf("w=%d r=%d good candidate calls=%d route_attempt(b/m)=%d want 1/1 (foreign=%d)",
						w, r, calls, attempts, foreignCount)
				}
			}
		}(w)
	}
	wg.Wait()
	close(failures)
	for msg := range failures {
		t.Errorf("chat failover iteration: %s", msg)
	}
}
