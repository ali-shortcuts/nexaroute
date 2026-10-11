package decision

import "testing"

func TestCoverageBehaviorRegistryListSnapshotAndResolve(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("local"); !ok {
		t.Fatal("local provider must be registered")
	}
	if _, err := r.Resolve(""); err != nil {
		t.Fatalf("empty name should resolve local: %v", err)
	}
	if _, err := r.Resolve("missing"); err == nil {
		t.Fatal("unknown provider should fail")
	}
	if len(r.List()) == 0 {
		t.Fatal("registry list should not be empty")
	}
	s := r.Snapshot()
	if s["local"].Status != HealthHealthy {
		t.Fatalf("local health=%+v", s["local"])
	}
	r.Register(nil)
}

func TestCoverageBehaviorDecisionResultContractPredicates(t *testing.T) {
	cases := []struct {
		name                   string
		result                 DecisionResult
		valid, abstain, strict bool
	}{
		{"select", DecisionResult{Action: ActionSelect}, true, false, false},
		{"rank", DecisionResult{Action: ActionRank}, true, false, false},
		{"abstain", DecisionResult{Action: ActionAbstain}, true, true, true},
		{"unknown", DecisionResult{Action: Action("OTHER")}, false, false, false},
		{"empty", DecisionResult{}, false, false, false},
		{"abstain payload", DecisionResult{Action: ActionAbstain, SelectedID: "x"}, true, true, false},
	}
	for _, tc := range cases {
		if got := tc.result.ValidAction(); got != tc.valid {
			t.Errorf("%s valid=%v want %v", tc.name, got, tc.valid)
		}
		if got := tc.result.IsAbstain(); got != tc.abstain {
			t.Errorf("%s abstain=%v want %v", tc.name, got, tc.abstain)
		}
		if got := tc.result.IsStrictAbstain(); got != tc.strict {
			t.Errorf("%s strict=%v want %v", tc.name, got, tc.strict)
		}
	}
}

func TestCoverageBehaviorReasonCodeContract(t *testing.T) {
	all := AllReasonCodes()
	if len(all) < 40 {
		t.Fatalf("reason code registry unexpectedly small: %d", len(all))
	}
	for _, code := range all {
		if !IsValidReasonCode(code) || !IsValidReasonCodeString(string(code)) {
			t.Fatalf("registered reason code rejected: %q", code)
		}
	}
	if IsValidReasonCode(ReasonCode("provider supplied text")) || IsValidReasonCodeString("") {
		t.Fatal("unregistered reason code accepted")
	}
}
