package eval

import (
	"context"
	"testing"
)

func TestQualityGate_NewEvaluators(t *testing.T) {
	reg := NewRegistry()
	byID := map[string]Evaluator{}
	for _, e := range reg.Deterministic() {
		byID[e.ID()] = e
	}
	cases := []struct {
		name    string
		expect  Expectation
		outcome Outcome
		want    Verdict
		evalID  string
	}{
		{"multi-turn final pass", Expectation{Kind: ExpectMultiTurnFinal, Expected: "NEEDLE-A"},
			Outcome{Status: OutcomeOK, Turns: []string{"first", "NEEDLE-A"}}, VerdictPass, "multi_turn_final"},
		{"multi-turn final is last turn only", Expectation{Kind: ExpectMultiTurnFinal, Expected: "NEEDLE-A"},
			Outcome{Status: OutcomeOK, Turns: []string{"NEEDLE-A", "other"}}, VerdictFail, "multi_turn_final"},
		{"multi-turn no turns", Expectation{Kind: ExpectMultiTurnFinal, Expected: "NEEDLE-A"},
			Outcome{Status: OutcomeOK}, VerdictFail, "multi_turn_final"},
		{"stream events pass", Expectation{Kind: ExpectStreamEvents, MinEvents: 3},
			Outcome{Status: OutcomeOK, Stream: &StreamResult{Terminated: true, Events: 5}}, VerdictPass, "stream_events"},
		{"stream events too few", Expectation{Kind: ExpectStreamEvents, MinEvents: 3},
			Outcome{Status: OutcomeOK, Stream: &StreamResult{Terminated: true, Events: 2}}, VerdictFail, "stream_events"},
		{"stream events unterminated", Expectation{Kind: ExpectStreamEvents, MinEvents: 3},
			Outcome{Status: OutcomeOK, Stream: &StreamResult{Terminated: false, Events: 9}}, VerdictFail, "stream_events"},
		{"cancellation clean", Expectation{Kind: ExpectCancellationClean},
			Outcome{Status: OutcomeOK, Stream: &StreamResult{Cancelled: true}}, VerdictPass, "cancellation_clean"},
		{"cancellation not recorded", Expectation{Kind: ExpectCancellationClean},
			Outcome{Status: OutcomeOK, Stream: &StreamResult{Terminated: true}}, VerdictFail, "cancellation_clean"},
		{"cancellation with error is not clean", Expectation{Kind: ExpectCancellationClean},
			Outcome{Status: OutcomeOK, Stream: &StreamResult{Cancelled: true, Error: "upstream reset"}}, VerdictFail, "cancellation_clean"},
		{"tool call bad arguments json", Expectation{Kind: ExpectToolCall, ToolName: "read_file"},
			Outcome{Status: OutcomeOK, ToolCall: &ToolCall{Name: "read_file", Arguments: "{not json"}}, VerdictFail, "expected_tool_call"},
		{"tool call valid arguments json", Expectation{Kind: ExpectToolCall, ToolName: "read_file"},
			Outcome{Status: OutcomeOK, ToolCall: &ToolCall{Name: "read_file", Arguments: `{"path":"x"}`}}, VerdictPass, "expected_tool_call"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := byID[tc.evalID]
			if !ok {
				t.Fatalf("evaluator %q not registered", tc.evalID)
			}
			if !e.Supports(tc.expect.Kind) {
				t.Fatalf("evaluator %q must support %q", tc.evalID, tc.expect.Kind)
			}
			got := e.Evaluate(context.Background(), Case{ID: "c", Expectation: tc.expect}, tc.outcome)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s (%s)", got.Verdict, tc.want, got.Reason)
			}
		})
	}
}

func TestQualityGate_NewSuitesAreValid(t *testing.T) {
	for _, id := range []string{"multi_turn", "streaming", "cancellation"} {
		suite, ok := LookupSuite(id)
		if !ok {
			t.Fatalf("suite %q must exist", id)
		}
		if err := ValidateSuite(suite); err != nil {
			t.Fatalf("suite %q invalid: %v", id, err)
		}
		if len(suite.Cases) != 3 {
			t.Fatalf("suite %q cases = %d, want 3", id, len(suite.Cases))
		}
	}
	if _, ok := LookupSuite("tool_calling"); !ok {
		t.Fatal("reused tool_calling suite must still exist")
	}
}

func TestQualityGate_RunnerSurfacesTTFTAndTurns(t *testing.T) {
	runner := NewRunner()
	outcomes := []Outcome{
		{CaseID: "multi-turn-context-recall", Status: OutcomeOK, Turns: []string{"context", "NEEDLE-A"}, LatencyMS: 120, TTFTMS: 30},
		{CaseID: "multi-turn-followup-correction", Status: OutcomeOK, Turns: []string{"wrong", "corrected"}, LatencyMS: 110, TTFTMS: 25},
		{CaseID: "multi-turn-tool-result-followup", Status: OutcomeOK, Turns: []string{"tool", "done"}, LatencyMS: 100, TTFTMS: 20},
	}
	res, err := runner.Run(context.Background(), Request{
		DeploymentID: "dep-1", SuiteID: "multi_turn", Outcomes: outcomes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Scoreable || res.Score != 1 {
		t.Fatalf("run scoreable=%v score=%v, want true/1", res.Scoreable, res.Score)
	}
	for _, c := range res.Cases {
		if c.TTFTMS <= 0 {
			t.Fatalf("case %s ttft = %d, want recorded TTFT surfaced", c.CaseID, c.TTFTMS)
		}
	}
	// Outcome bounds: too many turns, and a negative stream count, are
	// rejected rather than graded.
	if err := (Outcome{CaseID: "x", Turns: make([]string, MaxTurnsPerOutcome+1)}).Validate(); err == nil {
		t.Fatal("turn count over the bound must be rejected")
	}
	if err := (Outcome{CaseID: "x", Stream: &StreamResult{Events: -1}}).Validate(); err == nil {
		t.Fatal("negative stream event count must be rejected")
	}
	if (Expectation{Kind: ExpectStreamEvents, MinEvents: 0}).Valid() {
		t.Fatal("stream_events without a minimum event count must be invalid")
	}
	if (Expectation{Kind: ExpectMultiTurnFinal}).Valid() {
		t.Fatal("multi_turn_final without an expected final turn must be invalid")
	}
}
