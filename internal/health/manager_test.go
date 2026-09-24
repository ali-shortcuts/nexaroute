package health

import (
	"sync"
	"testing"
	"time"
)

func TestCooldownAfterThreshold(t *testing.T) {
	m := New(3, time.Hour)
	for i := 0; i < 3; i++ {
		m.RecordFailure("p/m", "x", time.Millisecond)
	}
	if s := m.Get("p/m"); s.Status != Cooldown {
		t.Fatalf("want cooldown, got %s", s.Status)
	}
}

func TestCooldownExpiryEntersHalfOpenAndFailureRecoolsImmediately(t *testing.T) {
	m := New(3, 5*time.Millisecond)
	for i := 0; i < 3; i++ {
		m.RecordFailure("p/m", "x", time.Millisecond)
	}
	time.Sleep(8 * time.Millisecond)
	if s := m.Get("p/m"); s.Status != HalfOpen {
		t.Fatalf("want half_open after cooldown, got %s", s.Status)
	}
	m.RecordFailure("p/m", "still bad", time.Millisecond)
	if s := m.Get("p/m"); s.Status != Cooldown {
		t.Fatalf("half-open failure should immediately re-cool, got %s", s.Status)
	}
}

func TestHalfOpenSuccessReturnsHealthy(t *testing.T) {
	m := New(1, 5*time.Millisecond)
	m.RecordFailure("p/m", "x", time.Millisecond)
	time.Sleep(8 * time.Millisecond)
	if s := m.Get("p/m"); s.Status != HalfOpen {
		t.Fatalf("want half_open, got %s", s.Status)
	}
	m.RecordSuccess("p/m", time.Millisecond)
	if s := m.Get("p/m"); s.Status != Healthy {
		t.Fatalf("want healthy, got %s", s.Status)
	}
}

func TestFiveFailuresTriggerOneHourCooldown(t *testing.T) {
	m := New(5, time.Hour)
	for i := 0; i < 4; i++ {
		m.RecordFailure("p/m", "x", time.Millisecond)
		if st := m.Get("p/m"); st.Status == Cooldown {
			t.Fatalf("cooldown triggered too early after %d failures", i+1)
		}
	}
	before := time.Now().Add(59 * time.Minute)
	after := time.Now().Add(61 * time.Minute)
	m.RecordFailure("p/m", "x", time.Millisecond)
	st := m.Get("p/m")
	if st.Status != Cooldown {
		t.Fatalf("want cooldown after fifth failure, got %s", st.Status)
	}
	if st.CooldownUntil.Before(before) || st.CooldownUntil.After(after) {
		t.Fatalf("unexpected cooldown deadline %v", st.CooldownUntil)
	}
}

func TestForceCooldownConcurrentWithConfigure(t *testing.T) {
	m := New(5, time.Second)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			m.Configure(1+(i%5), time.Duration(1+(i%3))*time.Second)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			m.ForceCooldown("p/m", "rate limited", 0)
		}
	}()
	wg.Wait()
	if st := m.Get("p/m"); st.Status != Cooldown {
		t.Fatalf("status=%s want cooldown", st.Status)
	}
}

func TestSnapshotClearsStaleErrorWhenCooldownBecomesHalfOpen(t *testing.T) {
	m := New(1, time.Millisecond)
	m.RecordFailure("p/m", "temporary failure", time.Millisecond)
	time.Sleep(3 * time.Millisecond)
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len=%d want 1", len(snap))
	}
	if snap[0].Status != HalfOpen || snap[0].LastError != "" {
		t.Fatalf("expired cooldown should be clean half-open state: %+v", snap[0])
	}
}

func TestQuarantineImmediatelyLeavesReadyState(t *testing.T) {
	m := New(5, 30*time.Minute)
	m.RecordSuccess("p/m", 10*time.Millisecond)
	m.Quarantine("p/m", "real request failed", 12*time.Millisecond)
	st := m.Get("p/m")
	if st.Status != Degraded {
		t.Fatalf("status=%s want degraded quarantine", st.Status)
	}
	if st.RecoveryFailures != 0 {
		t.Fatalf("new quarantine should start recovery budget at zero: %+v", st)
	}
}

func TestFiveRecoveryFailuresThenEnterCooldownKeepsExactCount(t *testing.T) {
	m := New(5, 30*time.Minute)
	m.Quarantine("p/m", "real request failed", time.Millisecond)
	for i := 0; i < 5; i++ {
		m.RecordRecoveryFailure("p/m", "probe failed", time.Millisecond)
	}
	m.EnterCooldown("p/m", "recovery exhausted", 30*time.Minute)
	st := m.Get("p/m")
	if st.Status != Cooldown || st.RecoveryFailures != 5 {
		t.Fatalf("unexpected cooldown state: %+v", st)
	}
}

func TestInvalidateAndRetainHealthProofs(t *testing.T) {
	m := New(5, 30*time.Minute)
	m.RecordSuccess("p/keep", time.Millisecond)
	m.RecordSuccess("p/change", time.Millisecond)
	m.RecordSuccess("p/remove", time.Millisecond)

	m.Invalidate("p/change")
	m.Retain(map[string]struct{}{"p/keep": {}, "p/change": {}})

	if st := m.Get("p/keep"); st.Status != Healthy {
		t.Fatalf("unchanged deployment lost health: %+v", st)
	}
	if st := m.Get("p/change"); st.Status != Unknown {
		t.Fatalf("changed deployment kept stale health proof: %+v", st)
	}
	for _, st := range m.Snapshot() {
		if st.Deployment == "p/remove" {
			t.Fatalf("removed deployment health state was retained: %+v", st)
		}
	}
}

func TestCapabilityCooldownDoesNotPoisonGlobalHealth(t *testing.T) {
	m := New(5, time.Hour)
	m.ConfigureAdvanced(5, time.Hour, 2, time.Hour)
	m.RecordSuccess("p/m", time.Millisecond)
	m.RecordScopeFailure("p/m", []string{"streaming"}, "x")
	if !m.ScopesReady("p/m", []string{"streaming"}) {
		t.Fatal("scope circuit opened too early")
	}
	m.RecordScopeFailure("p/m", []string{"streaming"}, "x")
	if m.ScopesReady("p/m", []string{"streaming"}) {
		t.Fatal("scope circuit should be cooling down")
	}
	if st := m.Get("p/m"); st.Status != Healthy {
		t.Fatalf("scope failure poisoned global health: %+v", st)
	}
	if !m.ScopesReady("p/m", []string{"tools"}) {
		t.Fatal("unrelated scope was poisoned")
	}
}

func TestSnapshotNormalizesExpiredCooldowns(t *testing.T) {
	m := New(1, 20*time.Millisecond)
	m.ForceCooldown("d", "x", 20*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len=%d want 1", len(snap))
	}
	if snap[0].Status != HalfOpen {
		t.Fatalf("expired cooldown status=%s want %s", snap[0].Status, HalfOpen)
	}
	if got := m.Get("d").Status; got != HalfOpen {
		t.Fatalf("normalized state not persisted: %s", got)
	}
}

func TestFailureEWMARecoversFromOldFailure(t *testing.T) {
	m := New(100, time.Hour)
	m.RecordFailure("p/m", "temporary", time.Millisecond)
	if got := m.Get("p/m").EWMAFailureRate; got < 0.99 {
		t.Fatalf("first failure EWMA=%f want near 1", got)
	}
	for i := 0; i < 20; i++ {
		m.RecordSuccess("p/m", time.Millisecond)
	}
	st := m.Get("p/m")
	if st.EWMAFailureRate >= 0.01 {
		t.Fatalf("old failure did not decay enough: %+v", st)
	}
	if st.Failures != 1 || st.Successes != 20 {
		t.Fatalf("lifetime counters must remain exact: %+v", st)
	}
}

func TestInFlightFailureDoesNotDowngradeActiveCooldown(t *testing.T) {
	m := New(3, time.Hour)
	m.ForceCooldown("d", "429", time.Hour)
	m.RecordFailure("d", "still failing", 10*time.Millisecond)
	st := m.Get("d")
	if st.Status != Cooldown {
		t.Fatalf("in-flight failure downgraded active cooldown: %s", st.Status)
	}
	if st.CooldownUntil.IsZero() || !st.CooldownUntil.After(time.Now()) {
		t.Fatal("active cooldown deadline was dropped")
	}
	if st.ConsecutiveFailures != 2 {
		t.Fatalf("consecutive failures=%d want 2", st.ConsecutiveFailures)
	}
}

func TestInFlightSuccessDoesNotReAdmitActiveCooldown(t *testing.T) {
	m := New(3, time.Hour)
	m.ForceCooldown("d", "429", time.Hour)
	m.RecordSuccess("d", 10*time.Millisecond)
	st := m.Get("d")
	if st.Status != Cooldown {
		t.Fatalf("stale success re-admitted a cooling deployment: %s", st.Status)
	}
	if st.CooldownUntil.IsZero() || !st.CooldownUntil.After(time.Now()) {
		t.Fatal("active cooldown deadline was cleared by a stale success")
	}
	if st.Successes != 1 {
		t.Fatalf("success counter=%d want 1", st.Successes)
	}
}

func TestHalfOpenStampedeStaysCooling(t *testing.T) {
	m := New(3, time.Hour)
	m.ForceCooldown("d", "boom", 5*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	// Deadline passed: both concurrent observations see HalfOpen.
	m.RecordFailure("d", "a", 0)
	m.RecordFailure("d", "b", 0)
	st := m.Get("d")
	if st.Status != Cooldown {
		t.Fatalf("half-open stampede left status=%s want cooldown", st.Status)
	}
	if st.CooldownUntil.IsZero() || !st.CooldownUntil.After(time.Now()) {
		t.Fatal("half-open stampede did not re-arm the cooldown deadline")
	}
}

func TestSuccessAfterExpiredCooldownRecovers(t *testing.T) {
	m := New(3, time.Hour)
	m.ForceCooldown("d", "429", 5*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	m.RecordSuccess("d", 10*time.Millisecond)
	st := m.Get("d")
	if st.Status != Healthy {
		t.Fatalf("post-deadline success status=%s want healthy", st.Status)
	}
}

func TestDegradedFailureClearsStaleCooldownDeadline(t *testing.T) {
	m := New(10, time.Hour)
	m.ForceCooldown("d", "429", 5*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	// State normalizes to HalfOpen on Get; force a Degraded path via low threshold.
	m2 := New(10, time.Hour)
	m2.EnterCooldown("e", "boom", 5*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	m2.RecordFailure("e", "slow", 0)
	st := m2.Get("e")
	if st.Status != Degraded {
		t.Fatalf("status=%s want degraded", st.Status)
	}
	if !st.CooldownUntil.IsZero() {
		t.Fatalf("degraded state kept stale cooldown_until=%v", st.CooldownUntil)
	}
	_ = m
}

func TestProviderIncidentRequiresDistinctDeployments(t *testing.T) {
	m := New(5, time.Hour)
	m.ConfigureProviderIncidents(3, time.Second, time.Minute)
	m.RecordProviderFailure("p", "p/a", "transport")
	m.RecordProviderFailure("p", "p/a", "transport again")
	if !m.ProviderAvailable("p") {
		t.Fatal("one failing deployment must not open provider circuit")
	}
	m.RecordProviderFailure("p", "p/b", "timeout")
	if !m.ProviderAvailable("p") {
		t.Fatal("two distinct failures must stay below threshold 3")
	}
	m.RecordProviderFailure("p", "p/c", "503")
	if m.ProviderAvailable("p") {
		t.Fatal("three distinct failures should open provider circuit")
	}
	snap := m.ProviderSnapshot()
	if len(snap) != 1 || snap[0].Status != Cooldown || snap[0].Evidence != 3 {
		t.Fatalf("unexpected provider state: %#v", snap)
	}
}

func TestProviderIncidentHalfOpenFailureRecoolsImmediately(t *testing.T) {
	m := New(5, time.Hour)
	m.ConfigureProviderIncidents(2, time.Second, 5*time.Millisecond)
	m.RecordProviderFailure("p", "p/a", "x")
	m.RecordProviderFailure("p", "p/b", "x")
	if m.ProviderAvailable("p") {
		t.Fatal("provider should be cooling")
	}
	time.Sleep(8 * time.Millisecond)
	if !m.ProviderAvailable("p") {
		t.Fatal("expired provider cooldown should enter half-open")
	}
	m.RecordProviderFailure("p", "p/a", "still bad")
	if m.ProviderAvailable("p") {
		t.Fatal("half-open provider failure should immediately re-open circuit")
	}
}

func TestProviderInFlightObservationsDoNotBreakActiveCooldown(t *testing.T) {
	m := New(5, time.Hour)
	m.ConfigureProviderIncidents(2, time.Second, time.Minute)
	m.RecordProviderFailure("p", "p/a", "x")
	m.RecordProviderFailure("p", "p/b", "x")
	before := m.ProviderSnapshot()[0].CooldownUntil
	m.RecordProviderSuccess("p")
	m.RecordProviderFailure("p", "p/a", "late failure")
	st := m.ProviderSnapshot()[0]
	if st.Status != Cooldown {
		t.Fatalf("in-flight observation changed active provider cooldown: %+v", st)
	}
	if st.CooldownUntil != before {
		t.Fatalf("provider cooldown deadline changed: before=%v after=%v", before, st.CooldownUntil)
	}
}

func TestRecordTTFTUsesEWMA(t *testing.T) {
	m := New(5, time.Hour)
	m.RecordTTFT("p/m", 100*time.Millisecond)
	m.RecordTTFT("p/m", 300*time.Millisecond)
	if got := m.Get("p/m").EWMATTFTMS; got < 149 || got > 151 {
		t.Fatalf("ttft ewma=%f want about 150", got)
	}
}

func TestInvalidateProviderClearsIncidentEvidence(t *testing.T) {
	m := New(5, time.Hour)
	m.ConfigureProviderIncidents(2, time.Second, time.Minute)
	m.RecordProviderFailure("p", "p/a", "x")
	m.RecordProviderFailure("p", "p/b", "x")
	if m.ProviderAvailable("p") {
		t.Fatal("fixture provider circuit did not open")
	}
	m.InvalidateProvider("p")
	if !m.ProviderAvailable("p") {
		t.Fatal("invalidated provider remained unavailable")
	}
	if got := m.ProviderSnapshot(); len(got) != 0 {
		t.Fatalf("invalidated provider state remained: %#v", got)
	}
}
