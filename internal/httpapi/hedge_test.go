package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func completionBody(tag string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-%s","object":"chat.completion","created":1,"model":"up-m1","choices":[{"index":0,"message":{"role":"assistant","content":"ok-%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, tag, tag)
}

// slowUpstream answers after delay. Note it cannot observe client
// cancellation: Go's HTTP server only surfaces pre-response client departure
// after the handler's first write, so tests prove loser cancellation
// client-side (fast winner + prompt race completion) instead.
func slowUpstream(delay time.Duration, tag string, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		time.Sleep(delay)
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, completionBody(tag))
	}))
}

func failingUpstream(delay time.Duration, status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func countKind(t *testing.T, s *Server, kind, deployment string) int {
	t.Helper()
	n := 0
	for _, e := range s.bus.Snapshot() {
		if e.Kind == kind && (deployment == "" || e.Deployment == deployment) {
			n++
		}
	}
	return n
}

// attemptsFor counts route attempts of one request by its response request
// ID. The event ring is small and shared, so cumulative diffs across many
// requests would be corrupted by eviction.
func attemptsFor(t *testing.T, s *Server, rr *httptest.ResponseRecorder) int {
	t.Helper()
	rid := rr.Header().Get("x-request-id")
	if rid == "" {
		t.Fatal("response must carry x-request-id")
	}
	n := 0
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "route_attempt" && e.RequestID == rid {
			n++
		}
	}
	return n
}

func TestHedgeBackupWinsWhenPrimarySlow(t *testing.T) {
	var primaryHits atomic.Int32
	slow := slowUpstream(1500*time.Millisecond, "flaky", &primaryHits)
	defer slow.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, slow, solid, func(c *config.Config) {
		c.Routing.HedgeDelayMS = 50
		c.Routing.RequestTimeoutMS = 5000
	})
	before := s.hm.Get("flaky/m1")
	solidBefore := s.hm.Get("solid/m1")
	start := time.Now()
	rr := postChat(t, s, chatBody("up-m1"))
	elapsed := time.Since(start)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("hedge must serve the fast backup: %d %s", rr.Code, rr.Body.String())
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("hedge must not wait for the slow primary: %v", elapsed)
	}
	if n := countKind(t, s, "hedge", "solid/m1"); n != 1 {
		t.Fatalf("expected exactly one hedge win for solid/m1, got %d", n)
	}
	if primaryHits.Load() != 1 {
		t.Fatalf("primary must be hit exactly once, got %d", primaryHits.Load())
	}
	// A merely-slow loser carries no health signal: no failure, no success.
	if st := s.hm.Get("flaky/m1"); st.Failures != before.Failures || st.Successes != before.Successes {
		t.Fatalf("slow hedge loser must record neither failure nor success: %+v", st)
	}
	if st := s.hm.Get("solid/m1"); st.Successes != solidBefore.Successes+1 {
		t.Fatalf("hedge winner must record exactly one success: %+v", st)
	}
}

func postAnthropic(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gw/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "nr-test")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestHedgeAnthropicIngressTranslatesWinner(t *testing.T) {
	slow := slowUpstream(1500*time.Millisecond, "flaky", nil)
	defer slow.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, slow, solid, func(c *config.Config) {
		c.Routing.HedgeDelayMS = 50
		c.Routing.RequestTimeoutMS = 5000
	})
	start := time.Now()
	rr := postAnthropic(t, s, anthropicBody("up-m1"))
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Fatalf("hedged Anthropic ingress must not wait for the slow primary: %v", elapsed)
	}
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-solid") {
		t.Fatalf("hedged Anthropic ingress must serve the translated backup: %d %s", rr.Code, rr.Body.String())
	}
	if n := countKind(t, s, "hedge", "solid/m1"); n != 1 {
		t.Fatalf("expected exactly one hedge win for solid/m1, got %d", n)
	}
}

func TestHedgeDisabledSlowPrimaryServes(t *testing.T) {
	slow := slowUpstream(300*time.Millisecond, "flaky", nil)
	defer slow.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, slow, solid, func(c *config.Config) {
		c.Routing.RequestTimeoutMS = 5000
	})
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-flaky") {
		t.Fatalf("without hedging the slow primary must serve: %d %s", rr.Code, rr.Body.String())
	}
	if n := countKind(t, s, "hedge", ""); n != 0 {
		t.Fatalf("no hedge event may fire when hedging is off, got %d", n)
	}
}

func TestHedgeSkippedWhenBudgetExhausted(t *testing.T) {
	slow := slowUpstream(300*time.Millisecond, "flaky", nil)
	defer slow.Close()
	solid := goodUpstream("solid")
	defer solid.Close()

	s := faultGateway(t, slow, solid, func(c *config.Config) {
		c.Routing.HedgeDelayMS = 50
		c.Routing.RequestTimeoutMS = 5000
	})
	// Drain the budget white-box: the single deposit from this request (1
	// token) cannot fund a retry costing 5, so no backup may launch.
	for s.retryBudget.allowRetry() {
	}
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "ok-flaky") {
		t.Fatalf("exhausted budget must downgrade to the lone primary: %d %s", rr.Code, rr.Body.String())
	}
	if n := countKind(t, s, "hedge", ""); n != 0 {
		t.Fatalf("no hedge may launch on an exhausted budget, got %d events", n)
	}
}

func TestHedgeWinnerErrorStillFailsOver(t *testing.T) {
	// Slow-failing primary, fast-failing backup: the backup wins the race
	// with an error, and the normal loop fails over afterwards.
	slowFail := failingUpstream(300*time.Millisecond, 500, `{"error":"primary-boom"}`)
	defer slowFail.Close()
	fastFail := failingUpstream(0, 500, `{"error":"secondary-boom"}`)
	defer fastFail.Close()

	s := faultGateway(t, slowFail, fastFail, func(c *config.Config) {
		c.Routing.HedgeDelayMS = 50
		c.Routing.RequestTimeoutMS = 5000
	})
	rr := postChat(t, s, chatBody("up-m1"))
	if rr.Code != 500 || !strings.Contains(rr.Body.String(), "secondary-boom") {
		t.Fatalf("hedge error must fail over to the terminal error: %d %s", rr.Code, rr.Body.String())
	}
	if n := countKind(t, s, "hedge", ""); n != 1 {
		t.Fatalf("expected one hedge race, got %d", n)
	}
	// Primary attempt + hedge backup spend the whole two-candidate attempt
	// budget, so the loop terminates instead of retrying the dead backup.
	if n := countKind(t, s, "route_attempt", ""); n != 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", n)
	}
	if st := s.hm.Get("solid/m1"); st.Failures == 0 {
		t.Fatalf("failing backup must record failure: %+v", st)
	}
}

func TestRetryBudgetExhaustionFailsFast(t *testing.T) {
	broken1 := failingUpstream(0, 500, `{"error":"boom-1"}`)
	defer broken1.Close()
	broken2 := failingUpstream(0, 500, `{"error":"boom-2"}`)
	defer broken2.Close()

	s := faultGateway(t, broken1, broken2, func(c *config.Config) {
		c.Routing.Strategy = "priority"
		c.Routing.FailureThreshold = 10000
		c.Routing.RetryBackoffMS = 0
		c.Routing.RequestTimeoutMS = 5000
	})
	// Each failing request spends one retry token (2 attempts). The full
	// bucket funds ~20 such requests; afterwards failover must fail fast
	// with a single attempt plus Retry-After.
	failed := 0
	sawFastRetryAfter := false
	for i := 0; i < 30; i++ {
		rr := postChat(t, s, chatBody("up-m1"))
		if rr.Code != 500 {
			t.Fatalf("request %d: expected terminal 500, got %d", i, rr.Code)
		}
		if attemptsFor(t, s, rr) == 1 {
			failed++
			if rr.Header().Get("Retry-After") == "1" {
				sawFastRetryAfter = true
			}
		}
	}
	if failed == 0 {
		t.Fatal("exhausted budget must fail some requests fast with a single attempt")
	}
	if !sawFastRetryAfter {
		t.Fatal("fail-fast responses must carry Retry-After: 1")
	}
	if n := countKind(t, s, "retry_budget_exhausted", ""); n == 0 {
		t.Fatal("budget exhaustion must emit retry_budget_exhausted events")
	}
}

func TestRetryBudgetUnlimitedKeepsFailingOver(t *testing.T) {
	broken1 := failingUpstream(0, 500, `{"error":"boom-1"}`)
	defer broken1.Close()
	broken2 := failingUpstream(0, 500, `{"error":"boom-2"}`)
	defer broken2.Close()

	s := faultGateway(t, broken1, broken2, func(c *config.Config) {
		c.Routing.Strategy = "priority"
		c.Routing.FailureThreshold = 10000
		c.Routing.RetryBackoffMS = 0
		c.Routing.RetryBudgetRatio = 0
		c.Routing.RequestTimeoutMS = 5000
	})
	for i := 0; i < 30; i++ {
		rr := postChat(t, s, chatBody("up-m1"))
		if rr.Code != 500 {
			t.Fatalf("request %d: expected 500, got %d", i, rr.Code)
		}
		if got := attemptsFor(t, s, rr); got != 2 {
			t.Fatalf("request %d: unlimited budget must try both candidates, got %d attempts", i, got)
		}
	}
	if n := countKind(t, s, "retry_budget_exhausted", ""); n != 0 {
		t.Fatalf("unlimited budget must never exhaust, got %d events", n)
	}
}

var _ = health.Healthy
