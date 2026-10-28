package nexaflow

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Phase 8 acceptance tests: engine + store behaviour for the end-to-end
// scenarios. All backends here are test doubles; no test touches a real
// provider, gateway, credential store, or network endpoint.

var (
	phase8Operator = authz.Identity{Subject: "op-1", Issuer: "test", Roles: []authz.Role{authz.RoleOperator}}
	phase8Approver = authz.Identity{Subject: "approver-1", Issuer: "test", Roles: []authz.Role{authz.RoleOperator}}
	phase8Viewer   = authz.Identity{Subject: "viewer-1", Issuer: "test", Roles: []authz.Role{authz.RoleViewer}}
	phase8Anon     = authz.Identity{}
)

// countingBackend wraps the #675 FakeBackend with mutex-guarded call
// counts and failure injection. The delegate's own counters are not
// mutex-guarded, so every overridden method holds b.mu across the call.
type countingBackend struct {
	*FakeBackend
	mu         sync.Mutex
	chatCalls  int
	chatFails  int
	usageFails int
	mcpCalls   int
	videoCalls int
}

var _ Backend = (*countingBackend)(nil)

func newCountingBackend() *countingBackend {
	return &countingBackend{FakeBackend: newFakeBackend()}
}

func (b *countingBackend) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chatCalls++
	if b.chatFails > 0 {
		b.chatFails--
		return ChatResponse{}, NewNodeError("timeout", true, "simulated provider timeout")
	}
	return b.FakeBackend.Chat(ctx, req)
}

func (b *countingBackend) UsageSnapshot(ctx context.Context) (UsageInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.usageFails > 0 {
		b.usageFails--
		return UsageInfo{}, NewNodeError("timeout", true, "simulated usage timeout")
	}
	return b.FakeBackend.UsageSnapshot(ctx)
}

func (b *countingBackend) MCPCall(ctx context.Context, serverID, tool string, args map[string]any) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mcpCalls++
	return b.FakeBackend.MCPCall(ctx, serverID, tool, args)
}

func (b *countingBackend) VideoSubmit(ctx context.Context, req VideoSubmitRequest) (VideoJobInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.videoCalls++
	return b.FakeBackend.VideoSubmit(ctx, req)
}

func testEngine(st Store, be Backend) *Engine {
	return NewEngine(nil, st, be, &MemoryAuditSink{}, EngineOptions{BackoffBase: time.Millisecond})
}

func nd(id, nodeID string, cfg, input map[string]any) DefinitionNode {
	return DefinitionNode{ID: id, NodeID: nodeID, Version: SupportedVersion, Config: cfg, Input: input}
}

func ndFE(id, nodeID string, cfg, input map[string]any, fe *ForEachSpec) DefinitionNode {
	n := nd(id, nodeID, cfg, input)
	n.ForEach = fe
	return n
}

func ed(id, from, to, branch string) DefinitionEdge {
	return DefinitionEdge{ID: id, From: from, To: to, Branch: branch}
}

func manualDef(id, tenant string, nodes []DefinitionNode, edges []DefinitionEdge, settings DefinitionSettings, onErr *ErrorHandlerSpec) *Definition {
	return &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            id,
		Name:          id,
		TenantScope:   tenant,
		Nodes:         nodes,
		Edges:         edges,
		Triggers:      []DefinitionTrigger{{Name: "start", Kind: TriggerManual}},
		Settings:      settings,
		OnError:       onErr,
	}
}

func manualStartNode() DefinitionNode {
	return nd("start", "nexaroute.trigger.manual", nil, nil)
}

func publishDef(t *testing.T, ctx context.Context, st Store, def *Definition) *DefinitionVersion {
	t.Helper()
	if err := st.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	ver, _, err := st.Publish(ctx, def, "tester")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return ver
}

func startManual(t *testing.T, ctx context.Context, e *Engine, defID string, input map[string]any) *Run {
	t.Helper()
	run, err := e.StartRun(ctx, phase8Operator, defID, 0, "manual", "", "", input)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return run
}

func mustStatus(t *testing.T, run *Run, want RunStatus) {
	t.Helper()
	if run.Status != want {
		t.Fatalf("run status = %s, want %s (error: %v, steps: %v)", run.Status, want, run.Error, run.Steps)
	}
}

func stepOf(t *testing.T, run *Run, nodeID string) Step {
	t.Helper()
	s, ok := run.LatestStep(nodeID)
	if !ok {
		t.Fatalf("no step for node %q (steps: %v)", nodeID, run.Steps)
	}
	return s
}

// asFloat normalizes the numeric types a step output may carry: nodes
// built in memory emit ints, JSON round-trips emit float64.
func asFloat(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			t.Fatalf("json.Number %q is not numeric: %v", string(n), err)
		}
		return f
	default:
		t.Fatalf("not a number: %T %v", v, v)
		return 0
	}
}

func outField(t *testing.T, run *Run, nodeID, field string) any {
	t.Helper()
	out, ok := run.Outputs[nodeID].(map[string]any)
	if !ok {
		t.Fatalf("no map output for node %q (outputs: %v)", nodeID, run.Outputs)
	}
	v, ok := out[field]
	if !ok {
		t.Fatalf("output of node %q lacks field %q (output: %v)", nodeID, field, out)
	}
	return v
}

// simpleChain builds start -> transform, where the transform applies ops to
// the workflow input's "value" object and sets "done" to true.
func simpleChainDef(id string) *Definition {
	return manualDef(id, "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("t", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"$ref": "input.value"},
				"operations": []any{
					map[string]any{"op": "set_const", "field": "done", "value": true},
				},
			}),
		},
		[]DefinitionEdge{ed("e1", "start", "t", "")},
		DefinitionSettings{}, nil)
}

func TestPhase8PublishImmutability(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	def := simpleChainDef("wf-immutable")
	v1 := publishDef(t, ctx, st, def)
	if v1.Revision != 1 || v1.Status != "published" {
		t.Fatalf("v1 = %+v, want revision 1 published", v1)
	}
	h1 := v1.ContentHash
	if h1 == "" {
		t.Fatal("v1 content hash is empty")
	}
	if h, err := ContentHash(def); err != nil || h != h1 {
		t.Fatalf("ContentHash recomputation = %q (err %v), want %q", h, err, h1)
	}

	// Mutate the draft and publish again: v1 must be untouched.
	def2 := simpleChainDef("wf-immutable")
	def2.Nodes[1].Input["operations"] = []any{
		map[string]any{"op": "set_const", "field": "done", "value": false},
	}
	v2 := publishDef(t, ctx, st, def2)
	if v2.Revision != 2 {
		t.Fatalf("v2 revision = %d, want 2", v2.Revision)
	}
	if v2.ContentHash == h1 {
		t.Fatal("changed definition kept the old content hash")
	}
	oldDef, oldVer, err := st.GetVersion(ctx, "wf-immutable", 1)
	if err != nil {
		t.Fatalf("GetVersion(1): %v", err)
	}
	if oldVer.Status != "superseded" {
		t.Fatalf("v1 status = %q, want superseded", oldVer.Status)
	}
	if got := oldDef.Nodes[1].Input["operations"]; len(got.([]any)) != 1 ||
		got.([]any)[0].(map[string]any)["value"] != true {
		t.Fatalf("published v1 mutated: operations = %v", got)
	}
	latest, latestVer, err := st.LatestPublished(ctx, "wf-immutable")
	if err != nil {
		t.Fatalf("LatestPublished: %v", err)
	}
	if latestVer.Revision != 2 || latest == nil {
		t.Fatalf("latest = rev %d, want 2", latestVer.Revision)
	}

	// Rollback is republication of the old snapshot as a NEW revision;
	// revisions are never mutated in place.
	v3 := publishDef(t, ctx, st, oldDef)
	if v3.Revision != 3 {
		t.Fatalf("rollback revision = %d, want 3", v3.Revision)
	}
	if v3.ContentHash != h1 {
		t.Fatalf("rollback hash = %q, want original %q", v3.ContentHash, h1)
	}
}

func TestPhase8ValidationRejects(t *testing.T) {
	reg := DefaultRegistry()
	cases := []struct {
		name string
		def  *Definition
	}{
		{
			"cycle",
			manualDef("wf-cycle", "tenant-a",
				[]DefinitionNode{
					manualStartNode(),
					nd("a", "nexaroute.control.json_transform", nil, map[string]any{"value": map[string]any{}, "operations": []any{}}),
					nd("b", "nexaroute.control.json_transform", nil, map[string]any{"value": map[string]any{}, "operations": []any{}}),
				},
				[]DefinitionEdge{ed("e1", "start", "a", ""), ed("e2", "a", "b", ""), ed("e3", "b", "a", "")},
				DefinitionSettings{}, nil),
		},
		{
			"unknown node",
			manualDef("wf-unknown", "tenant-a",
				[]DefinitionNode{
					manualStartNode(),
					nd("x", "nexaroute.control.nope", nil, nil),
				},
				[]DefinitionEdge{ed("e1", "start", "x", "")},
				DefinitionSettings{}, nil),
		},
		{
			"unknown ref root",
			manualDef("wf-ref", "tenant-a",
				[]DefinitionNode{
					manualStartNode(),
					nd("t", "nexaroute.control.json_transform", nil, map[string]any{
						"value":      map[string]any{"$ref": "ghost.value"},
						"operations": []any{},
					}),
				},
				[]DefinitionEdge{ed("e1", "start", "t", "")},
				DefinitionSettings{}, nil),
		},
		{
			"handler wrong type",
			manualDef("wf-handler", "tenant-a",
				[]DefinitionNode{
					manualStartNode(),
					nd("t", "nexaroute.control.json_transform", nil, map[string]any{"value": map[string]any{}, "operations": []any{}}),
				},
				[]DefinitionEdge{ed("e1", "start", "t", "")},
				DefinitionSettings{},
				&ErrorHandlerSpec{HandlerNodeID: "t"}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDefinitionExtended(tc.def, reg); err == nil {
				t.Fatalf("ValidateDefinitionExtended accepted %s definition", tc.name)
			}
		})
	}
}

func TestPhase8LinearTransform(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	publishDef(t, ctx, st, simpleChainDef("wf-linear"))
	run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-linear",
		map[string]any{"value": map[string]any{"a": float64(1)}})
	mustStatus(t, run, RunSucceeded)
	val := outField(t, run, "t", "value").(map[string]any)
	if val["a"] != float64(1) || val["done"] != true {
		t.Fatalf("transform output = %v", val)
	}
	s := stepOf(t, run, "t")
	if s.Attempt != 1 || s.DurationMs < 0 {
		t.Fatalf("transform step = %+v", s)
	}
	if s.StartedAt.IsZero() || s.FinishedAt == nil {
		t.Fatal("transform step lacks timestamps")
	}
}

func branchDef(id string) *Definition {
	return manualDef(id, "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("cond", "nexaroute.control.condition",
				map[string]any{"field": "score", "operator": "gte", "expected": "5"},
				map[string]any{"value": map[string]any{"$ref": "input.payload"}}),
			nd("br", "nexaroute.control.branch",
				map[string]any{"true_branch": "yes", "false_branch": "no"},
				map[string]any{"condition_result": map[string]any{"$ref": "cond.result"}}),
			nd("yes", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"branch": "yes"}, "operations": []any{}}),
			nd("no", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"branch": "no"}, "operations": []any{}}),
		},
		[]DefinitionEdge{
			ed("e1", "start", "cond", ""),
			ed("e2", "cond", "br", ""),
			ed("e3", "br", "yes", "yes"),
			ed("e4", "br", "no", "no"),
		},
		DefinitionSettings{}, nil)
}

func TestPhase8BranchLabelled(t *testing.T) {
	for _, tc := range []struct {
		score float64
		want  string
		skip  string
	}{
		{7, "yes", "no"},
		{2, "no", "yes"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			ctx := context.Background()
			st := NewMemoryStore()
			publishDef(t, ctx, st, branchDef("wf-branch"))
			run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-branch",
				map[string]any{"payload": map[string]any{"score": tc.score}})
			mustStatus(t, run, RunSucceeded)
			if stepOf(t, run, tc.want).Status != StepSucceeded {
				t.Fatalf("selected branch %q did not succeed", tc.want)
			}
			if stepOf(t, run, tc.skip).Status != StepSkipped {
				t.Fatalf("unselected branch %q was not skipped: %+v", tc.skip, stepOf(t, run, tc.skip))
			}
			if outField(t, run, "br", "selected_branch") != tc.want {
				t.Fatalf("selected_branch = %v, want %q", outField(t, run, "br", "selected_branch"), tc.want)
			}
		})
	}
}

func TestPhase8BranchUnlabelledAlwaysTaken(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	def := manualDef("wf-unlabelled", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("cond", "nexaroute.control.condition",
				map[string]any{"field": "score", "operator": "gte", "expected": "5"},
				map[string]any{"value": map[string]any{"$ref": "input.payload"}}),
			nd("br", "nexaroute.control.branch",
				map[string]any{"true_branch": "yes", "false_branch": "no"},
				map[string]any{"condition_result": map[string]any{"$ref": "cond.result"}}),
			nd("yes", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"branch": "yes"}, "operations": []any{}}),
			nd("always", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"branch": "always"}, "operations": []any{}}),
		},
		[]DefinitionEdge{
			ed("e1", "start", "cond", ""),
			ed("e2", "cond", "br", ""),
			ed("e3", "br", "yes", "yes"),
			ed("e4", "br", "always", ""),
		},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, st, def)
	// Score 2 selects "no", so the "yes" edge is inactive, but the
	// unlabelled edge is unconditional per the Phase 1 rule.
	run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-unlabelled",
		map[string]any{"payload": map[string]any{"score": float64(2)}})
	mustStatus(t, run, RunSucceeded)
	if stepOf(t, run, "yes").Status != StepSkipped {
		t.Fatal("labelled yes edge was taken despite selecting no")
	}
	if stepOf(t, run, "always").Status != StepSucceeded {
		t.Fatal("unlabelled branch edge was not taken")
	}
}

func TestPhase8ForEachBounded(t *testing.T) {
	items := func(n int) []any {
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, float64(i))
		}
		return out
	}
	build := func(id string) *Definition {
		return manualDef(id, "tenant-a",
			[]DefinitionNode{
				manualStartNode(),
				nd("plan", "nexaroute.control.loop_plan", nil, map[string]any{
					"items": map[string]any{"$ref": "input.items"}}),
				ndFE("worker", "nexaroute.control.json_transform", nil, map[string]any{
					// loop_plan emits iteration objects {index, item},
					// so the worker unwraps them; a raw array source
					// would expose the element itself as loop.item.
					"value": map[string]any{
						"item":  map[string]any{"$ref": "loop.item.item"},
						"index": map[string]any{"$ref": "loop.item.index"},
					},
					"operations": []any{
						map[string]any{"op": "set_const", "field": "seen", "value": true},
					},
				}, &ForEachSpec{Source: "plan.iterations"}),
			},
			[]DefinitionEdge{ed("e1", "start", "plan", ""), ed("e2", "plan", "worker", "")},
			DefinitionSettings{}, nil)
	}
	ctx := context.Background()

	t.Run("three items", func(t *testing.T) {
		st := NewMemoryStore()
		publishDef(t, ctx, st, build("wf-foreach"))
		run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-foreach",
			map[string]any{"items": items(3)})
		mustStatus(t, run, RunSucceeded)
		if outField(t, run, "plan", "truncated") != false {
			t.Fatal("plan truncated a 3-item list")
		}
		outs, ok := run.Outputs["worker"].([]any)
		if !ok || len(outs) != 3 {
			t.Fatalf("worker outputs = %v, want 3 items", run.Outputs["worker"])
		}
		for i, raw := range outs {
			v := raw.(map[string]any)["value"].(map[string]any)
			if asFloat(t, v["item"]) != float64(i) || asFloat(t, v["index"]) != float64(i) || v["seen"] != true {
				t.Fatalf("iteration %d output = %v", i, v)
			}
		}
		steps := 0
		for _, s := range run.Steps {
			if s.NodeID == "worker" {
				steps++
				if s.Iteration < 0 || s.Iteration > 2 {
					t.Fatalf("worker step iteration = %d", s.Iteration)
				}
			}
		}
		if steps != 3 {
			t.Fatalf("worker steps = %d, want 3", steps)
		}
	})

	t.Run("truncated at max_iterations", func(t *testing.T) {
		st := NewMemoryStore()
		def := build("wf-foreach-cap")
		// loop_plan hard-rejects over 100 items; truncation is the
		// max_iterations config cutting a shorter list.
		def.Nodes[1].Config = map[string]any{"max_iterations": float64(2)}
		publishDef(t, ctx, st, def)
		run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-foreach-cap",
			map[string]any{"items": items(3)})
		mustStatus(t, run, RunSucceeded)
		if outField(t, run, "plan", "truncated") != true {
			t.Fatal("plan did not report truncation at max_iterations 2")
		}
		outs, ok := run.Outputs["worker"].([]any)
		if !ok || len(outs) != 2 {
			t.Fatalf("worker outputs = %d items, want exactly 2", len(outs))
		}
	})
}

func TestPhase8ParallelMerge(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	def := manualDef("wf-parallel", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("a", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"x": float64(1), "shared": "a"}, "operations": []any{}}),
			nd("b", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"y": float64(2), "shared": "b"}, "operations": []any{}}),
			nd("m", "nexaroute.control.merge", map[string]any{"conflict": "last"}, map[string]any{
				"first":  map[string]any{"$ref": "a.value"},
				"second": map[string]any{"$ref": "b.value"},
			}),
		},
		[]DefinitionEdge{
			ed("e1", "start", "a", ""), ed("e2", "start", "b", ""),
			ed("e3", "a", "m", ""), ed("e4", "b", "m", ""),
		},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, st, def)
	run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-parallel", nil)
	mustStatus(t, run, RunSucceeded)
	merged := outField(t, run, "m", "merged").(map[string]any)
	if merged["x"] != float64(1) || merged["y"] != float64(2) || merged["shared"] != "b" {
		t.Fatalf("merged = %v, want last-wins {x:1 y:2 shared:b}", merged)
	}
}

func TestPhase8MergeConflictFails(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	def := manualDef("wf-conflict", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("a", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"k": "a"}, "operations": []any{}}),
			nd("b", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"k": "b"}, "operations": []any{}}),
			nd("m", "nexaroute.control.merge", map[string]any{"conflict": "error"}, map[string]any{
				"first":  map[string]any{"$ref": "a.value"},
				"second": map[string]any{"$ref": "b.value"},
			}),
		},
		[]DefinitionEdge{
			ed("e1", "start", "a", ""), ed("e2", "start", "b", ""),
			ed("e3", "a", "m", ""), ed("e4", "b", "m", ""),
		},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, st, def)
	run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-conflict", nil)
	mustStatus(t, run, RunFailed)
	if run.Error == nil || run.Error.Code != "merge_conflict" {
		t.Fatalf("run error = %v, want merge_conflict", run.Error)
	}
}

func usageDef(id string) *Definition {
	return manualDef(id, "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("usage", "nexaroute.cost.usage", nil, nil),
		},
		[]DefinitionEdge{ed("e1", "start", "usage", "")},
		DefinitionSettings{}, nil)
}

func TestPhase8RetryThenSuccess(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	be.usageFails = 1
	publishDef(t, ctx, st, usageDef("wf-retry"))
	run := startManual(t, ctx, testEngine(st, be), "wf-retry", nil)
	mustStatus(t, run, RunSucceeded)
	attempts := 0
	for _, s := range run.Steps {
		if s.NodeID == "usage" {
			attempts++
		}
	}
	if attempts != 2 {
		t.Fatalf("usage steps = %d, want 2 (1 timeout + 1 success)", attempts)
	}
	if asFloat(t, outField(t, run, "usage", "total_prompt_tokens")) != 100 {
		t.Fatalf("usage output = %v", run.Outputs["usage"])
	}
}

func TestPhase8RetryExhaustedFails(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	be.usageFails = 5
	publishDef(t, ctx, st, usageDef("wf-retry-exhaust"))
	run := startManual(t, ctx, testEngine(st, be), "wf-retry-exhaust", nil)
	mustStatus(t, run, RunFailed)
	attempts := 0
	for _, s := range run.Steps {
		if s.NodeID == "usage" {
			attempts++
			if s.Status != StepFailed {
				t.Fatalf("usage step status = %s, want failed", s.Status)
			}
		}
	}
	if attempts != 3 {
		t.Fatalf("usage attempts = %d, want MaxAttempts 3", attempts)
	}
}

func llmDef(id string, policy bool) *Definition {
	return manualDef(id, "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("llm", "nexaroute.llm.chat",
				map[string]any{"route_or_alias": "team/coding"},
				map[string]any{"prompt": "hello"}),
		},
		[]DefinitionEdge{ed("e1", "start", "llm", "")},
		DefinitionSettings{PolicyApproved: policy}, nil)
}

func TestPhase8LLMThroughFakeRouter(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	publishDef(t, ctx, st, llmDef("wf-llm", true))
	run := startManual(t, ctx, testEngine(st, be), "wf-llm", nil)
	mustStatus(t, run, RunSucceeded)
	if outField(t, run, "llm", "text") != "fake reply" {
		t.Fatalf("llm text = %v", outField(t, run, "llm", "text"))
	}
	if outField(t, run, "llm", "model") != "vendor/model" {
		t.Fatalf("llm model = %v", outField(t, run, "llm", "model"))
	}
	if be.chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1", be.chatCalls)
	}
}

func TestPhase8LLMTimeoutRetry(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	be.chatFails = 1
	publishDef(t, ctx, st, llmDef("wf-llm-retry", true))
	run := startManual(t, ctx, testEngine(st, be), "wf-llm-retry", nil)
	mustStatus(t, run, RunSucceeded)
	if be.chatCalls != 2 {
		t.Fatalf("chat calls = %d, want 2 (1 timeout + 1 success)", be.chatCalls)
	}
}

func TestPhase8LLMRequiresPolicy(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	publishDef(t, ctx, st, llmDef("wf-llm-nopolicy", false))
	run := startManual(t, ctx, testEngine(st, newCountingBackend()), "wf-llm-nopolicy", nil)
	mustStatus(t, run, RunFailed)
	if run.Error == nil || run.Error.Code != "policy_required" {
		t.Fatalf("run error = %v, want policy_required", run.Error)
	}
}

func TestPhase8MCPListAndCall(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	def := manualDef("wf-mcp", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("list", "nexaroute.mcp.list", nil, map[string]any{"server_id": "tools"}),
			nd("call", "nexaroute.mcp.call", map[string]any{"allowlist": []any{"tools__search_files"}}, map[string]any{
				"server_id": "tools", "tool": "search_files",
				"arguments": map[string]any{"q": "x"},
			}),
		},
		[]DefinitionEdge{ed("e1", "start", "list", ""), ed("e2", "list", "call", "")},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, st, def)
	run := startManual(t, ctx, testEngine(st, be), "wf-mcp", nil)
	mustStatus(t, run, RunSucceeded)
	tools := outField(t, run, "list", "tools").([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	if !names["search_files"] || !names["read_file"] {
		t.Fatalf("listed tools = %v", tools)
	}
	if outField(t, run, "call", "result").(map[string]any)["content"] != "ok" {
		t.Fatalf("call result = %v", run.Outputs["call"])
	}
	if be.mcpCalls != 1 {
		t.Fatalf("mcp calls = %d, want 1", be.mcpCalls)
	}
}

func TestPhase8MCPForbiddenToolsNeverCalled(t *testing.T) {
	cases := []struct {
		name      string
		allowlist []any
		tool      string
	}{
		{"not in allowlist", []any{"tools__search_files"}, "read_file"},
		{"not listed by gateway", []any{"tools__ghost"}, "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := NewMemoryStore()
			be := newCountingBackend()
			def := manualDef("wf-mcp-forbidden", "tenant-a",
				[]DefinitionNode{
					manualStartNode(),
					nd("call", "nexaroute.mcp.call", map[string]any{"allowlist": tc.allowlist}, map[string]any{
						"server_id": "tools", "tool": tc.tool,
					}),
				},
				[]DefinitionEdge{ed("e1", "start", "call", "")},
				DefinitionSettings{}, nil)
			publishDef(t, ctx, st, def)
			run := startManual(t, ctx, testEngine(st, be), "wf-mcp-forbidden", nil)
			mustStatus(t, run, RunFailed)
			if run.Error == nil || run.Error.Code != "tool_forbidden" {
				t.Fatalf("run error = %v, want tool_forbidden", run.Error)
			}
			if be.mcpCalls != 0 {
				t.Fatalf("mcp calls = %d, want 0 (fail-closed before the call)", be.mcpCalls)
			}
		})
	}
}

func TestPhase8VideoFlow(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	be := newCountingBackend()
	def := manualDef("wf-video", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("submit", "nexaroute.video.submit", nil, map[string]any{
				"prompt": "a cat", "duration_seconds": float64(5)}),
			nd("status", "nexaroute.video.status", nil, map[string]any{
				"job_id": map[string]any{"$ref": "submit.job_id"}}),
			nd("output", "nexaroute.video.output", nil, map[string]any{
				"job_id": map[string]any{"$ref": "submit.job_id"}}),
		},
		[]DefinitionEdge{
			ed("e1", "start", "submit", ""),
			ed("e2", "submit", "status", ""),
			ed("e3", "status", "output", ""),
		},
		// Video nodes require explicit operator policy approval.
		DefinitionSettings{PolicyApproved: true}, nil)
	publishDef(t, ctx, st, def)
	run := startManual(t, ctx, testEngine(st, be), "wf-video", nil)
	mustStatus(t, run, RunSucceeded)
	if outField(t, run, "submit", "job_id") != "job-1" {
		t.Fatalf("submit job_id = %v", outField(t, run, "submit", "job_id"))
	}
	if outField(t, run, "status", "state") != "completed" || outField(t, run, "status", "terminal") != true {
		t.Fatalf("status output = %v", run.Outputs["status"])
	}
	assets := outField(t, run, "output", "assets").([]any)
	if len(assets) != 1 || assets[0].(map[string]any)["id"] != "a1" {
		t.Fatalf("output assets = %v", assets)
	}
	if be.videoCalls != 1 {
		t.Fatalf("video submits = %d, want 1", be.videoCalls)
	}
}

func approvalDef(id string) *Definition {
	return manualDef(id, "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("appr", "nexaroute.control.approval", nil, nil),
			nd("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}),
		},
		[]DefinitionEdge{ed("e1", "start", "appr", ""), ed("e2", "appr", "done", "")},
		DefinitionSettings{}, nil)
}

func TestPhase8ApprovalPauseResumeAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	be := newCountingBackend()
	eng1 := testEngine(fs, be)
	publishDef(t, ctx, fs, approvalDef("wf-approval"))

	run := startManual(t, ctx, eng1, "wf-approval", nil)
	mustStatus(t, run, RunAwaitingApproval)
	if stepOf(t, run, "appr").Status != StepWaiting {
		t.Fatalf("approval step = %s, want waiting", stepOf(t, run, "appr").Status)
	}
	if _, ok := run.LatestStep("done"); ok {
		t.Fatal("downstream node ran before approval")
	}

	// Self-approval is rejected: the approver must differ from the actor.
	if _, err := eng1.ApproveRun(ctx, phase8Operator, run.ID, "approved"); err == nil {
		t.Fatal("self-approval was accepted")
	}

	// Simulated restart: a new engine over the same FileStore resumes it.
	fs2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen FileStore: %v", err)
	}
	if err := fs2.Migrate(ctx); err != nil {
		t.Fatalf("reopen Migrate: %v", err)
	}
	eng2 := testEngine(fs2, be)
	resumed, err := eng2.ApproveRun(ctx, phase8Approver, run.ID, "approved")
	if err != nil {
		t.Fatalf("ApproveRun after restart: %v", err)
	}
	mustStatus(t, resumed, RunSucceeded)
	if outField(t, resumed, "appr", "approved") != true {
		t.Fatalf("approval output = %v, want approved=true", resumed.Outputs["appr"])
	}
	if outField(t, resumed, "appr", "approver") != "approver-1" {
		t.Fatalf("approver = %v, want approver-1", outField(t, resumed, "appr", "approver"))
	}
	if stepOf(t, resumed, "done").Status != StepSucceeded {
		t.Fatal("downstream node did not run after approval")
	}
	// History keeps both the pause record and the decision execution.
	waits, execs := 0, 0
	for _, s := range resumed.Steps {
		if s.NodeID == "appr" {
			if s.Status == StepWaiting {
				waits++
			}
			if s.Status == StepSucceeded {
				execs++
			}
		}
	}
	if waits != 1 || execs != 1 {
		t.Fatalf("approval steps: %d waiting, %d executed; want 1 and 1", waits, execs)
	}
}

func TestPhase8ApprovalRejectionDoesNotFailRun(t *testing.T) {
	// Node semantics: a rejected approval yields approved=false; it is
	// the workflow author's job to branch on that flag. The engine must
	// not silently turn a rejection into a success with no record, nor
	// into a failure that hides the decision.
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, approvalDef("wf-reject"))
	run := startManual(t, ctx, eng, "wf-reject", nil)
	mustStatus(t, run, RunAwaitingApproval)
	resumed, err := eng.ApproveRun(ctx, phase8Approver, run.ID, "rejected")
	if err != nil {
		t.Fatalf("ApproveRun rejected: %v", err)
	}
	mustStatus(t, resumed, RunSucceeded)
	if outField(t, resumed, "appr", "approved") != false {
		t.Fatalf("approval output = %v, want approved=false", resumed.Outputs["appr"])
	}
	if _, err := eng.ApproveRun(ctx, phase8Approver, run.ID, "maybe"); err == nil {
		t.Fatal("invalid decision was accepted")
	}
}

func TestPhase8SuspendedBatchKeepsParallelWork(t *testing.T) {
	// A node that executes in the same batch as a suspending approval
	// node must keep its steps and outputs; resuming must not re-execute
	// it (no duplicate side effect).
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	be := newCountingBackend()
	eng1 := testEngine(fs, be)
	def := manualDef("wf-batch", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("appr", "nexaroute.control.approval", nil, nil),
			nd("call", "nexaroute.mcp.call", map[string]any{"allowlist": []any{"tools__search_files"}}, map[string]any{
				"server_id": "tools", "tool": "search_files",
			}),
			nd("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}),
		},
		[]DefinitionEdge{
			ed("e1", "start", "appr", ""), ed("e2", "start", "call", ""), ed("e3", "appr", "done", ""),
		},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, fs, def)
	run := startManual(t, ctx, eng1, "wf-batch", nil)
	mustStatus(t, run, RunAwaitingApproval)
	if stepOf(t, run, "call").Status != StepSucceeded {
		t.Fatalf("parallel call step = %s, want succeeded", stepOf(t, run, "call").Status)
	}
	if be.mcpCalls != 1 {
		t.Fatalf("mcp calls at suspension = %d, want 1", be.mcpCalls)
	}
	fs2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen FileStore: %v", err)
	}
	if err := fs2.Migrate(ctx); err != nil {
		t.Fatalf("reopen Migrate: %v", err)
	}
	resumed, err := testEngine(fs2, be).ApproveRun(ctx, phase8Approver, run.ID, "approved")
	if err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
	mustStatus(t, resumed, RunSucceeded)
	if be.mcpCalls != 1 {
		t.Fatalf("mcp calls after resume = %d, want exactly 1 (no re-execution)", be.mcpCalls)
	}
}

func TestPhase8CancelWait(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	def := manualDef("wf-cancel", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("wait", "nexaroute.control.wait", map[string]any{"max_duration": "5m"}, map[string]any{
				"duration_ms": float64(30000)}),
			nd("done", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{"finished": true}, "operations": []any{}}),
		},
		[]DefinitionEdge{ed("e1", "start", "wait", ""), ed("e2", "wait", "done", "")},
		DefinitionSettings{}, nil)
	publishDef(t, ctx, st, def)

	runCh := make(chan *Run, 1)
	go func() {
		run, _ := eng.StartRun(ctx, phase8Operator, "wf-cancel", 0, "manual", "", "", nil)
		runCh <- run
	}()
	// Wait until the run appears, then give the execution loop a moment
	// to register its in-flight cancellation handle before cancelling.
	// (Without the pause, CancelRun could take the not-in-flight path
	// while the loop is still starting.)
	deadline := time.Now().Add(5 * time.Second)
	var runID string
	for time.Now().Before(deadline) {
		runs, _ := st.ListRuns(ctx, RunFilter{DefinitionID: "wf-cancel"})
		if len(runs) == 1 {
			runID = runs[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("run never appeared in the store")
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := eng.CancelRun(ctx, phase8Operator, runID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	// Assert on the execution loop's final record, not CancelRun's
	// snapshot: the loop must observe the cancellation itself.
	finalRun := <-runCh
	if finalRun == nil {
		t.Fatal("cancelled run came back nil")
	}
	mustStatus(t, finalRun, RunCancelled)
	if s := stepOf(t, finalRun, "wait"); s.Error == nil || s.Error.Code != "cancelled" {
		t.Fatalf("wait step error = %v, want cancelled", s.Error)
	}
	if _, ok := finalRun.LatestStep("done"); ok {
		t.Fatal("downstream node ran after cancellation")
	}
}

func failingDef(id, policy string, withFallback bool) *Definition {
	nodes := []DefinitionNode{
		manualStartNode(),
		nd("fail", "nexaroute.control.json_transform", nil, map[string]any{
			"value": map[string]any{}, "operations": []any{
				map[string]any{"op": "bogus"},
			}}),
		nd("handler", "nexaroute.control.error_handler", map[string]any{"policy": policy}, nil),
		nd("down", "nexaroute.control.json_transform", nil, map[string]any{
			"value": map[string]any{"$ref": "fail.value"}, "operations": []any{
				map[string]any{"op": "set_const", "field": "done", "value": true},
			}}),
	}
	edges := []DefinitionEdge{
		ed("e1", "start", "fail", ""),
		ed("e2", "fail", "handler", ""),
		ed("e3", "fail", "down", ""),
	}
	onErr := &ErrorHandlerSpec{HandlerNodeID: "handler"}
	if withFallback {
		nodes = append(nodes, nd("fallback", "nexaroute.control.json_transform", nil, map[string]any{
			"value": map[string]any{"recovered": true}, "operations": []any{}}))
		edges = append(edges, ed("e4", "fail", "fallback", ""))
		onErr.FallbackNodeID = "fallback"
	}
	return manualDef(id, "tenant-a", nodes, edges, DefinitionSettings{}, onErr)
}

func countSteps(run *Run, nodeID string, status StepStatus) int {
	n := 0
	for _, s := range run.Steps {
		if s.NodeID == nodeID && s.Status == status {
			n++
		}
	}
	return n
}

func TestPhase8ErrorHandlerRetryBudget(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, failingDef("wf-handler-retry", "retry", false))
	run := startManual(t, ctx, eng, "wf-handler-retry", nil)
	mustStatus(t, run, RunFailed)
	if run.Error == nil || run.Error.Code != "validation_error" {
		t.Fatalf("run error = %v, want the original validation_error", run.Error)
	}
	if got := countSteps(run, "fail", StepFailed); got != 3 {
		t.Fatalf("failed node steps = %d, want 3 (initial + 2 handler retries)", got)
	}
	if got := countSteps(run, "handler", StepSucceeded); got != 2 {
		t.Fatalf("handler steps = %d, want exactly 2 (budget)", got)
	}
}

func TestPhase8ErrorHandlerFallbackStandsIn(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, failingDef("wf-handler-fallback", "fallback", true))
	run := startManual(t, ctx, eng, "wf-handler-fallback", nil)
	mustStatus(t, run, RunSucceeded)
	if got := countSteps(run, "fail", StepFailed); got != 1 {
		t.Fatalf("failed node steps = %d, want 1 kept in history", got)
	}
	if got := countSteps(run, "fallback", StepSucceeded); got != 1 {
		t.Fatalf("fallback steps = %d, want 1", got)
	}
	val := outField(t, run, "down", "value").(map[string]any)
	if val["recovered"] != true || val["done"] != true {
		t.Fatalf("downstream output = %v, want fallback value + done", val)
	}
}

func TestPhase8ErrorHandlerFail(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, failingDef("wf-handler-fail", "fail", false))
	run := startManual(t, ctx, eng, "wf-handler-fail", nil)
	mustStatus(t, run, RunFailed)
	if got := countSteps(run, "handler", StepSucceeded); got != 1 {
		t.Fatalf("handler steps = %d, want 1", got)
	}
	if run.Error == nil || run.Error.Code != "validation_error" {
		t.Fatalf("run error = %v, want the original validation_error", run.Error)
	}
}

func TestPhase8AccessControlAndTenancy(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())

	// Anonymous principals cannot start runs at all.
	if _, err := eng.StartRun(ctx, phase8Anon, "nope", 0, "manual", "", "", nil); err == nil {
		t.Fatal("anonymous StartRun was accepted")
	}

	// A viewer lacks video.manage: the run fails at the node with
	// permission_denied instead of executing the video node.
	publishDef(t, ctx, st, manualDef("wf-video-acl", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("submit", "nexaroute.video.submit", nil, map[string]any{
				"prompt": "a cat", "duration_seconds": float64(5)}),
		},
		[]DefinitionEdge{ed("e1", "start", "submit", "")},
		DefinitionSettings{PolicyApproved: true}, nil))
	viewerRun, err := eng.StartRun(ctx, phase8Viewer, "wf-video-acl", 0, "manual", "", "", nil)
	if err != nil {
		t.Fatalf("viewer StartRun errored at dispatch: %v", err)
	}
	mustStatus(t, viewerRun, RunFailed)
	if viewerRun.Error == nil || viewerRun.Error.Code != "permission_denied" {
		t.Fatalf("viewer run error = %v, want permission_denied", viewerRun.Error)
	}

	// Tenant scoping is enforced at the store-filter level: drafts and
	// runs of one scope are invisible to another scope's listing.
	publishDef(t, ctx, st, simpleChainDef("wf-tenant-a"))
	defB := simpleChainDef("wf-tenant-b")
	defB.TenantScope = "tenant-b"
	publishDef(t, ctx, st, defB)
	drafts, err := st.ListDrafts(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	for _, d := range drafts {
		if d.TenantScope != "tenant-a" {
			t.Fatalf("tenant-a listing leaked %q", d.ID)
		}
	}
	found := false
	for _, d := range drafts {
		if d.ID == "wf-tenant-a" {
			found = true
		}
	}
	if !found {
		t.Fatal("tenant-a listing missed its own definition")
	}
	runB := startManual(t, ctx, eng, "wf-tenant-b", map[string]any{"value": map[string]any{}})
	if runB.TenantScope != "tenant-b" {
		t.Fatalf("run tenant scope = %q, want tenant-b", runB.TenantScope)
	}
	runs, err := st.ListRuns(ctx, RunFilter{TenantScope: "tenant-a"})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, r := range runs {
		if r.TenantScope != "tenant-a" {
			t.Fatalf("tenant-a run listing leaked %q", r.ID)
		}
	}
}

func TestPhase8BoundsRedactionAudit(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	audit := &MemoryAuditSink{}
	eng := NewEngine(nil, st, newCountingBackend(), audit, EngineOptions{BackoffBase: time.Millisecond})

	// Secret material in a node output must not survive in stored history.
	publishDef(t, ctx, st, manualDef("wf-redact", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("t", "nexaroute.control.json_transform", nil, map[string]any{
				"value": map[string]any{
					"api_key": "sk-secret-123",
					"token":   "Bearer abcdef",
					"normal":  "keep",
				},
				"operations": []any{}}),
		},
		[]DefinitionEdge{ed("e1", "start", "t", "")},
		DefinitionSettings{}, nil))
	run := startManual(t, ctx, eng, "wf-redact", nil)
	mustStatus(t, run, RunSucceeded)
	stored, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	s := stepOf(t, stored, "t")
	val := s.Output["value"].(map[string]any)
	if val["api_key"] != "[REDACTED]" {
		t.Fatalf("stored api_key = %v, want [REDACTED]", val["api_key"])
	}
	// A bearer-shaped value is replaced wholesale, not just its token part.
	if tok, _ := val["token"].(string); tok != "[REDACTED]" {
		t.Fatalf("stored token = %q, want [REDACTED]", tok)
	}
	if val["normal"] != "keep" {
		t.Fatalf("non-secret value changed: %v", val)
	}

	// Workflow input over 64 KiB is refused before dispatch.
	big := make(map[string]any, 1)
	big["blob"] = string(make([]byte, 70<<10))
	if _, err := eng.StartRun(ctx, phase8Operator, "wf-redact", 0, "manual", "", "", big); err == nil {
		t.Fatal("oversized workflow input was accepted")
	}

	// Node input over the node's MaxInputBytes fails with input_too_large.
	publishDef(t, ctx, st, manualDef("wf-input-bound", "tenant-a",
		[]DefinitionNode{
			manualStartNode(),
			nd("t", "nexaroute.control.json_transform", nil, map[string]any{
				"value":      map[string]any{"blob": string(make([]byte, 70<<10))},
				"operations": []any{}}),
		},
		[]DefinitionEdge{ed("e1", "start", "t", "")},
		DefinitionSettings{}, nil))
	boundRun := startManual(t, ctx, eng, "wf-input-bound", nil)
	mustStatus(t, boundRun, RunFailed)
	if boundRun.Error == nil || boundRun.Error.Code != "input_too_large" {
		t.Fatalf("bound run error = %v, want input_too_large", boundRun.Error)
	}

	// Audit trail: start/finish events exist, carry no payloads.
	events := audit.Events()
	var started, finished bool
	for _, ev := range events {
		if ev.RunID == run.ID && ev.Action == "run_started" {
			started = true
		}
		if ev.RunID == run.ID && ev.Action == "run_finished" {
			finished = true
		}
		if ev.Detail == "" {
			continue
		}
		if containsSecret(ev.Detail) {
			t.Fatalf("audit detail leaks secret material: %q", ev.Detail)
		}
	}
	if !started || !finished {
		t.Fatalf("audit events for run: started=%v finished=%v (all: %v)", started, finished, events)
	}
}

func containsSecret(s string) bool {
	return len(s) > 0 && (contains(s, "sk-secret-123") || contains(s, "abcdef"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestPhase8StartRunAsync(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, simpleChainDef("wf-async"))
	run, err := eng.StartRunAsync(ctx, phase8Operator, "wf-async", 0, "manual", "", "", map[string]any{"value": map[string]any{"a": float64(1)}})
	if err != nil {
		t.Fatalf("StartRunAsync: %v", err)
	}
	if run.ID == "" {
		t.Fatal("async run has no id")
	}
	// The run completes in the background; the store record converges
	// to the terminal status without the caller blocking on execution.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status.IsTerminal() {
			mustStatus(t, got, RunSucceeded)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("async run did not finish: status %s", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
