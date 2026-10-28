package nexaflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// ---- Scenario 7 (part 2): retries, fallback limits, timeout, cancel, subworkflow ----

func TestTask8Scenario7_Retries(t *testing.T) {
	be := newTask8FakeBackend()
	be.usageErrs = []error{
		NewNodeError("backend_error", true, "transient 1"),
		NewNodeError("backend_error", true, "transient 2"),
	}
	s := newTask8Setup(t, be)
	def := task8Def("retry-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("cost", "nexaroute.cost.usage", nil, map[string]any{}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "cost", "")})
	pub := s.publish(t, def)
	run := s.runToEnd(t, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "retry-key")
	if run.State != RunSucceeded {
		t.Fatalf("run state = %q (err %q)", run.State, run.Error)
	}
	if be.usageCount() != 3 {
		t.Fatalf("usage calls = %d, want 3 (2 failures + success)", be.usageCount())
	}
	execs, err := s.store.GetNodeExecutions(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	attempts := 0
	for _, e := range execs {
		if e.NodeID == "cost" {
			attempts++
		}
	}
	if attempts != 3 {
		t.Fatalf("cost attempts recorded = %d, want 3", attempts)
	}
}

// Error-handler node maps an error to an action, but the canonical
// engine has no run-level fallback routing: a node downstream of a
// failed node is skipped, never fed the failure. This test proves both
// halves honestly: the mapping works, the routing does not exist yet.
func TestTask8Scenario7_FallbackLimits(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	def := task8Def("eh-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("eh", "nexaroute.control.error_handler", map[string]any{"policy": "fallback"}, map[string]any{"error_code": "backend_error", "message": "boom"}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "eh", "")})
	pub := s.publish(t, def)
	run := s.runToEnd(t, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "eh-key")
	if run.State != RunSucceeded {
		t.Fatalf("error_handler run state = %q", run.State)
	}
	// Downstream of a failed node is skipped: no fallback execution.
	s2 := newTask8Setup(t, newTask8FakeBackend())
	def2 := task8Def("fb-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("bad", "nexaroute.control.condition", map[string]any{"field": "x", "operator": "bogus"}, map[string]any{"value": map[string]any{"x": 1}}),
			task8Node("eh", "nexaroute.control.error_handler", map[string]any{"policy": "fallback"}, map[string]any{"error_code": "validation_error", "message": "m"}),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "bad", ""),
			task8Edge("e2", "bad", "eh", ""),
		})
	pub2 := s2.publish(t, def2)
	run2 := s2.runToEnd(t, pub2, task8Requester, map[string]any{"payload": map[string]any{}}, "fb-key")
	if run2.State != RunFailed {
		t.Fatalf("failing run state = %q, want failed", run2.State)
	}
	if st := task8NodeStates(t, s2.store, run2.ID); st["eh"].State != NodeSkipped {
		t.Fatalf("error_handler downstream state = %q, want skipped (routing not supported)", st["eh"].State)
	}
}

// Timeout mechanism at the registry level + cancellation at run level.
func TestTask8Scenario7_TimeoutAndCancel(t *testing.T) {
	reg, err := NewRegistry(append(DefaultRegistry().All(), &NodeSpec{
		ID: "test.slow", Version: SupportedVersion, Category: "test",
		InputSchema:    []Field{},
		OutputSchema:   []Field{},
		Retry:          RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: 50 * time.Millisecond, MaxTimeout: 50 * time.Millisecond,
		MaxInputBytes: 1024, MaxOutputBytes: 1024,
		Run: func(ctx context.Context, rc *RunContext) (map[string]any, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return map[string]any{}, nil
			}
		},
	}))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	spec, _ := reg.Lookup("test.slow", SupportedVersion)
	if _, err := reg.Execute(context.Background(), spec, task8Owner, map[string]any{}, map[string]any{}, nil, false); err == nil {
		t.Fatalf("slow node did not time out")
	} else {
		var nerr *NodeError
		if !errors.As(err, &nerr) || nerr.Code != "timeout" {
			t.Fatalf("slow node error = %v, want timeout", err)
		}
	}

	// Cancellation: a wait node is cancelled mid-flight via CancelRun.
	s := newTask8Setup(t, newTask8FakeBackend())
	def := task8Def("cancel-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("wait", "nexaroute.control.wait", map[string]any{"max_duration": "5m"}, map[string]any{"duration_ms": float64(60000)}),
			task8TransformLeaf("after", "after"),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "wait", ""),
			task8Edge("e2", "wait", "after", ""),
		})
	pub := s.publish(t, def)
	ctx := context.Background()
	run, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "cancel-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	done := make(chan Run, 1)
	go func() {
		r, _ := s.service.ExecuteRun(ctx, run.ID)
		done <- r
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := s.service.CancelRun(ctx, run.ID, task8Requester); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	select {
	case r := <-done:
		if r.State != RunCancelled {
			t.Fatalf("cancelled run state = %q, want cancelled", r.State)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ExecuteRun did not return after cancel")
	}
	if st := task8NodeStates(t, s.store, run.ID); st["after"].State != NodeSkipped && st["after"].State != NodeCancelled {
		t.Fatalf("downstream after cancel = %q, want skipped/cancelled", st["after"].State)
	}
}

// No subworkflow node exists in the registry: authoring one fails
// validation instead of silently doing nothing.
func TestTask8Scenario7_SubworkflowAbsent(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	def := task8Def("sub-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("child", "nexaroute.control.subworkflow", nil, nil),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "child", "")})
	if err := s.service.SaveDraft(context.Background(), def); err == nil {
		t.Fatalf("subworkflow node accepted, but no such node exists")
	}
}

// ---- Scenario 8: approval suspend / restart / separate approver / resume ----

func TestTask8Scenario8_ApprovalRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	reg := DefaultRegistry()
	be := newTask8FakeBackend()
	engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "w1"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	def := task8Def("appr-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("appr", "nexaroute.control.approval", nil, nil),
			task8TransformLeaf("done", "done"),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "appr", ""),
			task8Edge("e2", "appr", "done", ""),
		})
	ctx := context.Background()
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	pub, err := svc.Publish(ctx, def.ID, task8Owner)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	run, err := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "appr-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, err = svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #1: %v", err)
	}
	if run.State != RunWaitingForApproval {
		t.Fatalf("run state = %q, want waiting_for_approval", run.State)
	}
	// Gateway restart: a new engine over the same file store.
	if err := store.Close(); err != nil {
		t.Fatalf("store Close: %v", err)
	}
	store2, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	// Same worker id after restart: a restarted gateway reuses its
	// configured worker id. (A different worker id must wait out the
	// old worker's live 30s lease by design; Recover never breaks a
	// live foreign lease.)
	engine2, err := NewEngine(EngineOptions{Store: store2, Registry: reg, Backend: be, WorkerID: "w1"})
	if err != nil {
		t.Fatalf("NewEngine #2: %v", err)
	}
	svc2, err := NewService(store2, engine2, reg, nil)
	if err != nil {
		t.Fatalf("NewService #2: %v", err)
	}
	// Self-approval is rejected after restart too.
	if _, err := svc2.ApproveRun(ctx, run.ID, "appr", task8Requester, "approved"); err == nil {
		t.Fatalf("self-approval accepted after restart")
	}
	if _, err := svc2.ApproveRun(ctx, run.ID, "appr", task8Approver, "approved"); err != nil {
		t.Fatalf("separate approver rejected: %v", err)
	}
	run, err = svc2.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #2: %v", err)
	}
	if run.State != RunSucceeded {
		t.Fatalf("resumed run state = %q (err %q), want succeeded", run.State, run.Error)
	}
	states := task8NodeStates(t, store2, run.ID)
	if states["appr"].Output["approved"] != true || states["appr"].Output["approver"] != "op-1" {
		t.Fatalf("approval output = %v", states["appr"].Output)
	}
	if states["done"].State != NodeSucceeded {
		t.Fatalf("downstream state = %q, want succeeded", states["done"].State)
	}
}

// Rejection semantics, proven rather than assumed: rejecting an
// APPROVAL node re-runs it with decision=rejected, so the run
// succeeds with approved=false for downstream logic to react to.
// Rejecting a POLICY gate (llm.chat) is different: it fails the node
// with approval_rejected and fails the run.
func TestTask8Scenario8_ApprovalRejected(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	def := task8Def("appr-rej",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("appr", "nexaroute.control.approval", nil, nil),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "appr", "")})
	pub := s.publish(t, def)
	ctx := context.Background()
	run, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "appr-rej")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := s.service.ExecuteRun(ctx, run.ID); err != nil {
		t.Fatalf("ExecuteRun #1: %v", err)
	}
	if _, err := s.service.ApproveRun(ctx, run.ID, "appr", task8Approver, "rejected"); err != nil {
		t.Fatalf("ApproveRun rejected: %v", err)
	}
	run, err = s.service.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #2: %v", err)
	}
	if run.State != RunSucceeded {
		t.Fatalf("rejected-approval run state = %q, want succeeded (rejection is data)", run.State)
	}
	if out := task8NodeStates(t, s.store, run.ID)["appr"].Output; out["approved"] != false {
		t.Fatalf("approval output = %v, want approved=false", out)
	}

	// Policy-gate rejection: llm.chat node fails, run fails.
	be := newTask8FakeBackend()
	s2 := newTask8Setup(t, be)
	def2 := task8Def("llm-rej",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("chat", "nexaroute.llm.chat", map[string]any{"route_or_alias": "test-route"}, map[string]any{"prompt": "hi"}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "chat", "")})
	pub2 := s2.publish(t, def2)
	run2, err := s2.service.StartRun(ctx, pub2, task8Requester, map[string]any{"payload": map[string]any{}}, "llm-rej")
	if err != nil {
		t.Fatalf("StartRun llm: %v", err)
	}
	if _, err := s2.service.ExecuteRun(ctx, run2.ID); err != nil {
		t.Fatalf("ExecuteRun llm #1: %v", err)
	}
	if _, err := s2.service.ApproveRun(ctx, run2.ID, "chat", task8Approver, "rejected"); err != nil {
		t.Fatalf("ApproveRun llm rejected: %v", err)
	}
	run2, err = s2.service.ExecuteRun(ctx, run2.ID)
	if err != nil {
		t.Fatalf("ExecuteRun llm #2: %v", err)
	}
	if run2.State != RunFailed {
		t.Fatalf("policy-rejected run state = %q, want failed", run2.State)
	}
	if st := task8NodeStates(t, s2.store, run2.ID)["chat"]; st.ErrorCode != "approval_rejected" {
		t.Fatalf("policy node error = %q, want approval_rejected", st.ErrorCode)
	}
	if be.chatCount() != 0 {
		t.Fatalf("chat calls after policy rejection = %d, want 0", be.chatCount())
	}
}

// ---- Scenario 9 + 14: crash ambiguity and restart recovery matrix ----

func task8CraftNode(t *testing.T, store Store, runID, nodeID string, state NodeState, dispatched bool, leaseUntil time.Time, worker string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.UpdateRunState(ctx, runID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("UpdateRunState to running: %v", err)
	}
	// Claim the row CreateRun already created (no duplicate attempt).
	claimed, ok, err := store.TryClaim(ctx, runID, nodeID, worker, time.Hour, time.Now())
	if err != nil || !ok {
		t.Fatalf("TryClaim: ok=%v err=%v", ok, err)
	}
	next := claimed
	next.State = state
	next.Dispatched = dispatched
	next.LeaseUntil = leaseUntil
	next.LeaseOwner = worker
	if _, err := store.SaveNodeExecution(ctx, next, NodeClaimed); err != nil {
		t.Fatalf("SaveNodeExecution to %q: %v", state, err)
	}
}

func TestTask8Scenario9_CrashAmbiguity(t *testing.T) {
	be := newTask8FakeBackend()
	store := NewMemoryStore()
	reg := DefaultRegistry()
	engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "crashed-worker"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	def := task8Def("crash-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("call", "nexaroute.mcp.call", map[string]any{"allowlist": []any{"allowed-tool"}}, map[string]any{"server_id": "srv-1", "tool": "allowed-tool", "arguments": map[string]any{}}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "call", "")})
	pub := svc.publishForTest(t, def)
	ctx := context.Background()
	run, err := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "crash-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// Simulate: dispatch happened (Dispatched=true), lease expired,
	// process died before the result was persisted.
	past := time.Now().Add(-time.Hour)
	task8CraftNode(t, store, run.ID, "call", NodeRunning, true, past, "crashed-worker")
	// A fresh engine recovers: ambiguous effectful node is parked, and
	// Execute refuses to blindly re-run it.
	engine2, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "recovered-worker"})
	if err != nil {
		t.Fatalf("NewEngine #2: %v", err)
	}
	if err := engine2.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	states := task8NodeStates(t, store, run.ID)
	if states["call"].State != NodeNeedsManualAction || !states["call"].Ambiguous {
		t.Fatalf("crashed node = %q ambiguous=%v, want needs_manual_action/true", states["call"].State, states["call"].Ambiguous)
	}
	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != RunNeedsManualAction {
		t.Fatalf("run state = %q, want needs_manual_action", got.State)
	}
	if _, err := engine2.Execute(ctx, run.ID); !errors.Is(err, ErrRunParked) {
		t.Fatalf("Execute parked run error = %v, want ErrRunParked", err)
	}
	if be.mcpCount() != 0 {
		t.Fatalf("mcp calls after recovery = %d, want 0 (no blind repeat)", be.mcpCount())
	}

	// Counter-case: a pure node that was never dispatched re-queues and
	// completes safely after the same crash.
	store2 := NewMemoryStore()
	engine3, _ := NewEngine(EngineOptions{Store: store2, Registry: reg, Backend: be, WorkerID: "w3"})
	svc3, _ := NewService(store2, engine3, reg, nil)
	pub3 := svc3.publishForTest(t, task8Def("crash-pure",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8TransformLeaf("leaf", "leaf"),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "leaf", "")}))
	run3, err := svc3.StartRun(ctx, pub3, task8Requester, map[string]any{"payload": map[string]any{}}, "crash-pure")
	if err != nil {
		t.Fatalf("StartRun pure: %v", err)
	}
	task8CraftNode(t, store2, run3.ID, "leaf", NodeRunning, false, past, "crashed-worker")
	engine4, _ := NewEngine(EngineOptions{Store: store2, Registry: reg, Backend: be, WorkerID: "w4"})
	if err := engine4.Recover(ctx); err != nil {
		t.Fatalf("Recover pure: %v", err)
	}
	if st := task8NodeStates(t, store2, run3.ID); st["leaf"].State != NodePending {
		t.Fatalf("undispatched pure node = %q, want pending (safe requeue)", st["leaf"].State)
	}
	run3, err = engine4.Execute(ctx, run3.ID)
	if err != nil {
		t.Fatalf("Execute pure: %v", err)
	}
	if run3.State != RunSucceeded {
		t.Fatalf("recovered pure run = %q, want succeeded", run3.State)
	}
}

// ---- Scenario 10: video nodes through the Backend video port ----

func TestTask8Scenario10_Video(t *testing.T) {
	be := newTask8FakeBackend()
	be.videoAssets = map[string][]byte{"a1": []byte("fake-mp4")}
	s := newTask8Setup(t, be)
	def := task8Def("video-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("submit", "nexaroute.video.submit", nil, map[string]any{"prompt": "a cat", "duration_seconds": float64(5)}),
			task8Node("status", "nexaroute.video.status", nil, map[string]any{"job_id": "$.submit.job_id"}),
			task8Node("out", "nexaroute.video.output", nil, map[string]any{"job_id": "$.submit.job_id", "asset_id": "a1"}),
		},
		[]DefinitionEdge{
			task8Edge("e1", "start", "submit", ""),
			task8Edge("e2", "submit", "status", ""),
			task8Edge("e3", "status", "out", ""),
		})
	pub := s.publish(t, def)
	ctx := context.Background()
	run, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "video-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// Policy gate first (video.submit is billable), then approved run.
	if _, err := s.service.ExecuteRun(ctx, run.ID); err != nil {
		t.Fatalf("ExecuteRun #1: %v", err)
	}
	if _, err := s.service.ApproveRun(ctx, run.ID, "submit", task8Approver, "approved"); err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
	run, err = s.service.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun #2: %v", err)
	}
	if run.State != RunSucceeded {
		t.Fatalf("video run state = %q (err %q)", run.State, run.Error)
	}
	states := task8NodeStates(t, s.store, run.ID)
	if states["submit"].Output["job_id"] != "job-1" {
		t.Fatalf("submit output = %v", states["submit"].Output)
	}
	if states["status"].Output["terminal"] != true {
		t.Fatalf("status output = %v, want terminal=true", states["status"].Output)
	}
	assets, _ := states["out"].Output["assets"].([]any)
	if len(assets) != 1 {
		t.Fatalf("output assets = %v, want 1", states["out"].Output)
	}
	if _, hasPath := states["out"].Output["path"]; hasPath {
		t.Fatalf("video output leaks a filesystem path")
	}
	if be.videoSubmit != 1 {
		t.Fatalf("video submits = %d, want 1", be.videoSubmit)
	}
}

// ---- Scenario 11: permissions, ownership, budget gate, payload, redaction, audit ----

type task8Guard struct {
	reserves int
	releases int
	fail     bool
}

func (g *task8Guard) Reserve(_ context.Context, _ string, _ float64) (func(), error) {
	if g.fail {
		return nil, fmt.Errorf("nexaflow: budget exhausted (test guard)")
	}
	g.reserves++
	return func() { g.releases++ }, nil
}

func TestTask8Scenario11_Boundaries(t *testing.T) {
	be := newTask8FakeBackend()
	store := NewMemoryStore()
	reg := DefaultRegistry()
	guard := &task8Guard{}
	var svc *Service
	engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "b-worker", Audit: func(ev AuditEvent) {
		if svc != nil {
			svc.HandleEngineEvent(ev)
		}
	}})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err = NewService(store, engine, reg, guard)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	def := task8Def("bound-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8TransformLeaf("leaf", "leaf"),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "leaf", "")})
	pub := svc.publishForTest(t, def)

	// Anonymous principal cannot start a run.
	if _, err := svc.StartRun(ctx, pub, authz.Identity{}, map[string]any{"payload": map[string]any{}}, "anon"); err == nil {
		t.Fatalf("anonymous start accepted")
	}
	// Budget guard failure fails admission closed, with no run created.
	guard.fail = true
	if _, err := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "guard-fail"); err == nil {
		t.Fatalf("guard-failing start accepted")
	}
	if runs, _ := store.ListRuns(ctx, RunFilter{}); len(runs) != 0 {
		t.Fatalf("guard failure left %d runs", len(runs))
	}
	guard.fail = false
	run := sRunToEnd(t, svc, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "guard-ok")
	if guard.reserves != 1 {
		t.Fatalf("reserves = %d, want 1", guard.reserves)
	}
	if guard.releases != 1 {
		t.Fatalf("releases after terminal = %d, want 1 (engine run_finished released it)", guard.releases)
	}
	// Ownership: another subject cannot read the run; owner can.
	if _, _, err := svc.GetRun(ctx, run.ID, authz.Identity{Subject: "mallory", Roles: []authz.Role{authz.RoleViewer}}); err == nil {
		t.Fatalf("non-owner read accepted")
	}
	if _, _, err := svc.GetRun(ctx, run.ID, task8Owner); err != nil {
		t.Fatalf("owner read failed: %v", err)
	}
	if runs, _ := svc.ListRuns(ctx, RunFilter{}, task8Viewer); len(runs) != 0 {
		t.Fatalf("viewer listed %d runs, want 0 (only own runs)", len(runs))
	}
	// Payload bound: run input over 64 KiB is rejected.
	big := strings.Repeat("x", 70<<10)
	if _, err := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{"blob": big}}, "big-key"); err == nil {
		t.Fatalf("oversized run input accepted")
	}
	// Audit log records decisions.
	found := false
	for _, rec := range svc.Audit() {
		if rec.Kind == "run_started" && rec.RunID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit log has no run_started for %q", run.ID)
	}
	// budget_check is report-only: over threshold reports, blocks nothing.
	def2 := task8Def("budget-report",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("chk", "nexaroute.cost.budget_check", nil, map[string]any{"key": "k", "threshold_usd": float64(1)}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "chk", "")})
	pub2 := svc.publishForTest(t, def2)
	run2 := sRunToEnd(t, svc, pub2, task8Requester, map[string]any{"payload": map[string]any{}}, "budget-report")
	if out := task8NodeStates(t, store, run2.ID)["chk"].Output; out["over_threshold"] != true {
		t.Fatalf("budget_check output = %v, want over_threshold=true (1.5+0.5 > 1)", out)
	}
}

func TestTask8Scenario11_RedactionAndTruncation(t *testing.T) {
	// Output exceeds the 64 KiB stored-output bound even after the
	// redactor's per-string cap: 20 pads of 8000 chars each.
	padFields := []Field{{Name: "api_key", Type: "string"}}
	padOut := map[string]any{"api_key": "sk-live-do-not-store"}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("pad%02d", i)
		padFields = append(padFields, Field{Name: name, Type: "string"})
		padOut[name] = strings.Repeat("p", 8000)
	}
	reg, err := NewRegistry(append(DefaultRegistry().All(), &NodeSpec{
		ID: "test.leaky", Version: SupportedVersion, Category: "test",
		InputSchema:    []Field{},
		OutputSchema:   padFields,
		Retry:          RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: time.Second, MaxTimeout: time.Second,
		MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20,
		Run: func(_ context.Context, rc *RunContext) (map[string]any, error) {
			return padOut, nil
		},
	}))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	store := NewMemoryStore()
	engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: newTask8FakeBackend(), WorkerID: "r-worker"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	def := task8Def("leak-wf",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("leak", "test.leaky", nil, map[string]any{}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "leak", "")})
	pub := svc.publishForTest(t, def)
	run := sRunToEnd(t, svc, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "leak-key")
	if run.State != RunSucceeded {
		t.Fatalf("leak run state = %q", run.State)
	}
	st := task8NodeStates(t, store, run.ID)["leak"]
	if st.Output["api_key"] != "[REDACTED]" {
		t.Fatalf("stored api_key = %v, want [REDACTED]", st.Output["api_key"])
	}
	if !st.OutputTruncated || st.Output["_truncated"] != true {
		t.Fatalf("oversized output not marked truncated: truncated=%v out=%v", st.OutputTruncated, st.Output)
	}
}

// ---- Scenario 12: inspection of runs, nodes, history ----

func TestTask8Scenario12_Inspection(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	pub := s.publish(t, task8BranchDef())
	run := s.runToEnd(t, pub, task8Requester, map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}, "insp-key")
	got, execs, err := s.service.GetRun(context.Background(), run.ID, task8Requester)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != run.ID || got.State != RunSucceeded {
		t.Fatalf("GetRun = %+v", got.State)
	}
	byID := map[string]NodeExecution{}
	for _, e := range execs {
		byID[e.NodeID] = e
	}
	for _, id := range []string{"start", "chk", "br", "fast", "slow"} {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("no execution record for node %q", id)
		}
		if e.Attempt < 0 || e.FinishedAt.IsZero() {
			t.Fatalf("node %q lacks attempt/finish timing: %+v", id, e)
		}
		// Skipped nodes never started (StartedAt zero is correct for
		// them); executed nodes must carry a start time.
		if e.State == NodeSucceeded && e.StartedAt.IsZero() {
			t.Fatalf("succeeded node %q lacks StartedAt", id)
		}
	}
	if byID["fast"].Output["value"] == nil {
		t.Fatalf("fast node output not inspectable: %v", byID["fast"].Output)
	}
	revs, err := s.service.ListRevisions(context.Background(), pub.Version.DefinitionID)
	if err != nil || len(revs) != 1 {
		t.Fatalf("ListRevisions = %d, err %v, want 1", len(revs), err)
	}
}

// ---- Scenario 15: service construction fails closed ----

func TestTask8Scenario15_ServiceFailsClosed(t *testing.T) {
	if _, err := NewService(nil, nil, nil, nil); err == nil {
		t.Fatalf("NewService accepted nil dependencies")
	}
	if _, err := NewEngine(EngineOptions{}); err == nil {
		t.Fatalf("NewEngine accepted empty options")
	}
	if _, err := NewWebhookService(nil, nil, nil); err == nil {
		t.Fatalf("NewWebhookService accepted nil ledger/start")
	}
	if _, err := NewScheduler(nil, nil); err == nil {
		t.Fatalf("NewScheduler accepted nil ledger/start")
	}
	if _, err := NewFileDeliveryLedger(""); err == nil {
		t.Fatalf("NewFileDeliveryLedger accepted empty dir")
	}
}

// ---- PostgreSQL-gated: ledger claims + scheduler slots on shared PG ----

// publishForTest publishes a definition through a bare Service (used
// by tests that build the Service directly instead of task8Setup).
func (s *Service) publishForTest(t *testing.T, def Definition) PublishedDefinition {
	t.Helper()
	ctx := context.Background()
	if err := s.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft %q: %v", def.ID, err)
	}
	pub, err := s.Publish(ctx, def.ID, task8Owner)
	if err != nil {
		t.Fatalf("Publish %q: %v", def.ID, err)
	}
	return pub
}

func sRunToEnd(t *testing.T, svc *Service, pub PublishedDefinition, principal authz.Identity, input map[string]any, key string) Run {
	t.Helper()
	ctx := context.Background()
	run, err := svc.StartRun(ctx, pub, principal, input, key)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, err = svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	return run
}

func task8PGDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_PG_DSN not set; PostgreSQL live acceptance skipped")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("pg ping: %v (set NEXAROUTE_TEST_PG_DSN to a disposable database)", err)
	}
	if err := EnsurePostgresDeliverySchema(context.Background(), db); err != nil {
		t.Fatalf("EnsurePostgresDeliverySchema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTask8PostgresLedger(t *testing.T) {
	db := task8PGDB(t)
	ctx := context.Background()
	l1, err := NewPostgresDeliveryLedger(db)
	if err != nil {
		t.Fatalf("ledger1: %v", err)
	}
	l2, err := NewPostgresDeliveryLedger(db)
	if err != nil {
		t.Fatalf("ledger2: %v", err)
	}
	key := fmt.Sprintf("task8-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM nexaroute_workflow_deliveries WHERE delivery_key = $1`, key) })
	claimed, err := l1.Claim(ctx, key)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, err %v", claimed, err)
	}
	claimed, err = l2.Claim(ctx, key)
	if err != nil || claimed {
		t.Fatalf("second claim (other ledger handle, same DB) = %v, err %v, want false", claimed, err)
	}
	// The Postgres store contract itself runs against this DB via
	// the ported suite when the same DSN is set (see store_contract).
}

// ---- Scenario 13: migration of a #691 directory store ----

func task8WriteLegacyStore(t *testing.T, root string, withBlocked bool, corrupt bool) {
	t.Helper()
	def := task8BranchDef()
	def.ID = "mig-wf"
	hash, err := ContentHash(&def)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	writeJSON := func(path string, v any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		raw, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if corrupt {
		if err := os.MkdirAll(filepath.Join(root, "drafts"), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "drafts", "mig-wf.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}
		return
	}
	writeJSON(filepath.Join(root, "drafts", "mig-wf.json"), def)
	// Two revisions: rev1 superseded, rev2 published. The rev2 snapshot
	// differs (expected "bad") so hashes differ and publish reproduces.
	def2 := def
	nodes2 := append([]DefinitionNode{}, def.Nodes...)
	nodes2[1].Config = map[string]any{"field": "health.status", "operator": "eq", "expected": "bad"}
	def2.Nodes = nodes2
	hash2, _ := ContentHash(&def2)
	writeJSON(filepath.Join(root, "versions", "mig-wf", "1.json"), map[string]any{
		"definition": def,
		"version":    map[string]any{"definition_id": "mig-wf", "revision": 1, "content_hash": hash, "status": "superseded"},
	})
	writeJSON(filepath.Join(root, "versions", "mig-wf", "2.json"), map[string]any{
		"definition": def2,
		"version":    map[string]any{"definition_id": "mig-wf", "revision": 2, "content_hash": hash2, "status": "published"},
	})
	status := "succeeded"
	if withBlocked {
		status = "running"
	}
	writeJSON(filepath.Join(root, "runs", "run-mig-1.json"), map[string]any{
		"id": "run-mig-1", "definition_id": "mig-wf", "revision": 2,
		"actor": "alice", "status": status,
		"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}},
		"steps": []map[string]any{
			{"node_id": "start", "attempt": 0, "iteration": 0, "status": "succeeded",
				"output": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}},
			{"node_id": "chk", "attempt": 0, "iteration": 0, "status": "succeeded",
				"output": map[string]any{"result": true, "field": "health.status"}},
		},
		"created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:01:00Z",
		"finished_at": "2026-10-01T00:01:00Z",
	})
}

func TestTask8Scenario13_Migration(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	task8WriteLegacyStore(t, src, false, false)
	dst := NewMemoryStore()
	rep, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry())
	if err != nil {
		t.Fatalf("MigratePhase8Dir: %v", err)
	}
	if rep.Drafts != 1 || rep.Versions != 2 || rep.Runs != 1 {
		t.Fatalf("report = %+v, want drafts=1 versions=2 runs=1", rep)
	}
	revs, err := dst.ListRevisions(ctx, "mig-wf")
	if err != nil || len(revs) != 2 {
		t.Fatalf("revisions = %d, err %v", len(revs), err)
	}
	if revs[0].Version.Status != DefinitionSuperseded || revs[1].Version.Status != DefinitionPublished {
		t.Fatalf("revision statuses = %q/%q", revs[0].Version.Status, revs[1].Version.Status)
	}
	run, err := dst.GetRun(ctx, "run-mig-1")
	if err != nil {
		t.Fatalf("GetRun migrated: %v", err)
	}
	if run.State != RunSucceeded || run.DefinitionID != "mig-wf" || run.Revision != 2 {
		t.Fatalf("migrated run = %+v", run.State)
	}
	execs, err := dst.GetNodeExecutions(ctx, "run-mig-1")
	if err != nil || len(execs) != 2 {
		t.Fatalf("migrated executions = %d, err %v", len(execs), err)
	}

	// Corrupt input aborts with zero writes.
	srcBad := t.TempDir()
	task8WriteLegacyStore(t, srcBad, false, true)
	dstBad := NewMemoryStore()
	if _, err := MigratePhase8Dir(ctx, srcBad, dstBad, DefaultRegistry()); err == nil {
		t.Fatalf("corrupt migration accepted")
	}
	if _, err := dstBad.GetDraft(ctx, "mig-wf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt migration wrote a draft: %v", err)
	}
	if runs, _ := dstBad.ListRuns(ctx, RunFilter{}); len(runs) != 0 {
		t.Fatalf("corrupt migration wrote %d runs", len(runs))
	}

	// A non-terminal run blocks the whole migration, by id.
	srcBlocked := t.TempDir()
	task8WriteLegacyStore(t, srcBlocked, true, false)
	dstBlocked := NewMemoryStore()
	if _, err := MigratePhase8Dir(ctx, srcBlocked, dstBlocked, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "run-mig-1") {
		t.Fatalf("blocked migration error = %v, want it to name run-mig-1", err)
	}
	if runs, _ := dstBlocked.ListRuns(ctx, RunFilter{}); len(runs) != 0 {
		t.Fatalf("blocked migration wrote %d runs", len(runs))
	}

	// An on_error extension is rejected, never silently dropped.
	srcExt := t.TempDir()
	task8WriteLegacyStore(t, srcExt, false, false)
	raw, _ := os.ReadFile(filepath.Join(srcExt, "drafts", "mig-wf.json"))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["on_error"] = map[string]any{"node": "eh"}
	raw2, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(srcExt, "drafts", "mig-wf.json"), raw2, 0o600)
	if _, err := MigratePhase8Dir(ctx, srcExt, NewMemoryStore(), DefaultRegistry()); err == nil {
		t.Fatalf("on_error migration accepted")
	}
}

// ---- Scenario 14: restart recovery per execution state ----

func TestTask8Scenario14_RecoveryMatrix(t *testing.T) {
	ctx := context.Background()
	reg := DefaultRegistry()
	be := newTask8FakeBackend()
	now := time.Now()

	// Case A: claimed, never dispatched, lease expired -> pending.
	{
		store := NewMemoryStore()
		eng, _ := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "a"})
		svc, _ := NewService(store, eng, reg, nil)
		pub := svc.publishForTest(t, task8Def("rec-a",
			[]DefinitionNode{
				task8Node("start", "nexaroute.trigger.manual", nil, nil),
				task8TransformLeaf("leaf", "leaf"),
			},
			[]DefinitionEdge{task8Edge("e1", "start", "leaf", "")}))
		run, _ := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "rec-a")
		task8CraftNode(t, store, run.ID, "leaf", NodeClaimed, false, now.Add(-time.Hour), "dead-worker")
		eng2, _ := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "a2", Now: func() time.Time { return now }})
		if err := eng2.Recover(ctx); err != nil {
			t.Fatalf("Recover A: %v", err)
		}
		if st := task8NodeStates(t, store, run.ID); st["leaf"].State != NodePending {
			t.Fatalf("case A leaf = %q, want pending", st["leaf"].State)
		}
	}

	// Case B: waiting approval with an expired deadline -> failed approval_expired.
	{
		store := NewMemoryStore()
		eng, _ := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "b", ApprovalTimeout: time.Hour, Now: func() time.Time { return now }})
		svc, _ := NewService(store, eng, reg, nil)
		pub := svc.publishForTest(t, task8Def("rec-b",
			[]DefinitionNode{
				task8Node("start", "nexaroute.trigger.manual", nil, nil),
				task8Node("appr", "nexaroute.control.approval", nil, nil),
			},
			[]DefinitionEdge{task8Edge("e1", "start", "appr", "")}))
		run, _ := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "rec-b")
		if _, err := svc.ExecuteRun(ctx, run.ID); err != nil {
			t.Fatalf("ExecuteRun B: %v", err)
		}
		// Move the deadline into the past (same-state save).
		execs, _ := store.GetNodeExecutions(ctx, run.ID)
		for _, e := range execs {
			if e.NodeID == "appr" {
				e.Deadline = now.Add(-time.Minute)
				if _, err := store.SaveNodeExecution(ctx, e, NodeWaiting); err != nil {
					t.Fatalf("set deadline: %v", err)
				}
			}
		}
		eng2, _ := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "b2", ApprovalTimeout: time.Hour, Now: func() time.Time { return now }})
		if err := eng2.Recover(ctx); err != nil {
			t.Fatalf("Recover B: %v", err)
		}
		if st := task8NodeStates(t, store, run.ID); st["appr"].State != NodeFailed || st["appr"].ErrorCode != "approval_expired" {
			t.Fatalf("case B appr = %q/%q, want failed/approval_expired", st["appr"].State, st["appr"].ErrorCode)
		}
	}

	// Case C: waiting approval without expiry stays waiting across restart.
	{
		store := NewMemoryStore()
		eng, _ := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "c"})
		svc, _ := NewService(store, eng, reg, nil)
		pub := svc.publishForTest(t, task8Def("rec-c",
			[]DefinitionNode{
				task8Node("start", "nexaroute.trigger.manual", nil, nil),
				task8Node("appr", "nexaroute.control.approval", nil, nil),
			},
			[]DefinitionEdge{task8Edge("e1", "start", "appr", "")}))
		run, _ := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "rec-c")
		if _, err := svc.ExecuteRun(ctx, run.ID); err != nil {
			t.Fatalf("ExecuteRun C: %v", err)
		}
		eng2, _ := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "c2"})
		if err := eng2.Recover(ctx); err != nil {
			t.Fatalf("Recover C: %v", err)
		}
		if st := task8NodeStates(t, store, run.ID); st["appr"].State != NodeWaiting {
			t.Fatalf("case C appr = %q, want waiting", st["appr"].State)
		}
		if got, _ := store.GetRun(ctx, run.ID); got.State != RunWaitingForApproval {
			t.Fatalf("case C run = %q, want waiting_for_approval", got.State)
		}
	}
}

// Approval decisions must come from the approval record: a mapping on
// an approval node (which could inject decision=approved as a literal)
// is rejected at validation time, and an empty mapping does not
// survive JSON round-trips, so unmapped approval nodes start from an
// empty input instead of the predecessor merge.
func TestTask8ApprovalMappingRejected(t *testing.T) {
	s := newTask8Setup(t, newTask8FakeBackend())
	def := task8Def("appr-map",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("appr", "nexaroute.control.approval", nil, map[string]any{"decision": "approved"}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "appr", "")})
	if err := s.service.SaveDraft(context.Background(), def); err == nil {
		t.Fatalf("approval node with an input mapping was accepted (decision injection)")
	}
}
