package autopilot

import (
	"fmt"
	"testing"
)

// #418 stratification: samples carry a privacy-safe caller
// label; the window snapshot breaks the evidence down per
// stratum, bounded — labels past the cap fold into "other".
func TestWindowStrataBreakdownAndCap(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 1, MinPromoteSamples: 1000, MaxErrorRate: 1})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:cand"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	record := func(stratum string, success bool) {
		l := c.Select("k")
		s := Sample{Success: success, Stratum: stratum, HasNoEligibleSignal: true, HasBudgetAnomalySignal: true}
		if !success {
			s.ErrorClass = ClassServerError
		}
		c.Record(l, s)
	}
	record("vk-a", true)
	record("vk-a", false)
	record("vk-b", true)
	sig := c.Status().CandidateSignals
	if sig.Requests != 3 {
		t.Fatalf("aggregate requests = %d, want 3", sig.Requests)
	}
	byLabel := map[string]Signals{}
	for _, st := range sig.Strata {
		byLabel[st.Stratum] = st.Signals
	}
	if byLabel["vk-a"].Requests != 2 || byLabel["vk-a"].Errors != 1 {
		t.Fatalf("vk-a stratum = %+v, want 2 requests / 1 error", byLabel["vk-a"])
	}
	if byLabel["vk-b"].Requests != 1 || byLabel["vk-b"].Successes != 1 {
		t.Fatalf("vk-b stratum = %+v, want 1 successful request", byLabel["vk-b"])
	}

	// Cap: 20 further distinct labels fold so the breakdown
	// never exceeds maxStrata entries plus the fold bucket.
	c2 := newTestController(GateConfig{MinSamples: 1, MinPromoteSamples: 1000, MaxErrorRate: 1})
	if err := c2.StartCanary(PolicyRef{ID: "balanced", Version: "auto:cand"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	for i := 0; i < 20; i++ {
		l := c2.Select("k")
		c2.Record(l, Sample{Success: true, Stratum: fmt.Sprintf("vk-%02d", i), HasNoEligibleSignal: true, HasBudgetAnomalySignal: true})
	}
	strata := c2.Status().CandidateSignals.Strata
	if len(strata) > maxStrata+1 {
		t.Fatalf("strata entries = %d, want <= %d (cap + other)", len(strata), maxStrata+1)
	}
	var other *StratumSignals
	for i := range strata {
		if strata[i].Stratum == "other" {
			other = &strata[i]
		}
	}
	if other == nil || other.Signals.Requests != 4 {
		t.Fatalf("other stratum = %+v, want 4 folded requests", other)
	}
}
