package nexaflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitNodeRunning polls until the named node has a running attempt.
func waitNodeRunning(t *testing.T, store Store, runID, nodeID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		states := nodeStates(t, store, runID)
		if st, ok := states[nodeID]; ok && st.State == NodeRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node %q never reached running", nodeID)
}

func TestEngineCancellation(t *testing.T) {
	blocker := testSpec("test.blocker", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	reg := testRegistry(t, blocker)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"b", "test.blocker", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := eng.Execute(context.Background(), run.ID)
		errCh <- err
	}()
	waitNodeRunning(t, store, run.ID, "b", 5*time.Second)
	cancelled, err := eng.CancelRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if cancelled.State != RunCancelled {
		t.Fatalf("cancelled state = %q", cancelled.State)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Execute after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after CancelRun")
	}
	final, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if final.State != RunCancelled {
		t.Fatalf("final state = %q, want cancelled", final.State)
	}
	if nodeStates(t, store, run.ID)["b"].State != NodeCancelled {
		t.Fatalf("blocker state = %q, want cancelled", nodeStates(t, store, run.ID)["b"].State)
	}
}

func TestEngineIdempotencyAndReExecute(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	echo := testSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return map[string]any{"v": "x"}, nil
	})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	pub := publishDef(t, store, def)
	run1, err := eng.CreateRun(ctx, pub, principal(), runInput(), "idem-run")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run2, err := eng.CreateRun(ctx, pub, principal(), runInput(), "idem-run")
	if err != nil {
		t.Fatalf("CreateRun duplicate: %v", err)
	}
	if run1.ID != run2.ID {
		t.Fatalf("idempotent CreateRun gave different runs: %q vs %q", run1.ID, run2.ID)
	}
	other := def
	other.ID = "wf-other"
	if _, err := eng.CreateRun(ctx, publishDef(t, store, other), principal(), runInput(), "idem-run"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key reuse across definitions: want ErrIdempotencyConflict, got %v", err)
	}
	if _, err := eng.Execute(ctx, run1.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	afterFirst := calls
	mu.Unlock()
	if _, err := eng.Execute(ctx, run1.ID); err != nil {
		t.Fatalf("re-Execute terminal: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != afterFirst {
		t.Fatalf("re-Execute re-ran nodes: %d -> %d", afterFirst, calls)
	}
}

func TestEngineDefinitionConcurrencyGate(t *testing.T) {
	release := make(chan struct{})
	blocker := testSpec("test.blocker", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		select {
		case <-release:
			return map[string]any{"v": "x"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	reg := testRegistry(t, blocker)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"b", "test.blocker", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	pub := publishDef(t, store, def)
	run1, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun 1: %v", err)
	}
	run2, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := eng.Execute(ctx, run1.ID)
		done <- err
	}()
	waitNodeRunning(t, store, run1.ID, "b", 5*time.Second)
	if _, err := eng.Execute(ctx, run2.ID); !errors.Is(err, ErrDefinitionBusy) {
		t.Fatalf("second concurrent Execute: want ErrDefinitionBusy, got %v", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first Execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Execute did not finish after release")
	}
	final, err := eng.Execute(ctx, run2.ID)
	if err != nil {
		t.Fatalf("Execute run2 after gate freed: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("run2 state = %q, want succeeded", final.State)
	}
}

func TestEngineConcurrentClaimsExactlyOneWinner(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	run := Run{ID: "run-claim", DefinitionID: "wf-claim", State: RunCreated}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: run.ID, NodeID: "n", State: NodePending}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var wg sync.WaitGroup
	wins := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := store.TryClaim(ctx, run.ID, "n", "w"+itoa(i), time.Minute, time.Now())
			if err != nil {
				t.Errorf("TryClaim: %v", err)
				return
			}
			wins <- ok
		}(i)
	}
	wg.Wait()
	close(wins)
	count := 0
	for ok := range wins {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", count)
	}
}

func TestEngineRestartRecovery(t *testing.T) {
	safe := fixedSpec("test.safe", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "safe"})
	billable := fixedSpec("test.billable", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "bill"})
	billable.SideEffect = SideEffectBillable
	reg := testRegistry(t, safe, billable)
	ctx := context.Background()

	setupInterrupted := func(t *testing.T, nodeID string, dispatched bool) (Store, *Engine, Run) {
		t.Helper()
		store := NewMemoryStore()
		eng := mustEngine(t, store, reg)
		def := chainDef(struct {
			Instance, NodeID string
			Config           map[string]any
		}{"n", nodeID, nil})
		run, err := eng.CreateRun(ctx, publishDef(t, store, def), principal(), runInput(), "")
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if _, err := store.UpdateRunState(ctx, run.ID, RunCreated, RunRunning, "", ""); err != nil {
			t.Fatalf("to running: %v", err)
		}
		claimed, ok, err := store.TryClaim(ctx, run.ID, "n", "old-worker", time.Minute, time.Now())
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		next := claimed
		next.State = NodeRunning
		next.Dispatched = dispatched
		next.LeaseUntil = time.Now().Add(-time.Minute) // expired lease
		if _, err := store.SaveNodeExecution(ctx, next, NodeClaimed); err != nil {
			t.Fatalf("stage interrupted row: %v", err)
		}
		return store, eng, run
	}

	// (a) Claimed but never dispatched: re-queued, then executes.
	store, eng, run := setupInterrupted(t, "test.safe", false)
	if err := eng.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if nodeStates(t, store, run.ID)["n"].State != NodePending {
		t.Fatalf("undispatched state = %q, want pending", nodeStates(t, store, run.ID)["n"].State)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute after recovery: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("recovered run state = %q, want succeeded", final.State)
	}

	// (b) Dispatched on a safe node: re-queued, then executes.
	store, eng, run = setupInterrupted(t, "test.safe", true)
	if err := eng.Recover(ctx); err != nil {
		t.Fatalf("Recover safe: %v", err)
	}
	if nodeStates(t, store, run.ID)["n"].State != NodePending {
		t.Fatalf("safe dispatched state = %q, want pending", nodeStates(t, store, run.ID)["n"].State)
	}

	// (c) Dispatched on a billable node: parked, never auto-retried.
	store, eng, run = setupInterrupted(t, "test.billable", true)
	if err := eng.Recover(ctx); err != nil {
		t.Fatalf("Recover billable: %v", err)
	}
	st := nodeStates(t, store, run.ID)["n"]
	if st.State != NodeNeedsManualAction || st.ErrorCode != "ambiguous_recovery" {
		t.Fatalf("billable dispatched = %+v, want parked ambiguous_recovery", st)
	}
	after, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if after.State != RunNeedsManualAction {
		t.Fatalf("run after ambiguous recovery = %q, want needs_manual_action", after.State)
	}
}

func TestEngineApprovalFlowRealNodes(t *testing.T) {
	defReg := DefaultRegistry()
	trigger, _ := defReg.Lookup("nexaroute.trigger.manual", SupportedVersion)
	approval, _ := defReg.Lookup("nexaroute.control.approval", SupportedVersion)
	shaper := fixedSpec("test.shaper", []Field{{Name: "payload", Type: "object"}}, map[string]any{})
	sink := fixedSpec("test.sink", []Field{{Name: "approved", Type: "boolean"}, {Name: "approver", Type: "string"}}, map[string]any{"done": true})
	reg, err := NewRegistry([]*NodeSpec{trigger, approval, shaper, sink})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "shape", NodeID: "test.shaper", Version: SupportedVersion},
			{ID: "gate", NodeID: "nexaroute.control.approval", Version: SupportedVersion},
			{ID: "sink", NodeID: "test.sink", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "shape"},
			{ID: "e1", From: "shape", To: "gate"},
			{ID: "e2", From: "gate", To: "sink"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	run, err := eng.CreateRun(ctx, publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	waiting, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if waiting.State != RunWaitingForApproval {
		t.Fatalf("run state = %q, want waiting_for_approval", waiting.State)
	}
	if nodeStates(t, store, run.ID)["gate"].State != NodeWaiting {
		t.Fatalf("gate state = %q, want waiting", nodeStates(t, store, run.ID)["gate"].State)
	}
	// Separation of duties: the requester (alice) cannot approve.
	if _, err := eng.DecideApproval(ctx, run.ID, "gate", "alice", "approved"); err == nil {
		t.Fatal("self-approval accepted")
	}
	// Deciding a non-waiting node is rejected.
	if _, err := eng.DecideApproval(ctx, run.ID, "sink", "op1", "approved"); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("decide non-waiting: want ErrNotWaiting, got %v", err)
	}
	if _, err := eng.DecideApproval(ctx, run.ID, "gate", "op1", "approved"); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute after approval: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("final state = %q, want succeeded", final.State)
	}
	states := nodeStates(t, store, run.ID)
	if states["gate"].Output["approved"] != true || states["gate"].Output["approver"] != "op1" {
		t.Fatalf("gate output = %+v", states["gate"].Output)
	}
	if states["sink"].Output["done"] != true {
		t.Fatalf("sink output = %+v", states["sink"].Output)
	}
}

func TestEngineApprovalExpiry(t *testing.T) {
	defReg := DefaultRegistry()
	trigger, _ := defReg.Lookup("nexaroute.trigger.manual", SupportedVersion)
	approval, _ := defReg.Lookup("nexaroute.control.approval", SupportedVersion)
	shaper := fixedSpec("test.shaper", []Field{{Name: "payload", Type: "object"}}, map[string]any{})
	reg, err := NewRegistry([]*NodeSpec{trigger, approval, shaper})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "shape", NodeID: "test.shaper", Version: SupportedVersion},
			{ID: "gate", NodeID: "nexaroute.control.approval", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "shape"},
			{ID: "e1", From: "shape", To: "gate"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg, func(o *EngineOptions) { o.ApprovalTimeout = 100 * time.Millisecond })
	ctx := context.Background()
	run, err := eng.CreateRun(ctx, publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := eng.Execute(ctx, run.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute after deadline: %v", err)
	}
	if final.State != RunFailed {
		t.Fatalf("final state = %q, want failed", final.State)
	}
	if nodeStates(t, store, run.ID)["gate"].ErrorCode != "approval_expired" {
		t.Fatalf("gate code = %q, want approval_expired", nodeStates(t, store, run.ID)["gate"].ErrorCode)
	}
}

func TestEngineBranchAndMergeRealNodes(t *testing.T) {
	defReg := DefaultRegistry()
	trigger, _ := defReg.Lookup("nexaroute.trigger.manual", SupportedVersion)
	branch, _ := defReg.Lookup("nexaroute.control.branch", SupportedVersion)
	merge, _ := defReg.Lookup("nexaroute.control.merge", SupportedVersion)
	cond := fixedSpec("test.cond", []Field{{Name: "payload", Type: "object"}}, map[string]any{"condition_result": true})
	left := fixedSpec("test.left", []Field{{Name: "selected_branch", Type: "string"}}, map[string]any{"which": "left"})
	right := fixedSpec("test.right", []Field{{Name: "selected_branch", Type: "string"}}, map[string]any{"which": "right"})
	reg, err := NewRegistry([]*NodeSpec{trigger, branch, merge, cond, left, right})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "cond", NodeID: "test.cond", Version: SupportedVersion},
			{ID: "br", NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: map[string]any{"true_branch": "yes", "false_branch": "no"}},
			{ID: "left", NodeID: "test.left", Version: SupportedVersion},
			{ID: "right", NodeID: "test.right", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "cond"},
			{ID: "e1", From: "cond", To: "br"},
			{ID: "e2", From: "br", To: "left", Branch: "yes"},
			{ID: "e3", From: "br", To: "right", Branch: "no"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	run, err := eng.CreateRun(ctx, publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("branch run state = %q, want succeeded", final.State)
	}
	states := nodeStates(t, store, run.ID)
	if states["left"].State != NodeSucceeded {
		t.Fatalf("left state = %q, want succeeded", states["left"].State)
	}
	if states["right"].State != NodeSkipped {
		t.Fatalf("right state = %q, want skipped", states["right"].State)
	}

	// Merge: two producers feed the real merge node.
	pa := fixedSpec("test.pa", []Field{{Name: "payload", Type: "object"}}, map[string]any{"first": map[string]any{"a": float64(1)}})
	pb := fixedSpec("test.pb", []Field{{Name: "payload", Type: "object"}}, map[string]any{"second": map[string]any{"b": float64(2)}})
	reg2, err := NewRegistry([]*NodeSpec{trigger, merge, pa, pb})
	if err != nil {
		t.Fatalf("NewRegistry merge: %v", err)
	}
	def2 := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-merge", Name: "merge",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "pa", NodeID: "test.pa", Version: SupportedVersion},
			{ID: "pb", NodeID: "test.pb", Version: SupportedVersion},
			{ID: "m", NodeID: "nexaroute.control.merge", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "pa"},
			{ID: "e1", From: "start", To: "pb"},
			{ID: "e2", From: "pa", To: "m"},
			{ID: "e3", From: "pb", To: "m"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	store2 := NewMemoryStore()
	eng2 := mustEngine(t, store2, reg2)
	run2, err := eng2.CreateRun(ctx, publishDef(t, store2, def2), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun merge: %v", err)
	}
	final2, err := eng2.Execute(ctx, run2.ID)
	if err != nil {
		t.Fatalf("Execute merge: %v", err)
	}
	if final2.State != RunSucceeded {
		t.Fatalf("merge run state = %q, want succeeded", final2.State)
	}
	merged, _ := nodeStates(t, store2, run2.ID)["m"].Output["merged"].(map[string]any)
	if merged["a"] != float64(1) || merged["b"] != float64(2) {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestEnginePolicyGateRejected(t *testing.T) {
	defReg := DefaultRegistry()
	trigger, _ := defReg.Lookup("nexaroute.trigger.manual", SupportedVersion)
	llm, _ := defReg.Lookup("nexaroute.llm.chat", SupportedVersion)
	shaper := fixedSpec("test.shaper", []Field{{Name: "payload", Type: "object"}}, map[string]any{})
	reg, err := NewRegistry([]*NodeSpec{trigger, llm, shaper})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "shape", NodeID: "test.shaper", Version: SupportedVersion},
			{ID: "chat", NodeID: "nexaroute.llm.chat", Version: SupportedVersion, Config: map[string]any{"route_or_alias": "test-route"}},
		},
		Edges: []DefinitionEdge{
			{ID: "e0", From: "start", To: "shape"},
			{ID: "e1", From: "shape", To: "chat"},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	run, err := eng.CreateRun(ctx, publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// No backend is wired: the policy gate fires before any backend use,
	// so the node parks in waiting without touching a gateway.
	waiting, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if waiting.State != RunWaitingForApproval {
		t.Fatalf("run state = %q, want waiting_for_approval", waiting.State)
	}
	if nodeStates(t, store, run.ID)["chat"].ErrorCode != "policy_required" {
		t.Fatalf("chat code = %q, want policy_required", nodeStates(t, store, run.ID)["chat"].ErrorCode)
	}
	if _, err := eng.DecideApproval(ctx, run.ID, "chat", "op1", "rejected"); err != nil {
		t.Fatalf("DecideApproval rejected: %v", err)
	}
	st := nodeStates(t, store, run.ID)["chat"]
	if st.State != NodeFailed || st.ErrorCode != "approval_rejected" {
		t.Fatalf("rejected gate = %+v, want failed/approval_rejected", st)
	}
	after, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if after.State != RunFailed {
		t.Fatalf("run after rejection = %q, want failed", after.State)
	}
}

func TestEngineSnapshotImmutability(t *testing.T) {
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	echo.ConfigSchema = []Field{{Name: "k", Type: "string"}}
	reg := testRegistry(t, echo)
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", map[string]any{"k": "v1"}})
	pub1 := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub1, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	def.Nodes[1].Config = map[string]any{"k": "v2"}
	pub2 := publishDef(t, store, def)
	if pub2.Version.Revision != 2 {
		t.Fatalf("rev2 = %d", pub2.Version.Revision)
	}
	stored, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Definition.Nodes[1].Config["k"] != "v1" {
		t.Fatalf("run snapshot changed: %+v", stored.Definition.Nodes[1].Config)
	}
	rev1, err := store.GetRevision(ctx, def.ID, 1)
	if err != nil {
		t.Fatalf("GetRevision 1: %v", err)
	}
	if rev1.Definition.Nodes[1].Config["k"] != "v1" {
		t.Fatalf("revision 1 changed: %+v", rev1.Definition.Nodes[1].Config)
	}
}

func TestEngineOutputTruncationAndRedaction(t *testing.T) {
	// Many medium keys: redaction caps a single string at 8192 bytes,
	// so store-level truncation needs aggregate size over the bound.
	bigOut := map[string]any{}
	for i := 0; i < 20; i++ {
		bigOut["k"+itoa(i)] = strings.Repeat("x", 4096)
	}
	big := fixedSpec("test.big", []Field{{Name: "payload", Type: "object"}}, bigOut)
	big.MaxOutputBytes = 256 << 10 // registry passes it; the store truncates it
	secret := fixedSpec("test.secret", []Field{{Name: "payload", Type: "object"}}, map[string]any{"password": "hunter2", "ok": true})
	reg := testRegistry(t, big, secret)
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	defBig := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"b", "test.big", nil})
	defBig.ID = "wf-big"
	run, err := eng.CreateRun(ctx, publishDef(t, store, defBig), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun big: %v", err)
	}
	if _, err := eng.Execute(ctx, run.ID); err != nil {
		t.Fatalf("Execute big: %v", err)
	}
	st := nodeStates(t, store, run.ID)["b"]
	if !st.OutputTruncated {
		t.Fatal("over-bound output not marked truncated")
	}
	if jsonLen(st.Output) > MaxStoredOutputBytes+64 {
		t.Fatalf("stored output %d bytes, want bounded near %d", jsonLen(st.Output), MaxStoredOutputBytes)
	}
	defSecret := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"s", "test.secret", nil})
	defSecret.ID = "wf-secret"
	run2, err := eng.CreateRun(ctx, publishDef(t, store, defSecret), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun secret: %v", err)
	}
	if _, err := eng.Execute(ctx, run2.ID); err != nil {
		t.Fatalf("Execute secret: %v", err)
	}
	out := nodeStates(t, store, run2.ID)["s"].Output
	if out["password"] == "hunter2" {
		t.Fatal("secret stored in clear in node output")
	}
	if out["ok"] != true {
		t.Fatalf("non-secret output lost: %+v", out)
	}
}

func TestEngineRunDeadlineCancels(t *testing.T) {
	blocker := testSpec("test.blocker", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	reg := testRegistry(t, blocker)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"b", "test.blocker", nil})
	def.Settings.RunTimeoutSeconds = 1
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	ctx := context.Background()
	run, err := eng.CreateRun(ctx, publishDef(t, store, def), principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunCancelled {
		t.Fatalf("run state = %q, want cancelled on deadline", final.State)
	}
}

func TestEnginePersistFailureAfterSuccessParks(t *testing.T) {
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	inner := NewMemoryStore()
	failer := &failSucceededSaveStore{Store: inner}
	eng := mustEngine(t, failer, reg)
	ctx := context.Background()
	// Publish through the inner store; the wrapper only breaks saves.
	pub := publishDef(t, inner, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunNeedsManualAction {
		t.Fatalf("run state = %q, want needs_manual_action after persist failure", final.State)
	}
	st := nodeStates(t, inner, run.ID)["a"]
	if st.State != NodeNeedsManualAction || st.ErrorCode != "result_not_persisted" {
		t.Fatalf("node after persist failure = %+v", st)
	}
}

// failSucceededSaveStore fails exactly the save that would record a
// successful node outcome; every other operation delegates.
type failSucceededSaveStore struct {
	Store
	mu     sync.Mutex
	failed bool
}

func (s *failSucceededSaveStore) SaveNodeExecution(ctx context.Context, exec NodeExecution, expected NodeState) (NodeExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failed && exec.NodeID == "a" && exec.State == NodeSucceeded {
		s.failed = true
		return NodeExecution{}, errors.New("nexaflow: injected persist failure")
	}
	return s.Store.SaveNodeExecution(ctx, exec, expected)
}
