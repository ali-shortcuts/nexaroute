package taskprofile

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The decision table is executable documentation: every rule's probe must
// classify to exactly the class the table claims. If the classifier changes,
// this test forces the table to be updated with it — the table can never
// silently drift from the implementation.
func TestRuleTable_ProbesClassify(t *testing.T) {
	a := NewAnalyzer()
	seenNames := map[string]struct{}{}
	seenTypes := map[TaskType]bool{}
	for _, r := range RuleTable() {
		if r.Name == "" {
			t.Fatal("rule with empty name")
		}
		if _, dup := seenNames[r.Name]; dup {
			t.Fatalf("duplicate rule name %q", r.Name)
		}
		seenNames[r.Name] = struct{}{}
		if !r.Produces.Valid() {
			t.Fatalf("rule %q produces invalid task type %q", r.Name, r.Produces)
		}
		// Several rules may produce the same class (precedence chain);
		// each must still fire exactly as documented.
		seenTypes[r.Produces] = true
		p := a.Analyze(r.Probe)
		if p.Type != r.Produces {
			t.Errorf("rule %q: probe classified as %q, table claims %q (probe=%+v)",
				r.Name, p.Type, r.Produces, r.Probe)
		}
		if !p.Valid() {
			t.Errorf("rule %q: probe produced invalid profile %+v", r.Name, p)
		}
		if p.RuleVersion != RuleVersion {
			t.Errorf("rule %q: profile missing rule version (got %q, want %q)", r.Name, p.RuleVersion, RuleVersion)
		}
	}
	// The table must cover every class the classifier can produce from
	// non-degenerate input.
	for _, tt := range AllTaskTypes() {
		if !seenTypes[tt] {
			t.Errorf("task type %q has no rule-table row", tt)
		}
	}
}

// Deterministic and replayable: same signals + same rule version => same class.
// Proven across repeated runs and across a JSON round-trip of the features.
func TestRuleTable_Deterministic(t *testing.T) {
	a := NewAnalyzer()
	for _, r := range RuleTable() {
		first := a.Analyze(r.Probe)
		second := a.Analyze(r.Probe)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("rule %q: non-deterministic across runs", r.Name)
		}
		raw, err := json.Marshal(r.Probe)
		if err != nil {
			t.Fatalf("rule %q: marshal probe: %v", r.Name, err)
		}
		// Re-decode into fresh features via JSON to prove the serialized
		// signal set replays identically.
		var replayed = r.Probe
		if err := json.Unmarshal(raw, &replayed); err != nil {
			t.Fatalf("rule %q: unmarshal probe: %v", r.Name, err)
		}
		third := a.Analyze(replayed)
		if !reflect.DeepEqual(first, third) {
			t.Fatalf("rule %q: JSON round-trip changed the classification", r.Name)
		}
	}
}
