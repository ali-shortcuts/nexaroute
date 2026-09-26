package providerstate

import (
	"sync"
	"testing"
	"time"
)

func TestManager_InitialState(t *testing.T) {
	m := New(DefaultConfig(), nil)
	if m.IsCooldown("jev-main") {
		t.Fatalf("initial should not be cooldown")
	}
	snap := m.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("initial snapshot should be empty")
	}
	_, ok := m.SnapshotOne("missing")
	if ok {
		t.Fatalf("missing should not exist")
	}
}

func TestManager_RecordSuccess(t *testing.T) {
	clock := time.Now()
	m := New(Config{FailureThreshold: 3, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, func() time.Time { return clock })
	m.RecordSuccess("jev-main")
	if m.IsCooldown("jev-main") {
		t.Fatalf("success should not be cooldown")
	}
	s, ok := m.SnapshotOne("jev-main")
	if !ok || s.Successes != 1 || s.ConsecutiveFailures != 0 {
		t.Fatalf("success snapshot mismatch: %+v", s)
	}
}

func TestManager_RecordFailureThreshold(t *testing.T) {
	now := time.Now()
	cur := now
	m := New(Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, func() time.Time { return cur })
	m.RecordFailure("jev-main")
	if m.IsCooldown("jev-main") {
		t.Fatalf("1 failure should not open cooldown for threshold 2")
	}
	cur = cur.Add(1 * time.Second)
	m.RecordFailure("jev-main")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("2 failures should open cooldown")
	}
	s, _ := m.SnapshotOne("jev-main")
	if s.ConsecutiveFailures != 2 || s.FailuresInWindow != 2 {
		t.Fatalf("fail counts mismatch: %+v", s)
	}
}

func TestManager_FailureWindowPruning(t *testing.T) {
	now := time.Now()
	cur := now
	m := New(Config{FailureThreshold: 3, FailureWindow: 10 * time.Second, Cooldown: 60 * time.Second}, func() time.Time { return cur })
	// 2 failures at t0
	m.RecordFailure("jev-main")
	m.RecordFailure("jev-main")
	cur = cur.Add(5 * time.Second)
	m.RecordFailure("jev-main") // 3 within window -> cooldown
	if !m.IsCooldown("jev-main") {
		t.Fatalf("should be cooldown")
	}
	// advance beyond window: failures outside window pruned, but cooldown still active until expiry
	cur = cur.Add(15 * time.Second)
	// still cooldown until 60s from last failure (which was at now+5)
	if !m.IsCooldown("jev-main") {
		t.Fatalf("cooldown should still be active")
	}
	// advance beyond cooldown
	cur = cur.Add(60 * time.Second)
	if m.IsCooldown("jev-main") {
		t.Fatalf("cooldown should have expired")
	}
	// new failures after window should count only remaining (should be pruned)
	m.RecordFailure("jev-main")
	s, _ := m.SnapshotOne("jev-main")
	if s.FailuresInWindow != 1 {
		t.Fatalf("after expiry, window should be 1 got %d", s.FailuresInWindow)
	}
}

func TestManager_CooldownExpiryWithoutSleep(t *testing.T) {
	now := time.Now()
	cur := now
	clock := func() time.Time { return cur }
	m := New(Config{FailureThreshold: 1, FailureWindow: 30 * time.Second, Cooldown: 5 * time.Second}, clock)
	m.RecordFailure("jev-main")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("should be cooldown")
	}
	until := m.CooldownUntil("jev-main")
	if until.IsZero() || !until.After(now) {
		t.Fatalf("cooldown until not set")
	}
	cur = now.Add(6 * time.Second)
	if m.IsCooldown("jev-main") {
		t.Fatalf("should have expired")
	}
	// Snapshot should show healthy after expiry lazily
	s, _ := m.SnapshotOne("jev-main")
	if s.Status != "healthy" {
		t.Fatalf("status should revert to healthy after expiry, got %s", s.Status)
	}
}

func TestManager_RecoveryViaSuccess(t *testing.T) {
	now := time.Now()
	cur := now
	m := New(Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, func() time.Time { return cur })
	m.RecordFailure("jev-main")
	m.RecordFailure("jev-main")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("cooldown expected")
	}
	m.RecordSuccess("jev-main")
	if m.IsCooldown("jev-main") {
		t.Fatalf("success should clear cooldown")
	}
	s, _ := m.SnapshotOne("jev-main")
	if s.ConsecutiveFailures != 0 || s.FailuresInWindow != 0 {
		t.Fatalf("success should reset failures: %+v", s)
	}
}

func TestManager_AbstainHealthy(t *testing.T) {
	m := New(Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	// ABSTAIN modeled via RecordSuccess should remain healthy and not increment failures
	for i := 0; i < 5; i++ {
		m.RecordSuccess("jev-main")
	}
	if m.IsCooldown("jev-main") {
		t.Fatalf("abstain (success) should not open cooldown")
	}
	s, _ := m.SnapshotOne("jev-main")
	if s.Successes != 5 {
		t.Fatalf("successes should be 5 got %d", s.Successes)
	}
	if s.ConsecutiveFailures != 0 {
		t.Fatalf("failures should be 0")
	}
}

func TestManager_SuccessResetsFailureState(t *testing.T) {
	cur := time.Now()
	m := New(Config{FailureThreshold: 3, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, func() time.Time { return cur })
	m.RecordFailure("jev-main")
	m.RecordFailure("jev-main")
	s, _ := m.SnapshotOne("jev-main")
	if s.ConsecutiveFailures != 2 {
		t.Fatalf("expected 2")
	}
	m.RecordSuccess("jev-main")
	s2, _ := m.SnapshotOne("jev-main")
	if s2.ConsecutiveFailures != 0 || s2.FailuresInWindow != 0 {
		t.Fatalf("reset failed: %+v", s2)
	}
}

func TestManager_IsolatedProviderIDs(t *testing.T) {
	m := New(Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	m.RecordFailure("jev-main")
	m.RecordFailure("jev-main")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("jev-main should be cooldown")
	}
	if m.IsCooldown("jev-backup") {
		t.Fatalf("jev-backup should not be affected")
	}
	m.RecordSuccess("jev-backup")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("jev-main still cooldown")
	}
}

func TestManager_ConcurrentRecord(t *testing.T) {
	m := New(Config{FailureThreshold: 1000, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.RecordSuccess("jev-main")
		}()
		go func() {
			defer wg.Done()
			m.RecordFailure("jev-main")
		}()
	}
	wg.Wait()
	// should not panic or race (run with -race)
	s, _ := m.SnapshotOne("jev-main")
	_ = s
}

func TestManager_UpdateConfigPreservesState(t *testing.T) {
	now := time.Now()
	cur := now
	m := New(Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, func() time.Time { return cur })
	m.RecordFailure("jev-main")
	m.RecordFailure("jev-main")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("cooldown")
	}
	// Update to higher threshold; state should remain (cooldown still active until expiry)
	m.UpdateConfig(Config{FailureThreshold: 5, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second})
	if !m.IsCooldown("jev-main") {
		t.Fatalf("update should preserve cooldown")
	}
	// Update cooldown shorter
	m.UpdateConfig(Config{FailureThreshold: 5, FailureWindow: 30 * time.Second, Cooldown: 5 * time.Second})
	// cooldown until still original 60s from first config, so still active for now+0
	if !m.IsCooldown("jev-main") {
		t.Fatalf("still cooldown until original expiry")
	}
	cur = cur.Add(6 * time.Second)
	if !m.IsCooldown("jev-main") {
		t.Fatalf("after 6s still should be cooldown (original 60s not shortened by UpdateConfig)")
	}
	// advance beyond original 60s
	cur = cur.Add(55 * time.Second)
	if m.IsCooldown("jev-main") {
		t.Fatalf("after 61s should have expired")
	}
}

func TestManager_Reset(t *testing.T) {
	m := New(DefaultConfig(), nil)
	m.RecordFailure("jev-main")
	m.RecordSuccess("jev-backup")
	if _, ok := m.SnapshotOne("jev-main"); !ok {
		t.Fatalf("missing")
	}
	m.Reset("jev-main")
	if _, ok := m.SnapshotOne("jev-main"); ok {
		t.Fatalf("should be deleted")
	}
	if _, ok := m.SnapshotOne("jev-backup"); !ok {
		t.Fatalf("other provider should remain")
	}
}

func TestManager_CooldownShorterThanWindow(t *testing.T) {
	// window 30s, cooldown 5s: after cooldown expiry, failures in window may still be present but not cause immediate re-cooldown
	now := time.Now()
	cur := now
	m := New(Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 5 * time.Second}, func() time.Time { return cur })
	m.RecordFailure("jev-main")
	cur = cur.Add(1 * time.Second)
	m.RecordFailure("jev-main")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("cooldown")
	}
	cur = cur.Add(6 * time.Second) // cooldown expired, but both failures still within 30s window
	if m.IsCooldown("jev-main") {
		t.Fatalf("should have expired")
	}
	s, _ := m.SnapshotOne("jev-main")
	if s.FailuresInWindow != 2 {
		t.Fatalf("window should still have 2, got %d", s.FailuresInWindow)
	}
	// Next failure should immediately re-enter cooldown because window still has 2 + 1 =3 >=2
	cur = cur.Add(1 * time.Second)
	m.RecordFailure("jev-main")
	if !m.IsCooldown("jev-main") {
		t.Fatalf("should re-enter cooldown immediately")
	}
	// Advance beyond window
	cur = cur.Add(31 * time.Second)
	if m.IsCooldown("jev-main") {
		t.Fatalf("should have expired again")
	}
	// failures pruned?
	m.RecordFailure("jev-main") // only 1 in window now
	if m.IsCooldown("jev-main") {
		t.Fatalf("single failure should not open")
	}
}

func BenchmarkManager_IsCooldown(b *testing.B) {
	m := New(DefaultConfig(), nil)
	m.RecordFailure("jev-main")
	m.RecordFailure("jev-main")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.IsCooldown("jev-main")
	}
}

func BenchmarkManager_RecordSuccess(b *testing.B) {
	m := New(DefaultConfig(), nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.RecordSuccess("jev-main")
	}
}

func BenchmarkManager_RecordFailure(b *testing.B) {
	m := New(Config{FailureThreshold: 1000, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.RecordFailure("jev-main")
	}
}
