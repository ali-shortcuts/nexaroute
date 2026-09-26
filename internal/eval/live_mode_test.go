package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// countingExecutor is a test executor that reports upstream calls like the
// live executor does (the real one lives in internal/httpapi; the runner must
// trust any executor's report honestly).
type countingExecutor struct {
	calls    int
	evidence map[string]Outcome
}

func (c *countingExecutor) Execute(_ context.Context, cs Case) (Outcome, error) {
	o, ok := c.evidence[cs.ID]
	if !ok {
		return Outcome{}, fmt.Errorf("%w: %s", ErrNoEvidence, cs.ID)
	}
	c.calls++
	return o, nil
}

func (c *countingExecutor) UpstreamCalls() int { return c.calls }

func TestRunner_ModeDefaultsToReplay(t *testing.T) {
	outcomes := codingArtifacts(t, VerdictPass, VerdictPass, VerdictPass)
	res, err := NewRunner().Run(context.Background(), Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: outcomes})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != ModeReplay {
		t.Fatalf("mode = %q, want replay", res.Mode)
	}
	if res.Upstream != 0 {
		t.Fatalf("replay made upstream calls: %d", res.Upstream)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result failed validation: %v", err)
	}
}

func TestRunner_RejectsUnknownMode(t *testing.T) {
	outcomes := codingArtifacts(t, VerdictPass, VerdictPass, VerdictPass)
	for _, mode := range []string{"simulate", "REPLAYX", "auto", " live "} {
		// " live " normalizes (trim + lowercase) to live, so only treat unknowns strictly.
		if strings.TrimSpace(mode) == "live" {
			continue
		}
		exec, err := NewReplayExecutor(outcomes)
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewRunner().RunWithExecutor(context.Background(), Request{Mode: mode, DeploymentID: "p1/m1", SuiteID: "coding"}, exec)
		if err == nil {
			t.Fatalf("mode %q must be rejected", mode)
		}
		if !strings.Contains(err.Error(), "unknown evaluation mode") {
			t.Fatalf("mode %q error = %v", mode, err)
		}
	}
}

func TestRunner_LiveModeRecordsUpstreamCalls(t *testing.T) {
	suite, ok := LookupSuite("reasoning")
	if !ok {
		t.Fatal("reasoning suite missing")
	}
	exec := &countingExecutor{evidence: map[string]Outcome{
		"reasoning-multi-step-arithmetic": {CaseID: "reasoning-multi-step-arithmetic", Status: OutcomeOK, Output: "42", LatencyMS: 5},
		"reasoning-logic-deduction":       {CaseID: "reasoning-logic-deduction", Status: OutcomeOK, Output: "carol", LatencyMS: 5},
		"reasoning-numeric-estimate":      {CaseID: "reasoning-numeric-estimate", Status: OutcomeOK, Output: "3.141", LatencyMS: 5},
	}}
	res, err := NewRunner().RunWithExecutor(context.Background(), Request{
		Mode:         ModeLive,
		DeploymentID: "p1/m1",
		ProviderID:   "p1",
		Model:        "model-a",
		SuiteID:      suite.ID,
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != ModeLive {
		t.Fatalf("mode = %q, want live", res.Mode)
	}
	if res.Upstream != len(suite.Cases) {
		t.Fatalf("upstream calls = %d, want %d (one per case)", res.Upstream, len(suite.Cases))
	}
	if exec.calls != len(suite.Cases) {
		t.Fatalf("executor calls = %d", exec.calls)
	}
	if !res.Scoreable || res.Score != 1 {
		t.Fatalf("all-correct live evidence must be scoreable with score 1: %+v", res)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result failed validation: %v", err)
	}
	sc, ok, err := res.Scorecard()
	if err != nil || !ok {
		t.Fatalf("scorecard missing: ok=%v err=%v", ok, err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("scorecard invalid: %v", err)
	}
}

func TestRunner_ExecutorWithoutEvidenceIsMissingNotFailure(t *testing.T) {
	exec := &countingExecutor{evidence: map[string]Outcome{}}
	res, err := NewRunner().RunWithExecutor(context.Background(), Request{
		Mode: ModeLive, DeploymentID: "p1/m1", SuiteID: "reasoning",
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[VerdictMissing] != 3 {
		t.Fatalf("missing cases = %v, want all three cases missing", res.Counts)
	}
	if res.Scoreable {
		t.Fatal("a run with zero evidence must not be scoreable")
	}
	if res.Upstream != 0 {
		t.Fatalf("no-call cases must not count as upstream calls: %d", res.Upstream)
	}
	for _, cr := range res.Cases {
		if cr.ErrorType != "missing_artifact" {
			t.Fatalf("case %s error type = %q", cr.CaseID, cr.ErrorType)
		}
		if cr.Verdict.Decisive() {
			t.Fatalf("missing evidence must never be decisive: %v", cr.Verdict)
		}
	}
}

func TestRunner_LiveFailureOutcomeIsHonestEvidence(t *testing.T) {
	exec := &countingExecutor{evidence: map[string]Outcome{
		"reasoning-multi-step-arithmetic": {CaseID: "reasoning-multi-step-arithmetic", Status: OutcomeError, ErrorType: "http_500", LatencyMS: 9},
	}}
	res, err := NewRunner().RunWithExecutor(context.Background(), Request{
		Mode: ModeLive, DeploymentID: "p1/m1", SuiteID: "reasoning",
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[VerdictFail] != 1 {
		t.Fatalf("verdicts = %v, one honest failure expected", res.Counts)
	}
	if res.Counts[VerdictMissing] != 2 {
		t.Fatalf("verdicts = %v, the other two cases are missing evidence", res.Counts)
	}
	if res.Scoreable {
		t.Fatal("1 decisive sample < min_samples 3: must not be scoreable")
	}
}

func TestResultValidateModeVocabulary(t *testing.T) {
	base := Result{RunID: "r", SuiteID: "coding", DeploymentID: "p1/m1"}
	for _, mode := range []string{"", ModeReplay, ModeLive} {
		r := base
		r.Mode = mode
		if err := r.Validate(); err != nil {
			t.Fatalf("mode %q must validate: %v", mode, err)
		}
	}
	r := base
	r.Mode = "simulate"
	if err := r.Validate(); err == nil {
		t.Fatal("unknown mode must fail validation (stored runs are trusted evidence)")
	}
}

func TestInput_ValidateBounds(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		ok   bool
	}{
		{"valid", Input{CaseID: "c1", Prompt: "compute 40+2"}, true},
		{"valid with system", Input{CaseID: "c1", Prompt: "p", System: "be terse", MaxTokens: 42}, true},
		{"missing case", Input{Prompt: "x"}, false},
		{"long case id", Input{CaseID: strings.Repeat("c", 257), Prompt: "x"}, false},
		{"empty prompt", Input{CaseID: "c1", Prompt: "   "}, false},
		{"oversized prompt", Input{CaseID: "c1", Prompt: strings.Repeat("x", MaxLivePromptBytes+1)}, false},
		{"oversized system", Input{CaseID: "c1", Prompt: "x", System: strings.Repeat("s", MaxLivePromptBytes+1)}, false},
		{"negative tokens", Input{CaseID: "c1", Prompt: "x", MaxTokens: -1}, false},
		{"too many tokens", Input{CaseID: "c1", Prompt: "x", MaxTokens: MaxLiveTokens + 1}, false},
		{"max tokens at bound", Input{CaseID: "c1", Prompt: "x", MaxTokens: MaxLiveTokens}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if tc.ok && err != nil {
				t.Fatalf("valid input rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

// TestErrNoEvidenceIsTheMissingSentinel pins the cross-package contract the
// httpapi live executor relies on: returning ErrNoEvidence maps to a "missing"
// case, exactly like a replay artifact gap.
func TestErrNoEvidenceIsTheMissingSentinel(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrapped: %w", ErrNoEvidence), errNoArtifact) {
		t.Fatal("ErrNoEvidence must alias the replay missing-artifact sentinel")
	}
	exec := &countingExecutor{evidence: map[string]Outcome{
		"reasoning-multi-step-arithmetic": {CaseID: "reasoning-multi-step-arithmetic", Status: OutcomeOK, Output: "42"},
	}}
	res, err := NewRunner().RunWithExecutor(context.Background(), Request{
		Mode: ModeLive, DeploymentID: "p1/m1", SuiteID: "reasoning",
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[VerdictPass] != 1 || res.Counts[VerdictMissing] != 2 {
		t.Fatalf("mixed evidence ruled wrong: %v", res.Counts)
	}
}
