package autopilot

import "testing"

// #418: ObserveNoEligible attributes a leaseless
// no-eligible outcome to the policy version the request
// would have leased to, and the observations alone can trip
// the automatic rollback.
func TestObserveNoEligibleAttributionAndTrip(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 5, MinPromoteSamples: 1000, MaxErrorRate: 1.0, MaxNoEligibleRate: 0.5})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:bad"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	for i := 0; i < 5; i++ {
		c.ObserveNoEligible("req-key", "")
		if i < 4 && c.Status().State != StateCanary {
			t.Fatalf("after %d observations state = %s, want canary", i+1, c.Status().State)
		}
	}
	if st := c.Status(); st.State != StateRolledBack {
		t.Fatalf("state = %s, want rolled_back after 5 no-eligible observations", st.State)
	}
	// After the rollback the same call attributes to the
	// incumbent window.
	c.ObserveNoEligible("req-key", "")
	if got := c.Status().IncumbentSignals.NoEligibleObserved; got != 1 {
		t.Fatalf("incumbent no-eligible observed = %d, want 1", got)
	}
}
