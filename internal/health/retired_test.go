package health

import (
	"testing"
	"time"
)

func TestRetiredDeploymentIsStickyUntilIdentityInvalidation(t *testing.T) {
	m := New(3, time.Hour)
	m.Retire("p/m", "upstream EOL", "model_retired")
	st := m.Get("p/m")
	if st.Status != Retired || st.LastErrorClass != "model_retired" {
		t.Fatalf("retire state=%+v", st)
	}

	m.RecordSuccess("p/m", time.Millisecond)
	m.Quarantine("p/m", "late failure", time.Millisecond)
	m.ForceCooldown("p/m", "late 429", time.Minute)
	m.RecordFailure("p/m", "late 5xx", time.Millisecond)
	if st = m.Get("p/m"); st.Status != Retired {
		t.Fatalf("stale observations revived retired deployment: %+v", st)
	}

	m.Invalidate("p/m")
	if st = m.Get("p/m"); st.Status != Unknown {
		t.Fatalf("identity invalidation must clear retirement: %+v", st)
	}
}
