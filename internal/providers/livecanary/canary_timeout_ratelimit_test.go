package livecanary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #646: the runbook records a live call that exceeds --call-timeout
// as a timeout observation and a live 429 as an observed-only rate limit.
// Neither path had a canary-level regression test: cancellation was
// covered only through the overall timeout, and 429 handling only through
// adapter unit tests. These tests pin both paths against loopback fakes
// only; they make no live provider call and never induce a real 429.
//
// Honest scope note on the timeout test: the adapter's HTTP timeout and
// the canary's per-call context deadline are both set to the same
// CallTimeout, so which one fires first is a race and the
// "call timed out after ..." annotation in livecanary.go is NOT
// guaranteed. What is guaranteed, and what this test pins, is the
// observable timeout itself: a FAIL whose error names a timeout, exactly
// one upstream call, and an elapsed time near the call timeout rather
// than the overall timeout.

func TestCanaryCallTimeoutIsRecordedAndNotRetried(t *testing.T) {
	fake := &fakeProvider{block: true, release: make(chan struct{})}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 5
	opts.CallTimeout = 2 * time.Second
	opts.OverallTimeout = 10 * time.Second
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()

	start := time.Now()
	report := runner.Run(context.Background())
	elapsed := time.Since(start)
	close(fake.release)
	if len(report.Scenarios) != 1 {
		t.Fatalf("scenarios=%d, want 1", len(report.Scenarios))
	}
	res := report.Scenarios[0]
	if res.ID != "text" {
		t.Fatalf("scenario=%q, want text", res.ID)
	}
	if res.Status != StatusFail {
		t.Fatalf("status=%q, want %q (error=%q)", res.Status, StatusFail, res.Error)
	}
	if !strings.Contains(strings.ToLower(res.Error), "timeout") {
		t.Fatalf("error=%q, want it to record a timeout observation, not a silent pass", res.Error)
	}
	if report.Outcome != OutcomeFailed {
		t.Fatalf("outcome=%q, want %q", report.Outcome, OutcomeFailed)
	}
	if fake.hits.Load() != 1 {
		t.Fatalf("upstream hits=%d, want exactly 1 (a timeout must not be retried as failover)", fake.hits.Load())
	}
	if elapsed < 1500*time.Millisecond || elapsed > 8*time.Second {
		t.Fatalf("elapsed=%v, want roughly the 2s call timeout, not the overall timeout", elapsed)
	}
}

func TestCanaryRateLimitIsReportedAsFailureWithOneCall(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"text"}
	opts.MaxCalls = 5
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()

	report := runner.Run(context.Background())
	if len(report.Scenarios) != 1 {
		t.Fatalf("scenarios=%d, want 1", len(report.Scenarios))
	}
	res := report.Scenarios[0]
	if res.Status != StatusFail {
		t.Fatalf("status=%q, want %q (a 429 must never be reported as a pass)", res.Status, StatusFail)
	}
	if res.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http status=%d, want 429", res.HTTPStatus)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits=%d, want exactly 1 (the canary must not re-run a rate-limited call to confirm it)", hits.Load())
	}
}
