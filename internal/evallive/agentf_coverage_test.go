package evallive

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFDeploymentValidateBounds verifies every deployment validation
// rejection, including the identifier length bound.
func TestAgentFDeploymentValidateBounds(t *testing.T) {
	if err := (Deployment{}).Validate(); err == nil || err.Error() != "live evaluation requires deployment_id" {
		t.Fatalf("empty deployment: err = %v, want deployment_id error", err)
	}
	if err := (Deployment{ID: "p1/m1"}).Validate(); err == nil || err.Error() != "live evaluation deployment is missing provider_id" {
		t.Fatalf("deployment without provider: err = %v, want provider_id error", err)
	}
	if err := (Deployment{ID: strings.Repeat("x", MaxCaseIDBytes+1), ProviderID: "p1", Model: "m1"}).Validate(); err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("over-long deployment id: err = %v, want safe-limit error", err)
	}
}

// TestAgentFPromptValidateBounds verifies prompt case-id length bound.
func TestAgentFPromptValidateBounds(t *testing.T) {
	p := Prompt{CaseID: strings.Repeat("x", MaxCaseIDBytes+1), Prompt: "hi"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds safe limit") {
		t.Fatalf("over-long case id: err = %v, want safe-limit error", err)
	}
}

// TestAgentFMissingPromptUnwrap verifies a missing prompt unwraps to the
// missing-evidence sentinel, so the runner records "missing" instead of an
// infrastructure failure.
func TestAgentFMissingPromptUnwrap(t *testing.T) {
	err := &MissingPromptError{CaseID: "c1"}
	if err.Error() != `no live prompt for case "c1"` {
		t.Fatalf("Error() = %q", err.Error())
	}
	if !errors.Is(err, eval.ErrMissingEvidence) {
		t.Fatal("MissingPromptError must unwrap to eval.ErrMissingEvidence")
	}
}

// TestAgentFTooManyPromptsRejected verifies the per-run prompt bound: more
// than MaxPromptsPerRun prompts is refused before any upstream call.
func TestAgentFTooManyPromptsRejected(t *testing.T) {
	srv := liveUpstream(t, nil, http.StatusOK, 0)
	twin := liveTwin(t, srv.URL)
	prompts := make([]Prompt, 0, MaxPromptsPerRun+1)
	for i := 0; i <= MaxPromptsPerRun; i++ {
		prompts = append(prompts, Prompt{CaseID: "case-" + strconv.Itoa(i), Prompt: "hi"})
	}
	if _, err := NewLiveEvaluationExecutor(liveDeployment(), twin, prompts, 0); err == nil {
		t.Fatalf("%d prompts must be rejected (bound %d)", len(prompts), MaxPromptsPerRun)
	} else if !strings.Contains(err.Error(), fmt.Sprintf("%d", MaxPromptsPerRun)) {
		t.Fatalf("error = %q, want bound error", err.Error())
	}
}
