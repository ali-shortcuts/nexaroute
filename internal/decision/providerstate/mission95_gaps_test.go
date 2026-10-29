package providerstate

import (
	"testing"
	"time"
)

func TestMission95NewAppliesDefaults(t *testing.T) {
	m := New(Config{}, nil)
	if got := m.Config(); got.FailureThreshold != 3 || got.FailureWindow != 30*time.Second || got.Cooldown != 60*time.Second {
		t.Fatalf("defaults = %+v", got)
	}
	// A zero-config manager still records and recovers normally.
	m.RecordSuccess("p")
	st, ok := m.SnapshotOne("p")
	if !ok || st.Successes != 1 || st.Status != "healthy" {
		t.Fatalf("success state = %+v ok=%v", st, ok)
	}
	if !m.CooldownUntil("missing").IsZero() {
		t.Fatalf("CooldownUntil(missing) should be zero")
	}
	if _, ok := m.SnapshotOne("missing"); ok {
		t.Fatalf("SnapshotOne(missing) should report absent")
	}
}

func TestMission95UpdateConfigAppliesDefaults(t *testing.T) {
	m := New(DefaultConfig(), nil)
	m.UpdateConfig(Config{})
	if got := m.Config(); got.FailureThreshold != 3 || got.FailureWindow != 30*time.Second || got.Cooldown != 60*time.Second {
		t.Fatalf("updated defaults = %+v", got)
	}
	// Partial config: only the unset fields fall back.
	m.UpdateConfig(Config{FailureThreshold: 2})
	if got := m.Config(); got.FailureThreshold != 2 || got.Cooldown != 60*time.Second {
		t.Fatalf("partial update = %+v", got)
	}
}

func TestMission95EmptyStatusRecoveryPaths(t *testing.T) {
	// getStateLocked always seeds "healthy", so the empty-status guards are
	// exercised by injecting a bare state directly (same-package test).
	m := New(DefaultConfig(), nil)
	m.states["bare"] = &State{}
	m.RecordSuccess("bare")
	if st := m.states["bare"]; st.Status != "healthy" {
		t.Fatalf("RecordSuccess on bare state: status %q", st.Status)
	}
	m.states["bare2"] = &State{}
	m.RecordFailure("bare2")
	if st := m.states["bare2"]; st.Status != "healthy" {
		t.Fatalf("RecordFailure on bare state: status %q", st.Status)
	}
	// CooldownUntil for a known provider in cooldown returns the expiry,
	// and IsCooldown clears it lazily once the clock passes it.
	now := time.Now()
	clock := now
	m2 := New(Config{FailureThreshold: 1, FailureWindow: time.Minute, Cooldown: time.Minute}, func() time.Time { return clock })
	m2.RecordFailure("c")
	if until := m2.CooldownUntil("c"); until.IsZero() || !until.Equal(now.Add(time.Minute)) {
		t.Fatalf("CooldownUntil = %v", until)
	}
	if !m2.IsCooldown("c") {
		t.Fatalf("should be in cooldown")
	}
	clock = clock.Add(2 * time.Minute)
	if m2.IsCooldown("c") {
		t.Fatalf("should have expired")
	}
	if st, _ := m2.SnapshotOne("c"); st.Status != "healthy" || !st.CooldownUntil.IsZero() {
		t.Fatalf("after expiry: %+v", st)
	}
	// IsCooldown on an unknown provider is false and does not create state.
	if m2.IsCooldown("ghost") {
		t.Fatalf("ghost should not be in cooldown")
	}
	if _, ok := m2.SnapshotOne("ghost"); ok {
		t.Fatalf("IsCooldown must not create state")
	}
}

func TestMission95SnapshotOneHidesFailTimes(t *testing.T) {
	m := New(DefaultConfig(), nil)
	m.RecordFailure("p")
	m.RecordFailure("p")
	st, ok := m.SnapshotOne("p")
	if !ok {
		t.Fatalf("missing state")
	}
	if st.FailuresInWindow != 2 || st.ConsecutiveFailures != 2 {
		t.Fatalf("counts: %+v", st)
	}
	if st.failTimes != nil {
		t.Fatalf("failTimes must be hidden")
	}
	snap := m.Snapshot()
	if snap["p"].failTimes != nil {
		t.Fatalf("Snapshot must hide failTimes")
	}
}
