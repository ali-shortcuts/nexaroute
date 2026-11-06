package eval

import (
	"context"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFStreamEventsEvaluatorMissingStream verifies a stream_events case
// with no recorded stream fails honestly instead of inventing evidence.
func TestAgentFStreamEventsEvaluatorMissingStream(t *testing.T) {
	ev := streamEventsEvaluator()
	if ev.ID() != "stream_events" {
		t.Fatalf("evaluator id = %q, want stream_events", ev.ID())
	}
	res := ev.Evaluate(context.Background(), Case{ID: "s1", Expectation: Expectation{Kind: ExpectStreamEvents, MinEvents: 3}}, Outcome{CaseID: "s1"})
	if res.Verdict != VerdictFail {
		t.Fatalf("verdict = %q, want fail", res.Verdict)
	}
	if res.Reason != "no stream result recorded" {
		t.Fatalf("reason = %q, want %q", res.Reason, "no stream result recorded")
	}
}

// TestAgentFCancellationCleanEvaluatorMissingStream verifies a
// cancellation_clean case with no recorded stream fails honestly.
func TestAgentFCancellationCleanEvaluatorMissingStream(t *testing.T) {
	ev := cancellationCleanEvaluator()
	if ev.ID() != "cancellation_clean" {
		t.Fatalf("evaluator id = %q, want cancellation_clean", ev.ID())
	}
	res := ev.Evaluate(context.Background(), Case{ID: "c1", Expectation: Expectation{Kind: ExpectCancellationClean}}, Outcome{CaseID: "c1"})
	if res.Verdict != VerdictFail {
		t.Fatalf("verdict = %q, want fail", res.Verdict)
	}
	if res.Reason != "no stream result recorded" {
		t.Fatalf("reason = %q, want %q", res.Reason, "no stream result recorded")
	}
}

// TestAgentFOutcomeTurnTooLong verifies an artifact with a turn beyond the
// safe output bound is rejected, so unbounded recorded text cannot enter a
// run record.
func TestAgentFOutcomeTurnTooLong(t *testing.T) {
	o := Outcome{CaseID: "c1", Status: OutcomeOK, Turns: []string{strings.Repeat("x", MaxOutputBytes+1)}}
	if err := o.Validate(); err == nil {
		t.Fatal("over-long turn must fail validation")
	} else if !strings.Contains(err.Error(), "turn exceeds safe limit") {
		t.Fatalf("error = %q, want turn-limit error", err.Error())
	}
}

// TestAgentFTTFTDimensionScoredAgainstTarget verifies TTFT is scored only
// against an operator-supplied target (like latency) and omitted without one.
func TestAgentFTTFTDimensionScoredAgainstTarget(t *testing.T) {
	r := NewRunner()
	outs := codingArtifacts(t, VerdictPass, VerdictPass, VerdictPass)
	for i := range outs {
		outs[i].TTFTMS = int64(50 + 10*i)
	}
	req := Request{DeploymentID: "p1/m1", ProviderID: "p1", Model: "m1", SuiteID: "coding", Outcomes: outs}
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	sc, ok, err := res.Scorecard()
	if err != nil || !ok {
		t.Fatalf("scorecard ok=%v err=%v", ok, err)
	}
	if _, present := sc.Values[scorecards.DimTTFTMS]; present {
		t.Fatal("TTFT must be absent without a target")
	}
	withTarget := req
	withTarget.TTFTTargetMS = 1000
	res2, err := r.Run(context.Background(), withTarget)
	if err != nil {
		t.Fatalf("Run with target: %v", err)
	}
	sc2, _, err := res2.Scorecard()
	if err != nil {
		t.Fatalf("scorecard: %v", err)
	}
	v, present := sc2.Values[scorecards.DimTTFTMS]
	if !present {
		t.Fatal("TTFT must be present once a target is configured")
	}
	// p95 of {50,60,70} is 70; score = 1 - 70/1000 = 0.93.
	if diff := v.Score - 0.93; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("TTFT score = %v, want 0.93 (p95 70ms against 1000ms target)", v.Score)
	}
	if v.Raw != 70 || v.SampleCount != 3 {
		t.Fatalf("TTFT value = %+v, want raw 70 over 3 samples", v)
	}
}

// TestAgentFCloneResultRejectsNonFiniteScore verifies a result carrying a
// non-finite score clones to the zero value instead of smuggling NaN into
// the store.
func TestAgentFCloneResultRejectsNonFiniteScore(t *testing.T) {
	got := cloneResult(Result{RunID: "r1", SuiteID: "coding", DeploymentID: "p1/m1", Score: math.NaN()})
	if got.RunID != "" || got.SuiteID != "" || got.Score != 0 {
		t.Fatalf("cloneResult(NaN score) = %+v, want zero Result", got)
	}
}

// TestAgentFLoadSuitesUnreadableFile verifies a suite file that exists but
// cannot be read fails closed with a clear error. A Unix socket stands in
// for the unreadable file: stat succeeds (it is not a directory) while
// opening it for read fails deterministically.
func TestAgentFLoadSuitesUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pack.json")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()
	if _, err := LoadSuitesFromPath(path); err == nil {
		t.Fatal("unreadable suite file must fail")
	} else if !strings.Contains(err.Error(), "evaluation suites file") {
		t.Fatalf("error = %q, want suites-file error", err.Error())
	}
}

// TestAgentFLoadSuitesOversizedPack verifies a directory pack beyond the
// byte bound is rejected before parsing.
func TestAgentFLoadSuitesOversizedPack(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.json"), []byte(strings.Repeat("x", MaxSuitePackBytes+1)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("oversized suite pack must fail")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %q, want size error", err.Error())
	}
}

// TestAgentFLoadSuitesDanglingPack verifies a pack that disappears between
// the directory listing and its stat fails the whole load. A dangling
// symlink stands in for the vanished file deterministically: the listing
// sees the *.json entry, but stat follows the link to a missing target.
func TestAgentFLoadSuitesDanglingPack(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "missing-target.json"), filepath.Join(dir, "pack.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("dangling pack must fail")
	} else if !strings.Contains(err.Error(), "suite pack") {
		t.Fatalf("error = %q, want suite-pack error", err.Error())
	}
}

// TestAgentFLoadSuitesUnreadablePackInDir verifies a pack file inside a
// directory that cannot be read fails the whole load. A Unix socket stands
// in for the unreadable pack: stat succeeds with size 0 (under the bound)
// while reading fails deterministically.
func TestAgentFLoadSuitesUnreadablePackInDir(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "pack.json"))
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()
	if _, err := LoadSuitesFromPath(dir); err == nil {
		t.Fatal("unreadable pack in directory must fail")
	} else if !strings.Contains(err.Error(), "suite pack") {
		t.Fatalf("error = %q, want suite-pack error", err.Error())
	}
}
