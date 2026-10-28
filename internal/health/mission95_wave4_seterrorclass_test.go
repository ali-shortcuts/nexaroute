package health

import (
	"testing"
	"time"
)

func wave4HealthSnapshot(t *testing.T, m *Manager, id string) (State, bool) {
	t.Helper()
	for _, s := range m.Snapshot() {
		if s.Deployment == id {
			return s, true
		}
	}
	return State{}, false
}

// SetErrorClass on a fresh id records the class without touching counters
// or status: it is a classification-only write, not a failure record.
func TestWave4SetErrorClassFreshID(t *testing.T) {
	m := New(3, time.Hour)
	m.SetErrorClass("dep-a", "timeout")
	s, ok := wave4HealthSnapshot(t, m, "dep-a")
	if !ok {
		t.Fatal("expected a state entry for dep-a")
	}
	if s.LastErrorClass != "timeout" {
		t.Fatalf("LastErrorClass = %q, want timeout", s.LastErrorClass)
	}
	if s.Status != "" {
		t.Fatalf("Status = %q, want empty (classification must not set a status)", s.Status)
	}
	if s.Failures != 0 || s.ConsecutiveFailures != 0 || s.Successes != 0 {
		t.Fatalf("counters changed by classification: %+v", s)
	}
}

// SetErrorClass preserves existing counters and status while updating the class.
func TestWave4SetErrorClassPreservesExistingState(t *testing.T) {
	m := New(3, time.Hour)
	m.RecordSuccess("dep-b", 10*time.Millisecond)
	m.RecordFailure("dep-b", "boom", 5*time.Millisecond)
	m.SetErrorClass("dep-b", "auth")
	s, ok := wave4HealthSnapshot(t, m, "dep-b")
	if !ok {
		t.Fatal("expected a state entry for dep-b")
	}
	if s.LastErrorClass != "auth" {
		t.Fatalf("LastErrorClass = %q, want auth", s.LastErrorClass)
	}
	if s.Successes != 1 || s.Failures != 1 || s.ConsecutiveFailures != 1 {
		t.Fatalf("counters changed by classification: successes=%d failures=%d consecutive=%d", s.Successes, s.Failures, s.ConsecutiveFailures)
	}
	if s.LastError != "boom" {
		t.Fatalf("LastError = %q, want boom (classification must not overwrite the error text)", s.LastError)
	}
}

// An empty class is a no-op: no entry is created and existing state is untouched.
func TestWave4SetErrorClassEmptyIsNoop(t *testing.T) {
	m := New(3, time.Hour)
	m.SetErrorClass("dep-c", "")
	if _, ok := wave4HealthSnapshot(t, m, "dep-c"); ok {
		t.Fatal("empty class must not create a state entry")
	}
	m.RecordSuccess("dep-d", time.Millisecond)
	m.SetErrorClass("dep-d", "")
	s, ok := wave4HealthSnapshot(t, m, "dep-d")
	if !ok {
		t.Fatal("expected a state entry for dep-d")
	}
	if s.LastErrorClass != "" {
		t.Fatalf("LastErrorClass = %q, want empty after empty-class call", s.LastErrorClass)
	}
}

// SetErrorClass on a retired deployment leaves the retired record untouched,
// including the class recorded at retirement time.
func TestWave4SetErrorClassRetiredIsNoop(t *testing.T) {
	m := New(3, time.Hour)
	m.Retire("dep-e", "decommissioned", "retired-class")
	m.SetErrorClass("dep-e", "timeout")
	s, ok := wave4HealthSnapshot(t, m, "dep-e")
	if !ok {
		t.Fatal("expected a state entry for dep-e")
	}
	if s.Status != Retired {
		t.Fatalf("Status = %q, want retired", s.Status)
	}
	if s.LastErrorClass != "retired-class" {
		t.Fatalf("LastErrorClass = %q, want retired-class (retired state must not be overwritten)", s.LastErrorClass)
	}
}

// A second classification overwrites the first; the latest class wins.
func TestWave4SetErrorClassOverwrite(t *testing.T) {
	m := New(3, time.Hour)
	m.SetErrorClass("dep-f", "timeout")
	m.SetErrorClass("dep-f", "rate_limited")
	s, ok := wave4HealthSnapshot(t, m, "dep-f")
	if !ok {
		t.Fatal("expected a state entry for dep-f")
	}
	if s.LastErrorClass != "rate_limited" {
		t.Fatalf("LastErrorClass = %q, want rate_limited", s.LastErrorClass)
	}
}
