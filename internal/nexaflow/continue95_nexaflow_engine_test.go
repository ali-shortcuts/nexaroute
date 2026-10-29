package nexaflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// Continue95NexaflowDirectNode runs a registry node directly, bypassing
// Registry.Execute's schema validation, so the node's own error branches
// (not the schema guard) are exercised.
func Continue95NexaflowDirectNode(t *testing.T, id string, input, config map[string]any) (map[string]any, error) {
	t.Helper()
	reg := DefaultRegistry()
	spec, err := reg.Lookup(id, SupportedVersion)
	if err != nil {
		t.Fatalf("lookup %s: %v", id, err)
	}
	return spec.Run(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: input, Config: config})
}

func Continue95NexaflowDirectNodeErr(t *testing.T, id string, input, config map[string]any, want string) {
	t.Helper()
	if _, err := Continue95NexaflowDirectNode(t, id, input, config); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: err = %v, want containing %q", id, err, want)
	}
}

// Continue95NexaflowGuard is a BudgetGuard test double that counts releases.
type Continue95NexaflowGuard struct {
	mu       sync.Mutex
	reserved int
	released int
	fail     bool
}

func (g *Continue95NexaflowGuard) Reserve(ctx context.Context, key string, estimateUSD float64) (func(), error) {
	if g.fail {
		return nil, errors.New("nexaflow: budget exhausted")
	}
	g.mu.Lock()
	g.reserved++
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.released++
		g.mu.Unlock()
	}, nil
}

func (g *Continue95NexaflowGuard) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reserved, g.released
}

func TestContinue95NexaflowControlConditionBranches(t *testing.T) {
	cond := "nexaroute.control.condition"
	val := func(v map[string]any) map[string]any { return map[string]any{"value": v} }
	// Missing field: false for every operator except ne.
	out, err := Continue95NexaflowDirectNode(t, cond, val(map[string]any{}), map[string]any{"field": "a.b", "operator": "eq", "expected": "1"})
	if err != nil || out["result"] != false {
		t.Fatalf("missing field eq = %v, err %v", out, err)
	}
	out, err = Continue95NexaflowDirectNode(t, cond, val(map[string]any{}), map[string]any{"field": "a.b", "operator": "ne", "expected": "1"})
	if err != nil || out["result"] != true {
		t.Fatalf("missing field ne = %v, err %v", out, err)
	}
	// Numeric comparisons parse string numbers; non-numeric fails.
	out, err = Continue95NexaflowDirectNode(t, cond, val(map[string]any{"n": "5"}), map[string]any{"field": "n", "operator": "gt", "expected": "3"})
	if err != nil || out["result"] != true {
		t.Fatalf("string gt = %v, err %v", out, err)
	}
	out, err = Continue95NexaflowDirectNode(t, cond, val(map[string]any{"n": float64(5)}), map[string]any{"field": "n", "operator": "lte", "expected": "5"})
	if err != nil || out["result"] != true {
		t.Fatalf("lte = %v, err %v", out, err)
	}
	out, err = Continue95NexaflowDirectNode(t, cond, val(map[string]any{"n": float64(2)}), map[string]any{"field": "n", "operator": "gte", "expected": "5"})
	if err != nil || out["result"] != false {
		t.Fatalf("gte false = %v, err %v", out, err)
	}
	out, err = Continue95NexaflowDirectNode(t, cond, val(map[string]any{"n": float64(1)}), map[string]any{"field": "n", "operator": "lt", "expected": "5"})
	if err != nil || out["result"] != true {
		t.Fatalf("lt = %v, err %v", out, err)
	}
	Continue95NexaflowDirectNodeErr(t, cond, val(map[string]any{"n": "abc"}), map[string]any{"field": "n", "operator": "gt", "expected": "3"}, "requires numeric values")
	Continue95NexaflowDirectNodeErr(t, cond, val(map[string]any{"s": float64(1)}), map[string]any{"field": "s", "operator": "contains", "expected": "x"}, "requires a string field")
	Continue95NexaflowDirectNodeErr(t, cond, val(map[string]any{"s": "hello"}), map[string]any{"field": "s", "operator": "bogus", "expected": "x"}, "unknown operator")
	Continue95NexaflowDirectNodeErr(t, cond, map[string]any{"value": "not-object"}, map[string]any{"field": "s", "operator": "eq", "expected": "x"}, "must be an object")
	Continue95NexaflowDirectNodeErr(t, cond, val(map[string]any{"s": "x"}), map[string]any{"operator": "eq"}, "field and operator are required")
	// Happy contains path (kept separate from the error table above).
	out, err = Continue95NexaflowDirectNode(t, cond, val(map[string]any{"s": "hello"}), map[string]any{"field": "s", "operator": "contains", "expected": "ell"})
	if err != nil || out["result"] != true {
		t.Fatalf("contains = %v, err %v", out, err)
	}
}

func TestContinue95NexaflowControlBranchTransformLoopMerge(t *testing.T) {
	branch := "nexaroute.control.branch"
	Continue95NexaflowDirectNodeErr(t, branch, map[string]any{}, map[string]any{"true_branch": "a", "false_branch": "b"}, "condition_result is required")
	Continue95NexaflowDirectNodeErr(t, branch, map[string]any{"condition_result": true}, map[string]any{}, "true_branch and false_branch are required")
	out, err := Continue95NexaflowDirectNode(t, branch, map[string]any{"condition_result": false}, map[string]any{"true_branch": "yes", "false_branch": "no"})
	if err != nil || out["selected_branch"] != "no" {
		t.Fatalf("branch false = %v, err %v", out, err)
	}

	jt := "nexaroute.control.json_transform"
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": "x"}, map[string]any{}, "must be an object")
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": map[string]any{}}, map[string]any{}, "must be an array")
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": map[string]any{}, "operations": []any{"nope"}}, map[string]any{}, "must be an object")
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": map[string]any{}, "operations": []any{map[string]any{"op": "pick"}}}, map[string]any{}, "pick requires a field")
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": map[string]any{"a": float64(1)}, "operations": []any{map[string]any{"op": "rename", "field": "a"}}}, map[string]any{}, "rename requires field and to")
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": map[string]any{}, "operations": []any{map[string]any{"op": "set_const"}}}, map[string]any{}, "set_const requires a field")
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": map[string]any{}, "operations": []any{map[string]any{"op": "delete"}}}, map[string]any{}, "delete requires a field")
	Continue95NexaflowDirectNodeErr(t, jt, map[string]any{"value": map[string]any{}, "operations": []any{map[string]any{"op": "explode", "field": "a"}}}, map[string]any{}, "unknown transform op")
	out, err = Continue95NexaflowDirectNode(t, jt, map[string]any{
		"value":      map[string]any{"a": float64(1), "b": float64(2), "c": float64(3)},
		"operations": []any{map[string]any{"op": "pick", "field": "a"}, map[string]any{"op": "pick", "field": "b"}, map[string]any{"op": "rename", "field": "a", "to": "x"}, map[string]any{"op": "set_const", "field": "z", "value": float64(9)}, map[string]any{"op": "delete", "field": "b"}}},
		map[string]any{})
	if err != nil {
		t.Fatalf("transform happy: %v", err)
	}
	got, _ := out["value"].(map[string]any)
	if got["x"] != float64(1) || got["z"] != float64(9) || len(got) != 2 {
		t.Fatalf("transform result = %v", got)
	}

	lp := "nexaroute.control.loop_plan"
	Continue95NexaflowDirectNodeErr(t, lp, map[string]any{}, map[string]any{}, "must be an array")
	many := make([]any, 101)
	Continue95NexaflowDirectNodeErr(t, lp, map[string]any{"items": many}, map[string]any{}, "exceed 100")
	Continue95NexaflowDirectNodeErr(t, lp, map[string]any{"items": []any{1}}, map[string]any{"max_iterations": float64(0)}, "must be in [1,100]")
	out, err = Continue95NexaflowDirectNode(t, lp, map[string]any{"items": []any{"a", "b", "c"}}, map[string]any{"max_iterations": float64(2)})
	if err != nil || out["truncated"] != true {
		t.Fatalf("loop truncated = %v, err %v", out, err)
	}
	if iters, _ := out["iterations"].([]any); len(iters) != 2 {
		t.Fatalf("loop iterations = %d", len(iters))
	}

	mg := "nexaroute.control.merge"
	Continue95NexaflowDirectNodeErr(t, mg, map[string]any{}, map[string]any{}, "first must be an object")
	Continue95NexaflowDirectNodeErr(t, mg, map[string]any{"first": map[string]any{}}, map[string]any{}, "second must be an object")
	Continue95NexaflowDirectNodeErr(t, mg, map[string]any{"first": map[string]any{}, "second": map[string]any{}}, map[string]any{"conflict": "weird"}, "conflict must be")
	Continue95NexaflowDirectNodeErr(t, mg, map[string]any{"first": map[string]any{"k": float64(1)}, "second": map[string]any{"k": float64(2)}}, map[string]any{"conflict": "error"}, "merge_conflict")
	out, err = Continue95NexaflowDirectNode(t, mg, map[string]any{"first": map[string]any{"k": float64(1)}, "second": map[string]any{"k": float64(2)}}, map[string]any{"conflict": "first"})
	if err != nil {
		t.Fatalf("merge first: %v", err)
	}
	merged, _ := out["merged"].(map[string]any)
	if merged["k"] != float64(1) {
		t.Fatalf("merge first result = %v", merged)
	}
	big1, big2 := map[string]any{}, map[string]any{}
	for i := 0; i < 600; i++ {
		big1[fmt.Sprintf("a%d", i)] = float64(i)
		big2[fmt.Sprintf("b%d", i)] = float64(i)
	}
	Continue95NexaflowDirectNodeErr(t, mg, map[string]any{"first": big1, "second": big2}, map[string]any{}, "exceed 1000 keys")
}

func TestContinue95NexaflowControlWaitApprovalErrorHandler(t *testing.T) {
	wt := "nexaroute.control.wait"
	Continue95NexaflowDirectNodeErr(t, wt, map[string]any{}, map[string]any{"max_duration": "bogus"}, "positive duration")
	Continue95NexaflowDirectNodeErr(t, wt, map[string]any{"duration_ms": float64(60000)}, map[string]any{"max_duration": "1s"}, "exceeds configured maximum")
	Continue95NexaflowDirectNodeErr(t, wt, map[string]any{"duration_ms": float64(-1)}, map[string]any{}, "exceeds configured maximum")
	out, err := Continue95NexaflowDirectNode(t, wt, map[string]any{"duration_ms": float64(0)}, map[string]any{})
	if err != nil || out["waited_ms"] != float64(0) {
		t.Fatalf("wait 0 = %v, err %v", out, err)
	}

	ap := "nexaroute.control.approval"
	Continue95NexaflowDirectNodeErr(t, ap, map[string]any{}, map[string]any{}, "approval_required")
	Continue95NexaflowDirectNodeErr(t, ap, map[string]any{"decision": "maybe"}, map[string]any{}, "must be approved or rejected")
	Continue95NexaflowDirectNodeErr(t, ap, map[string]any{"decision": "approved", "requester": "alice"}, map[string]any{}, "identified approver")
	Continue95NexaflowDirectNodeErr(t, ap, map[string]any{"decision": "approved", "approver": "bob"}, map[string]any{}, "identified requester")
	Continue95NexaflowDirectNodeErr(t, ap, map[string]any{"decision": "approved", "approver": "alice", "requester": "alice"}, map[string]any{}, "must not be the requester")
	out, err = Continue95NexaflowDirectNode(t, ap, map[string]any{"decision": "rejected", "approver": "bob", "requester": "alice"}, map[string]any{})
	if err != nil || out["approved"] != false || out["approver"] != "bob" {
		t.Fatalf("approval rejected = %v, err %v", out, err)
	}

	eh := "nexaroute.control.error_handler"
	Continue95NexaflowDirectNodeErr(t, eh, map[string]any{}, map[string]any{"policy": "fail"}, "error_code is required")
	Continue95NexaflowDirectNodeErr(t, eh, map[string]any{"error_code": "boom"}, map[string]any{"policy": "panic"}, "policy must be")
	out, err = Continue95NexaflowDirectNode(t, eh, map[string]any{"error_code": "boom", "message": "token abc"}, map[string]any{"policy": "fallback"})
	if err != nil || out["action"] != "fallback" || out["error_code"] != "boom" {
		t.Fatalf("error_handler = %v, err %v", out, err)
	}
}

func TestContinue95NexaflowEngineCreateRunErrors(t *testing.T) {
	ctx := context.Background()
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)

	// Too many nodes / edges / bytes: bounds are checked before validation.
	big := Definition{SchemaVersion: DefinitionSchemaVersion, ID: "wf-big", Name: "big"}
	for i := 0; i < EngineMaxNodes+1; i++ {
		big.Nodes = append(big.Nodes, DefinitionNode{ID: fmt.Sprintf("n%d", i), NodeID: "test.echo", Version: SupportedVersion})
	}
	if _, err := eng.CreateRun(ctx, PublishedDefinition{Definition: big, Version: DefinitionVersion{DefinitionID: "wf-big", Revision: 1}}, principal(), nil, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("too many nodes: err = %v", err)
	}
	edgeDef := Definition{SchemaVersion: DefinitionSchemaVersion, ID: "wf-edge", Name: "edge"}
	for i := 0; i < EngineMaxEdges+1; i++ {
		edgeDef.Edges = append(edgeDef.Edges, DefinitionEdge{ID: fmt.Sprintf("e%d", i), From: "a", To: "b"})
	}
	if _, err := eng.CreateRun(ctx, PublishedDefinition{Definition: edgeDef, Version: DefinitionVersion{DefinitionID: "wf-edge", Revision: 1}}, principal(), nil, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("too many edges: err = %v", err)
	}
	hugeDef := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	hugeDef.Description = strings.Repeat("x", EngineMaxDefinitionJSONBytes+1)
	if _, err := eng.CreateRun(ctx, PublishedDefinition{Definition: hugeDef, Version: DefinitionVersion{DefinitionID: "wf-test", Revision: 1}}, principal(), nil, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize definition: err = %v", err)
	}
	// Invalid definition (unknown node) is rejected by validation.
	bad := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-bad", Name: "bad",
		Nodes:    []DefinitionNode{{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion}, {ID: "a", NodeID: "test.nope", Version: SupportedVersion}},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "a"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if _, err := eng.CreateRun(ctx, publishDef(t, store, bad), principal(), runInput(), ""); err == nil {
		t.Fatal("unknown node accepted")
	}
	// Oversize run input.
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	pub := publishDef(t, store, def)
	if _, err := eng.CreateRun(ctx, pub, principal(), map[string]any{"blob": strings.Repeat("x", MaxRunInputBytes+1)}, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize input: err = %v", err)
	}
	// Idempotency: same key + same revision returns the original run;
	// same key + a different definition is a conflict.
	first, err := eng.CreateRun(ctx, pub, principal(), runInput(), "c95-idem-key")
	if err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}
	again, err := eng.CreateRun(ctx, pub, principal(), runInput(), "c95-idem-key")
	if err != nil || again.ID != first.ID {
		t.Fatalf("idempotent CreateRun = %v, err %v", again, err)
	}
	other := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	other.ID = "wf-other"
	otherPub := publishDef(t, store, other)
	if _, err := eng.CreateRun(ctx, otherPub, principal(), runInput(), "c95-idem-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross-definition idempotency: want ErrIdempotencyConflict, got %v", err)
	}
}

func TestContinue95NexaflowEngineExecuteTerminalParkedBusyInflight(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	blocker := testSpec("test.blocker", []Field{{Name: "payload", Type: "object"}}, func(ctx context.Context, rc *RunContext) (map[string]any, error) {
		once.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return map[string]any{"done": true}, nil
		}
	})
	reg := testRegistry(t, blocker)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"b", "test.blocker", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	pub := publishDef(t, store, def)

	if _, err := eng.Execute(ctx, "c95-no-such-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Execute missing: want ErrNotFound, got %v", err)
	}
	run1, err := eng.CreateRun(ctx, pub, principal(), runInput(), "c95-k1")
	if err != nil {
		t.Fatalf("CreateRun 1: %v", err)
	}
	run2, err := eng.CreateRun(ctx, pub, principal(), runInput(), "c95-k2")
	if err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := eng.Execute(ctx, run1.ID)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocker node never started")
	}
	if _, err := eng.Execute(ctx, run1.ID); !errors.Is(err, ErrRunInFlight) {
		t.Fatalf("concurrent Execute same run: want ErrRunInFlight, got %v", err)
	}
	if _, err := eng.Execute(ctx, run2.ID); !errors.Is(err, ErrDefinitionBusy) {
		t.Fatalf("concurrent Execute same definition: want ErrDefinitionBusy, got %v", err)
	}
	close(release)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("first Execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Execute never finished")
	}
	// Terminal runs re-execute as a no-op.
	final, err := eng.Execute(ctx, run1.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("re-Execute terminal = %+v, err %v", final, err)
	}
	// Parked runs refuse Execute.
	if _, err := store.UpdateRunState(ctx, run2.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("run2 -> running: %v", err)
	}
	if _, err := store.UpdateRunState(ctx, run2.ID, RunRunning, RunNeedsManualAction, "", ""); err != nil {
		t.Fatalf("run2 -> parked: %v", err)
	}
	if _, err := eng.Execute(ctx, run2.ID); !errors.Is(err, ErrRunParked) {
		t.Fatalf("Execute parked: want ErrRunParked, got %v", err)
	}
}

func TestContinue95NexaflowEngineDecideApprovalErrors(t *testing.T) {
	ctx := context.Background()
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := eng.DecideApproval(ctx, run.ID, "a", "bob", "maybe"); err == nil || !strings.Contains(err.Error(), "must be approved or rejected") {
		t.Fatalf("invalid decision: err = %v", err)
	}
	if _, err := eng.DecideApproval(ctx, run.ID, "a", "alice", "approved"); err == nil || !strings.Contains(err.Error(), "must not be the requester") {
		t.Fatalf("self-approval: err = %v", err)
	}
	if _, err := eng.DecideApproval(ctx, run.ID, "a", "bob", "approved"); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("not-waiting: want ErrNotWaiting, got %v", err)
	}
	if _, err := eng.DecideApproval(ctx, "c95-no-such-run", "a", "bob", "approved"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run: want ErrNotFound, got %v", err)
	}
}

func TestContinue95NexaflowEngineDecideApprovalPolicyRejected(t *testing.T) {
	ctx := context.Background()
	policySpec := testSpec("test.policygate", []Field{}, func(context.Context, *RunContext) (map[string]any, error) {
		return nil, NewNodeError("approval_required", false, "needs operator policy")
	})
	policySpec.RequiresOperatorPolicy = true
	reg := testRegistry(t, policySpec)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"gate", "test.policygate", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	waiting, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute to waiting: %v", err)
	}
	if waiting.State != RunWaitingForApproval {
		t.Fatalf("run state = %q, want waiting_for_approval", waiting.State)
	}
	states := nodeStates(t, store, run.ID)
	if states["gate"].State != NodeWaiting {
		t.Fatalf("gate state = %q, want waiting", states["gate"].State)
	}
	appr, err := eng.DecideApproval(ctx, run.ID, "gate", "bob", "rejected")
	if err != nil {
		t.Fatalf("DecideApproval rejected: %v", err)
	}
	if appr.Decision != "rejected" || appr.Approver != "bob" {
		t.Fatalf("approval = %+v", appr)
	}
	states = nodeStates(t, store, run.ID)
	if states["gate"].State != NodeFailed || states["gate"].ErrorCode != "approval_rejected" {
		t.Fatalf("gate after rejection = %+v", states["gate"])
	}
	final, err := store.GetRun(ctx, run.ID)
	if err != nil || final.State != RunFailed {
		t.Fatalf("run after policy rejection = %+v, err %v", final, err)
	}
}

func TestContinue95NexaflowEngineApprovalResumeToSuccess(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	eng := mustEngine(t, store, DefaultRegistry())
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "gate", NodeID: "nexaroute.control.approval", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "gate"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	waiting, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute to waiting: %v", err)
	}
	if waiting.State != RunWaitingForApproval {
		t.Fatalf("run state = %q, want waiting_for_approval", waiting.State)
	}
	if _, err := eng.DecideApproval(ctx, run.ID, "gate", "bob", "approved"); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute after approval: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", final.State)
	}
	states := nodeStates(t, store, run.ID)
	if states["gate"].State != NodeSucceeded || states["gate"].Output["approved"] != true {
		t.Fatalf("gate = %+v", states["gate"])
	}
}

func TestContinue95NexaflowEngineCancelAndForceFail(t *testing.T) {
	ctx := context.Background()
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	pub := publishDef(t, store, def)

	if _, err := eng.CancelRun(ctx, "c95-no-such-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CancelRun missing: want ErrNotFound, got %v", err)
	}
	created, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cancelled, err := eng.CancelRun(ctx, created.ID)
	if err != nil {
		t.Fatalf("CancelRun created: %v", err)
	}
	if cancelled.State != RunCancelled {
		t.Fatalf("cancelled state = %q", cancelled.State)
	}
	states := nodeStates(t, store, created.ID)
	if states["a"].State != NodeCancelled {
		t.Fatalf("node after cancel = %q", states["a"].State)
	}
	// Cancelling a terminal run returns it unchanged.
	succeeded, err := eng.CreateRun(ctx, pub, principal(), runInput(), "c95-succeed")
	if err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}
	if _, err := eng.Execute(ctx, succeeded.ID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	again, err := eng.CancelRun(ctx, succeeded.ID)
	if err != nil || again.State != RunSucceeded {
		t.Fatalf("cancel terminal = %+v, err %v", again, err)
	}
	// Force-fail a parked run; empty code defaults.
	parked, err := eng.CreateRun(ctx, pub, principal(), runInput(), "c95-parked")
	if err != nil {
		t.Fatalf("CreateRun 3: %v", err)
	}
	if _, err := store.UpdateRunState(ctx, parked.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("parked -> running: %v", err)
	}
	if _, err := store.UpdateRunState(ctx, parked.ID, RunRunning, RunNeedsManualAction, "", ""); err != nil {
		t.Fatalf("parked -> needs_manual_action: %v", err)
	}
	failed, err := eng.ForceFailRun(ctx, parked.ID, "", "operator gave up")
	if err != nil {
		t.Fatalf("ForceFailRun: %v", err)
	}
	if failed.State != RunFailed || failed.ErrorCode != "operator_force_failed" {
		t.Fatalf("force-failed = %+v", failed)
	}
	// Force-failing a terminal run to a *different* state is rejected
	// (same-state force-fail is idempotent by design, so the
	// terminal case here uses a succeeded run, not the failed one).
	term, err := eng.CreateRun(ctx, pub, principal(), runInput(), "c95-term")
	if err != nil {
		t.Fatalf("CreateRun terminal: %v", err)
	}
	if _, err := eng.Execute(ctx, term.ID); err != nil {
		t.Fatalf("Execute terminal: %v", err)
	}
	if _, err := eng.ForceFailRun(ctx, term.ID, "x", "y"); err == nil {
		t.Fatal("ForceFailRun on succeeded run accepted")
	}
}

func TestContinue95NexaflowEngineRecover(t *testing.T) {
	ctx := context.Background()
	// Expired approval deadline: engine clock fixed past the deadline.
	now := time.Now()
	store := NewMemoryStore()
	eng := mustEngine(t, store, DefaultRegistry(), func(o *EngineOptions) {
		o.ApprovalTimeout = time.Second
		o.Now = func() time.Time { return now }
	})
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "gate", NodeID: "nexaroute.control.approval", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "gate"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := eng.Execute(ctx, run.ID); err != nil {
		t.Fatalf("Execute to waiting: %v", err)
	}
	later := mustEngine(t, store, DefaultRegistry(), func(o *EngineOptions) {
		o.ApprovalTimeout = time.Second
		o.Now = func() time.Time { return now.Add(2 * time.Second) }
	})
	if err := later.Recover(ctx); err != nil {
		t.Fatalf("Recover expired approval: %v", err)
	}
	states := nodeStates(t, store, run.ID)
	if states["gate"].State != NodeFailed || states["gate"].ErrorCode != "approval_expired" {
		t.Fatalf("gate after recover = %+v", states["gate"])
	}

	// Undispatched claimed attempt with an expired lease re-queues.
	store2 := NewMemoryStore()
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	eng2 := mustEngine(t, store2, testRegistry(t, echo))
	def2 := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	pub2 := publishDef(t, store2, def2)
	run2, err := eng2.CreateRun(ctx, pub2, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}
	if _, err := store2.UpdateRunState(ctx, run2.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("run2 -> running: %v", err)
	}
	execs, err := store2.GetNodeExecutions(ctx, run2.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	claimed := execs[0]
	claimed.State = NodeClaimed
	claimed.LeaseOwner = "other-engine"
	claimed.LeaseUntil = time.Now().Add(-time.Hour)
	if _, err := store2.SaveNodeExecution(ctx, claimed, NodePending); err != nil {
		t.Fatalf("claim node: %v", err)
	}
	if err := eng2.Recover(ctx); err != nil {
		t.Fatalf("Recover requeue: %v", err)
	}
	states = nodeStates(t, store2, run2.ID)
	if states["a"].State != NodePending || states["a"].LeaseOwner != "" {
		t.Fatalf("requeued node = %+v", states["a"])
	}

	// Dispatched external attempt with an expired lease parks ambiguous.
	store3 := NewMemoryStore()
	ext := testSpec("test.external", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		return map[string]any{"v": "x"}, nil
	})
	ext.SideEffect = SideEffectExternal
	eng3 := mustEngine(t, store3, testRegistry(t, ext))
	def3 := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.external", nil})
	pub3 := publishDef(t, store3, def3)
	run3, err := eng3.CreateRun(ctx, pub3, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun 3: %v", err)
	}
	if _, err := store3.UpdateRunState(ctx, run3.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("run3 -> running: %v", err)
	}
	execs, err = store3.GetNodeExecutions(ctx, run3.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions 3: %v", err)
	}
	disp := execs[0]
	disp.State = NodeClaimed
	disp.Dispatched = true
	disp.LeaseOwner = "other-engine"
	disp.LeaseUntil = time.Now().Add(-time.Hour)
	if _, err := store3.SaveNodeExecution(ctx, disp, NodePending); err != nil {
		t.Fatalf("claim dispatched node: %v", err)
	}
	if err := eng3.Recover(ctx); err != nil {
		t.Fatalf("Recover park: %v", err)
	}
	states = nodeStates(t, store3, run3.ID)
	if states["a"].State != NodeNeedsManualAction || !states["a"].Ambiguous || states["a"].ErrorCode != "ambiguous_recovery" {
		t.Fatalf("parked node = %+v", states["a"])
	}
}

func TestContinue95NexaflowEngineRetryAndAmbiguousTimeout(t *testing.T) {
	ctx := context.Background()
	var calls int
	flaky := testSpec("test.flaky", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, NewNodeError("boom", true, "transient boom")
		}
		return map[string]any{"v": "recovered"}, nil
	})
	flaky.Retry = RetryPolicy{MaxAttempts: 2, RetryableCodes: []string{"boom"}}
	reg := testRegistry(t, flaky)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.flaky", nil})
	store := NewMemoryStore()
	eng := mustEngine(t, store, reg)
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute flaky: %v", err)
	}
	if final.State != RunSucceeded || calls != 2 {
		t.Fatalf("flaky run = %q, calls = %d", final.State, calls)
	}
	execs, err := store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	var aAttempts []NodeExecution
	for _, e := range execs {
		if e.NodeID == "a" {
			aAttempts = append(aAttempts, e)
		}
	}
	if len(aAttempts) != 2 || aAttempts[1].State != NodeSucceeded {
		t.Fatalf("flaky attempts for node a = %+v", aAttempts)
	}

	billable := testSpec("test.billable", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		return nil, NewNodeError("timeout", false, "upstream timed out")
	})
	billable.SideEffect = SideEffectBillable
	reg2 := testRegistry(t, billable)
	def2 := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.billable", nil})
	store2 := NewMemoryStore()
	eng2 := mustEngine(t, store2, reg2)
	pub2 := publishDef(t, store2, def2)
	run2, err := eng2.CreateRun(ctx, pub2, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun billable: %v", err)
	}
	final2, err := eng2.Execute(ctx, run2.ID)
	if err != nil {
		t.Fatalf("Execute billable: %v", err)
	}
	if final2.State != RunNeedsManualAction {
		t.Fatalf("billable timeout run = %q, want needs_manual_action", final2.State)
	}
	states := nodeStates(t, store2, run2.ID)
	if states["a"].State != NodeNeedsManualAction || !states["a"].Ambiguous || states["a"].ErrorCode != "ambiguous_timeout" {
		t.Fatalf("billable node = %+v", states["a"])
	}
}

func TestContinue95NexaflowEngineUnknownNodeAndBadInput(t *testing.T) {
	ctx := context.Background()
	// Registry changed after creation: the sweep fails the node with
	// node_not_found instead of dispatching blindly.
	store := NewMemoryStore()
	fullReg := DefaultRegistry()
	eng := mustEngine(t, store, fullReg)
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "jt", NodeID: "nexaroute.control.json_transform", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "jt"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A second engine with a registry lacking json_transform drives it.
	// CURRENT CONTRACT (reconciliation): validNodeTransition
	// explicitly allows claimed->failed as the fail-without-dispatch
	// path, so Execute settles the run as failed with the node's
	// error code instead of surfacing ErrStateConflict. An earlier
	// version of this test asserted ErrStateConflict, which was the
	// observed behaviour on the pre-reconciliation engine; that
	// expectation is stale against current main and is corrected
	// here, not the implementation.
	triggerOnly := testRegistry(t)
	eng2 := mustEngine(t, store, triggerOnly)
	final, err := eng2.Execute(ctx, run.ID)
	if err != nil {
		t.Fatalf("Execute unknown node: want nil error, got %v", err)
	}
	if final.State != RunFailed || final.ErrorCode != "node_not_found" {
		t.Fatalf("Execute unknown node: run = %q/%q, want failed/node_not_found", final.State, final.ErrorCode)
	}
	states := nodeStates(t, store, run.ID)
	if states["jt"].State != NodeFailed || states["jt"].ErrorCode != "node_not_found" {
		t.Fatalf("unknown node jt = %+v, want failed/node_not_found", states["jt"])
	}

	// Unresolvable input mapping fails the node with input_resolution.
	store2 := NewMemoryStore()
	eng3 := mustEngine(t, store2, fullReg)
	def2 := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "jt", NodeID: "nexaroute.control.json_transform", Version: SupportedVersion, Input: map[string]any{"value": map[string]any{"$ref": "input.nope.deep"}}},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "jt"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	pub2 := publishDef(t, store2, def2)
	run2, err := eng3.CreateRun(ctx, pub2, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}
	// Same fail-without-dispatch path as the unknown-node case: the
	// input_resolution failure settles the run as failed, nil error.
	final2, err := eng3.Execute(ctx, run2.ID)
	if err != nil {
		t.Fatalf("Execute bad input: want nil error, got %v", err)
	}
	if final2.State != RunFailed || final2.ErrorCode != "input_resolution" {
		t.Fatalf("Execute bad input: run = %q/%q, want failed/input_resolution", final2.State, final2.ErrorCode)
	}
	states2 := nodeStates(t, store2, run2.ID)
	if states2["jt"].State != NodeFailed || states2["jt"].ErrorCode != "input_resolution" {
		t.Fatalf("bad-input node jt = %+v, want failed/input_resolution", states2["jt"])
	}
}

func TestContinue95NexaflowFileStorePersistenceAndCorruption(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenFileStore(""); err == nil {
		t.Fatal("empty file store path accepted")
	}
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := OpenFileStore(badPath); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt file store: err = %v", err)
	}
	path := filepath.Join(dir, "sub", "store.json")
	fs, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-file", Name: "file",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "a", NodeID: "test.echo", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "a"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if err := fs.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	pub, err := fs.Publish(ctx, def.ID, "tester")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	run := Run{ID: "run-file-1", DefinitionID: def.ID, Revision: pub.Version.Revision, Definition: pub.Definition, State: RunCreated}
	if err := fs.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	reopened, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetDraft(ctx, def.ID)
	if err != nil || got.Name != "file" {
		t.Fatalf("reopened draft = %+v, err %v", got, err)
	}
	gotRun, err := reopened.GetRun(ctx, "run-file-1")
	if err != nil || gotRun.State != RunCreated {
		t.Fatalf("reopened run = %+v, err %v", gotRun, err)
	}
	revs, err := reopened.ListRevisions(ctx, def.ID)
	if err != nil || len(revs) != 1 || revs[0].Version.Revision != 1 {
		t.Fatalf("reopened revisions = %v, err %v", revs, err)
	}
}

func TestContinue95NexaflowGatewayClient(t *testing.T) {
	ctx := context.Background()
	var lastAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/chat/completions":
			fmt.Fprint(w, `{"model":"vendor/model","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
		case r.URL.Path == "/mcp":
			var req struct {
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "tools/list" {
				fmt.Fprint(w, `{"result":{"tools":[{"name":"tools__search_files","description":"search","inputSchema":{}},{"name":"other__read_file","description":"other"}]}}`)
			} else {
				fmt.Fprint(w, `{"result":{"content":"called"}}`)
			}
		case r.URL.Path == "/v1/video/jobs" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"job_id":"job-9","state":"queued"}`)
		case strings.HasPrefix(r.URL.Path, "/v1/video/jobs/job-9/assets/"):
			fmt.Fprint(w, `asset-bytes`)
		case r.URL.Path == "/v1/video/jobs/job-9":
			fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
		case r.URL.Path == "/echo":
			fmt.Fprint(w, `{"ok":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"nope"}`)
		}
	}))
	defer srv.Close()

	client := &GatewayClient{
		BaseURL: srv.URL, AuthTokenRef: "tok-ref",
		SecretResolver: func(ref string) (string, error) {
			if ref != "tok-ref" {
				return "", errors.New("unknown ref")
			}
			return "test-token", nil
		},
	}
	chat, err := client.Chat(ctx, ChatRequest{Selector: "vendor/model", Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if chat.Text != "hello" || chat.TotalTokens != 7 || lastAuth != "Bearer test-token" {
		t.Fatalf("chat = %+v, auth = %q", chat, lastAuth)
	}
	tools, err := client.MCPList(ctx, "tools")
	if err != nil {
		t.Fatalf("MCPList: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "search_files" {
		t.Fatalf("MCPList filtered = %+v", tools)
	}
	called, err := client.MCPCall(ctx, "tools", "search_files", map[string]any{"q": "x"})
	if err != nil || called["content"] != "called" {
		t.Fatalf("MCPCall = %v, err %v", called, err)
	}
	job, err := client.VideoSubmit(ctx, VideoSubmitRequest{Prompt: "a cat", Mode: "t2v"})
	if err != nil || job.JobID != "job-9" || job.State != "queued" {
		t.Fatalf("VideoSubmit = %+v, err %v", job, err)
	}
	status, err := client.VideoStatus(ctx, "job-9")
	if err != nil || len(status.Assets) != 1 || status.Assets[0].Filename != "out.mp4" {
		t.Fatalf("VideoStatus = %+v, err %v", status, err)
	}
	meta, body, err := client.VideoAsset(ctx, "job-9", "a1")
	if err != nil || meta.ID != "a1" || string(body) != "asset-bytes" {
		t.Fatalf("VideoAsset = %+v %q, err %v", meta, body, err)
	}
	if _, _, err := client.VideoAsset(ctx, "job-9", "not-listed"); err == nil || !strings.Contains(err.Error(), "asset_not_found") {
		t.Fatalf("unlisted asset: err = %v", err)
	}
	resp, err := client.HTTPDo(ctx, http.MethodGet, srv.URL+"/echo", nil, nil)
	if err != nil || resp.Status != 200 {
		t.Fatalf("HTTPDo = %+v, err %v", resp, err)
	}
	if _, err := client.HTTPDo(ctx, http.MethodPost, srv.URL+"/echo", map[string]string{"X-Api-Key": "secret"}, nil); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("credential header: err = %v", err)
	}
	if _, err := client.HTTPDo(ctx, http.MethodPost, srv.URL+"/echo", nil, make([]byte, (1<<20)+1)); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversize body: err = %v", err)
	}

	// Error paths: non-2xx, undecodable body, oversize response, bad secret.
	bad := &GatewayClient{BaseURL: srv.URL}
	if _, err := bad.Chat(ctx, ChatRequest{Selector: "x", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("no-auth chat should still succeed against test server: %v", err)
	}
	missing := &GatewayClient{BaseURL: srv.URL + "/gone"}
	_ = missing
	notFound := &GatewayClient{BaseURL: srv.URL}
	if _, err := notFound.VideoStatus(ctx, "no-such-job"); err == nil {
		t.Fatal("VideoStatus 404 accepted")
	}
	tiny := &GatewayClient{BaseURL: srv.URL, MaxResponseBytes: 10}
	if _, err := tiny.Chat(ctx, ChatRequest{Selector: "x", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}); err == nil || !strings.Contains(err.Error(), "output_too_large") {
		t.Fatalf("oversize response: err = %v", err)
	}
	badSecret := &GatewayClient{BaseURL: srv.URL, AuthTokenRef: "nope", SecretResolver: func(string) (string, error) {
		return "", errors.New("boom")
	}}
	if _, err := badSecret.Chat(ctx, ChatRequest{Selector: "x"}); err == nil || !strings.Contains(err.Error(), "secret_error") {
		t.Fatalf("bad secret: err = %v", err)
	}
	// A server returning garbage JSON fails decode, not success.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{broken`)
	}))
	defer garbage.Close()
	if _, err := (&GatewayClient{BaseURL: garbage.URL}).Chat(ctx, ChatRequest{Selector: "x"}); err == nil || !strings.Contains(err.Error(), "decode failed") {
		t.Fatalf("garbage JSON: err = %v", err)
	}
	// Unreachable gateway fails as backend_error.
	dead := &GatewayClient{BaseURL: "http://127.0.0.1:1"}
	if _, err := dead.Chat(ctx, ChatRequest{Selector: "x"}); err == nil || !strings.Contains(err.Error(), "backend_error") {
		t.Fatalf("dead gateway: err = %v", err)
	}
}

func Continue95NexaflowWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func Continue95NexaflowMigDef(id, name string) Definition {
	return Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: id, Name: name,
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "work", NodeID: "nexaroute.control.json_transform", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "work"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
}

func TestContinue95NexaflowMigrateHappyPath(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	defV1 := Continue95NexaflowMigDef("wf-mig", "mig-v1")
	defV2 := Continue95NexaflowMigDef("wf-mig", "mig-v2")
	hash1, err := ContentHash(&defV1)
	if err != nil {
		t.Fatalf("hash v1: %v", err)
	}
	hash2, err := ContentHash(&defV2)
	if err != nil {
		t.Fatalf("hash v2: %v", err)
	}
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), defV1)
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
		Definition: &defV1, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash1, Status: DefinitionSuperseded},
	})
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "2.json"), legacyVersionRecord{
		Definition: &defV2, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 2, ContentHash: hash2, Status: DefinitionPublished},
	})
	now := time.Now()
	fin := now
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "run-mig-1.json"), legacyRun{
		ID: "run-mig-1", DefinitionID: "wf-mig", Revision: 2, Actor: "alice", Status: "succeeded",
		Input:     map[string]any{"api_key": "sekret", "x": float64(1)},
		CreatedAt: now, UpdatedAt: now, FinishedAt: &fin,
		Steps: []legacyStep{
			{NodeID: "start", Attempt: 0, Status: "succeeded", Output: map[string]any{"payload": map[string]any{"x": float64(1)}}, StartedAt: now, Finished: &fin},
			{NodeID: "work", Attempt: 0, Status: "succeeded", Output: map[string]any{"api_key": "sekret2"}, StartedAt: now, Finished: &fin},
		},
	})
	dst := NewMemoryStore()
	rep, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry())
	if err != nil {
		t.Fatalf("MigratePhase8Dir: %v", err)
	}
	if rep.Drafts != 1 || rep.Versions != 2 || rep.Runs != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.RedactedInputs != 1 || rep.RedactedOutputs != 1 {
		t.Fatalf("redaction counts = %+v", rep)
	}
	draft, err := dst.GetDraft(ctx, "wf-mig")
	if err != nil || draft.Name != "mig-v2" {
		t.Fatalf("migrated draft = %+v, err %v", draft, err)
	}
	revs, err := dst.ListRevisions(ctx, "wf-mig")
	if err != nil || len(revs) != 2 || revs[0].Version.Status != DefinitionSuperseded || revs[1].Version.Status != DefinitionPublished {
		t.Fatalf("migrated revisions = %+v, err %v", revs, err)
	}
	got, err := dst.GetRun(ctx, "run-mig-1")
	if err != nil {
		t.Fatalf("migrated run: %v", err)
	}
	if got.State != RunSucceeded || got.Principal != "alice" || got.Input["api_key"] == "sekret" {
		t.Fatalf("migrated run = %+v", got)
	}
	execs, err := dst.GetNodeExecutions(ctx, "run-mig-1")
	if err != nil || len(execs) != 2 {
		t.Fatalf("migrated executions = %d, err %v", len(execs), err)
	}
	for _, e := range execs {
		if e.State != NodeSucceeded {
			t.Fatalf("migrated execution = %+v", e)
		}
	}
}

func TestContinue95NexaflowMigrateErrors(t *testing.T) {
	ctx := context.Background()
	mkSrc := func(t *testing.T) string {
		t.Helper()
		return t.TempDir()
	}
	// Blocked non-terminal run.
	src := mkSrc(t)
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r1.json"), legacyRun{
		ID: "r1", DefinitionID: "wf-x", Revision: 1, Actor: "alice", Status: "running",
	})
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("blocked run: err = %v", err)
	}
	// Draft carrying on_error is refused, never silently dropped.
	src = mkSrc(t)
	def := Continue95NexaflowMigDef("wf-err", "err")
	raw, _ := json.Marshal(def)
	var rawMap map[string]any
	_ = json.Unmarshal(raw, &rawMap)
	rawMap["on_error"] = "retry"
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-err.json"), rawMap)
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "on_error") {
		t.Fatalf("on_error draft: err = %v", err)
	}
	// Corrupt draft JSON.
	src = mkSrc(t)
	if err := os.MkdirAll(filepath.Join(src, "drafts"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "drafts", "bad.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "corrupt JSON") {
		t.Fatalf("corrupt draft: err = %v", err)
	}
	// Version hash mismatch.
	src = mkSrc(t)
	defV := Continue95NexaflowMigDef("wf-hash", "hash")
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-hash", "1.json"), legacyVersionRecord{
		Definition: &defV, Version: &DefinitionVersion{DefinitionID: "wf-hash", Revision: 1, ContentHash: "wrong", Status: DefinitionPublished},
	})
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
		t.Fatalf("hash mismatch: err = %v", err)
	}
	// Non-numeric version file.
	src = mkSrc(t)
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-nn", "abc.json"), legacyVersionRecord{
		Definition: &defV, Version: &DefinitionVersion{DefinitionID: "wf-nn", Revision: 1, ContentHash: "x", Status: DefinitionPublished},
	})
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "non-numeric version file") {
		t.Fatalf("non-numeric version: err = %v", err)
	}
	// Revision gap: only revision 2 present.
	src = mkSrc(t)
	hashV, err := ContentHash(&defV)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-gap", "2.json"), legacyVersionRecord{
		Definition: &defV, Version: &DefinitionVersion{DefinitionID: "wf-gap", Revision: 2, ContentHash: hashV, Status: DefinitionPublished},
	})
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "revision gap") {
		t.Fatalf("revision gap: err = %v", err)
	}
	// Run referencing an unknown version.
	src = mkSrc(t)
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r9.json"), legacyRun{
		ID: "r9", DefinitionID: "wf-unknown", Revision: 9, Actor: "alice", Status: "succeeded",
	})
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "unknown version") {
		t.Fatalf("unknown version run: err = %v", err)
	}
	// Destination already holds the draft.
	src = mkSrc(t)
	defD := Continue95NexaflowMigDef("wf-dup", "dup")
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-dup.json"), defD)
	dst := NewMemoryStore()
	if err := dst.SaveDraft(ctx, defD); err != nil {
		t.Fatalf("preseed: %v", err)
	}
	if _, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "already holds draft") {
		t.Fatalf("destination collision: err = %v", err)
	}
}

func TestContinue95NexaflowServiceFacade(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	reg := DefaultRegistry()
	eng := mustEngine(t, store, reg)
	if _, err := NewService(nil, eng, reg, nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewService(store, nil, reg, nil); err == nil {
		t.Fatal("nil engine accepted")
	}
	if _, err := NewService(store, eng, nil, nil); err == nil {
		t.Fatal("nil registry accepted")
	}
	guard := &Continue95NexaflowGuard{}
	svc, err := NewService(store, eng, reg, guard)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-svc", Name: "svc",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "a", NodeID: "nexaroute.control.json_transform", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "a"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if err := svc.Validate(&def); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := svc.Publish(ctx, def.ID, authz.Identity{}); err == nil {
		t.Fatal("anonymous publish accepted")
	}
	pub, err := svc.Publish(ctx, def.ID, principal())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	latest, err := svc.LatestPublished(ctx, def.ID)
	if err != nil || latest.Version.Revision != pub.Version.Revision {
		t.Fatalf("LatestPublished = %+v, err %v", latest, err)
	}
	if _, err := svc.LatestPublished(ctx, "c95-no-such-def"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestPublished missing: want ErrNotFound, got %v", err)
	}
	if _, err := svc.StartRun(ctx, pub, authz.Identity{}, nil, ""); err == nil {
		t.Fatal("anonymous StartRun accepted")
	}
	run, err := svc.StartRun(ctx, pub, principal(), runInput(), "c95-svc-run")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := svc.ApproveRun(ctx, run.ID, "a", authz.Identity{}, "approved"); err == nil {
		t.Fatal("anonymous ApproveRun accepted")
	}
	if _, _, err := svc.GetRun(ctx, run.ID, authz.Identity{Subject: "mallory", Roles: []authz.Role{authz.RoleViewer}}); err == nil {
		t.Fatal("foreign principal read accepted")
	}
	got, execs, err := svc.GetRun(ctx, run.ID, principal())
	if err != nil || got.ID != run.ID || len(execs) == 0 {
		t.Fatalf("GetRun = %+v %d, err %v", got, len(execs), err)
	}
	visible, err := svc.ListRuns(ctx, RunFilter{}, principal())
	if err != nil || len(visible) == 0 {
		t.Fatalf("ListRuns owner = %d, err %v", len(visible), err)
	}
	viewerVisible, err := svc.ListRuns(ctx, RunFilter{}, authz.Identity{Subject: "mallory", Roles: []authz.Role{authz.RoleViewer}})
	if err != nil {
		t.Fatalf("ListRuns viewer: %v", err)
	}
	for _, r := range viewerVisible {
		if r.Principal == "alice" {
			t.Fatalf("viewer sees owner's run: %+v", r)
		}
	}
	final, err := svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	// Budget release: the run_finished event for a terminal run
	// releases the reservation recorded at StartRun.
	svc.HandleEngineEvent(AuditEvent{RunID: run.ID, Kind: "run_finished", State: string(final.State)})
	if _, released := guard.counts(); released != 1 {
		t.Fatalf("guard released = %d, want 1", released)
	}
	// Reconcile is idempotent: a second release must not re-fire.
	if err := svc.ReconcileBudget(ctx); err != nil {
		t.Fatalf("ReconcileBudget: %v", err)
	}
	if _, released := guard.counts(); released != 1 {
		t.Fatalf("guard released after reconcile = %d, want 1", released)
	}
	if _, err := svc.CancelRun(ctx, run.ID, authz.Identity{Subject: "mallory", Roles: []authz.Role{authz.RoleViewer}}); err == nil {
		t.Fatal("foreign CancelRun accepted")
	}
	if err := svc.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	audit := svc.Audit()
	if len(audit) == 0 {
		t.Fatal("audit log empty")
	}
	// Guard failure fails admission closed and creates no run:
	// StartRun must fail before touching the engine.
	failGuard := &Continue95NexaflowGuard{fail: true}
	store3 := NewMemoryStore()
	eng3 := mustEngine(t, store3, reg)
	if err := store3.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft 3: %v", err)
	}
	pub3, err := store3.Publish(ctx, def.ID, "tester")
	if err != nil {
		t.Fatalf("Publish 3: %v", err)
	}
	svc3, err := NewService(store3, eng3, reg, failGuard)
	if err != nil {
		t.Fatalf("NewService 3: %v", err)
	}
	if _, err := svc3.StartRun(ctx, pub3, principal(), runInput(), "c95-fail"); err == nil || !strings.Contains(err.Error(), "budget reservation failed") {
		t.Fatalf("guard failure: err = %v", err)
	}
}

// Continue95NexaflowChatErrBackend wraps the fake backend and fails Chat,
// so the LLM node's gateway-error passthrough can be exercised.
type Continue95NexaflowChatErrBackend struct {
	*FakeBackend
	Err error
}

func (b *Continue95NexaflowChatErrBackend) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, b.Err
}

// Continue95NexaflowDirectNodeBackend runs a registry node directly with an
// explicit backend, bypassing Registry.Execute's schema validation.
func Continue95NexaflowDirectNodeBackend(t *testing.T, id string, input, config map[string]any, backend Backend) (map[string]any, error) {
	t.Helper()
	reg := DefaultRegistry()
	spec, err := reg.Lookup(id, SupportedVersion)
	if err != nil {
		t.Fatalf("lookup %s: %v", id, err)
	}
	return spec.Run(context.Background(), &RunContext{Principal: ownerPrincipal(), Input: input, Config: config, Backend: backend})
}

// Continue95NexaflowWebhookSig computes the webhook HMAC used by the trigger
// node: hex HMAC-SHA256 over timestamp + "." + body.
func Continue95NexaflowWebhookSig(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// Continue95NexaflowHijackTruncate answers with a truncated body: the
// headers promise 100 bytes but the connection closes after 5, so the
// client sees an unexpected EOF while reading.
func Continue95NexaflowHijackTruncate(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\nshort"))
	_ = conn.Close()
}

// Continue95NexaflowHijackClose drops the connection without any response.
func Continue95NexaflowHijackClose(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

func TestContinue95NexaflowRegistryAndHelpers(t *testing.T) {
	echo := func(context.Context, *RunContext) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	// NewRegistry rejection paths.
	bad := []struct {
		name string
		spec *NodeSpec
	}{
		{"nil", nil},
		{"version", &NodeSpec{ID: "x", Version: "v2", Run: echo}},
		{"no-id", &NodeSpec{Version: SupportedVersion, Run: echo}},
		{"no-run", &NodeSpec{ID: "x", Version: SupportedVersion}},
		{"retry-low", &NodeSpec{ID: "x", Version: SupportedVersion, Run: echo, Retry: RetryPolicy{MaxAttempts: 0}, DefaultTimeout: time.Second, MaxTimeout: time.Second, MaxInputBytes: 1, MaxOutputBytes: 1}},
		{"retry-high", &NodeSpec{ID: "x", Version: SupportedVersion, Run: echo, Retry: RetryPolicy{MaxAttempts: 4}, DefaultTimeout: time.Second, MaxTimeout: time.Second, MaxInputBytes: 1, MaxOutputBytes: 1}},
		{"timeout-zero", &NodeSpec{ID: "x", Version: SupportedVersion, Run: echo, Retry: RetryPolicy{MaxAttempts: 1}, MaxTimeout: time.Second, MaxInputBytes: 1, MaxOutputBytes: 1}},
		{"timeout-inverted", &NodeSpec{ID: "x", Version: SupportedVersion, Run: echo, Retry: RetryPolicy{MaxAttempts: 1}, DefaultTimeout: 2 * time.Second, MaxTimeout: time.Second, MaxInputBytes: 1, MaxOutputBytes: 1}},
		{"bytes-zero", &NodeSpec{ID: "x", Version: SupportedVersion, Run: echo, Retry: RetryPolicy{MaxAttempts: 1}, DefaultTimeout: time.Second, MaxTimeout: time.Second}},
	}
	for _, tc := range bad {
		if _, err := NewRegistry([]*NodeSpec{tc.spec}); err == nil {
			t.Fatalf("NewRegistry %s: accepted invalid spec", tc.name)
		}
	}
	if _, err := NewRegistry([]*NodeSpec{testSpec("dup", nil, echo), testSpec("dup", nil, echo)}); err == nil {
		t.Fatal("NewRegistry duplicate accepted")
	}
	reg, err := NewRegistry([]*NodeSpec{testSpec("one", nil, echo)})
	if err != nil {
		t.Fatalf("NewRegistry valid: %v", err)
	}
	if _, err := reg.Lookup("one", "v2"); err == nil {
		t.Fatal("Lookup bad version accepted")
	}
	if _, err := reg.Lookup("missing", SupportedVersion); err == nil {
		t.Fatal("Lookup unknown id accepted")
	}
	if _, err := reg.Lookup("one", SupportedVersion); err != nil {
		t.Fatalf("Lookup valid: %v", err)
	}
	// NodeError helpers.
	var nilErr *NodeError
	if nilErr.Error() != "" {
		t.Fatalf("nil NodeError.Error() = %q, want empty", nilErr.Error())
	}
	if got := asNodeError(nil); got != nil {
		t.Fatalf("asNodeError(nil) = %v, want nil", got)
	}
	if got := asNodeError(errors.New("plain")); got.Code != "backend_error" {
		t.Fatalf("asNodeError(plain).Code = %q, want backend_error", got.Code)
	}
	if got := asNodeError(&NodeError{Code: "c", Retryable: true, Message: "m"}); got.Code != "c" || !got.Retryable {
		t.Fatalf("asNodeError(NodeError) = %+v", got)
	}
	// Execute: nil spec, field-type validation, config schema validation.
	if _, err := reg.Execute(context.Background(), nil, ownerPrincipal(), nil, nil, nil, false); err == nil || !strings.Contains(err.Error(), "node_not_found") {
		t.Fatalf("Execute nil spec: err = %v", err)
	}
	for _, tc := range []struct {
		typ string
		val any
	}{
		{"number", "not-a-number"},
		{"boolean", "not-a-bool"},
		{"object", "not-an-object"},
		{"array", "not-an-array"},
	} {
		spec := testSpec("typed", []Field{{Name: "f", Type: tc.typ}}, echo)
		if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(), map[string]any{"f": tc.val}, nil, nil, false); err == nil || !strings.Contains(err.Error(), "validation_error") {
			t.Fatalf("Execute %s with %v: err = %v", tc.typ, tc.val, err)
		}
	}
	bogus := testSpec("bogus", nil, echo)
	bogus.ConfigSchema = []Field{{Name: "x", Type: "bogus"}}
	if err := reg.ValidateConfig(bogus, map[string]any{"x": 1}); err == nil || !strings.Contains(err.Error(), "invalid_schema") {
		t.Fatalf("ValidateConfig bogus type: err = %v", err)
	}
	// Accessor helpers.
	if _, ok := getNumber(map[string]any{"k": json.Number("abc")}, "k"); ok {
		t.Fatal("getNumber invalid json.Number accepted")
	}
	if v, ok := getNumber(map[string]any{"k": json.Number("1.5")}, "k"); !ok || v != 1.5 {
		t.Fatalf("getNumber valid json.Number = %v, %v", v, ok)
	}
	if _, ok := getStringSlice(map[string]any{"k": "not-array"}, "k"); ok {
		t.Fatal("getStringSlice non-array accepted")
	}
	if _, ok := getStringSlice(map[string]any{"k": []any{"a", 1}}, "k"); ok {
		t.Fatal("getStringSlice non-string item accepted")
	}
	if got, ok := getStringSlice(map[string]any{"k": []any{"a", "b"}}, "k"); !ok || len(got) != 2 {
		t.Fatalf("getStringSlice valid = %v, %v", got, ok)
	}
	if err := requireBackend(&RunContext{}); err == nil {
		t.Fatal("requireBackend nil backend accepted")
	}
	if err := requireBackend(&RunContext{Backend: newFakeBackend()}); err != nil {
		t.Fatalf("requireBackend wired: %v", err)
	}
	// Execute: cancelled context, timeout retryability, post-run cancel,
	// nil output, output too large.
	cancelRun := func(ctx context.Context, _ *RunContext) (map[string]any, error) {
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reg.Execute(ctx, testSpec("cx", nil, cancelRun), ownerPrincipal(), nil, nil, nil, false); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("Execute cancelled: err = %v", err)
	}
	for _, tc := range []struct {
		codes []string
		want  bool
	}{
		{[]string{"timeout"}, true},
		{[]string{}, false},
	} {
		timeoutRun := func(ctx context.Context, _ *RunContext) (map[string]any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		spec := testSpec("to", nil, timeoutRun)
		spec.DefaultTimeout = 50 * time.Millisecond
		spec.MaxTimeout = 100 * time.Millisecond
		spec.Retry = RetryPolicy{MaxAttempts: 2, RetryableCodes: tc.codes}
		_, err := reg.Execute(context.Background(), spec, ownerPrincipal(), nil, nil, nil, false)
		var ne *NodeError
		if !errors.As(err, &ne) || ne.Code != "timeout" || ne.Retryable != tc.want {
			t.Fatalf("Execute timeout codes=%v: err = %v, want timeout retryable=%v", tc.codes, err, tc.want)
		}
	}
	sleepRun := func(_ context.Context, _ *RunContext) (map[string]any, error) {
		time.Sleep(120 * time.Millisecond)
		return map[string]any{"ok": true}, nil
	}
	slow := testSpec("slow", nil, sleepRun)
	slow.DefaultTimeout = 50 * time.Millisecond
	slow.MaxTimeout = 100 * time.Millisecond
	if _, err := reg.Execute(context.Background(), slow, ownerPrincipal(), nil, nil, nil, false); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("Execute post-run cancel: err = %v", err)
	}
	nilOut := testSpec("nilout", nil, func(context.Context, *RunContext) (map[string]any, error) { return nil, nil })
	out, err := reg.Execute(context.Background(), nilOut, ownerPrincipal(), nil, nil, nil, false)
	if err != nil || out == nil || len(out) != 0 {
		t.Fatalf("Execute nil output = %v, err = %v", out, err)
	}
	big := testSpec("big", nil, func(context.Context, *RunContext) (map[string]any, error) {
		return map[string]any{"big": strings.Repeat("x", 100)}, nil
	})
	big.MaxOutputBytes = 10
	if _, err := reg.Execute(context.Background(), big, ownerPrincipal(), nil, nil, nil, false); err == nil || !strings.Contains(err.Error(), "output_too_large") {
		t.Fatalf("Execute output too large: err = %v", err)
	}
}

func TestContinue95NexaflowDirectBackends(t *testing.T) {
	ctx := context.Background()
	hb := &DirectHealthCompatBackend{}
	if _, err := hb.ProviderHealth(ctx, "d"); err == nil {
		t.Fatal("ProviderHealth nil manager accepted")
	}
	if _, err := hb.Compat(ctx, "d"); err == nil {
		t.Fatal("Compat nil store accepted")
	}
	cb := &DirectCostBackend{}
	if _, err := cb.UsageSnapshot(ctx); err == nil {
		t.Fatal("UsageSnapshot nil tracker accepted")
	}
	if _, err := cb.BudgetSnapshot(ctx, "k"); err == nil {
		t.Fatal("BudgetSnapshot nil ledger accepted")
	}
	wired := &DirectHealthCompatBackend{Health: health.New(3, time.Minute), CompatStore: compat.NewStore()}
	ph, err := wired.ProviderHealth(ctx, "absent")
	if err != nil || ph.DeploymentID != "absent" {
		t.Fatalf("ProviderHealth wired absent = %+v, err = %v", ph, err)
	}
	ci, err := wired.Compat(ctx, "absent")
	if err != nil || ci.DeploymentID != "absent" || len(ci.Support) != len(compat.AllCapabilities) {
		t.Fatalf("Compat wired absent = %+v, err = %v", ci, err)
	}
	for name, v := range ci.Support {
		if v != "unknown" {
			t.Fatalf("Compat absent support[%q] = %q, want unknown", name, v)
		}
	}
	// Compat without a health manager still reports "unknown" health.
	noHealth := &DirectHealthCompatBackend{CompatStore: compat.NewStore()}
	if _, err := noHealth.Compat(ctx, "absent"); err != nil {
		t.Fatalf("Compat no-health: %v", err)
	}
	cost := &DirectCostBackend{Tracker: usage.New(), Ledger: usage.NewBudgetLedger()}
	us, err := cost.UsageSnapshot(ctx)
	if err != nil || us.TotalRequests != 0 {
		t.Fatalf("UsageSnapshot wired = %+v, err = %v", us, err)
	}
	bi, err := cost.BudgetSnapshot(ctx, "team")
	if err != nil || bi.Key != "team" || bi.Committed != 0 || bi.Reserved != 0 {
		t.Fatalf("BudgetSnapshot wired = %+v, err = %v", bi, err)
	}
}

func TestContinue95NexaflowDeliveryLedgers(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryDeliveryLedger()
	if _, err := mem.Claim(ctx, ""); err == nil {
		t.Fatal("memory Claim empty key accepted")
	}
	if ok, err := mem.Claim(ctx, "k1"); err != nil || !ok {
		t.Fatalf("memory Claim first = %v, %v", ok, err)
	}
	if ok, err := mem.Claim(ctx, "k1"); err != nil || ok {
		t.Fatalf("memory Claim duplicate = %v, %v", ok, err)
	}
	if _, err := NewFileDeliveryLedger(""); err == nil {
		t.Fatal("file ledger empty dir accepted")
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if _, err := NewFileDeliveryLedger(filepath.Join(blocker, "sub")); err == nil {
		t.Fatal("file ledger under a regular file accepted")
	}
	file, err := NewFileDeliveryLedger(t.TempDir())
	if err != nil {
		t.Fatalf("file ledger: %v", err)
	}
	if _, err := file.Claim(ctx, ""); err == nil {
		t.Fatal("file Claim empty key accepted")
	}
	if ok, err := file.Claim(ctx, "k1"); err != nil || !ok {
		t.Fatalf("file Claim first = %v, %v", ok, err)
	}
	if ok, err := file.Claim(ctx, "k1"); err != nil || ok {
		t.Fatalf("file Claim duplicate = %v, %v", ok, err)
	}
}

func TestContinue95NexaflowTriggerNodes(t *testing.T) {
	// Manual trigger with no payload yields an empty object, not nil.
	out, err := Continue95NexaflowDirectNode(t, "nexaroute.trigger.manual", map[string]any{}, nil)
	if err != nil {
		t.Fatalf("manual trigger: %v", err)
	}
	if out["payload"] == nil {
		t.Fatal("manual trigger payload is nil, want empty object")
	}
	// Schedule trigger: exactly-one rule, bad last_run, bad interval,
	// bad daily, and the next-day rollover.
	sched := func(input, config map[string]any) (map[string]any, error) {
		return Continue95NexaflowDirectNode(t, "nexaroute.trigger.schedule", input, config)
	}
	if _, err := sched(nil, map[string]any{"interval": "1h", "daily": "09:00"}); err == nil {
		t.Fatal("schedule both interval and daily accepted")
	}
	if _, err := sched(nil, map[string]any{}); err == nil {
		t.Fatal("schedule neither interval nor daily accepted")
	}
	if _, err := sched(map[string]any{"last_run": "not-a-time"}, map[string]any{"interval": "1h"}); err == nil {
		t.Fatal("schedule bad last_run accepted")
	}
	for _, iv := range []string{"bogus", "0s", "400d"} {
		if _, err := sched(nil, map[string]any{"interval": iv}); err == nil {
			t.Fatalf("schedule interval %q accepted", iv)
		}
	}
	if out, err := sched(map[string]any{"last_run": "2026-01-01T10:00:00Z"}, map[string]any{"interval": "1h30m"}); err != nil || out["next_run"] != "2026-01-01T11:30:00Z" {
		t.Fatalf("schedule interval = %v, err = %v", out, err)
	}
	if _, err := sched(nil, map[string]any{"daily": "9am"}); err == nil {
		t.Fatal("schedule daily without colon accepted")
	}
	if _, err := sched(nil, map[string]any{"daily": "25:00"}); err == nil {
		t.Fatal("schedule daily hour 25 accepted")
	}
	if _, err := sched(nil, map[string]any{"daily": "09:60"}); err == nil {
		t.Fatal("schedule daily minute 60 accepted")
	}
	if out, err := sched(map[string]any{"last_run": "2026-01-01T15:00:00Z"}, map[string]any{"daily": "09:00"}); err != nil || out["next_run"] != "2026-01-02T09:00:00Z" {
		t.Fatalf("schedule daily rollover = %v, err = %v", out, err)
	}
	if out, err := sched(map[string]any{"last_run": "2026-01-01T08:00:00Z"}, map[string]any{"daily": "09:00"}); err != nil || out["next_run"] != "2026-01-01T09:00:00Z" {
		t.Fatalf("schedule daily same day = %v, err = %v", out, err)
	}
	// Webhook verifier: body bound, timestamp parsing, window, hex,
	// mismatch, RFC3339 acceptance, replay, and stale-entry pruning.
	now := time.Now().UTC()
	ts := fmt.Sprintf("%d", now.Unix())
	if err := (&WebhookVerifier{seen: map[string]time.Time{}, now: time.Now}).VerifySignature(ts, "00", strings.Repeat("x", (1<<20)+1), "s"); err == nil {
		t.Fatal("webhook oversize body accepted")
	}
	if err := (&WebhookVerifier{seen: map[string]time.Time{}, now: time.Now}).VerifySignature("not-a-time", "00", "b", "s"); err == nil {
		t.Fatal("webhook unparseable timestamp accepted")
	}
	old := fmt.Sprintf("%d", now.Add(-10*time.Minute).Unix())
	if err := (&WebhookVerifier{seen: map[string]time.Time{}, now: time.Now}).VerifySignature(old, "00", "b", "s"); err == nil || !strings.Contains(err.Error(), "webhook_expired") {
		t.Fatalf("webhook expired: err = %v", err)
	}
	if err := (&WebhookVerifier{seen: map[string]time.Time{}, now: time.Now}).VerifySignature(ts, "zz", "b", "s"); err == nil {
		t.Fatal("webhook non-hex signature accepted")
	}
	if err := (&WebhookVerifier{seen: map[string]time.Time{}, now: time.Now}).VerifySignature(ts, "00", "b", "s"); err == nil || !strings.Contains(err.Error(), "webhook_invalid_signature") {
		t.Fatalf("webhook mismatch: err = %v", err)
	}
	rfc := now.Format(time.RFC3339)
	if err := (&WebhookVerifier{seen: map[string]time.Time{}, now: time.Now}).VerifySignature(rfc, Continue95NexaflowWebhookSig("s", rfc, "b"), "b", "s"); err != nil {
		t.Fatalf("webhook RFC3339 timestamp: %v", err)
	}
	v := NewWebhookVerifier()
	if err := v.Verify(ts, Continue95NexaflowWebhookSig("s", ts, "b"), "b", "s"); err != nil {
		t.Fatalf("webhook first verify: %v", err)
	}
	if err := v.Verify(ts, Continue95NexaflowWebhookSig("s", ts, "b"), "b", "s"); err == nil || !strings.Contains(err.Error(), "webhook_replay") {
		t.Fatalf("webhook replay: err = %v", err)
	}
	// Prune: a stale entry is dropped when a fresh delivery verifies. The
	// replay key is the MAC itself, so the pruned entry's key must be
	// absent from the cache afterwards (the same delivery could not
	// re-verify anyway: its timestamp would be outside the window).
	clock := now
	pv := &WebhookVerifier{seen: map[string]time.Time{}, now: func() time.Time { return clock }}
	if err := pv.Verify(ts, Continue95NexaflowWebhookSig("s", ts, "a"), "a", "s"); err != nil {
		t.Fatalf("prune setup verify: %v", err)
	}
	keyA := Continue95NexaflowWebhookSig("s", ts, "a")
	clock = now.Add(6 * time.Minute)
	tsB := fmt.Sprintf("%d", clock.Unix())
	if err := pv.Verify(tsB, Continue95NexaflowWebhookSig("s", tsB, "b"), "b", "s"); err != nil {
		t.Fatalf("prune second verify: %v", err)
	}
	if _, still := pv.seen[keyA]; still {
		t.Fatal("prune: stale entry still in cache after a later verify")
	}
	if len(pv.seen) != 1 {
		t.Fatalf("prune: cache size = %d, want 1", len(pv.seen))
	}
	// Cache full: 10000 distinct deliveries, then the next is refused.
	full := NewWebhookVerifier()
	fullTS := fmt.Sprintf("%d", time.Now().Unix())
	for i := 0; i < 10000; i++ {
		body := fmt.Sprintf("body-%d", i)
		if err := full.Verify(fullTS, Continue95NexaflowWebhookSig("s", fullTS, body), body, "s"); err != nil {
			t.Fatalf("cache fill %d: %v", i, err)
		}
	}
	if err := full.Verify(fullTS, Continue95NexaflowWebhookSig("s", fullTS, "one-more"), "one-more", "s"); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("cache full: err = %v", err)
	}
	// Webhook trigger node: missing fields, nil backend, secret error,
	// verification error, and success.
	webhook := func(input, config map[string]any, backend Backend) (map[string]any, error) {
		return Continue95NexaflowDirectNodeBackend(t, "nexaroute.trigger.webhook", input, config, backend)
	}
	if _, err := webhook(map[string]any{"body": "b"}, map[string]any{"secret_ref": "webhook-secret"}, newFakeBackend()); err == nil {
		t.Fatal("webhook trigger missing fields accepted")
	}
	fullInput := map[string]any{"timestamp": ts, "signature": Continue95NexaflowWebhookSig("s3cret", ts, "hello"), "body": "hello"}
	if _, err := webhook(fullInput, map[string]any{"secret_ref": "webhook-secret"}, nil); err == nil {
		t.Fatal("webhook trigger nil backend accepted")
	}
	if _, err := webhook(fullInput, map[string]any{"secret_ref": "missing-ref"}, newFakeBackend()); err == nil {
		t.Fatal("webhook trigger unknown secret accepted")
	}
	badInput := map[string]any{"timestamp": ts, "signature": "00", "body": "hello"}
	if _, err := webhook(badInput, map[string]any{"secret_ref": "webhook-secret"}, newFakeBackend()); err == nil {
		t.Fatal("webhook trigger bad signature accepted")
	}
	if out, err := webhook(fullInput, map[string]any{"secret_ref": "webhook-secret"}, newFakeBackend()); err != nil || out["verified"] != true || out["body"] != "hello" {
		t.Fatalf("webhook trigger success = %v, err = %v", out, err)
	}
	// HTTP request node: url, method, headers, and secret branches.
	httpNode := func(input, config map[string]any, backend Backend) (map[string]any, error) {
		return Continue95NexaflowDirectNodeBackend(t, "nexaroute.http.request", input, config, backend)
	}
	fb := newFakeBackend()
	if _, err := httpNode(map[string]any{}, nil, fb); err == nil {
		t.Fatal("http node missing url accepted")
	}
	if _, err := httpNode(map[string]any{"url": "http://user:pass@example.com/"}, nil, fb); err == nil {
		t.Fatal("http node userinfo url accepted")
	}
	if _, err := httpNode(map[string]any{"url": "https://example.com/", "method": "FETCH"}, nil, fb); err == nil {
		t.Fatal("http node bad method accepted")
	}
	if _, err := httpNode(map[string]any{"url": "https://example.com/", "headers": "nope"}, nil, fb); err == nil {
		t.Fatal("http node non-object headers accepted")
	}
	if _, err := httpNode(map[string]any{"url": "https://example.com/", "headers": map[string]any{"X-Ok": 1}}, nil, fb); err == nil {
		t.Fatal("http node non-string header accepted")
	}
	if _, err := httpNode(map[string]any{"url": "https://example.com/", "headers": map[string]any{"X-Api-Key": "sekret"}}, nil, fb); err == nil {
		t.Fatal("http node credential header accepted")
	}
	if _, err := httpNode(map[string]any{"url": "https://example.com/"}, map[string]any{"auth_secret_ref": "  "}, fb); err == nil {
		t.Fatal("http node blank auth_secret_ref accepted")
	}
	if _, err := httpNode(map[string]any{"url": "https://example.com/"}, map[string]any{"auth_secret_ref": "missing-ref"}, fb); err == nil {
		t.Fatal("http node unknown auth secret accepted")
	}
	fbErr := newFakeBackend()
	fbErr.HTTPErr = errors.New("downstream down")
	if _, err := httpNode(map[string]any{"url": "https://example.com/"}, nil, fbErr); err == nil || !strings.Contains(err.Error(), "downstream down") {
		t.Fatalf("http node backend error passthrough: err = %v", err)
	}
	if out, err := httpNode(map[string]any{"url": "https://example.com/", "headers": map[string]any{"X-Ok": "v"}}, map[string]any{"auth_secret_ref": "api-token"}, fb); err != nil || out["status"] != 200 || out["body"] != "hello" {
		t.Fatalf("http node success = %v, err = %v", out, err)
	}
}

func TestContinue95NexaflowLLMNode(t *testing.T) {
	llm := func(input, config map[string]any, backend Backend) (map[string]any, error) {
		return Continue95NexaflowDirectNodeBackend(t, "nexaroute.llm.chat", input, config, backend)
	}
	fb := newFakeBackend()
	if _, err := llm(map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding"}, nil); err == nil {
		t.Fatal("llm node nil backend accepted")
	}
	if _, err := llm(map[string]any{"prompt": "hi"}, map[string]any{}, fb); err == nil {
		t.Fatal("llm node missing selector accepted")
	}
	for _, mt := range []any{float64(0), float64(9000), math.NaN()} {
		if _, err := llm(map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding", "max_tokens": mt}, fb); err == nil {
			t.Fatalf("llm node max_tokens %v accepted", mt)
		}
	}
	for _, temp := range []any{float64(-1), float64(3), math.NaN()} {
		if _, err := llm(map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding", "temperature": temp}, fb); err == nil {
			t.Fatalf("llm node temperature %v accepted", temp)
		}
	}
	cfg := map[string]any{"route_or_alias": "team/coding"}
	badInputs := []struct {
		name  string
		input map[string]any
	}{
		{"both", map[string]any{"prompt": "hi", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"neither", map[string]any{}},
		{"empty-prompt", map[string]any{"prompt": "  "}},
		{"long-prompt", map[string]any{"prompt": strings.Repeat("x", 8001)}},
		{"empty-messages", map[string]any{"messages": []any{}}},
		{"non-object-message", map[string]any{"messages": []any{"nope"}}},
		{"empty-role", map[string]any{"messages": []any{map[string]any{"role": "", "content": "hi"}}}},
		{"long-content", map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 8001)}}}},
	}
	for _, tc := range badInputs {
		if _, err := llm(tc.input, cfg, fb); err == nil {
			t.Fatalf("llm node input %s accepted", tc.name)
		}
	}
	if _, err := llm(map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "nope"}, fb); err == nil || !strings.Contains(err.Error(), "selector_not_found") {
		t.Fatalf("llm node unknown selector: err = %v", err)
	}
	chatErr := &Continue95NexaflowChatErrBackend{FakeBackend: newFakeBackend(), Err: errors.New("chat boom")}
	if _, err := llm(map[string]any{"prompt": "hi"}, cfg, chatErr); err == nil || !strings.Contains(err.Error(), "chat boom") {
		t.Fatalf("llm node chat error: err = %v", err)
	}
	out, err := llm(map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding", "max_tokens": float64(100), "temperature": float64(0.5)}, fb)
	if err != nil || out["model"] != "vendor/model" || out["text"] != "fake reply" || out["resolved_route"] != "team/coding" {
		t.Fatalf("llm node success = %v, err = %v", out, err)
	}
}

func TestContinue95NexaflowGatewayClientExtras(t *testing.T) {
	ctx := context.Background()
	// Custom HTTPClient branch + Temperature payload branch.
	var seenTemp any
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		seenTemp = payload["temperature"]
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"vendor/model","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	defer chatSrv.Close()
	temp := 1.5
	client := &GatewayClient{BaseURL: chatSrv.URL, HTTPClient: chatSrv.Client()}
	chat, err := client.Chat(ctx, ChatRequest{Selector: "team/coding", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, Temperature: &temp})
	if err != nil || chat.Text != "hello" || seenTemp != 1.5 {
		t.Fatalf("chat custom client + temperature = %+v, seenTemp=%v, err = %v", chat, seenTemp, err)
	}
	// doJSON: marshal error, request-construction error, read error.
	var decoded map[string]any
	if _, err := client.doJSON(ctx, http.MethodPost, "/x", func() {}, &decoded); err == nil || !strings.Contains(err.Error(), "request encoding failed") {
		t.Fatalf("doJSON marshal error: err = %v", err)
	}
	badClient := &GatewayClient{BaseURL: "://bad"}
	if _, err := badClient.doJSON(ctx, http.MethodGet, "/x", nil, &decoded); err == nil || !strings.Contains(err.Error(), "gateway request failed") {
		t.Fatalf("doJSON request error: err = %v", err)
	}
	truncSrv := httptest.NewServer(http.HandlerFunc(Continue95NexaflowHijackTruncate))
	defer truncSrv.Close()
	truncClient := &GatewayClient{BaseURL: truncSrv.URL}
	if _, err := truncClient.doJSON(ctx, http.MethodGet, "/trunc", nil, &decoded); err == nil || !strings.Contains(err.Error(), "response read failed") {
		t.Fatalf("doJSON read error: err = %v", err)
	}
	// MCP: rpc transport error, rpc error object, null result, non-map tool.
	mcp404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer mcp404.Close()
	if _, err := (&GatewayClient{BaseURL: mcp404.URL}).MCPList(ctx, "srv"); err == nil {
		t.Fatal("MCPList transport error accepted")
	}
	mcpErrObj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer mcpErrObj.Close()
	if _, err := (&GatewayClient{BaseURL: mcpErrObj.URL}).MCPList(ctx, "srv"); err == nil || !strings.Contains(err.Error(), "mcp gateway error") {
		t.Fatalf("MCPList rpc error object: err = %v", err)
	}
	mcpNull := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result": null}`)
	}))
	defer mcpNull.Close()
	tools, err := (&GatewayClient{BaseURL: mcpNull.URL}).MCPList(ctx, "srv")
	if err != nil || len(tools) != 0 {
		t.Fatalf("MCPList null result = %v, err = %v", tools, err)
	}
	mcpMixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":{"tools":["oops",{"name":"srv__tool","description":"d","inputSchema":{}}]}}`)
	}))
	defer mcpMixed.Close()
	tools, err = (&GatewayClient{BaseURL: mcpMixed.URL}).MCPList(ctx, "srv")
	if err != nil || len(tools) != 1 || tools[0].Name != "tool" {
		t.Fatalf("MCPList non-map tool = %v, err = %v", tools, err)
	}
	// VideoSubmit: transport error and missing job id.
	vid500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer vid500.Close()
	if _, err := (&GatewayClient{BaseURL: vid500.URL}).VideoSubmit(ctx, VideoSubmitRequest{Prompt: "p"}); err == nil {
		t.Fatal("VideoSubmit transport error accepted")
	}
	vidEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer vidEmpty.Close()
	if _, err := (&GatewayClient{BaseURL: vidEmpty.URL}).VideoSubmit(ctx, VideoSubmitRequest{Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "no job id") {
		t.Fatalf("VideoSubmit no job id: err = %v", err)
	}
	// VideoStatus with a non-map asset entry: only the valid asset survives.
	vidMixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":["oops",{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
	}))
	defer vidMixed.Close()
	info, err := (&GatewayClient{BaseURL: vidMixed.URL}).VideoStatus(ctx, "job-9")
	if err != nil || len(info.Assets) != 1 || info.Assets[0].ID != "a1" {
		t.Fatalf("VideoStatus non-map asset = %+v, err = %v", info, err)
	}
	// VideoAsset: status error, auth error, download error, non-2xx,
	// read error, and oversize.
	vid404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer vid404.Close()
	if _, _, err := (&GatewayClient{BaseURL: vid404.URL}).VideoAsset(ctx, "missing", "a1"); err == nil {
		t.Fatal("VideoAsset status error accepted")
	}
	vidJob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/assets/a1"):
			// 500 bytes: larger than the ~140-byte status JSON, so a
			// 200-byte bound passes status but fails the download.
			fmt.Fprint(w, strings.Repeat("x", 500))
		default:
			fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
		}
	}))
	defer vidJob.Close()
	authErrClient := &GatewayClient{BaseURL: vidJob.URL, AuthTokenRef: "ref", SecretResolver: func(string) (string, error) { return "", errors.New("resolver down") }}
	if _, _, err := authErrClient.VideoAsset(ctx, "job-9", "a1"); err == nil || !strings.Contains(err.Error(), "auth token resolution failed") {
		t.Fatalf("VideoAsset auth error: err = %v", err)
	}
	vidDrop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assets/a1") {
			Continue95NexaflowHijackClose(w, r)
			return
		}
		fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
	}))
	defer vidDrop.Close()
	if _, _, err := (&GatewayClient{BaseURL: vidDrop.URL}).VideoAsset(ctx, "job-9", "a1"); err == nil || !strings.Contains(err.Error(), "video asset download failed") {
		t.Fatalf("VideoAsset download error: err = %v", err)
	}
	vidAsset500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assets/a1") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
	}))
	defer vidAsset500.Close()
	if _, _, err := (&GatewayClient{BaseURL: vidAsset500.URL}).VideoAsset(ctx, "job-9", "a1"); err == nil || !strings.Contains(err.Error(), "video asset gateway returned status 500") {
		t.Fatalf("VideoAsset non-2xx: err = %v", err)
	}
	vidAssetTrunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assets/a1") {
			Continue95NexaflowHijackTruncate(w, r)
			return
		}
		fmt.Fprint(w, `{"job_id":"job-9","state":"completed","output_assets":[{"id":"a1","filename":"out.mp4","content_type":"video/mp4","sha256":"abc","size_bytes":11}]}`)
	}))
	defer vidAssetTrunc.Close()
	if _, _, err := (&GatewayClient{BaseURL: vidAssetTrunc.URL}).VideoAsset(ctx, "job-9", "a1"); err == nil || !strings.Contains(err.Error(), "video asset read failed") {
		t.Fatalf("VideoAsset read error: err = %v", err)
	}
	smallClient := &GatewayClient{BaseURL: vidJob.URL, MaxResponseBytes: 200}
	if _, _, err := smallClient.VideoAsset(ctx, "job-9", "a1"); err == nil || !strings.Contains(err.Error(), "video asset exceeds bound") {
		t.Fatalf("VideoAsset oversize: err = %v", err)
	}
	// HTTPDo: request-build error, read error, oversize.
	echoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/trunc" {
			Continue95NexaflowHijackTruncate(w, r)
			return
		}
		fmt.Fprint(w, "hello world")
	}))
	defer echoSrv.Close()
	plain := &GatewayClient{}
	if _, err := plain.HTTPDo(ctx, "BAD METHOD", echoSrv.URL+"/echo", nil, nil); err == nil || !strings.Contains(err.Error(), "http request build failed") {
		t.Fatalf("HTTPDo request build error: err = %v", err)
	}
	if _, err := plain.HTTPDo(ctx, http.MethodGet, echoSrv.URL+"/trunc", nil, nil); err == nil || !strings.Contains(err.Error(), "http response read failed") {
		t.Fatalf("HTTPDo read error: err = %v", err)
	}
	smallHTTP := &GatewayClient{MaxResponseBytes: 5}
	if _, err := smallHTTP.HTTPDo(ctx, http.MethodGet, echoSrv.URL+"/echo", nil, nil); err == nil || !strings.Contains(err.Error(), "http response exceeds bound") {
		t.Fatalf("HTTPDo oversize: err = %v", err)
	}
}

// Continue95NexaflowFailStore wraps a Store and fails selected calls, so
// engine error branches can be exercised without touching production code.
// Counted variants fail exactly at the Nth call so later calls succeed.
type Continue95NexaflowFailStore struct {
	Store
	mode           string
	getRunCalls    int
	getNodesCalls  int
	appendCalls    int
	updateCalls    int
	failGetRunAt   int
	failGetNodesAt int
	failAppendAt   int
	failUpdateAt   int
}

func (s *Continue95NexaflowFailStore) boom() error { return errors.New("c95 failstore boom") }

func (s *Continue95NexaflowFailStore) GetRunByIdempotency(ctx context.Context, key string) (Run, error) {
	if s.mode == "idem" {
		return Run{}, s.boom()
	}
	return s.Store.GetRunByIdempotency(ctx, key)
}

func (s *Continue95NexaflowFailStore) CreateRun(ctx context.Context, run Run) error {
	if s.mode == "createrun" {
		return s.boom()
	}
	return s.Store.CreateRun(ctx, run)
}

func (s *Continue95NexaflowFailStore) AppendNodeExecution(ctx context.Context, exec NodeExecution) (NodeExecution, error) {
	s.appendCalls++
	if s.mode == "append" || (s.failAppendAt > 0 && s.appendCalls == s.failAppendAt) {
		return NodeExecution{}, s.boom()
	}
	return s.Store.AppendNodeExecution(ctx, exec)
}

func (s *Continue95NexaflowFailStore) GetNodeExecutions(ctx context.Context, runID string) ([]NodeExecution, error) {
	s.getNodesCalls++
	if s.mode == "nodes" || (s.failGetNodesAt > 0 && s.getNodesCalls == s.failGetNodesAt) {
		return nil, s.boom()
	}
	return s.Store.GetNodeExecutions(ctx, runID)
}

func (s *Continue95NexaflowFailStore) GetApproval(ctx context.Context, runID, nodeID string) (Approval, error) {
	if s.mode == "approval" {
		return Approval{}, s.boom()
	}
	return s.Store.GetApproval(ctx, runID, nodeID)
}

func (s *Continue95NexaflowFailStore) TryClaim(ctx context.Context, runID, nodeID, owner string, ttl time.Duration, now time.Time) (NodeExecution, bool, error) {
	if s.mode == "claim" {
		return NodeExecution{}, false, s.boom()
	}
	return s.Store.TryClaim(ctx, runID, nodeID, owner, ttl, now)
}

func (s *Continue95NexaflowFailStore) SaveNodeExecution(ctx context.Context, exec NodeExecution, expected NodeState) (NodeExecution, error) {
	switch s.mode {
	case "save":
		return NodeExecution{}, s.boom()
	case "saveFailed":
		if exec.State == NodeFailed {
			return NodeExecution{}, s.boom()
		}
	case "saveFailedConflict":
		if exec.State == NodeFailed {
			return NodeExecution{}, ErrStateConflict
		}
	case "savePending":
		if exec.State == NodePending {
			return NodeExecution{}, s.boom()
		}
	case "saveSkipped":
		if exec.State == NodeSkipped {
			return NodeExecution{}, s.boom()
		}
	case "saveSkippedConflict":
		if exec.State == NodeSkipped {
			return NodeExecution{}, ErrStateConflict
		}
	}
	return s.Store.SaveNodeExecution(ctx, exec, expected)
}

func (s *Continue95NexaflowFailStore) UpdateRunState(ctx context.Context, runID string, expected, state RunState, code, msg string) (Run, error) {
	s.updateCalls++
	if s.mode == "update" || (s.mode == "updateAt" && s.failUpdateAt > 0 && s.updateCalls == s.failUpdateAt) {
		return Run{}, s.boom()
	}
	if s.mode == "updateconflict" {
		return Run{}, ErrStateConflict
	}
	return s.Store.UpdateRunState(ctx, runID, expected, state, code, msg)
}

func (s *Continue95NexaflowFailStore) GetRun(ctx context.Context, runID string) (Run, error) {
	s.getRunCalls++
	if s.mode == "getrun" || (s.failGetRunAt > 0 && s.getRunCalls == s.failGetRunAt) {
		return Run{}, s.boom()
	}
	return s.Store.GetRun(ctx, runID)
}

func (s *Continue95NexaflowFailStore) RecordApproval(ctx context.Context, appr Approval) (Approval, error) {
	if s.mode == "record" {
		return Approval{}, s.boom()
	}
	return s.Store.RecordApproval(ctx, appr)
}

func (s *Continue95NexaflowFailStore) ListRuns(ctx context.Context, filter RunFilter) ([]Run, error) {
	if s.mode == "list" {
		return nil, s.boom()
	}
	return s.Store.ListRuns(ctx, filter)
}

func (s *Continue95NexaflowFailStore) GetDraft(ctx context.Context, id string) (Definition, error) {
	if s.mode == "getdraft" {
		return Definition{}, s.boom()
	}
	return s.Store.GetDraft(ctx, id)
}

func (s *Continue95NexaflowFailStore) ListRevisions(ctx context.Context, id string) ([]PublishedDefinition, error) {
	if s.mode == "listrevisions" {
		return nil, s.boom()
	}
	return s.Store.ListRevisions(ctx, id)
}

func (s *Continue95NexaflowFailStore) SaveDraft(ctx context.Context, def Definition) error {
	if s.mode == "savedraft" {
		return s.boom()
	}
	return s.Store.SaveDraft(ctx, def)
}

func (s *Continue95NexaflowFailStore) Publish(ctx context.Context, id, by string) (PublishedDefinition, error) {
	if s.mode == "publish" {
		return PublishedDefinition{}, s.boom()
	}
	return s.Store.Publish(ctx, id, by)
}

func (s *Continue95NexaflowFailStore) SupersedeRevision(ctx context.Context, id string, rev uint64) (PublishedDefinition, error) {
	if s.mode == "supersede" {
		return PublishedDefinition{}, s.boom()
	}
	return s.Store.SupersedeRevision(ctx, id, rev)
}

// Continue95NexaflowWaitingRun drives an approval-gated definition to the
// waiting state and returns the run.
func Continue95NexaflowWaitingRun(t *testing.T, store Store, eng *Engine) Run {
	t.Helper()
	ctx := context.Background()
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "gate", NodeID: "nexaroute.control.approval", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "gate"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun waiting: %v", err)
	}
	waiting, err := eng.Execute(ctx, run.ID)
	if err != nil || waiting.State != RunWaitingForApproval {
		t.Fatalf("Execute to waiting = %q, err = %v", waiting.State, err)
	}
	return run
}

func TestContinue95NexaflowEngineNewAndCreateFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := NewEngine(EngineOptions{Registry: DefaultRegistry()}); err == nil {
		t.Fatal("NewEngine nil store accepted")
	}
	if _, err := NewEngine(EngineOptions{Store: NewMemoryStore()}); err == nil {
		t.Fatal("NewEngine nil registry accepted")
	}
	eng, err := NewEngine(EngineOptions{Store: NewMemoryStore(), Registry: DefaultRegistry(), Workers: 100})
	if err != nil || eng.workers != EngineMaxWorkers {
		t.Fatalf("NewEngine Workers=100: workers = %d, err = %v", eng.workers, err)
	}
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	// CreateRun with nil input defaults to an empty map.
	store := NewMemoryStore()
	eng = mustEngine(t, store, reg)
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), nil, "")
	if err != nil || run.Input == nil || len(run.Input) != 0 {
		t.Fatalf("CreateRun nil input = %+v, err = %v", run, err)
	}
	// Idempotency lookup failure, create failure, append failure.
	fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "idem"}
	eng = mustEngine(t, fs, reg)
	pub = publishDef(t, fs, def)
	if _, err := eng.CreateRun(ctx, pub, principal(), runInput(), "key-1"); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("CreateRun idem failure: err = %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "createrun"}
	eng = mustEngine(t, fs, reg)
	pub = publishDef(t, fs, def)
	if _, err := eng.CreateRun(ctx, pub, principal(), runInput(), ""); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("CreateRun create failure: err = %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "append"}
	eng = mustEngine(t, fs, reg)
	pub = publishDef(t, fs, def)
	if _, err := eng.CreateRun(ctx, pub, principal(), runInput(), ""); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("CreateRun append failure: err = %v", err)
	}
	// CreateRun's final GetRun failure (call 1).
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), failGetRunAt: 1}
	eng = mustEngine(t, fs, reg)
	pub = publishDef(t, fs, def)
	if _, err := eng.CreateRun(ctx, pub, principal(), runInput(), ""); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("CreateRun final GetRun failure: err = %v", err)
	}
}

func TestContinue95NexaflowEngineExecuteFailures(t *testing.T) {
	ctx := context.Background()
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	newChain := func(fs *Continue95NexaflowFailStore) (*Engine, Run) {
		eng := mustEngine(t, fs, reg)
		pub := publishDef(t, fs, def)
		run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
		if err != nil {
			t.Fatalf("CreateRun chain: %v", err)
		}
		return eng, run
	}
	// Counted GetRun failures. CreateRun's final GetRun is call 1, so
	// Execute entry is 2, post-expire 3, after sweep 1 is 4, after
	// sweep 2 is 5, recompute 6, finishRun final 7.
	for _, at := range []int{2, 3, 4, 6, 7} {
		fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), failGetRunAt: at}
		eng, run := newChain(fs)
		if _, err := eng.Execute(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
			t.Fatalf("Execute failGetRunAt=%d: err = %v", at, err)
		}
	}
	// Counted GetNodeExecutions failures: sweep 1 is 1, sweep 2 is 2,
	// recompute is 3.
	for _, at := range []int{1, 3} {
		fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), failGetNodesAt: at}
		eng, run := newChain(fs)
		if _, err := eng.Execute(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
			t.Fatalf("Execute failGetNodesAt=%d: err = %v", at, err)
		}
	}
	// Expire-approval GetNodeExecutions failure (ApprovalTimeout > 0).
	fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "nodes"}
	eng := mustEngine(t, fs, reg, func(o *EngineOptions) { o.ApprovalTimeout = time.Hour })
	pub := publishDef(t, fs, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun expire-nodes: %v", err)
	}
	if _, err := eng.Execute(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Execute expire nodes failure: err = %v", err)
	}
	// Sweep TryClaim failure.
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "claim"}
	eng, run = newChain(fs)
	if _, err := eng.Execute(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Execute claim failure: err = %v", err)
	}
	// Sweep waiting-loop GetApproval failure: build a waiting run on a
	// plain store, then swap in the failing store for Execute.
	plain := NewMemoryStore()
	plainEng := mustEngine(t, plain, DefaultRegistry())
	waiting := Continue95NexaflowWaitingRun(t, plain, plainEng)
	fs = &Continue95NexaflowFailStore{Store: plain, mode: "approval"}
	eng = mustEngine(t, fs, DefaultRegistry())
	if _, err := eng.Execute(ctx, waiting.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Execute sweep approval failure: err = %v", err)
	}
	// Sweep skip-save failure and conflict: start -> failing node -> echo.
	failer := testSpec("test.failer", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		return nil, NewNodeError("boom", false, "boom")
	})
	skipReg := testRegistry(t, echo, failer)
	skipDef := chainDef(
		struct {
			Instance, NodeID string
			Config           map[string]any
		}{"a", "test.failer", nil},
		struct {
			Instance, NodeID string
			Config           map[string]any
		}{"b", "test.echo", nil},
	)
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "saveSkipped"}
	eng = mustEngine(t, fs, skipReg)
	pub = publishDef(t, fs, skipDef)
	run, err = eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun skip: %v", err)
	}
	if _, err := eng.Execute(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Execute skip-save failure: err = %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "saveSkippedConflict"}
	eng = mustEngine(t, fs, skipReg)
	pub = publishDef(t, fs, skipDef)
	run, err = eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun skip-conflict: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil || final.State != RunRunning {
		t.Fatalf("Execute skip-save conflict = %q, err = %v", final.State, err)
	}
	// UpdateRunState failures on a created run and on a waiting run.
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "update"}
	eng, run = newChain(fs)
	if _, err := eng.Execute(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Execute update failure (created): err = %v", err)
	}
	plain = NewMemoryStore()
	plainEng = mustEngine(t, plain, DefaultRegistry())
	waiting = Continue95NexaflowWaitingRun(t, plain, plainEng)
	fs = &Continue95NexaflowFailStore{Store: plain, mode: "update"}
	eng = mustEngine(t, fs, DefaultRegistry())
	if _, err := eng.Execute(ctx, waiting.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Execute update failure (waiting): err = %v", err)
	}
	// Cancelled context: the run is cancelled via finishRun.
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore()}
	eng, run = newChain(fs)
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	final, err = eng.Execute(cancelCtx, run.ID)
	if err != nil || final.State != RunCancelled {
		t.Fatalf("Execute cancelled = %q, err = %v", final.State, err)
	}
	// Cancelled context + finishRun UpdateRunState failure (2nd update:
	// 1st is created -> running at sweep entry).
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "updateAt", failUpdateAt: 2}
	eng, run = newChain(fs)
	cancelCtx, cancel = context.WithCancel(ctx)
	cancel()
	if _, err := eng.Execute(cancelCtx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Execute cancelled + finish update failure: err = %v", err)
	}
	// Sweep error + cancelled context: finishRun cancels the run.
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "claim"}
	eng, run = newChain(fs)
	cancelCtx, cancel = context.WithCancel(ctx)
	cancel()
	final, err = eng.Execute(cancelCtx, run.ID)
	if err != nil || final.State != RunCancelled {
		t.Fatalf("Execute sweep-error + cancelled = %q, err = %v", final.State, err)
	}
	// Recompute hasRunnable: a run with a live foreign lease stays running.
	plain = NewMemoryStore()
	plainEng = mustEngine(t, plain, reg)
	pub = publishDef(t, plain, def)
	run, err = plainEng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun hasRunnable: %v", err)
	}
	if _, err := plain.UpdateRunState(ctx, run.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("UpdateRunState to running: %v", err)
	}
	if _, ok, err := plain.TryClaim(ctx, run.ID, "start", "foreign", time.Hour, time.Now()); err != nil || !ok {
		t.Fatalf("foreign claim = %v, %v", ok, err)
	}
	final, err = plainEng.Execute(ctx, run.ID)
	if err != nil || final.State != RunRunning {
		t.Fatalf("Execute live-lease run = %q, err = %v", final.State, err)
	}
	// Sweep-resume: an approval recorded directly in the store (bypassing
	// DecideApproval) resumes the run on the next Execute.
	plain = NewMemoryStore()
	plainEng = mustEngine(t, plain, DefaultRegistry())
	waiting = Continue95NexaflowWaitingRun(t, plain, plainEng)
	if _, err := plain.RecordApproval(ctx, Approval{RunID: waiting.ID, NodeID: "gate", Requester: "alice", Approver: "bob", Decision: "approved"}); err != nil {
		t.Fatalf("direct RecordApproval: %v", err)
	}
	final, err = plainEng.Execute(ctx, waiting.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("Execute sweep-resume = %q, err = %v", final.State, err)
	}
	states := nodeStates(t, plain, waiting.ID)
	if states["gate"].State != NodeSucceeded || states["gate"].Output["approved"] != true {
		t.Fatalf("sweep-resumed gate = %+v", states["gate"])
	}
}

func TestContinue95NexaflowEngineDecideCancelRecoverFailures(t *testing.T) {
	ctx := context.Background()
	// DecideApproval: nodes failure, record failure.
	plain := NewMemoryStore()
	plainEng := mustEngine(t, plain, DefaultRegistry())
	waiting := Continue95NexaflowWaitingRun(t, plain, plainEng)
	fs := &Continue95NexaflowFailStore{Store: plain, mode: "nodes"}
	eng := mustEngine(t, fs, DefaultRegistry())
	if _, err := eng.DecideApproval(ctx, waiting.ID, "gate", "bob", "approved"); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("DecideApproval nodes failure: err = %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: plain, mode: "record"}
	eng = mustEngine(t, fs, DefaultRegistry())
	if _, err := eng.DecideApproval(ctx, waiting.ID, "gate", "bob", "approved"); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("DecideApproval record failure: err = %v", err)
	}
	// DecideApproval: policy-rejected save failure and approved save failure.
	policySpec := testSpec("test.policygate", []Field{}, func(context.Context, *RunContext) (map[string]any, error) {
		return nil, NewNodeError("approval_required", false, "needs operator policy")
	})
	policySpec.RequiresOperatorPolicy = true
	policyReg := testRegistry(t, policySpec)
	policyDef := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"gate", "test.policygate", nil})
	plain = NewMemoryStore()
	plainEng = mustEngine(t, plain, policyReg)
	pub := publishDef(t, plain, policyDef)
	run, err := plainEng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun policygate: %v", err)
	}
	if _, err := plainEng.Execute(ctx, run.ID); err != nil {
		t.Fatalf("Execute policygate to waiting: %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: plain, mode: "saveFailed"}
	eng = mustEngine(t, fs, policyReg)
	if _, err := eng.DecideApproval(ctx, run.ID, "gate", "bob", "rejected"); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("DecideApproval policy-rejected save failure: err = %v", err)
	}
	plain = NewMemoryStore()
	plainEng = mustEngine(t, plain, DefaultRegistry())
	waiting = Continue95NexaflowWaitingRun(t, plain, plainEng)
	fs = &Continue95NexaflowFailStore{Store: plain, mode: "savePending"}
	eng = mustEngine(t, fs, DefaultRegistry())
	if _, err := eng.DecideApproval(ctx, waiting.ID, "gate", "bob", "approved"); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("DecideApproval approved save failure: err = %v", err)
	}
	// CancelRun: nodes failure, update conflict, update failure.
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	mkCreated := func(mode string) (*Engine, Run) {
		fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: mode}
		eng := mustEngine(t, fs, reg)
		pub := publishDef(t, fs, def)
		run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
		if err != nil {
			t.Fatalf("CreateRun cancel: %v", err)
		}
		return eng, run
	}
	eng, run = mkCreated("nodes")
	if _, err := eng.CancelRun(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("CancelRun nodes failure: err = %v", err)
	}
	eng, run = mkCreated("updateconflict")
	cancelled, err := eng.CancelRun(ctx, run.ID)
	if err != nil || cancelled.State != RunCreated {
		t.Fatalf("CancelRun update conflict = %q, err = %v", cancelled.State, err)
	}
	eng, run = mkCreated("update")
	if _, err := eng.CancelRun(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("CancelRun update failure: err = %v", err)
	}
	// Recover: list failure, nodes failure, skip of terminal/created
	// runs, and a live foreign lease left alone.
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "list"}
	eng = mustEngine(t, fs, reg)
	if err := eng.Recover(ctx); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Recover list failure: err = %v", err)
	}
	plain = NewMemoryStore()
	plainEng = mustEngine(t, plain, reg)
	pub = publishDef(t, plain, def)
	run, err = plainEng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun recover-nodes: %v", err)
	}
	if _, err := plain.UpdateRunState(ctx, run.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("UpdateRunState recover-nodes: %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: plain, mode: "nodes"}
	eng = mustEngine(t, fs, reg)
	if err := eng.Recover(ctx); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Recover nodes failure: err = %v", err)
	}
	plain = NewMemoryStore()
	plainEng = mustEngine(t, plain, reg)
	pub = publishDef(t, plain, def)
	done, err := plainEng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun recover-skip: %v", err)
	}
	if _, err := plainEng.Execute(ctx, done.ID); err != nil {
		t.Fatalf("Execute recover-skip: %v", err)
	}
	created, err := plainEng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun recover-created: %v", err)
	}
	running, err := plainEng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun recover-live: %v", err)
	}
	if _, err := plain.UpdateRunState(ctx, running.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("UpdateRunState recover-live: %v", err)
	}
	if _, ok, err := plain.TryClaim(ctx, running.ID, "start", "foreign", time.Hour, time.Now()); err != nil || !ok {
		t.Fatalf("foreign claim recover-live = %v, %v", ok, err)
	}
	if err := plainEng.Recover(ctx); err != nil {
		t.Fatalf("Recover skip/live: %v", err)
	}
	if got, err := plain.GetRun(ctx, done.ID); err != nil || got.State != RunSucceeded {
		t.Fatalf("Recover touched terminal run: %q, %v", got.State, err)
	}
	if got, err := plain.GetRun(ctx, created.ID); err != nil || got.State != RunCreated {
		t.Fatalf("Recover touched created run: %q, %v", got.State, err)
	}
	if got, err := plain.GetRun(ctx, running.ID); err != nil || got.State != RunRunning {
		t.Fatalf("Recover touched live-lease run: %q, %v", got.State, err)
	}
	if states := nodeStates(t, plain, running.ID); states["start"].State != NodeClaimed || states["start"].LeaseOwner != "foreign" {
		t.Fatalf("Recover touched live lease: %+v", states["start"])
	}
	// Expire approvals: success, generic save failure, conflict save.
	t0 := time.Now().UTC().Truncate(time.Second)
	mkExpire := func(mode string) (Store, *Engine, *Engine, Run) {
		base := NewMemoryStore()
		eng1 := mustEngine(t, base, DefaultRegistry(), func(o *EngineOptions) {
			o.ApprovalTimeout = time.Hour
			o.Now = func() time.Time { return t0 }
		})
		waiting := Continue95NexaflowWaitingRun(t, base, eng1)
		fs := &Continue95NexaflowFailStore{Store: base, mode: mode}
		eng2 := mustEngine(t, fs, DefaultRegistry(), func(o *EngineOptions) {
			o.ApprovalTimeout = time.Hour
			o.Now = func() time.Time { return t0.Add(2 * time.Hour) }
		})
		return base, eng1, eng2, waiting
	}
	_, _, eng2, waiting := mkExpire("")
	final, err := eng2.Execute(ctx, waiting.ID)
	if err != nil || final.State != RunFailed {
		t.Fatalf("expire approvals = %q, err = %v", final.State, err)
	}
	_, _, eng2, waiting = mkExpire("saveFailed")
	if _, err := eng2.Execute(ctx, waiting.ID); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("expire save failure: err = %v", err)
	}
	_, _, eng2, waiting = mkExpire("saveFailedConflict")
	final, err = eng2.Execute(ctx, waiting.ID)
	if err != nil || final.State != RunWaitingForApproval {
		t.Fatalf("expire save conflict = %q, err = %v", final.State, err)
	}
}

func TestContinue95NexaflowEngineRetryPersistFailures(t *testing.T) {
	ctx := context.Background()
	flaky := func() (*NodeSpec, *int) {
		calls := 0
		spec := testSpec("test.flaky", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
			calls++
			if calls == 1 {
				return nil, NewNodeError("boom", true, "boom")
			}
			return map[string]any{"v": "ok"}, nil
		})
		spec.Retry = RetryPolicy{MaxAttempts: 2, RetryableCodes: []string{"boom"}}
		return spec, &calls
	}
	flakyDef := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.flaky", nil})
	// runJobs persist failure: every Save fails, so the node outcome
	// cannot be proven and the run stays running.
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "save"}
	eng := mustEngine(t, fs, reg)
	pub := publishDef(t, fs, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun save-all: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil || final.State != RunRunning {
		t.Fatalf("Execute save-all = %q, err = %v", final.State, err)
	}
	if states := nodeStates(t, fs.Store, run.ID); states["start"].State != NodeClaimed {
		t.Fatalf("save-all start = %+v", states["start"])
	}
	// Retry append failure parks the run with result_not_persisted.
	spec, _ := flaky()
	reg = testRegistry(t, spec)
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), failAppendAt: 3}
	eng = mustEngine(t, fs, reg)
	pub = publishDef(t, fs, flakyDef)
	run, err = eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun append-fail: %v", err)
	}
	final, err = eng.Execute(ctx, run.ID)
	if err != nil || final.State != RunNeedsManualAction {
		t.Fatalf("Execute append-fail = %q, err = %v", final.State, err)
	}
	if states := nodeStates(t, fs.Store, run.ID); states["a"].ErrorCode != "result_not_persisted" {
		t.Fatalf("append-fail node = %+v", states["a"])
	}
	// Retry mark-failed save failure: the park of attempt 0 cannot land
	// on MemoryStore because the just-appended attempt 1 is already the
	// current attempt (attempt mismatch => StateConflict, audit-only),
	// so attempt 0 stays running and orphaned while attempt 1 succeeds
	// and the run succeeds. (Observed MemoryStore behaviour; the
	// append-failure case above, with no attempt 1, parks cleanly.)
	spec, _ = flaky()
	reg = testRegistry(t, spec)
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "saveFailed"}
	eng = mustEngine(t, fs, reg)
	pub = publishDef(t, fs, flakyDef)
	run, err = eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun save-failed: %v", err)
	}
	final, err = eng.Execute(ctx, run.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("Execute save-failed = %q, err = %v", final.State, err)
	}
	execs, err := fs.Store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions save-failed: %v", err)
	}
	var orphaned, succeeded bool
	for _, e := range execs {
		if e.NodeID != "a" {
			continue
		}
		if e.Attempt == 0 && e.State == NodeRunning {
			orphaned = true
		}
		if e.Attempt == 1 && e.State == NodeSucceeded {
			succeeded = true
		}
	}
	if !orphaned || !succeeded {
		t.Fatalf("save-failed attempts orphaned=%v succeeded=%v: %+v", orphaned, succeeded, execs)
	}
	// Backoff branch: a 1ms backoff still retries to success.
	spec, _ = flaky()
	reg = testRegistry(t, spec)
	eng = mustEngine(t, NewMemoryStore(), reg, func(o *EngineOptions) {
		o.Backoff = func(int) time.Duration { return time.Millisecond }
	})
	store := NewMemoryStore()
	eng = mustEngine(t, store, reg, func(o *EngineOptions) {
		o.Backoff = func(int) time.Duration { return time.Millisecond }
	})
	pub = publishDef(t, store, flakyDef)
	run, err = eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun backoff: %v", err)
	}
	final, err = eng.Execute(ctx, run.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("Execute backoff = %q, err = %v", final.State, err)
	}
	// codeIn false: a retryable error whose code is not listed is not
	// retried; the node fails on its first attempt.
	other := testSpec("test.other", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		return nil, NewNodeError("other", true, "other")
	})
	other.Retry = RetryPolicy{MaxAttempts: 2, RetryableCodes: []string{"boom"}}
	reg = testRegistry(t, other)
	otherDef := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.other", nil})
	store = NewMemoryStore()
	eng = mustEngine(t, store, reg)
	pub = publishDef(t, store, otherDef)
	run, err = eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun codeIn: %v", err)
	}
	final, err = eng.Execute(ctx, run.ID)
	if err != nil || final.State != RunFailed {
		t.Fatalf("Execute codeIn false = %q, err = %v", final.State, err)
	}
	execs, err = store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions codeIn: %v", err)
	}
	n := 0
	for _, e := range execs {
		if e.NodeID == "a" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("codeIn false attempts for a = %d, want 1", n)
	}
}

func TestContinue95NexaflowMemoryStoreDirect(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.SaveDraft(ctx, Definition{}); err == nil {
		t.Fatal("SaveDraft empty id accepted")
	}
	for _, id := range []string{"b", "a", "c"} {
		if err := store.SaveDraft(ctx, Definition{SchemaVersion: DefinitionSchemaVersion, ID: id, Name: id}); err != nil {
			t.Fatalf("SaveDraft %s: %v", id, err)
		}
	}
	drafts, err := store.ListDrafts(ctx)
	if err != nil || len(drafts) != 3 || drafts[0].ID != "a" || drafts[1].ID != "b" || drafts[2].ID != "c" {
		t.Fatalf("ListDrafts order = %v, err = %v", drafts, err)
	}
	// FileStore and Service ListDrafts passthroughs share the ordering.
	fs, err := OpenFileStore(filepath.Join(t.TempDir(), "fs.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	for _, id := range []string{"b", "a"} {
		if err := fs.SaveDraft(ctx, Definition{SchemaVersion: DefinitionSchemaVersion, ID: id, Name: id}); err != nil {
			t.Fatalf("FileStore SaveDraft %s: %v", id, err)
		}
	}
	fd, err := fs.ListDrafts(ctx)
	if err != nil || len(fd) != 2 || fd[0].ID != "a" || fd[1].ID != "b" {
		t.Fatalf("FileStore ListDrafts = %v, err = %v", fd, err)
	}
	svc, err := NewService(store, mustEngine(t, store, DefaultRegistry()), DefaultRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sd, err := svc.ListDrafts(ctx)
	if err != nil || len(sd) != 3 || sd[0].ID != "a" {
		t.Fatalf("Service ListDrafts = %v, err = %v", sd, err)
	}
	// Publish with an unmarshalable config fails in ContentHash.
	badDef := Definition{SchemaVersion: DefinitionSchemaVersion, ID: "bad", Name: "bad",
		Nodes: []DefinitionNode{{ID: "n", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{"f": func() {}}}},
	}
	if err := store.SaveDraft(ctx, badDef); err != nil {
		t.Fatalf("SaveDraft bad: %v", err)
	}
	if _, err := store.Publish(ctx, "bad", "tester"); err == nil || !strings.Contains(err.Error(), "JSON-encodable") {
		t.Fatalf("Publish func config: err = %v", err)
	}
	if _, err := store.GetRevision(ctx, "bad", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRevision missing: err = %v", err)
	}
	// CreateRun validation and defaults.
	if err := store.CreateRun(ctx, Run{}); err == nil {
		t.Fatal("CreateRun empty id accepted")
	}
	if err := store.CreateRun(ctx, Run{ID: "r1", DefinitionID: "d"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if got, err := store.GetRun(ctx, "r1"); err != nil || got.State != RunCreated {
		t.Fatalf("CreateRun blank state = %q, err = %v", got.State, err)
	}
	if err := store.CreateRun(ctx, Run{ID: "r1"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateRun duplicate: err = %v", err)
	}
	if err := store.CreateRun(ctx, Run{ID: "r2", IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("CreateRun idem: %v", err)
	}
	if err := store.CreateRun(ctx, Run{ID: "r3", IdempotencyKey: "k1"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("CreateRun idem conflict: err = %v", err)
	}
	if _, err := store.GetRunByIdempotency(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRunByIdempotency missing: err = %v", err)
	}
	// UpdateRunState errors and terminal guard.
	if _, err := store.UpdateRunState(ctx, "missing", "", RunRunning, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateRunState missing: err = %v", err)
	}
	if _, err := store.UpdateRunState(ctx, "r1", RunRunning, RunRunning, "", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("UpdateRunState expected mismatch: err = %v", err)
	}
	if _, err := store.UpdateRunState(ctx, "r1", "", RunSucceeded, "", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("UpdateRunState created->succeeded: err = %v", err)
	}
	if _, err := store.UpdateRunState(ctx, "r1", "", RunRunning, "", ""); err != nil {
		t.Fatalf("UpdateRunState to running: %v", err)
	}
	if _, err := store.UpdateRunState(ctx, "r1", "", RunSucceeded, "", ""); err != nil {
		t.Fatalf("UpdateRunState to succeeded: %v", err)
	}
	if _, err := store.UpdateRunState(ctx, "r1", "", RunFailed, "", ""); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("UpdateRunState terminal: err = %v", err)
	}
	// ListRuns filters, limit, and CreatedAt tie-break by id.
	store2 := NewMemoryStore()
	fixed := time.Now().UTC().Truncate(time.Second)
	store2.Clock = func() time.Time { return fixed }
	for _, tc := range []struct {
		id, def string
	}{
		{"b", "d1"}, {"a", "d1"}, {"c", "d2"},
	} {
		if err := store2.CreateRun(ctx, Run{ID: tc.id, DefinitionID: tc.def}); err != nil {
			t.Fatalf("CreateRun tie %s: %v", tc.id, err)
		}
	}
	all, err := store2.ListRuns(ctx, RunFilter{})
	if err != nil || len(all) != 3 || all[0].ID != "a" || all[1].ID != "b" || all[2].ID != "c" {
		t.Fatalf("ListRuns tie-break = %v, err = %v", all, err)
	}
	byDef, err := store2.ListRuns(ctx, RunFilter{DefinitionID: "d1"})
	if err != nil || len(byDef) != 2 {
		t.Fatalf("ListRuns DefinitionID = %v, err = %v", byDef, err)
	}
	byState, err := store2.ListRuns(ctx, RunFilter{State: RunCreated})
	if err != nil || len(byState) != 3 {
		t.Fatalf("ListRuns State = %v, err = %v", byState, err)
	}
	limited, err := store2.ListRuns(ctx, RunFilter{Limit: 2})
	if err != nil || len(limited) != 2 {
		t.Fatalf("ListRuns Limit = %v, err = %v", limited, err)
	}
	// Node execution errors and defaults.
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: "missing", NodeID: "n"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AppendNodeExecution missing run: err = %v", err)
	}
	appended, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n"})
	if err != nil || appended.State != NodePending || appended.Attempt != 0 {
		t.Fatalf("AppendNodeExecution blank state = %+v, err = %v", appended, err)
	}
	if _, err := store.SaveNodeExecution(ctx, NodeExecution{RunID: "missing", NodeID: "n"}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveNodeExecution missing node: err = %v", err)
	}
	if _, err := store.GetNodeExecutions(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetNodeExecutions missing run: err = %v", err)
	}
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n"}); err != nil {
		t.Fatalf("AppendNodeExecution second: %v", err)
	}
	if _, err := store.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n", Attempt: 0, State: NodePending}, ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("SaveNodeExecution attempt mismatch: err = %v", err)
	}
	if _, _, err := store.TryClaim(ctx, "missing", "n", "w", time.Minute, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TryClaim missing node: err = %v", err)
	}
	if err := store.ReleaseLease(ctx, "missing", "n", "w"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReleaseLease missing node: err = %v", err)
	}
	// Prune deletes the run and its approval record.
	store3 := NewMemoryStore()
	if err := store3.CreateRun(ctx, Run{ID: "pr", DefinitionID: "d"}); err != nil {
		t.Fatalf("CreateRun prune: %v", err)
	}
	if _, err := store3.RecordApproval(ctx, Approval{RunID: "pr", NodeID: "g", Requester: "alice", Approver: "bob", Decision: "approved"}); err != nil {
		t.Fatalf("RecordApproval prune: %v", err)
	}
	if _, err := store3.UpdateRunState(ctx, "pr", "", RunRunning, "", ""); err != nil {
		t.Fatalf("UpdateRunState prune running: %v", err)
	}
	if _, err := store3.UpdateRunState(ctx, "pr", "", RunSucceeded, "", ""); err != nil {
		t.Fatalf("UpdateRunState prune succeeded: %v", err)
	}
	removed, err := store3.Prune(ctx, time.Now().Add(time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("Prune = %d, err = %v", removed, err)
	}
	if _, err := store3.GetApproval(ctx, "pr", "g"); !errors.Is(err, ErrNoApprovalRecord) {
		t.Fatalf("GetApproval after prune: err = %v", err)
	}
	if _, err := store3.GetRun(ctx, "pr"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRun after prune: err = %v", err)
	}
}

func TestContinue95NexaflowServiceExtras(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	eng := mustEngine(t, store, DefaultRegistry())
	svc, err := NewService(store, eng, DefaultRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Audit truncation: at 10000 the log drops the first 1000 before
	// appending, so 10001 events leave 9001 records.
	for i := 0; i < 10001; i++ {
		svc.HandleEngineEvent(AuditEvent{RunID: "r", Kind: "x", State: "running"})
	}
	if got := svc.Audit(); len(got) != 9001 {
		t.Fatalf("Audit truncation len = %d, want 9001", len(got))
	}
	// ReconcileBudget propagates a ListRuns failure.
	fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "list"}
	svcFail, err := NewService(fs, mustEngine(t, fs, DefaultRegistry()), DefaultRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService fail: %v", err)
	}
	if err := svcFail.ReconcileBudget(ctx); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("ReconcileBudget list failure: err = %v", err)
	}
	// SaveDraft with an invalid definition fails validation.
	badDef := Definition{SchemaVersion: DefinitionSchemaVersion, ID: "bad", Name: "bad",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "a", NodeID: "test.nope", Version: SupportedVersion},
		},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if err := svc.SaveDraft(ctx, badDef); err == nil {
		t.Fatal("SaveDraft invalid definition accepted")
	}
	// LatestPublished propagates a ListRevisions failure.
	fs = &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: "listrevisions"}
	svcFail, err = NewService(fs, mustEngine(t, fs, DefaultRegistry()), DefaultRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService fail2: %v", err)
	}
	if _, err := svcFail.LatestPublished(ctx, "d"); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("LatestPublished list failure: err = %v", err)
	}
	// Publish: anonymous, missing draft, invalid draft, store failure.
	if _, err := svc.Publish(ctx, "d", authz.Identity{}); err == nil {
		t.Fatal("Publish anonymous accepted")
	}
	if _, err := svc.Publish(ctx, "missing", ownerPrincipal()); err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Publish missing draft: err = %v", err)
	}
	if err := store.SaveDraft(ctx, badDef); err != nil {
		t.Fatalf("SaveDraft bad direct: %v", err)
	}
	if _, err := svc.Publish(ctx, "bad", ownerPrincipal()); err == nil {
		t.Fatal("Publish invalid draft accepted")
	}
	fs = &Continue95NexaflowFailStore{Store: store, mode: "publish"}
	svcFail, err = NewService(fs, mustEngine(t, fs, DefaultRegistry()), DefaultRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService fail3: %v", err)
	}
	if err := fs.Store.SaveDraft(ctx, Definition{SchemaVersion: DefinitionSchemaVersion, ID: "ok", Name: "ok",
		Nodes:    []DefinitionNode{{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}); err != nil {
		t.Fatalf("SaveDraft ok direct: %v", err)
	}
	if _, err := svcFail.Publish(ctx, "ok", ownerPrincipal()); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("Publish store failure: err = %v", err)
	}
	// GetRun access: the run's own non-owner principal is allowed, an
	// unauthenticated principal is not. Owner CancelRun succeeds.
	echo := fixedSpec("test.echo", []Field{{Name: "payload", Type: "object"}}, map[string]any{"v": "x"})
	reg := testRegistry(t, echo)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	store2 := NewMemoryStore()
	eng2 := mustEngine(t, store2, reg)
	svc2, err := NewService(store2, eng2, reg, nil)
	if err != nil {
		t.Fatalf("NewService2: %v", err)
	}
	pub := publishDef(t, store2, def)
	mallory := authz.Identity{Subject: "mallory", Roles: []authz.Role{authz.RoleViewer}}
	run, err := eng2.CreateRun(ctx, pub, mallory, runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun mallory: %v", err)
	}
	if _, _, err := svc2.GetRun(ctx, run.ID, mallory); err != nil {
		t.Fatalf("GetRun own non-owner: %v", err)
	}
	if _, _, err := svc2.GetRun(ctx, run.ID, authz.Identity{}); err == nil {
		t.Fatal("GetRun unauthenticated accepted")
	}
	if _, _, err := svc2.GetRun(ctx, "missing", mallory); err == nil {
		t.Fatal("GetRun missing run accepted")
	}
	cancelled, err := svc2.CancelRun(ctx, run.ID, ownerPrincipal())
	if err != nil || cancelled.State != RunCancelled {
		t.Fatalf("owner CancelRun = %q, err = %v", cancelled.State, err)
	}
	found := false
	for _, rec := range svc2.Audit() {
		if rec.Kind == "cancelled" && rec.RunID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("owner CancelRun left no cancelled audit record")
	}
	// GetRun store failures via FailStore.
	fs = &Continue95NexaflowFailStore{Store: store2, mode: "getrun"}
	svcFail, err = NewService(fs, mustEngine(t, fs, reg), reg, nil)
	if err != nil {
		t.Fatalf("NewService fail4: %v", err)
	}
	if _, _, err := svcFail.GetRun(ctx, run.ID, ownerPrincipal()); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("GetRun store failure: err = %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: store2, mode: "nodes"}
	svcFail, err = NewService(fs, mustEngine(t, fs, reg), reg, nil)
	if err != nil {
		t.Fatalf("NewService fail5: %v", err)
	}
	if _, _, err := svcFail.GetRun(ctx, run.ID, ownerPrincipal()); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("GetRun nodes failure: err = %v", err)
	}
	// Service ListRuns viewer loop: a viewer sees only their own runs.
	aliceRun, err := eng2.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun alice: %v", err)
	}
	visible, err := svc2.ListRuns(ctx, RunFilter{}, mallory)
	if err != nil {
		t.Fatalf("ListRuns viewer: %v", err)
	}
	for _, r := range visible {
		if r.ID == aliceRun.ID {
			t.Fatal("ListRuns viewer sees another principal's run")
		}
	}
	seen := false
	for _, r := range visible {
		if r.ID == run.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("ListRuns viewer misses their own run")
	}
	if _, err := svc2.ListRuns(ctx, RunFilter{}, ownerPrincipal()); err != nil {
		t.Fatalf("ListRuns owner: %v", err)
	}
	fs = &Continue95NexaflowFailStore{Store: store2, mode: "list"}
	svcFail, err = NewService(fs, mustEngine(t, fs, reg), reg, nil)
	if err != nil {
		t.Fatalf("NewService fail6: %v", err)
	}
	if _, err := svcFail.ListRuns(ctx, RunFilter{}, ownerPrincipal()); err == nil || !strings.Contains(err.Error(), "c95 failstore boom") {
		t.Fatalf("ListRuns store failure: err = %v", err)
	}
	// Full service approval flow: StartRun -> ExecuteRun (waiting) ->
	// ApproveRun -> ExecuteRun (success).
	store3 := NewMemoryStore()
	eng3 := mustEngine(t, store3, DefaultRegistry())
	svc3, err := NewService(store3, eng3, DefaultRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService3: %v", err)
	}
	approvalDef := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-test", Name: "test",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "gate", NodeID: "nexaroute.control.approval", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e0", From: "start", To: "gate"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	pub3 := publishDef(t, store3, approvalDef)
	srun, err := svc3.StartRun(ctx, pub3, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	waiting, err := svc3.ExecuteRun(ctx, srun.ID)
	if err != nil || waiting.State != RunWaitingForApproval {
		t.Fatalf("ExecuteRun waiting = %q, err = %v", waiting.State, err)
	}
	if _, err := svc3.ApproveRun(ctx, srun.ID, "gate", authz.Identity{Subject: "bob", Roles: []authz.Role{authz.RoleViewer}}, "approved"); err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
	final, err := svc3.ExecuteRun(ctx, srun.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("ExecuteRun after approval = %q, err = %v", final.State, err)
	}
}

// Continue95NexaflowMigSrc builds a minimal valid migration source: one
// draft plus one published revision of the same definition.
func Continue95NexaflowMigSrc(t *testing.T) (string, Definition) {
	t.Helper()
	src := t.TempDir()
	def := Continue95NexaflowMigDef("wf-mig", "mig-v1")
	hash, err := ContentHash(&def)
	if err != nil {
		t.Fatalf("hash mig: %v", err)
	}
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), def)
	Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
		Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash, Status: DefinitionPublished},
	})
	return src, def
}

func TestContinue95NexaflowMigratePlanBranches(t *testing.T) {
	ctx := context.Background()
	migErr := func(t *testing.T, src string, dst Store) error {
		t.Helper()
		_, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry())
		return err
	}
	// Draft checkLegacyDefinition branches.
	{
		src, _ := Continue95NexaflowMigSrc(t)
		raw := map[string]any{"schema_version": "v9", "id": "wf-mig", "name": "x",
			"nodes":    []any{map[string]any{"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"}},
			"triggers": []any{map[string]any{"name": "start", "kind": "manual", "config": map[string]any{}}}}
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), raw)
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "schema_version") {
			t.Fatalf("draft wrong schema_version: err = %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		raw := map[string]any{"schema_version": "v1", "id": "wf-mig", "name": "x",
			"nodes":    []any{nil, map[string]any{"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"}},
			"triggers": []any{map[string]any{"name": "start", "kind": "manual", "config": map[string]any{}}}}
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), raw)
		if err := migErr(t, src, NewMemoryStore()); err == nil {
			t.Fatal("draft null node accepted")
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		raw := map[string]any{"schema_version": "v1", "id": "wf-mig", "name": "x",
			"nodes":    []any{map[string]any{"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1", "for_each": "items"}},
			"triggers": []any{map[string]any{"name": "start", "kind": "manual", "config": map[string]any{}}}}
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), raw)
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "for_each") {
			t.Fatalf("draft for_each: err = %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		raw := map[string]any{"schema_version": "v1", "id": "wf-mig", "name": "x",
			"nodes":    []any{map[string]any{"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"}},
			"triggers": []any{map[string]any{"name": "start", "kind": "manual", "config": map[string]any{}}},
			"settings": map[string]any{"policy_approved": true}}
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), raw)
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "policy_approved") {
			t.Fatalf("draft policy_approved: err = %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		bad := Continue95NexaflowMigDef("wf-mig", "bad")
		bad.Nodes = append(bad.Nodes, DefinitionNode{ID: "zz", NodeID: "test.nope", Version: SupportedVersion})
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), bad)
		if err := migErr(t, src, NewMemoryStore()); err == nil {
			t.Fatal("draft invalid definition accepted")
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		raw := map[string]any{"schema_version": "v1", "id": "wf-mig", "nodes": "not-a-list"}
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "drafts", "wf-mig.json"), raw)
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "corrupt JSON") {
			t.Fatalf("draft nodes-as-string: err = %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		link := filepath.Join(src, "drafts", "broken.json")
		if err := os.Symlink(filepath.Join(src, "does-not-exist.json"), link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "read") {
			t.Fatalf("draft unreadable symlink: err = %v", err)
		}
	}
	// Drafts dir skips subdirectories and non-JSON files; a drafts path
	// that is a regular file errors; a srcRoot that is a file errors.
	{
		src, _ := Continue95NexaflowMigSrc(t)
		if err := os.MkdirAll(filepath.Join(src, "drafts", "subdir"), 0o750); err != nil {
			t.Fatalf("mkdir subdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(src, "drafts", "notes.txt"), []byte("hi"), 0o600); err != nil {
			t.Fatalf("write notes: %v", err)
		}
		rep, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry())
		if err != nil || rep.Drafts != 1 || rep.Versions != 1 {
			t.Fatalf("draft skips = %+v, err = %v", rep, err)
		}
	}
	{
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "drafts"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write drafts file: %v", err)
		}
		if err := migErr(t, src, NewMemoryStore()); err == nil {
			t.Fatal("drafts-as-file accepted")
		}
	}
	{
		file := filepath.Join(t.TempDir(), "rootfile")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("write rootfile: %v", err)
		}
		if err := migErr(t, file, NewMemoryStore()); err == nil {
			t.Fatal("srcRoot-as-file accepted")
		}
	}
	// Version plan branches.
	{
		src, _ := Continue95NexaflowMigSrc(t)
		if err := os.WriteFile(filepath.Join(src, "versions", "notes.txt"), []byte("hi"), 0o600); err != nil {
			t.Fatalf("write versions notes: %v", err)
		}
		if err := os.WriteFile(filepath.Join(src, "versions", "wf-mig", "notes.txt"), []byte("hi"), 0o600); err != nil {
			t.Fatalf("write version notes: %v", err)
		}
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err != nil {
			t.Fatalf("version skips: %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "2.json"), map[string]any{})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "missing definition or version") {
			t.Fatalf("version record empty: err = %v", err)
		}
	}
	{
		src, def := Continue95NexaflowMigSrc(t)
		hash, _ := ContentHash(&def)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 2, ContentHash: hash, Status: DefinitionPublished},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "does not match its path") {
			t.Fatalf("version path mismatch: err = %v", err)
		}
	}
	{
		src, def := Continue95NexaflowMigSrc(t)
		hash, _ := ContentHash(&def)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash, Status: "draft"},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "unknown version status") {
			t.Fatalf("version status draft: err = %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		bad := Continue95NexaflowMigDef("wf-mig", "bad")
		bad.Nodes = append(bad.Nodes, DefinitionNode{ID: "zz", NodeID: "test.nope", Version: SupportedVersion})
		hash, _ := ContentHash(&bad)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &bad, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash, Status: DefinitionPublished},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil {
			t.Fatal("version invalid definition accepted")
		}
	}
	{
		src, def := Continue95NexaflowMigSrc(t)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: "deadbeef", Status: DefinitionPublished},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
			t.Fatalf("version hash mismatch: err = %v", err)
		}
	}
	{
		src, def := Continue95NexaflowMigSrc(t)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "abc.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: "x", Status: DefinitionPublished},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "non-numeric version file") {
			t.Fatalf("version non-numeric file: err = %v", err)
		}
	}
	{
		src, def := Continue95NexaflowMigSrc(t)
		hash, _ := ContentHash(&def)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash, Status: DefinitionSuperseded},
		})
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "2.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 2, ContentHash: hash, Status: DefinitionPublished},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "identical") {
			t.Fatalf("identical consecutive revisions: err = %v", err)
		}
	}
	{
		src, def := Continue95NexaflowMigSrc(t)
		def2 := Continue95NexaflowMigDef("wf-mig", "mig-v2")
		hash, _ := ContentHash(&def)
		hash2, _ := ContentHash(&def2)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash, Status: DefinitionPublished},
		})
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "2.json"), legacyVersionRecord{
			Definition: &def2, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 2, ContentHash: hash2, Status: DefinitionPublished},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "want superseded") {
			t.Fatalf("earlier revision published: err = %v", err)
		}
	}
	{
		src, def := Continue95NexaflowMigSrc(t)
		def3 := Continue95NexaflowMigDef("wf-mig", "mig-v3")
		hash, _ := ContentHash(&def)
		hash3, _ := ContentHash(&def3)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash, Status: DefinitionSuperseded},
		})
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "3.json"), legacyVersionRecord{
			Definition: &def3, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 3, ContentHash: hash3, Status: DefinitionPublished},
		})
		if err := migErr(t, src, NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "revision gap") {
			t.Fatalf("revision gap: err = %v", err)
		}
	}
}

func TestContinue95NexaflowMigrateRunsAndDestination(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	fin := now
	mkRun := func(id, status string, steps []legacyStep) legacyRun {
		return legacyRun{ID: id, DefinitionID: "wf-mig", Revision: 1, Actor: "alice", Status: status,
			CreatedAt: now, UpdatedAt: now, FinishedAt: &fin, Steps: steps}
	}
	// Runs dir skips, garbage, and as-a-file errors; blocked runs;
	// unknown version; step validation errors.
	{
		src, _ := Continue95NexaflowMigSrc(t)
		if err := os.WriteFile(filepath.Join(src, "runs", "notes.txt"), []byte("hi"), 0o600); err != nil {
			// runs dir may not exist yet; create it first.
			if err := os.MkdirAll(filepath.Join(src, "runs"), 0o750); err != nil {
				t.Fatalf("mkdir runs: %v", err)
			}
			if err := os.WriteFile(filepath.Join(src, "runs", "notes.txt"), []byte("hi"), 0o600); err != nil {
				t.Fatalf("write runs notes: %v", err)
			}
		}
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err != nil {
			t.Fatalf("runs skip: %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		if err := os.MkdirAll(filepath.Join(src, "runs"), 0o750); err != nil {
			t.Fatalf("mkdir runs: %v", err)
		}
		if err := os.WriteFile(filepath.Join(src, "runs", "bad.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write bad run: %v", err)
		}
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "corrupt JSON") {
			t.Fatalf("runs garbage: err = %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		if err := os.WriteFile(filepath.Join(src, "runs"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write runs file: %v", err)
		}
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil {
			t.Fatal("runs-as-file accepted")
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r1.json"), mkRun("r1", "running", nil))
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Fatalf("blocked run: err = %v", err)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		r := mkRun("r1", "succeeded", nil)
		r.Revision = 9
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r1.json"), r)
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "unknown version") {
			t.Fatalf("run unknown version: err = %v", err)
		}
	}
	stepCases := []struct {
		name  string
		steps []legacyStep
		want  string
	}{
		{"unknown-node", []legacyStep{{NodeID: "nope", Attempt: 0, Status: "succeeded", StartedAt: now}}, "unknown node"},
		{"iteration", []legacyStep{{NodeID: "start", Attempt: 0, Iteration: 1, Status: "succeeded", StartedAt: now}}, "iteration"},
		{"status", []legacyStep{{NodeID: "start", Attempt: 0, Status: "waiting", StartedAt: now}}, "unrepresentable status"},
		{"attempt", []legacyStep{{NodeID: "start", Attempt: 1, Status: "succeeded", StartedAt: now}}, "non-contiguous attempt"},
	}
	for _, tc := range stepCases {
		src, _ := Continue95NexaflowMigSrc(t)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r1.json"), mkRun("r1", "succeeded", tc.steps))
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("step %s: err = %v", tc.name, err)
		}
	}
	// Failed run with an error and pending/running/failed steps; a
	// cancelled run with skipped/cancelled steps.
	{
		src, _ := Continue95NexaflowMigSrc(t)
		r := mkRun("r-fail", "failed", []legacyStep{
			{NodeID: "start", Attempt: 0, Status: "pending", StartedAt: now},
			{NodeID: "start", Attempt: 1, Status: "running", StartedAt: now},
			{NodeID: "work", Attempt: 0, Status: "failed", StartedAt: now, Finished: &fin, Error: &NodeError{Code: "boom", Message: "boom"}},
		})
		r.Error = &NodeError{Code: "node_failed", Message: "a node failed"}
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r-fail.json"), r)
		dst := NewMemoryStore()
		rep, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry())
		if err != nil || rep.Runs != 1 {
			t.Fatalf("failed run migrate = %+v, err = %v", rep, err)
		}
		got, err := dst.GetRun(ctx, "r-fail")
		if err != nil || got.State != RunFailed || got.ErrorCode != "node_failed" {
			t.Fatalf("failed run = %+v, err = %v", got, err)
		}
		execs, err := dst.GetNodeExecutions(ctx, "r-fail")
		if err != nil || len(execs) != 3 {
			t.Fatalf("failed run execs = %d, err = %v", len(execs), err)
		}
		byAttempt := map[int]NodeState{}
		for _, e := range execs {
			if e.NodeID == "start" {
				byAttempt[e.Attempt] = e.State
			}
			if e.NodeID == "work" && (e.State != NodeFailed || e.ErrorCode != "boom") {
				t.Fatalf("failed work step = %+v", e)
			}
		}
		if byAttempt[0] != NodePending || byAttempt[1] != NodeRunning {
			t.Fatalf("failed start attempts = %v", byAttempt)
		}
	}
	{
		src, _ := Continue95NexaflowMigSrc(t)
		r := mkRun("r-cancel", "cancelled", []legacyStep{
			{NodeID: "start", Attempt: 0, Status: "skipped", StartedAt: now},
			{NodeID: "work", Attempt: 0, Status: "cancelled", StartedAt: now},
		})
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r-cancel.json"), r)
		dst := NewMemoryStore()
		if _, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry()); err != nil {
			t.Fatalf("cancelled run migrate: %v", err)
		}
		execs, err := dst.GetNodeExecutions(ctx, "r-cancel")
		if err != nil || len(execs) != 2 {
			t.Fatalf("cancelled run execs = %d, err = %v", len(execs), err)
		}
		for _, e := range execs {
			if e.NodeID == "start" && e.State != NodeSkipped {
				t.Fatalf("cancelled start = %+v", e)
			}
			if e.NodeID == "work" && e.State != NodeCancelled {
				t.Fatalf("cancelled work = %+v", e)
			}
		}
	}
	// Nil registry defaults to DefaultRegistry.
	{
		src, _ := Continue95NexaflowMigSrc(t)
		if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), nil); err != nil {
			t.Fatalf("nil registry migrate: %v", err)
		}
	}
	// Destination FailStore errors across phase 1 and phase 2.
	fullSrc := func() string {
		src, def := Continue95NexaflowMigSrc(t)
		def2 := Continue95NexaflowMigDef("wf-mig", "mig-v2")
		hash, _ := ContentHash(&def)
		hash2, _ := ContentHash(&def2)
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "1.json"), legacyVersionRecord{
			Definition: &def, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 1, ContentHash: hash, Status: DefinitionSuperseded},
		})
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "versions", "wf-mig", "2.json"), legacyVersionRecord{
			Definition: &def2, Version: &DefinitionVersion{DefinitionID: "wf-mig", Revision: 2, ContentHash: hash2, Status: DefinitionSuperseded},
		})
		Continue95NexaflowWriteJSON(t, filepath.Join(src, "runs", "r1.json"), mkRun("r1", "succeeded", []legacyStep{
			{NodeID: "start", Attempt: 0, Status: "succeeded", StartedAt: now, Finished: &fin},
		}))
		return src
	}
	dstCases := []struct {
		mode string
		want string
	}{
		{"getdraft", "c95 failstore boom"},
		{"getrun", "c95 failstore boom"},
		{"listrevisions", "c95 failstore boom"},
		{"savedraft", "save draft"},
		{"publish", "publish"},
		{"supersede", "supersede"},
		{"createrun", "create run"},
		{"append", "append step"},
	}
	for _, tc := range dstCases {
		fs := &Continue95NexaflowFailStore{Store: NewMemoryStore(), mode: tc.mode}
		if _, err := MigratePhase8Dir(ctx, fullSrc(), fs, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("migrate dst %s: err = %v", tc.mode, err)
		}
	}
}

func TestContinue95NexaflowDeliveryLedgerNilDB(t *testing.T) {
	if _, err := NewPostgresDeliveryLedger(nil); err == nil {
		t.Fatal("PostgresDeliveryLedger nil db accepted")
	}
}
