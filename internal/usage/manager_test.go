package usage

import "testing"

func TestRecordExactUsageAndConfiguredCost(t *testing.T) {
	m := New()
	p := &Pricing{InputUSDPerMillion: 2.5, OutputUSDPerMillion: 10}
	m.Record("p/m", "p", Sample{InputTokens: 1000, OutputTokens: 500, CacheReadInputTokens: 250, ReasoningTokens: 50}, p)
	st := m.Snapshot()[0]
	if st.ExactRequests != 1 || st.InputTokens != 1000 || st.OutputTokens != 500 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	// $0.0025 input + $0.005 output = $0.0075.
	if st.EstimatedCostNanoUSD != 7_500_000 || st.PricedRequests != 1 {
		t.Fatalf("unexpected cost: %+v", st)
	}
}

func TestUnknownUsageDoesNotInventTokensOrCost(t *testing.T) {
	m := New()
	m.RecordUnknown("p/m", "p")
	st := m.Snapshot()[0]
	if st.UnknownRequests != 1 || st.ExactRequests != 0 || st.EstimatedCostNanoUSD != 0 {
		t.Fatalf("unexpected unknown stats: %+v", st)
	}
}

func TestUnpricedExactUsageRemainsExact(t *testing.T) {
	m := New()
	m.Record("p/m", "p", Sample{InputTokens: 12, OutputTokens: 4}, nil)
	st := m.Snapshot()[0]
	if st.ExactRequests != 1 || st.PricedRequests != 0 || st.EstimatedCostNanoUSD != 0 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}

func TestRetainBoundsRuntimeKeys(t *testing.T) {
	m := New()
	m.Record("keep/m", "keep", Sample{InputTokens: 1}, nil)
	m.Record("drop/m", "drop", Sample{InputTokens: 1}, nil)
	m.Retain(map[string]struct{}{"keep/m": {}}, map[string]struct{}{"keep": {}})
	if got := m.Snapshot(); len(got) != 1 || got[0].Deployment != "keep/m" {
		t.Fatalf("unexpected deployment snapshot: %+v", got)
	}
	if got := m.ProviderSnapshot(); len(got) != 1 || got[0].Provider != "keep" {
		t.Fatalf("unexpected provider snapshot: %+v", got)
	}
}
