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
	m:=New(5,time.Hour);m.ConfigureAdvanced(5,time.Hour,2,time.Hour);m.RecordSuccess("p/m",time.Millisecond)
	m.RecordScopeFailure("p/m",[]string{"streaming"},"x");if !m.ScopesReady("p/m",[]string{"streaming"}){t.Fatal("opened early")}
	m.RecordScopeFailure("p/m",[]string{"streaming"},"x");if m.ScopesReady("p/m",[]string{"streaming"}){t.Fatal("scope should cool")}
	if st:=m.Get("p/m");st.Status!=Healthy{t.Fatalf("global poisoned %+v",st)};if !m.ScopesReady("p/m",[]string{"tools"}){t.Fatal("unrelated scope poisoned")}
}
