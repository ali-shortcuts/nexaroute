package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
)

const goodPlanJSON = `{"schema_version":"video-plan.v1","title":"T","duration_seconds":4,"width":320,"height":240,"fps":12,"scenes":[{"id":"a","duration_seconds":2,"background":"#101828","elements":[{"type":"text","text":"Hello","x":0.5,"y":0.4,"font_size":40,"color":"#ffffff"}]},{"id":"b","duration_seconds":2,"background":"#ffffff","elements":[{"type":"shape","shape":"rect","x":0.1,"y":0.1,"width":0.5,"height":0.2,"color":"#f79009"}]}]}`

type scriptedPlanner struct {
	outputs  []string
	err      error
	calls    int
	lastUser string
}

func (s *scriptedPlanner) Complete(_ context.Context, _, _, user string) (string, error) {
	s.calls++
	s.lastUser = user
	if s.err != nil {
		return "", s.err
	}
	if s.calls > len(s.outputs) {
		return s.outputs[len(s.outputs)-1], nil
	}
	return s.outputs[s.calls-1], nil
}

func TestGeneratePlanFirstTry(t *testing.T) {
	sp := &scriptedPlanner{outputs: []string{goodPlanJSON}}
	a := &agent.Agent{Planner: sp}
	res, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "make a video", Model: "deepseek-chat", DurationSeconds: 4})
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if res.Attempts != 1 || len(res.Plan.Scenes) != 2 || res.Plan.TotalDuration() != 4 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestGeneratePlanRepairsMalformedOutput(t *testing.T) {
	sp := &scriptedPlanner{outputs: []string{"not json at all", "```json\n" + goodPlanJSON + "\n```"}}
	a := &agent.Agent{Planner: sp, MaxAttempts: 3}
	res, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m", DurationSeconds: 4})
	if err != nil {
		t.Fatalf("GeneratePlan with repair: %v", err)
	}
	if res.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", res.Attempts)
	}
	if !strings.Contains(sp.lastUser, "rejected by the validator") {
		t.Fatalf("repair prompt did not carry the defect: %q", sp.lastUser)
	}
}

func TestGeneratePlanRepairsValidationFailure(t *testing.T) {
	bad := strings.Replace(goodPlanJSON, `"duration_seconds":4`, `"duration_seconds":9`, 1)
	sp := &scriptedPlanner{outputs: []string{bad, goodPlanJSON}}
	a := &agent.Agent{Planner: sp, MaxAttempts: 2}
	if _, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m", DurationSeconds: 4}); err != nil {
		t.Fatalf("expected repair to succeed: %v", err)
	}
}

func TestGeneratePlanBoundedFailure(t *testing.T) {
	sp := &scriptedPlanner{outputs: []string{"{oops"}}
	a := &agent.Agent{Planner: sp, MaxAttempts: 3}
	_, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "after 3 attempt") {
		t.Fatalf("expected bounded failure, got %v", err)
	}
	if sp.calls != 3 {
		t.Fatalf("planner calls = %d, want exactly 3 (bounded)", sp.calls)
	}
}

func TestGeneratePlanProviderErrorNotRepairedAsBadPlan(t *testing.T) {
	sp := &scriptedPlanner{err: errors.New("upstream 503")}
	a := &agent.Agent{Planner: sp}
	_, err := a.GeneratePlan(context.Background(), agent.Request{Brief: "b", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "planning model call failed") {
		t.Fatalf("expected provider error, got %v", err)
	}
	if sp.calls != 1 {
		t.Fatalf("provider errors must not burn repair attempts, calls=%d", sp.calls)
	}
}

func TestParsePlanRejectsUnknownFields(t *testing.T) {
	raw := strings.Replace(goodPlanJSON, `"title":"T"`, `"title":"T","shell_command":"rm -rf /"`, 1)
	if _, err := agent.ParsePlan(raw); err == nil {
		t.Fatal("unknown fields (e.g. a command field) must be rejected")
	}
}

func TestParsePlanRejectsNoJSON(t *testing.T) {
	if _, err := agent.ParsePlan("I cannot make videos."); err == nil {
		t.Fatal("expected error for non-JSON output")
	}
}
