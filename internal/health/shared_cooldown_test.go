package health

import (
	"testing"
	"time"
)

func sharedTestManager(now time.Time) *Manager {
	m := New(3, time.Minute)
	m.SetNowFunc(func() time.Time { return now })
	return m
}

func TestApplySharedCooldownCreatesAndExtends(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	m := sharedTestManager(now)
	until := now.Add(5 * time.Minute)
	if !m.ApplySharedCooldown("dep", until, "shared cooldown") {
		t.Fatal("expected change for a new deployment")
	}
	s := m.Get("dep")
	if s.Status != Cooldown || !s.CooldownUntil.Equal(until) {
		t.Fatalf("state = %+v, want cooldown until %v", s, until)
	}
	if s.Failures != 0 || s.ConsecutiveFailures != 0 || s.EWMAFailureRate != 0 {
		t.Fatalf("shared application touched failure accounting: %+v", s)
	}
	// Same deadline again: no change.
	if m.ApplySharedCooldown("dep", until, "shared cooldown") {
		t.Fatal("re-applying the same deadline must be a no-op")
	}
	// Later deadline extends.
	later := now.Add(9 * time.Minute)
	if !m.ApplySharedCooldown("dep", later, "shared cooldown") {
		t.Fatal("expected extension")
	}
	if got := m.Get("dep").CooldownUntil; !got.Equal(later) {
		t.Fatalf("cooldown until = %v, want %v", got, later)
	}
}

func TestApplySharedCooldownNeverShortens(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	m := sharedTestManager(now)
	m.EnterCooldown("dep", "local", 20*time.Minute)
	before := m.Get("dep").CooldownUntil
	if m.ApplySharedCooldown("dep", now.Add(5*time.Minute), "shared cooldown") {
		t.Fatal("a shorter shared deadline must not shorten the local cooldown")
	}
	if got := m.Get("dep").CooldownUntil; !got.Equal(before) {
		t.Fatalf("cooldown until changed: %v -> %v", before, got)
	}
}

func TestApplySharedCooldownRetiredImmuneAndExpiredIgnored(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	m := sharedTestManager(now)
	m.Retire("dep", "local retire", "auth")
	if m.ApplySharedCooldown("dep", now.Add(10*time.Minute), "shared cooldown") {
		t.Fatal("shared marker must not modify a locally retired deployment")
	}
	if s := m.Get("dep"); s.Status != Retired {
		t.Fatalf("status = %v, want retired", s.Status)
	}
	if m.ApplySharedCooldown("other", now.Add(-time.Minute), "shared cooldown") {
		t.Fatal("an expired shared deadline must be ignored")
	}
	if s := m.Get("other"); s.Status == Cooldown {
		t.Fatalf("expired deadline created a cooldown: %+v", s)
	}
}
