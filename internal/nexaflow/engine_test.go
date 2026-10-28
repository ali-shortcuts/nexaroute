package nexaflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

func testEngine(backend Backend, policy bool) *Engine {
	return &Engine{Registry: DefaultRegistry(), Backend: backend, Store: NewMemoryRunStore(), PolicyApproved: policy}
}

func mustRun(t *testing.T, eng *Engine, wf *Workflow, input map[string]any) *Run {
	t.Helper()
	run, err := eng.Run(context.Background(), wf, ownerPrincipal(), input)
	if err != nil {
		t.Fatalf("engine Run: %v", err)
	}
	return run
}

func TestEngineValidation(t *testing.T) {
	eng := testEngine(newFakeBackend(), true)
	cases := []struct {
		name string
		wf   *Workflow
		want string
	}{
		{
			name: "unknown node",
			wf: &Workflow{ID: "w", Steps: []Step{
				{ID: "a", NodeID: "nexaroute.nope"},
			}},
			want: "not found",
		},
		{
			name: "cycle",
			wf: &Workflow{ID: "w", Steps: []Step{
				{ID: "a", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}},
				{ID: "b", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}},
			}, Edges: [][2]string{{"a", "b"}, {"b", "a"}}},
			want: "cycle",
		},
		{
			name: "unresolved placeholder",
			wf: &Workflow{ID: "w", Steps: []Step{
				{ID: "a", NodeID: "nexaroute.provider.health", Input: map[string]any{"deployment_id": "{{deployment_id}}"}},
			}},
			want: "unresolved placeholder",
		},
		{
			name: "branch gate without branch step",
			wf: &Workflow{ID: "w", Steps: []Step{
				{ID: "a", NodeID: "nexaroute.control.merge", Branch: "healthy", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}},
			}},
			want: "branch",
		},
		{
			name: "binding to unknown step",
			wf: &Workflow{ID: "w", Steps: []Step{
				{ID: "a", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}}, Bindings: map[string]string{"second": "ghost.merged"}},
			}},
			want: "unknown step",
		},
		{
			name: "missing required input",
			wf: &Workflow{ID: "w", Steps: []Step{
				{ID: "a", NodeID: "nexaroute.provider.health", Input: map[string]any{}},
			}},
			want: "missing required input",
		},
		{
			name: "policy node without approval rejected before execution",
			wf: &Workflow{ID: "w", Steps: []Step{
				{ID: "a", NodeID: "nexaroute.llm.chat", Config: map[string]any{"route_or_alias": "team/coding"}, Input: map[string]any{"prompt": "hi"}},
			}},
			want: "policy_required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := eng
			if tc.name == "policy node without approval rejected before execution" {
				eng = testEngine(newFakeBackend(), false)
			}
			_, err := eng.Run(context.Background(), tc.wf, ownerPrincipal(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestEngineBranchSkipsUnselected(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)
	wf := &Workflow{ID: "w", Steps: []Step{
		{ID: "cond", NodeID: "nexaroute.control.condition", Config: map[string]any{"field": "status", "operator": "eq", "expected": "healthy"}, Input: map[string]any{"value": map[string]any{"status": "degraded"}}},
		{ID: "br", NodeID: "nexaroute.control.branch", Config: map[string]any{"true_branch": "healthy", "false_branch": "fallback"}, Bindings: map[string]string{"condition_result": "cond.result"}},
		{ID: "yes", NodeID: "nexaroute.control.merge", Branch: "healthy", Input: map[string]any{"first": map[string]any{"a": 1.0}, "second": map[string]any{}}},
		{ID: "no", NodeID: "nexaroute.control.merge", Branch: "fallback", Input: map[string]any{"first": map[string]any{"b": 2.0}, "second": map[string]any{}}},
	}, Edges: [][2]string{{"cond", "br"}, {"br", "yes"}, {"br", "no"}}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("status = %s, err = %v", run.Status, run.Error)
	}
	if got := run.Outputs["br"]["selected_branch"]; got != "fallback" {
		t.Fatalf("branch = %v, want fallback", got)
	}
	statuses := map[string]string{}
	for _, rec := range run.History {
		statuses[rec.StepID] = rec.Status
	}
	if statuses["yes"] != "skipped" {
		t.Fatalf("healthy branch step status = %q, want skipped (it must never execute)", statuses["yes"])
	}
	if statuses["no"] != "completed" {
		t.Fatalf("fallback branch step status = %q, want completed", statuses["no"])
	}
}

func TestEngineLoopAndCap(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)
	loopWf := func(n int) *Workflow {
		items := make([]any, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, float64(i))
		}
		return &Workflow{ID: "w", Steps: []Step{
			{ID: "plan", NodeID: "nexaroute.control.loop_plan", Config: map[string]any{"max_iterations": 100.0}, Input: map[string]any{"items": items}},
			{ID: "each", NodeID: "nexaroute.control.json_transform", IterateFrom: "plan", Bindings: map[string]string{"value": "loop.item"}, Input: map[string]any{"operations": []any{}}},
		}, Edges: [][2]string{{"plan", "each"}}}
	}
	// loop.item for json_transform "value" must be an object; use maps.
	items := []any{map[string]any{"x": 1.0}, map[string]any{"x": 2.0}, map[string]any{"x": 3.0}}
	wf := &Workflow{ID: "w", Steps: []Step{
		{ID: "plan", NodeID: "nexaroute.control.loop_plan", Config: map[string]any{"max_iterations": 100.0}, Input: map[string]any{"items": items}},
		{ID: "each", NodeID: "nexaroute.control.json_transform", IterateFrom: "plan", Bindings: map[string]string{"value": "loop.item"}, Input: map[string]any{"operations": []any{}}},
	}, Edges: [][2]string{{"plan", "each"}}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("status = %s, err = %v", run.Status, run.Error)
	}
	if got := run.Outputs["each"]["count"]; got != 3.0 && got != 3 {
		t.Fatalf("loop count = %v, want 3", got)
	}
	// Cap: default engine cap is 10; an 11-item plan fails before any
	// iteration executes.
	big := loopWf(11)
	run = mustRun(t, eng, big, nil)
	if run.Status != RunFailed || run.Error == nil || run.Error.Code != "loop_iteration_cap" {
		t.Fatalf("over-cap run = %+v, want failed loop_iteration_cap", run)
	}
	// A custom cap of 2 rejects a 3-item plan the same way.
	eng2 := testEngine(fake, true)
	eng2.MaxLoopIterations = 2
	wf3 := &Workflow{ID: "w", Steps: []Step{
		{ID: "plan", NodeID: "nexaroute.control.loop_plan", Config: map[string]any{"max_iterations": 100.0}, Input: map[string]any{"items": items}},
		{ID: "each", NodeID: "nexaroute.control.json_transform", IterateFrom: "plan", Bindings: map[string]string{"value": "loop.item"}, Input: map[string]any{"operations": []any{}}},
	}, Edges: [][2]string{{"plan", "each"}}}
	run = mustRun(t, eng2, wf3, nil)
	if run.Status != RunFailed || run.Error.Code != "loop_iteration_cap" {
		t.Fatalf("custom-cap run = %+v, want failed loop_iteration_cap", run)
	}
}

func TestEngineSubworkflowAndDepthCap(t *testing.T) {
	eng := testEngine(newFakeBackend(), true)
	inner := &Workflow{ID: "inner", Steps: []Step{
		{ID: "m", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{"a": 1.0}, "second": map[string]any{}}},
	}}
	// The inner merge needs its "first" from the parent $input via the
	// subworkflow step's input scope: bind inside the subworkflow.
	inner.Steps[0].Bindings = map[string]string{"first": "$input.first"}
	inner.Steps[0].Input = map[string]any{"second": map[string]any{}}
	wf := &Workflow{ID: "w", Steps: []Step{
		{ID: "sub", Subworkflow: inner, Input: map[string]any{"first": map[string]any{"a": 1.0}}},
	}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("status = %s, err = %v", run.Status, run.Error)
	}
	nested, ok := run.Outputs["sub"]["outputs"].(map[string]map[string]any)
	if !ok {
		t.Fatalf("sub output = %#v, want nested outputs map", run.Outputs["sub"])
	}
	if nested["m"]["merged"] == nil {
		t.Fatalf("nested merge output missing: %#v", nested)
	}
	// Depth cap: nest deeper than 3.
	deep := &Workflow{ID: "l0", Steps: []Step{{ID: "s", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}}}}
	for i := 0; i < 4; i++ {
		deep = &Workflow{ID: "wrap", Steps: []Step{{ID: "sub", Subworkflow: deep}}}
	}
	if _, err := eng.Run(context.Background(), deep, ownerPrincipal(), nil); err == nil {
		t.Fatal("depth-4 nesting accepted, want validation error")
	}
}

func TestEngineSubworkflowErrorUnwindsToParentPolicy(t *testing.T) {
	// Inner step fails (tool not allowlisted). With a fallback on the
	// subworkflow step, the parent continues with the fallback value;
	// with propagate (default), the whole run fails.
	inner := &Workflow{ID: "inner", Steps: []Step{
		{ID: "call", NodeID: "nexaroute.mcp.call", Config: map[string]any{"allowlist": []any{"other_tool"}}, Input: map[string]any{"server_id": "tools", "tool": "search_files"}},
	}}
	fallbackWf := &Workflow{ID: "w", Steps: []Step{
		{ID: "sub", Subworkflow: inner, OnError: "fallback", FallbackOutput: map[string]any{"fallback": true}},
		{ID: "after", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}},
	}, Edges: [][2]string{{"sub", "after"}}}
	eng := testEngine(newFakeBackend(), true)
	run := mustRun(t, eng, fallbackWf, nil)
	if run.Status != RunCompleted || run.Outputs["sub"]["fallback"] != true {
		t.Fatalf("fallback unwind run = %+v", run)
	}
	propWf := &Workflow{ID: "w", Steps: []Step{
		{ID: "sub", Subworkflow: inner},
		{ID: "after", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}},
	}, Edges: [][2]string{{"sub", "after"}}}
	run = mustRun(t, eng, propWf, nil)
	if run.Status != RunFailed || run.Error == nil || run.Error.Code != "tool_forbidden" {
		t.Fatalf("propagate unwind run = %+v, want failed tool_forbidden", run)
	}
	if _, executed := run.Outputs["after"]; executed {
		t.Fatal("step after a propagated subworkflow failure executed")
	}
}

func TestEngineApprovalPauseResume(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)
	wf := &Workflow{ID: "w", Steps: []Step{
		{ID: "appr", NodeID: "nexaroute.control.approval", Input: map[string]any{"requester": "requester-1"}},
		{ID: "after", NodeID: "nexaroute.control.merge", Bindings: map[string]string{"first": "appr"}, Input: map[string]any{"second": map[string]any{}}},
	}, Edges: [][2]string{{"appr", "after"}}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunPaused || run.ResumeToken == "" {
		t.Fatalf("run = %+v, want paused with a token", run)
	}
	// A bogus token fails closed.
	if _, err := eng.Resume(context.Background(), "nope", "approved", ownerPrincipal()); err == nil {
		t.Fatal("resume with unknown token accepted")
	}
	// A bogus decision fails closed without consuming the token.
	if _, err := eng.Resume(context.Background(), run.ResumeToken, "maybe", ownerPrincipal()); err == nil {
		t.Fatal("resume with invalid decision accepted")
	}
	resumed := mustRunResume(t, eng, run.ResumeToken, "approved")
	if resumed.Status != RunCompleted {
		t.Fatalf("resumed status = %s, err = %v", resumed.Status, resumed.Error)
	}
	if got := resumed.Outputs["appr"]["approved"]; got != true {
		t.Fatalf("approval output = %#v, want approved=true", resumed.Outputs["appr"])
	}
	// Rejected path: a fresh run pauses, then rejection records
	// approved=false and the workflow still completes (downstream
	// steps decide what a rejection means).
	run2 := mustRun(t, eng, wf, nil)
	rejected := mustRunResume(t, eng, run2.ResumeToken, "rejected")
	if rejected.Status != RunCompleted || rejected.Outputs["appr"]["approved"] != false {
		t.Fatalf("rejected run = %+v", rejected)
	}
	// Self-approval is rejected by the node, failing the run.
	run3 := mustRun(t, eng, wf, nil)
	resumedSelf, err := eng.Resume(context.Background(), run3.ResumeToken, "approved", authz.Identity{Subject: "requester-1", Roles: []authz.Role{authz.RoleOwner}})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumedSelf.Status != RunFailed {
		t.Fatalf("self-approval run status = %s, want failed", resumedSelf.Status)
	}
}

func mustRunResume(t *testing.T, eng *Engine, token, decision string) *Run {
	t.Helper()
	run, err := eng.Resume(context.Background(), token, decision, ownerPrincipal())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	return run
}

func TestEngineErrorPolicies(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)
	// Failing step: provider health for an unknown deployment still
	// returns "unknown" from the fake, so force a failure with an
	// MCP call whose tool is not allowlisted (tool_forbidden).
	failStep := Step{ID: "call", NodeID: "nexaroute.mcp.call", Config: map[string]any{"allowlist": []any{"other_tool"}}, Input: map[string]any{"server_id": "tools", "tool": "search_files"}}
	after := Step{ID: "after", NodeID: "nexaroute.control.merge", Input: map[string]any{"first": map[string]any{}, "second": map[string]any{}}}

	fallbackWf := &Workflow{ID: "w", Steps: []Step{
		{ID: failStep.ID, NodeID: failStep.NodeID, Config: failStep.Config, Input: failStep.Input, OnError: "fallback", FallbackOutput: map[string]any{"fallback": true}},
		after,
	}, Edges: [][2]string{{"call", "after"}}}
	run := mustRun(t, eng, fallbackWf, nil)
	if run.Status != RunCompleted || run.Outputs["call"]["fallback"] != true {
		t.Fatalf("fallback run = %+v", run)
	}
	stopWf := &Workflow{ID: "w", Steps: []Step{
		{ID: failStep.ID, NodeID: failStep.NodeID, Config: failStep.Config, Input: failStep.Input, OnError: "stop"},
		after,
	}, Edges: [][2]string{{"call", "after"}}}
	run = mustRun(t, eng, stopWf, nil)
	if run.Status != RunStopped || run.Error == nil {
		t.Fatalf("stop run = %+v, want stopped with error", run)
	}
	if _, executed := run.Outputs["after"]; executed {
		t.Fatal("step after a stopped step executed")
	}
	propWf := &Workflow{ID: "w", Steps: []Step{failStep, after}, Edges: [][2]string{{"call", "after"}}}
	run = mustRun(t, eng, propWf, nil)
	if run.Status != RunFailed || run.Error == nil || run.Error.Code != "tool_forbidden" {
		t.Fatalf("propagate run = %+v, want failed tool_forbidden", run)
	}
}

func TestEngineRetryRules(t *testing.T) {
	// A backend whose chat fails once with a retryable timeout, then
	// succeeds: the LLM node declares timeout retryable, so the
	// engine retries and the run completes.
	flaky := &flakyBackend{FakeBackend: newFakeBackend(), failCode: "timeout", failTimes: 1}
	eng := testEngine(flaky, true)
	wf := &Workflow{ID: "w", Steps: []Step{
		{ID: "llm", NodeID: "nexaroute.llm.chat", Config: map[string]any{"route_or_alias": "team/coding"}, Input: map[string]any{"prompt": "hi"}},
	}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("flaky-timeout run = %+v", run)
	}
	if flaky.chatAttempts != 2 {
		t.Fatalf("chat attempts = %d, want 2 (one retry on timeout)", flaky.chatAttempts)
	}
	// A non-retryable backend error is not retried even for a
	// retry-declaring node.
	flaky2 := &flakyBackend{FakeBackend: newFakeBackend(), failCode: "backend_error", failTimes: 5}
	eng2 := testEngine(flaky2, true)
	run = mustRun(t, eng2, wf, nil)
	if run.Status != RunFailed || flaky2.chatAttempts != 1 {
		t.Fatalf("non-retryable run = %+v, attempts = %d, want failed with 1 attempt", run, flaky2.chatAttempts)
	}
}

type flakyBackend struct {
	*FakeBackend
	failCode     string
	failTimes    int
	chatAttempts int
}

func (f *flakyBackend) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	f.chatAttempts++
	if f.chatAttempts <= f.failTimes {
		return ChatResponse{}, NewNodeError(f.failCode, f.failCode == "timeout", "injected %s", f.failCode)
	}
	return f.FakeBackend.Chat(ctx, req)
}

func TestEngineTriggerSteps(t *testing.T) {
	eng := testEngine(newFakeBackend(), true)
	// Manual trigger as the entry step: payload passes through, and a
	// later step binds from it.
	wf := &Workflow{ID: "w", Steps: []Step{
		{ID: "trig", NodeID: "nexaroute.trigger.manual", Input: map[string]any{"payload": map[string]any{"hello": "world"}}},
		{ID: "sched", NodeID: "nexaroute.trigger.schedule", Config: map[string]any{"interval": "1h"}, Input: map[string]any{"last_run": "2026-01-01T00:00:00Z"}},
		{ID: "use", NodeID: "nexaroute.control.merge", Bindings: map[string]string{"first": "trig.payload"}, Input: map[string]any{"second": map[string]any{}}},
	}, Edges: [][2]string{{"trig", "use"}, {"sched", "use"}}}
	run := mustRun(t, eng, wf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("status = %s, err = %v", run.Status, run.Error)
	}
	if run.Outputs["trig"]["payload"] == nil {
		t.Fatalf("manual trigger output = %#v", run.Outputs["trig"])
	}
	if run.Outputs["sched"]["next_run"] != "2026-01-01T01:00:00Z" {
		t.Fatalf("schedule next_run = %v, want 2026-01-01T01:00:00Z", run.Outputs["sched"]["next_run"])
	}
	merged, ok := run.Outputs["use"]["merged"].(map[string]any)
	if !ok || merged["hello"] != "world" {
		t.Fatalf("merge from trigger payload = %#v", run.Outputs["use"])
	}
}

func TestEngineWaitCancellation(t *testing.T) {
	eng := testEngine(newFakeBackend(), true)
	wf := &Workflow{ID: "w", Steps: []Step{
		{ID: "wait", NodeID: "nexaroute.control.wait", Input: map[string]any{"duration_ms": 60000.0}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	run, err := eng.Run(ctx, wf, ownerPrincipal(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != RunFailed || run.Error == nil || run.Error.Code != "cancelled" {
		t.Fatalf("cancelled wait run = %+v, want failed cancelled", run)
	}
}
