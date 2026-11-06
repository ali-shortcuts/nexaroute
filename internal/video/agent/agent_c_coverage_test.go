package agent_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
)

// Agent C coverage: the validation, clamping and repair branches that the
// happy-path tests never exercise.

func TestGeneratePlanRejectsMissingPlannerBriefAndModel(t *testing.T) {
	if _, err := (&agent.Agent{}).GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m"}); err == nil || !strings.Contains(err.Error(), "no planner configured") {
		t.Fatalf("nil planner err = %v", err)
	}
	a := &agent.Agent{Planner: &scriptedPlanner{outputs: []string{goodPlanJSON}}}
	if _, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "  ", Model: "m"}); err == nil || !strings.Contains(err.Error(), "brief is required") {
		t.Fatalf("empty brief err = %v", err)
	}
	if _, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: " "}); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("empty model err = %v", err)
	}
}

func TestGeneratePlanClampsAttemptsToFive(t *testing.T) {
	sp := &scriptedPlanner{outputs: []string{"{oops"}}
	a := &agent.Agent{Planner: sp, MaxAttempts: 99}
	_, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "after 5 attempt") {
		t.Fatalf("expected bounded failure after 5 attempts, got %v", err)
	}
	if sp.calls != 5 {
		t.Fatalf("planner calls = %d, want exactly 5 (clamped)", sp.calls)
	}
}

func TestGeneratePlanCancelledContextReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &agent.Agent{Planner: &scriptedPlanner{outputs: []string{goodPlanJSON}}}
	if _, err := a.GeneratePlan(ctx, agent.Request{Brief: "b", Model: "m"}); err != context.Canceled {
		t.Fatalf("cancelled context err = %v, want context.Canceled", err)
	}
}

type capturingPlanner struct {
	scriptedPlanner
	lastSystem string
}

func (c *capturingPlanner) Complete(ctx context.Context, model, system, user string) (string, error) {
	c.lastSystem = system
	return c.scriptedPlanner.Complete(ctx, model, system, user)
}

func TestGeneratePlanUsesRequestedFPSInSystemPrompt(t *testing.T) {
	sp := &capturingPlanner{scriptedPlanner: scriptedPlanner{outputs: []string{goodPlanJSON}}}
	a := &agent.Agent{Planner: sp}
	if _, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m", FPS: 30}); err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if !strings.Contains(sp.lastSystem, `"fps":30`) {
		t.Fatalf("system prompt did not carry the requested fps: %q", sp.lastSystem)
	}
}

func TestGeneratePlanRepairPromptTruncatesLongOutput(t *testing.T) {
	bad := strings.Repeat("x", 6000) + "TAIL-MARKER" + strings.Repeat("x", 1000)
	sp := &scriptedPlanner{outputs: []string{bad, goodPlanJSON}}
	a := &agent.Agent{Planner: sp, MaxAttempts: 3}
	if _, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m"}); err != nil {
		t.Fatalf("GeneratePlan with repair: %v", err)
	}
	if strings.Contains(sp.lastUser, "TAIL-MARKER") {
		t.Fatal("repair prompt must truncate the previous output to 6000 characters")
	}
	if !strings.Contains(sp.lastUser, strings.Repeat("x", 6000)) {
		t.Fatal("repair prompt must carry the first 6000 characters of the previous output")
	}
}

func TestNewOpenAIPlannerRejectsEmptyAndInvalidBaseURL(t *testing.T) {
	for _, base := range []string{"", "   ", "ftp://example.com", "http://", "://bad"} {
		if _, err := agent.NewOpenAIPlanner(base, "", false); err == nil {
			t.Fatalf("base URL %q must be rejected", base)
		}
	}
}

func TestNewOpenAIPlannerAcceptsLocalhostLoopback(t *testing.T) {
	for _, base := range []string{"http://localhost:8080", "http://[::1]:8080"} {
		if _, err := agent.NewOpenAIPlanner(base, "", false); err != nil {
			t.Fatalf("loopback base URL %q must be accepted: %v", base, err)
		}
	}
}

func TestOpenAIPlannerCompleteMarshalAndRequestErrors(t *testing.T) {
	// NaN temperature cannot be marshalled to JSON, so Complete must
	// surface the marshal error instead of sending a request.
	p := &agent.OpenAIPlanner{BaseURL: "http://127.0.0.1:9", Temperature: math.NaN()}
	if _, err := p.Complete(context.Background(), "m", "s", "u"); err == nil {
		t.Fatal("NaN temperature must make Complete fail at marshal time")
	}
	// A base URL containing a control character is rejected by
	// http.NewRequest even when the planner is built directly.
	p = &agent.OpenAIPlanner{BaseURL: "http://127.0.0.1:9\x7f"}
	if _, err := p.Complete(context.Background(), "m", "s", "u"); err == nil {
		t.Fatal("control character in the base URL must make Complete fail")
	}
}
