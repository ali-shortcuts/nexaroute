package probe

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestFiveRecoveryFailuresApplyThirtyMinuteCooldownAndClockExpiryRequeues(t *testing.T) {
	var requests atomic.Int32
	e, hm, rt, _, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= 6 { // initial failure plus exactly five recovery probes
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"offline"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}, 5, 1800)
	defer cancel()
	defer e.cancelAllRecoveries()

	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(base.UnixNano())
	hm.SetClock(func() time.Time { return time.Unix(0, now.Load()).UTC() })
	if got := e.Prime(ctx); got.Failed != 1 {
		t.Fatalf("initial probe=%+v", got)
	}
	st := waitForState(t, hm, "p/m", health.Cooldown, 2*time.Second)
	if st.RecoveryFailures != 5 {
		t.Fatalf("recovery failures=%d want exactly 5", st.RecoveryFailures)
	}
	if want := base.Add(30 * time.Minute); !st.CooldownUntil.Equal(want) {
		t.Fatalf("cooldown_until=%s want exactly %s", st.CooldownUntil, want)
	}
	if got := requests.Load(); got != 6 {
		t.Fatalf("probe requests=%d want initial + five recovery probes", got)
	}
	if candidates := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(candidates) != 0 {
		t.Fatalf("cooldown deployment remained routable: %#v", candidates)
	}

	// Fake-clock advance: no 30-minute sleep. The regular scheduler pass sees
	// the now-expired cooldown, transitions it to half-open and queues recovery.
	now.Add(int64((30 * time.Minute).Nanoseconds()))
	// Drop the production real-time timer after advancing the injected health
	// clock; a scheduler sweep must re-enqueue the now-eligible deployment.
	e.cancelAllRecoveries()
	e.setRunContext(ctx)
	e.runOnce(ctx, false)
	st = waitForState(t, hm, "p/m", health.Healthy, 2*time.Second)
	if st.RecoveryFailures != 0 {
		t.Fatalf("successful requalification did not reset recovery count: %+v", st)
	}
	if got := requests.Load(); got != 7 {
		t.Fatalf("requests=%d want exactly one cooldown requalification probe after six failures", got)
	}
	if candidates := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(candidates) != 1 {
		t.Fatalf("expired/requalified model did not return to route pool: %#v", candidates)
	}
}
