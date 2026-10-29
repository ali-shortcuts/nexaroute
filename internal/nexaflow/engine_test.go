package nexaflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

func testRegistry(t *testing.T, extra ...*NodeSpec) *Registry {
	t.Helper()
	def := DefaultRegistry()
	trigger, err := def.Lookup("nexaroute.trigger.manual", SupportedVersion)
	if err != nil {
		t.Fatalf("trigger lookup: %v", err)
	}
	specs := append([]*NodeSpec{trigger}, extra...)
	reg, err := NewRegistry(specs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func testSpec(id string, in []Field, run func(ctx context.Context, rc *RunContext) (map[string]any, error)) *NodeSpec {
	return &NodeSpec{
		ID: id, Version: SupportedVersion, Category: "control",
		SideEffect: SideEffectNone, Retry: RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
		MaxInputBytes: 64 << 10, MaxOutputBytes: 64 << 10,
		InputSchema: in, Run: run,
	}
}

func fixedSpec(id string, in []Field, out map[string]any) *NodeSpec {
	return testSpec(id, in, func(context.Context, *RunContext) (map[string]any, error) {
		cp := make(map[string]any, len(out))
		for k, v := range out {
			cp[k] = v
		}
		return cp, nil
	})
}

// chainDef builds a definition: manual trigger "start" followed by the
// given node instances in order, each fed by its predecessor. Nodes
// are (instanceID, registryNodeID, config).
func chainDef(nodes ...struct {
	Instance, NodeID string
	Config           map[string]any
}) Definition {
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "wf-test",
		Name:          "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	prev := "start"
	for i, n := range nodes {
		def.Nodes = append(def.Nodes, DefinitionNode{ID: n.Instance, NodeID: n.NodeID, Version: SupportedVersion, Config: n.Config})
		def.Edges = append(def.Edges, DefinitionEdge{ID: "e" + itoa(i), From: prev, To: n.Instance})
		prev = n.Instance
	}
	return def
}

func publishDef(t *testing.T, store Store, def Definition) PublishedDefinition {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	pub, err := store.Publish(ctx, def.ID, "tester")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return pub
}

func mustEngine(t *testing.T, store Store, reg *Registry, opts ...func(*EngineOptions)) *Engine {
	t.Helper()
	o := EngineOptions{Store: store, Registry: reg}
	for _, f := range opts {
		f(&o)
	}
	eng, err := NewEngine(o)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

func principal() authz.Identity {
	return authz.Identity{Subject: "alice", Roles: []authz.Role{"owner"}}
}

func runInput() map[string]any {
	return map[string]any{"payload": map[string]any{"x": 1}}
}

func nodeStates(t *testing.T, store Store, runID string) map[string]NodeExecution {
	t.Helper()
	execs, err := store.GetNodeExecutions(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	return currentAttempts(execs)
}

func TestEngineDAGOrderAndParallelBranches(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	pa := testSpec("test.pa", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		record("pa")
		return map[string]any{"a": float64(1)}, nil
	})
	pb := testSpec("test.pb", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		record("pb")
		return map[string]any{"b": float64(2)}, nil
	})
	join := testSpec("test.join", []Field{{Name: "a", Type: "number"}, {Name: "b", Type: "number"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		record("join")
		return map[string]any{"a": rc.Input["a"], "b": rc.Input["b"]}, nil
	})
	reg := testRegistry(t, pa, pb, join)
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "pa", NodeID: "test.pa", Version: SupportedVersion},
			{ID: "pb", NodeID: "test.pb", Version: SupportedVersion},
			{ID: "join", NodeID: "test.join", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "pa"},
			{ID: "e1", From: "start", To: "pb"},
			{ID: "e2", From: "pa", To: "join"},
			{ID: "e3", From: "pb", To: "join"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", final.State)
	}
	states := nodeStates(t, store, run.ID)
	if states["join"].Output["a"] != float64(1) || states["join"].Output["b"] != float64(2) {
		t.Fatalf("join output = %+v, want merged a+b", states["join"].Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[2] != "join" {
		t.Fatalf("execution order = %v, want join last", order)
	}
}

func TestEngineParallelDispatchIsConcurrent(t *testing.T) {
	paStarted := make(chan struct{})
	pbStarted := make(chan struct{})
	var onceA, onceB sync.Once
	pa := testSpec("test.pa", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		onceA.Do(func() { close(paStarted) })
		select {
		case <-pbStarted:
		case <-time.After(5 * time.Second):
			return nil, NewNodeError("internal_error", false, "pb never started: dispatch was sequential")
		}
		return map[string]any{"a": float64(1)}, nil
	})
	pb := testSpec("test.pb", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		onceB.Do(func() { close(pbStarted) })
		select {
		case <-paStarted:
		case <-time.After(5 * time.Second):
			return nil, NewNodeError("internal_error", false, "pa never started: dispatch was sequential")
		}
		return map[string]any{"b": float64(2)}, nil
	})
	reg := testRegistry(t, pa, pb)
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "pa", NodeID: "test.pa", Version: SupportedVersion},
			{ID: "pb", NodeID: "test.pb", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "pa"},
			{ID: "e1", From: "start", To: "pb"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", final.State)
	}
}

func TestEngineInvalidGraphsAndBounds(t *testing.T) {
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()

	// Cycle: start -> a -> b -> a.
	cyclic := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-cycle", Name: "cycle",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "a", NodeID: "test.echo", Version: SupportedVersion},
			{ID: "b", NodeID: "test.echo", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "a"},
			{ID: "e1", From: "a", To: "b"},
			{ID: "e2", From: "b", To: "a"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if _, err := eng.CreateRun(ctx, publishDef(t, store, cyclic), principal(), runInput(), ""); err == nil {
		t.Fatal("cyclic definition accepted")
	}
	// Orphan: non-trigger node without incoming edge.
	orphan := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-orphan", Name: "orphan",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "a", NodeID: "test.echo", Version: SupportedVersion},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if _, err := eng.CreateRun(ctx, publishDef(t, store, orphan), principal(), runInput(), ""); err == nil {
		t.Fatal("orphan node accepted")
	}
	// Unknown node.
	unknown := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.missing", nil})
	unknown.ID = "wf-unknown"
	if _, err := eng.CreateRun(ctx, publishDef(t, store, unknown), principal(), runInput(), ""); err == nil {
		t.Fatal("unknown node accepted")
	}
	// Node-count bound.
	big := Definition{SchemaVersion: DefinitionSchemaVersion, ID: "wf-big", Name: "big"}
	big.Nodes = append(big.Nodes, DefinitionNode{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion})
	big.Triggers = append(big.Triggers, DefinitionTrigger{Name: "start", Kind: TriggerManual, Config: map[string]any{}})
	prev := "start"
	for i := 0; i < EngineMaxNodes; i++ {
		id := "n" + itoa(i)
		big.Nodes = append(big.Nodes, DefinitionNode{ID: id, NodeID: "test.echo", Version: SupportedVersion})
		big.Edges = append(big.Edges, DefinitionEdge{ID: "e" + itoa(i), From: prev, To: id})
		prev = id
	}
	if _, err := eng.CreateRun(ctx, publishDef(t, store, big), principal(), runInput(), ""); err == nil {
		t.Fatal("over-bound node count accepted")
	}
	// Run-input bound.
	large := map[string]any{"payload": strings.Repeat("x", MaxRunInputBytes)}
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	def.ID = "wf-input-bound"
	if _, err := eng.CreateRun(ctx, publishDef(t, store, def), principal(), large, ""); err == nil {
		t.Fatal("over-bound run input accepted")
	}
}

func TestEngineFailurePropagationSkipsDownstream(t *testing.T) {
	failer := testSpec("test.failer", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		return nil, NewNodeError("backend_error", false, "upstream refused")
	})
	reg := testRegistry(t, failer, fixedSpec("test.down", []Field{{Name: "payload", Type: "object"}}, map[string]any{"done": true}))
	def := chainDef(
		struct {
			Instance, NodeID string
			Config           map[string]any
		}{"bad", "test.failer", nil},
		struct {
			Instance, NodeID string
			Config           map[string]any
		}{"down", "test.down", nil},
	)
	// The downstream node never runs, so its input schema is irrelevant;
	// give it the failer's (never produced) shape via a passthrough.
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunFailed {
		t.Fatalf("run state = %q, want failed", final.State)
	}
	states := nodeStates(t, store, run.ID)
	if states["bad"].ErrorCode != "backend_error" {
		t.Fatalf("bad node code = %q", states["bad"].ErrorCode)
	}
	if states["down"].State != NodeSkipped {
		t.Fatalf("downstream state = %q, want skipped", states["down"].State)
	}
}

func TestEngineRetryableNodeRetries(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	flaky := testSpec("test.flaky", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil, NewNodeError("backend_unavailable", true, "transient")
		}
		return map[string]any{"ok": true}, nil
	})
	flaky.SideEffect = SideEffectRead
	flaky.Retry = RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"backend_unavailable"}}
	reg := testRegistry(t, flaky)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"f", "test.flaky", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded after retry", final.State)
	}
	execs, err := store.GetNodeExecutions(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	flakyAttempts := 0
	for _, e := range execs {
		if e.NodeID == "f" {
			flakyAttempts++
		}
	}
	if flakyAttempts != 2 {
		t.Fatalf("flaky attempts = %d, want 2", flakyAttempts)
	}
}

func TestEngineNodeTimeoutFails(t *testing.T) {
	slow := testSpec("test.slow", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	slow.DefaultTimeout = 50 * time.Millisecond
	slow.MaxTimeout = 50 * time.Millisecond
	reg := testRegistry(t, slow)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"s", "test.slow", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunFailed {
		t.Fatalf("run state = %q, want failed", final.State)
	}
	if nodeStates(t, store, run.ID)["s"].ErrorCode != "timeout" {
		t.Fatalf("slow node code = %q, want timeout", nodeStates(t, store, run.ID)["s"].ErrorCode)
	}
}

func TestEngineBillableTimeoutParksAmbiguous(t *testing.T) {
	slow := testSpec("test.billable-slow", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	slow.SideEffect = SideEffectBillable
	slow.Retry = RetryPolicy{MaxAttempts: 1}
	slow.DefaultTimeout = 50 * time.Millisecond
	slow.MaxTimeout = 50 * time.Millisecond
	reg := testRegistry(t, slow)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"s", "test.billable-slow", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunNeedsManualAction {
		t.Fatalf("run state = %q, want needs_manual_action", final.State)
	}
	st := nodeStates(t, store, run.ID)["s"]
	if st.State != NodeNeedsManualAction || !st.Ambiguous || st.ErrorCode != "ambiguous_timeout" {
		t.Fatalf("parked node = %+v", st)
	}
	if _, err := eng.Execute(context.Background(), run.ID); !errors.Is(err, ErrRunParked) {
		t.Fatalf("re-Execute parked run: want ErrRunParked, got %v", err)
	}
	forced, err := eng.ForceFailRun(context.Background(), run.ID, "operator_force_failed", "operator decided")
	if err != nil {
		t.Fatalf("ForceFailRun: %v", err)
	}
	if forced.State != RunFailed {
		t.Fatalf("forced state = %q, want failed", forced.State)
	}
}
