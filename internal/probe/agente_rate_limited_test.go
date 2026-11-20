package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// TestAgentEProviderRateLimitedEmittedOnRetryAfterRecovery is the
// regression test for Agent E finding #2: in the recovery path, the
// provider_rate_limited event was emitted only under
// `if status == http.StatusTooManyRequests`, but that guard sits inside
// the `providers.RetryAfter(err)` branch, where Probe always returns
// status 0 (a Do error carries no HTTP response). The event could never
// fire from the recovery-deferral path. A RetryAfter error IS a rate-limit
// signal, so the recovery deferral must emit provider_rate_limited.
func TestAgentEProviderRateLimitedEmittedOnRetryAfterRecovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	e := Continue95ProbeHarness(t, srv, func(cfg *config.Config) {
		cfg.Routing.MaxRetryAfterSeconds = 60
		cfg.Routing.FailureThreshold = 10
		cfg.Probe.RecoveryRetryMS = 3600000
	})
	defer e.cancelAllRecoveries()
	task := recoveryTask{id: "p/m", attempt: 1, generation: 3}
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 3
	e.recoveryMu.Unlock()
	// The first probe fails with a plain 429, which cools the credential
	// down; only the second probe's Do finds all credentials cooling down
	// and returns a RetryAfterError, reaching the recovery-deferral path
	// (same two-call shape as TestContinue95ProcessRecoveryRetryAfterClamp).
	e.processRecoveryTask(context.Background(), task)
	e.processRecoveryTask(context.Background(), task)
	deferred := false
	limited := false
	for _, ev := range e.bus.Snapshot() {
		if ev.Kind == "recovery_deferred" {
			deferred = true
		}
		if ev.Kind == "provider_rate_limited" && ev.ErrorType == "rate_limit" &&
			strings.Contains(ev.Message, "rate-limit cooldown") {
			limited = true
		}
	}
	if !deferred {
		t.Fatalf("want recovery_deferred on the RetryAfter path; bus events: %+v", e.bus.Snapshot())
	}
	if !limited {
		t.Errorf("want provider_rate_limited on the RetryAfter recovery path; bus events: %+v", e.bus.Snapshot())
	}
}
