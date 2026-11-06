package health

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// Agent E coverage tests for internal/health. These target the blocks the
// CI baseline left uncovered; each test names the block it covers.

func TestAgentEHistogramNegativeObservationClampedToZero(t *testing.T) {
	// histogram.go:27-28 — a negative sample is clamped to 0 before
	// bucketing instead of landing in the first bucket with a negative sum.
	var h Histogram
	h.Observe(-5)
	h.Observe(-0.5)
	if h.Count != 2 {
		t.Fatalf("count = %d, want 2", h.Count)
	}
	if h.SumMS != 0 {
		t.Fatalf("sum = %v, want 0 (negative samples clamped)", h.SumMS)
	}
	if len(h.Buckets) != len(LatencyHistogramBoundsMS)+1 {
		t.Fatalf("buckets = %d, want %d", len(h.Buckets), len(LatencyHistogramBoundsMS)+1)
	}
	if h.Buckets[0] != 2 {
		t.Fatalf("first bucket = %d, want 2 (both clamped samples)", h.Buckets[0])
	}
}

func TestAgentERecordTTFTNonPositiveDropped(t *testing.T) {
	// manager.go:276-277 — a non-positive TTFT sample is dropped before
	// any state or histogram is created.
	m := New(3, time.Minute)
	m.RecordTTFT("dep", 0)
	m.RecordTTFT("dep", -time.Second)
	if _, _, ok := m.Histograms("dep"); ok {
		t.Fatal("non-positive TTFT samples created a histogram")
	}
	if st := m.Get("dep"); st.EWMATTFTMS != 0 {
		t.Fatalf("EWMATTFTMS = %v, want 0", st.EWMATTFTMS)
	}
}

func TestAgentERecordRecoverySuccessOnRetiredAndHealthyIsNoop(t *testing.T) {
	// manager.go:598-600 — a recovery success on a Retired or Healthy
	// deployment records nothing: no counters move, no status change.
	m := New(3, time.Minute)
	m.Retire("retired", "operator retired", "retired")
	m.RecordRecoverySuccess("retired", 10*time.Millisecond)
	st := m.Get("retired")
	if st.Status != Retired || st.Successes != 0 || st.RecoverySuccesses != 0 {
		t.Fatalf("retired recovery success mutated state: %+v", st)
	}

	m.RecordSuccess("healthy", 10*time.Millisecond)
	before := m.Get("healthy")
	m.RecordRecoverySuccess("healthy", 10*time.Millisecond)
	after := m.Get("healthy")
	if after.Successes != before.Successes || after.RecoverySuccesses != 0 {
		t.Fatalf("healthy recovery success mutated counters: before=%+v after=%+v", before, after)
	}
}

func TestAgentESetNextRetryOnRetiredIsNoop(t *testing.T) {
	// manager.go:624-626 — SetNextRetry on a Retired deployment stores
	// the state unchanged and leaves NextRetryAt unset.
	m := New(3, time.Minute)
	m.Retire("retired", "operator retired", "retired")
	m.SetNextRetry("retired", time.Now().Add(time.Hour))
	if st := m.Get("retired"); st.Status != Retired || !st.NextRetryAt.IsZero() {
		t.Fatalf("SetNextRetry mutated retired state: %+v", st)
	}
}

func TestAgentEApplySharedCooldownGuards(t *testing.T) {
	// manager.go:722-723 — an empty id or a zero deadline is rejected
	// without touching state.
	m := New(3, time.Minute)
	future := time.Now().Add(time.Hour)
	if m.ApplySharedCooldown("", future, "no-id") {
		t.Fatal("empty id accepted a shared cooldown")
	}
	if m.ApplySharedCooldown("dep", time.Time{}, "no-deadline") {
		t.Fatal("zero deadline accepted a shared cooldown")
	}
	if st := m.Get("dep"); st.Status == Cooldown {
		t.Fatalf("rejected shared cooldown mutated state: %+v", st)
	}
	// Positive control: a well-formed shared cooldown applies.
	if !m.ApplySharedCooldown("dep", future, "shared incident") {
		t.Fatal("well-formed shared cooldown was rejected")
	}
	if st := m.Get("dep"); st.Status != Cooldown {
		t.Fatalf("shared cooldown not applied: %+v", st)
	}
}

func TestAgentEScopeCooldownExpiryNormalizesOnRead(t *testing.T) {
	// manager.go:194-195 — stateNeedsNormalization returns true when a
	// scope (not the global state) sits in an expired cooldown, and the
	// read path normalizes that scope back to Unknown instead of leaving
	// the deployment blocked.
	clock := coverageHealthManagerNewClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	m := New(2, time.Minute)
	m.ConfigureAdvanced(2, time.Minute, 1, time.Minute)
	m.SetNowFunc(clock.Now)

	m.RecordScopeFailure("dep", []string{"s"}, "scope boom")
	st, ready := m.GetWithScopes("dep", []string{"s"})
	if ready {
		t.Fatalf("scope in active cooldown reported ready: %+v", st)
	}
	if st.Scopes["s"].Status != Cooldown {
		t.Fatalf("scope status = %q, want cooldown", st.Scopes["s"].Status)
	}

	clock.Advance(2 * time.Minute) // past the 1m scope cooldown
	st, ready = m.GetWithScopes("dep", []string{"s"})
	if !ready {
		t.Fatalf("expired scope cooldown still blocks readiness: %+v", st)
	}
	sc := st.Scopes["s"]
	if sc.Status != Unknown || !sc.CooldownUntil.IsZero() || sc.ConsecutiveFailures != 0 {
		t.Fatalf("expired scope not normalized: %+v", sc)
	}
}

func TestAgentEGetWithScopesStateDeletedBetweenLocks(t *testing.T) {
	// manager.go:229-232 — GetWithScopes re-checks the map under the
	// write lock after finding a state that needs normalization under the
	// read lock. The !ok branch fires only when another goroutine deletes
	// the state in the gap between RUnlock and Lock (Retain is the only
	// deleter of deployment states). A continuously spinning deleter plus
	// many fresh expired-scope-cooldown states makes the race land; the
	// call still returns the Unknown fallback either way, so coverage is
	// the witness for the branch itself.
	//
	// The gap is only a few instructions wide, so a bare spinner almost
	// never lands in it: the deleter must already be blocked in Lock()
	// (holding the writer mutex) when RUnlock fires, so that the fresh
	// Lock() queues behind it and the delete happens first. To make that
	// near-certain, the state carries hundreds of healthy filler scopes:
	// stateNeedsNormalization scans the scopes map in random order and
	// short-circuits on the first expired cooldown, so with one expired
	// scope among hundreds of healthy ones the RLock window stretches to
	// microseconds and the deleter is reliably pending when it closes.
	clock := coverageHealthManagerNewClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	m := New(2, time.Minute)
	m.ConfigureAdvanced(2, time.Minute, 1, time.Minute)
	m.SetNowFunc(clock.Now)

	filler := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		filler = append(filler, "filler-scope-"+strconv.Itoa(i))
	}

	stop := make(chan struct{})
	defer close(stop)
	started := make(chan struct{})
	var startOnce sync.Once
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				m.Retain(map[string]struct{}{})
				startOnce.Do(func() { close(started) })
			}
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("deleter goroutine never ran; race harness starved by scheduler load")
	}
	var raced int
	for i := 0; i < 1000; i++ {
		clock.Advance(2 * time.Minute)
		m.RecordScopeFailure("dep", []string{"s"}, "boom")
		m.RecordScopeSuccess("dep", filler)
		clock.Advance(2 * time.Minute) // expire the 1m scope cooldown
		st, ready := m.GetWithScopes("dep", []string{"s"})
		// Every outcome is a legitimate Unknown/ready fallback; the loop
		// exists to land the deleter inside the RUnlock->Lock gap.
		if st.Deployment != "dep" {
			t.Fatalf("wrong deployment in result: %+v", st)
		}
		if ready {
			raced++
		}
	}
	if raced == 0 {
		t.Fatal("no GetWithScopes call ever observed a ready deployment; race harness may be starved")
	}
}

func TestAgentEProviderAvailableStateDeletedBetweenLocks(t *testing.T) {
	// manager.go:397-399 — ProviderAvailable re-checks the provider map
	// under the write lock after finding an expired cooldown under the
	// read lock. The !ok branch fires only when another goroutine deletes
	// the provider in the gap between RUnlock and Lock
	// (InvalidateProvider). A continuously spinning deleter plus many
	// fresh expired-cooldown provider states makes the race land; every
	// path returns available=true here, so coverage is the witness for
	// the branch itself.
	clock := coverageHealthManagerNewClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	m := New(2, time.Minute)
	m.ConfigureProviderIncidents(2, time.Minute, 30*time.Second)
	m.SetNowFunc(clock.Now)

	stop := make(chan struct{})
	defer close(stop)
	started := make(chan struct{})
	var startOnce sync.Once
	// Several spinners raise the Lock-attempt rate inside the ~100ns
	// RLock window; a startup handshake proves the harness is actually
	// scheduled before the race loop begins (under heavy parallel-agent
	// CPU load a lone spinner can otherwise starve for the whole test).
	// 8 spinners x 30000 iterations: measured hit rate for the 397-399
	// branch was ~2/3 with 4 spinners x 3000 and 4/5 with 8 x 10000, so
	// the harness is deliberately oversized — the branch itself is pure
	// race hygiene with no behavioral difference.
	for g := 0; g < 8; g++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					m.InvalidateProvider("p")
					startOnce.Do(func() { close(started) })
				}
			}
		}()
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("deleter goroutine never ran; race harness starved by scheduler load")
	}
	for i := 0; i < 30000; i++ {
		clock.Advance(31 * time.Second)
		m.RecordProviderFailure("p", "dep-a", "boom")
		m.RecordProviderFailure("p", "dep-b", "boom")
		clock.Advance(31 * time.Second) // expire the 30s provider cooldown
		if !m.ProviderAvailable("p") {
			t.Fatalf("iteration %d: provider with expired cooldown reported unavailable", i)
		}
	}
}
