package health

import (
	"testing"
	"time"
)

// TestRecordRecoverySuccessDoesNotReAdmit pins the threshold-recovery contract:
// partial probe successes accumulate evidence but must never make a
// quarantined deployment Healthy until RecordSuccess is called.
func TestRecordRecoverySuccessDoesNotReAdmit(t *testing.T) {
	m := New(3, time.Minute)
	m.Quarantine("p/m", "probe failed", time.Millisecond)

	m.RecordRecoverySuccess("p/m", 10*time.Millisecond)
	m.RecordRecoverySuccess("p/m", 11*time.Millisecond)
	st := m.Get("p/m")
	if st.Status == Healthy {
		t.Fatalf("partial recovery admitted the deployment: %+v", st)
	}
	if st.RecoverySuccesses != 2 {
		t.Fatalf("recovery successes=%d want 2", st.RecoverySuccesses)
	}
	if st.LastProbeDurationMS != 11 {
		t.Fatalf("last probe duration=%d want 11", st.LastProbeDurationMS)
	}

	m.RecordSuccess("p/m", 12*time.Millisecond)
	st = m.Get("p/m")
	if st.Status != Healthy {
		t.Fatalf("RecordSuccess did not re-admit: %+v", st)
	}
	if st.RecoverySuccesses != 0 || st.RecoveryFailures != 0 {
		t.Fatalf("re-admission must clear recovery counters: %+v", st)
	}
}

// TestRecordProbeFailurePreservesPartialSuccessesAndClass pins that a
// transient failure inside a threshold cycle keeps accumulated successes and
// records the classification.
func TestRecordProbeFailurePreservesPartialSuccessesAndClass(t *testing.T) {
	m := New(3, time.Minute)
	m.Quarantine("p/m", "probe failed", time.Millisecond)
	m.RecordRecoverySuccess("p/m", 10*time.Millisecond)

	m.RecordProbeFailure("p/m", "invalid key", "credential_failure", 20*time.Millisecond)
	st := m.Get("p/m")
	if st.RecoverySuccesses != 1 {
		t.Fatalf("failure reset accumulated successes: %+v", st)
	}
	if st.RecoveryFailures != 1 {
		t.Fatalf("recovery failures=%d want 1", st.RecoveryFailures)
	}
	if st.LastErrorClass != "credential_failure" {
		t.Fatalf("last_error_class=%q want credential_failure", st.LastErrorClass)
	}
}

// TestSetNextRetryIsObservabilityOnly pins that the retry hint never changes
// status and is cleared on re-admission.
func TestSetNextRetryIsObservabilityOnly(t *testing.T) {
	now := time.Now()
	m := New(3, time.Minute)
	m.SetNowFunc(func() time.Time { return now })

	m.Quarantine("p/m", "probe failed", time.Millisecond)
	m.EnterCooldown("p/m", "cycle exhausted", time.Minute)
	until := now.Add(30 * time.Second)
	m.SetNextRetry("p/m", until)

	st := m.Get("p/m")
	if st.Status != Cooldown {
		t.Fatalf("status=%s want cooldown", st.Status)
	}
	if !st.NextRetryAt.Equal(until) {
		t.Fatalf("next retry=%s want %s", st.NextRetryAt, until)
	}

	// Expire the cooldown, then re-admit; the retry hint must be cleared.
	now = now.Add(2 * time.Minute)
	m.RecordSuccess("p/m", time.Millisecond)
	if st := m.Get("p/m"); !st.NextRetryAt.IsZero() {
		t.Fatalf("re-admission did not clear next retry: %s", st.NextRetryAt)
	}
}
