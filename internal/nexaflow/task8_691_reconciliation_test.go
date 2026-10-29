package nexaflow

// Reconciliation tests for PR #691 against the canonical Task 8 line
// (PR #693, now in main). #691's executor.go and its parallel
// runtime/store lineage were retired, not ported; these tests pin
// only what was ported onto the canonical interfaces, plus the
// hardening the reconciliation itself added:
//
//   - durable per-definition run-limit admission at the store
//     boundary (from #691 prepareRun's active-run count, made
//     atomic per store instead of count-then-create);
//   - idempotency: same key + different input is a conflict, and a
//     concurrent duplicate create resolves to the original run
//     instead of a spurious conflict;
//   - engine-level empty-approver rejection (separation of duties
//     previously applied only to non-empty approvers);
//   - stale-lease recovery: live leases are never broken, expired
//     undispatched attempts re-queue, expired dispatched effectful
//     attempts park as ambiguous and are never re-executed;
//   - approval resume and cancel across a file-store restart with a
//     different worker id (approval waits carry no lease, so the
//     worker id must not gate them);
//   - concurrent TryClaim: exactly one winner per (run, node).

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

func reconLimitDef(id string, limit int) Definition {
	def := task8Def(id,
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8TransformLeaf("done", "done"),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "done", "")})
	def.Settings.MaxConcurrentRuns = limit
	return def
}

func TestReconciliationRunLimitAdmission(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	pub := s.publish(t, reconLimitDef("limit-wf", 1))

	run1, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "limit-1")
	if err != nil {
		t.Fatalf("StartRun #1: %v", err)
	}
	if _, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "limit-2"); !errors.Is(err, ErrRunLimit) {
		t.Fatalf("second admission error = %v, want ErrRunLimit", err)
	}
	// A terminal run frees its slot.
	if _, err := s.service.CancelRun(ctx, run1.ID, task8Requester); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if _, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "limit-3"); err != nil {
		t.Fatalf("StartRun after cancel: %v", err)
	}

	// Zero cap means no admission cap (pre-existing behaviour).
	pub0 := s.publish(t, reconLimitDef("limit-free", 0))
	for i := 0; i < 3; i++ {
		if _, err := s.service.StartRun(ctx, pub0, task8Requester, map[string]any{"payload": map[string]any{}}, fmt.Sprintf("free-%d", i)); err != nil {
			t.Fatalf("uncapped StartRun %d: %v", i, err)
		}
	}
}

func TestReconciliationRunLimitConcurrent(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	pub := s.publish(t, reconLimitDef("limit-race", 3))
	var wg sync.WaitGroup
	ok := make(chan struct{}, 24)
	limited := make(chan struct{}, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, fmt.Sprintf("race-%d", i))
			if err == nil {
				ok <- struct{}{}
			} else if errors.Is(err, ErrRunLimit) {
				limited <- struct{}{}
			} else {
				t.Errorf("StartRun %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(ok)
	close(limited)
	if n := len(ok); n != 3 {
		t.Fatalf("concurrent admissions = %d, want exactly 3", n)
	}
	if n := len(limited); n != 21 {
		t.Fatalf("concurrent rejections = %d, want 21", n)
	}
}

func TestReconciliationParkedRunHoldsSlot(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	pub := s.publish(t, reconLimitDef("limit-park", 1))
	run, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "park-1")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := s.store.UpdateRunState(ctx, run.ID, RunCreated, RunRunning, "", ""); err != nil {
		t.Fatalf("to running: %v", err)
	}
	if _, err := s.store.UpdateRunState(ctx, run.ID, RunRunning, RunNeedsManualAction, "ambiguous_recovery", "parked"); err != nil {
		t.Fatalf("to parked: %v", err)
	}
	if _, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "park-2"); !errors.Is(err, ErrRunLimit) {
		t.Fatalf("admission past parked run error = %v, want ErrRunLimit", err)
	}
	if _, err := s.engine.ForceFailRun(ctx, run.ID, "operator_force_failed", "cleared"); err != nil {
		t.Fatalf("ForceFailRun: %v", err)
	}
	if _, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "park-3"); err != nil {
		t.Fatalf("StartRun after force-fail: %v", err)
	}
}

func TestReconciliationIdempotencyInputConflict(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	pub := s.publish(t, reconLimitDef("idem-wf", 0))
	input := map[string]any{"payload": map[string]any{}}
	first, err := s.service.StartRun(ctx, pub, task8Requester, input, "idem-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	replay, err := s.service.StartRun(ctx, pub, task8Requester, input, "idem-key")
	if err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay run = %q, want %q", replay.ID, first.ID)
	}
	// Same key, different input: a conflict, never a silent replay.
	other := map[string]any{"payload": map[string]any{"different": true}}
	if _, err := s.service.StartRun(ctx, pub, task8Requester, other, "idem-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different-input replay error = %v, want ErrIdempotencyConflict", err)
	}
	// Numeric spelling must not false-conflict: 1 and 1.0 are the
	// same JSON value.
	n1, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"n": 1}, "idem-num")
	if err != nil {
		t.Fatalf("numeric StartRun: %v", err)
	}
	n2, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"n": float64(1)}, "idem-num")
	if err != nil || n2.ID != n1.ID {
		t.Fatalf("numeric replay: run %q err %v, want %q", n2.ID, err, n1.ID)
	}
}

func TestReconciliationIdempotencyConcurrentDuplicate(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	pub := s.publish(t, reconLimitDef("idem-race", 0))
	input := map[string]any{"payload": map[string]any{}}
	ids := make(chan string, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := s.service.StartRun(ctx, pub, task8Requester, input, "idem-race-key")
			if err != nil {
				t.Errorf("concurrent duplicate StartRun: %v", err)
				return
			}
			ids <- run.ID
		}()
	}
	wg.Wait()
	close(ids)
	var want string
	for id := range ids {
		if want == "" {
			want = id
		} else if id != want {
			t.Fatalf("concurrent duplicate produced run %q, want single run %q", id, want)
		}
	}
	if want == "" {
		t.Fatal("no run created")
	}
}

func TestReconciliationEmptyApproverDenied(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	def := task8Def("appr-empty",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("appr", "nexaroute.control.approval", nil, nil),
			task8TransformLeaf("done", "done"),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "appr", ""), task8Edge("e2", "appr", "done", "")})
	pub := s.publish(t, def)
	run, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "appr-empty-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := s.service.ExecuteRun(ctx, run.ID); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	// Engine boundary: an empty approver must not decide, even
	// though the old check only applied when approver != "".
	if _, err := s.engine.DecideApproval(ctx, run.ID, "appr", "", "approved"); err == nil {
		t.Fatal("empty-approver decision accepted")
	}
	// The failed attempt recorded nothing: a real approver still works.
	if _, err := s.service.ApproveRun(ctx, run.ID, "appr", task8Approver, "approved"); err != nil {
		t.Fatalf("ApproveRun after empty attempt: %v", err)
	}
	// Service boundary: an unauthenticated identity cannot approve.
	if _, err := s.service.ApproveRun(ctx, run.ID, "appr", authz.Identity{}, "approved"); err == nil {
		t.Fatal("unauthenticated approval accepted")
	}
}

func TestReconciliationRecoverStaleLeases(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	pub := s.publish(t, reconLimitDef("lease-wf", 0))

	// Live lease: a different worker's unexpired claim is untouched,
	// even though that worker may already be dead in reality.
	live, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "lease-live")
	if err != nil {
		t.Fatalf("StartRun live: %v", err)
	}
	task8CraftNode(t, s.store, live.ID, "done", NodeClaimed, false, time.Now().Add(time.Hour), "dead-worker")
	if err := s.engine.Recover(ctx); err != nil {
		t.Fatalf("Recover live: %v", err)
	}
	states := task8NodeStates(t, s.store, live.ID)
	if states["done"].State != NodeClaimed || states["done"].LeaseOwner != "dead-worker" {
		t.Fatalf("live lease touched: state %q owner %q", states["done"].State, states["done"].LeaseOwner)
	}

	// Expired, never dispatched: re-queued, not parked.
	undisp, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "lease-undisp")
	if err != nil {
		t.Fatalf("StartRun undispatched: %v", err)
	}
	task8CraftNode(t, s.store, undisp.ID, "done", NodeClaimed, false, time.Now().Add(-time.Hour), "dead-worker")
	if err := s.engine.Recover(ctx); err != nil {
		t.Fatalf("Recover undispatched: %v", err)
	}
	states = task8NodeStates(t, s.store, undisp.ID)
	if states["done"].State != NodePending {
		t.Fatalf("expired undispatched state = %q, want pending", states["done"].State)
	}

	// Expired, dispatched, effectful: parked ambiguous, never
	// re-executed (scenario 9's node choice: mcp.call is effectful).
	mcpDef := task8Def("lease-mcp",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("call", "nexaroute.mcp.call", map[string]any{"allowlist": []any{"allowed-tool"}}, map[string]any{"server_id": "srv-1", "tool": "allowed-tool", "arguments": map[string]any{}}),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "call", "")})
	pubMCP := s.publish(t, mcpDef)
	amb, err := s.service.StartRun(ctx, pubMCP, task8Requester, map[string]any{"payload": map[string]any{}}, "lease-amb")
	if err != nil {
		t.Fatalf("StartRun ambiguous: %v", err)
	}
	task8CraftNode(t, s.store, amb.ID, "call", NodeRunning, true, time.Now().Add(-time.Hour), "dead-worker")
	if err := s.engine.Recover(ctx); err != nil {
		t.Fatalf("Recover ambiguous: %v", err)
	}
	states = task8NodeStates(t, s.store, amb.ID)
	if states["call"].State != NodeNeedsManualAction || !states["call"].Ambiguous {
		t.Fatalf("ambiguous node = %q ambiguous=%v, want needs_manual_action/true", states["call"].State, states["call"].Ambiguous)
	}
	if _, err := s.engine.Execute(ctx, amb.ID); !errors.Is(err, ErrRunParked) {
		t.Fatalf("Execute parked error = %v, want ErrRunParked", err)
	}
	if be.mcpCount() != 0 {
		t.Fatalf("mcp calls after park = %d, want 0", be.mcpCount())
	}
}

func TestReconciliationConcurrentClaims(t *testing.T) {
	be := newTask8FakeBackend()
	s := newTask8Setup(t, be)
	ctx := context.Background()
	pub := s.publish(t, reconLimitDef("claim-wf", 0))
	run, err := s.service.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "claim-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	won := make(chan bool, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := s.store.TryClaim(ctx, run.ID, "done", fmt.Sprintf("worker-%d", i), time.Hour, time.Now())
			if err != nil {
				t.Errorf("TryClaim %d: %v", i, err)
				return
			}
			won <- ok
		}(i)
	}
	wg.Wait()
	close(won)
	winners := 0
	for ok := range won {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent claims winners = %d, want exactly 1", winners)
	}
}

func TestReconciliationApprovalResumeNewWorkerAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	store, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	reg := DefaultRegistry()
	be := newTask8FakeBackend()
	engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: be, WorkerID: "w-old"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	def := task8Def("appr-restart",
		[]DefinitionNode{
			task8Node("start", "nexaroute.trigger.manual", nil, nil),
			task8Node("appr", "nexaroute.control.approval", nil, nil),
			task8TransformLeaf("done", "done"),
		},
		[]DefinitionEdge{task8Edge("e1", "start", "appr", ""), task8Edge("e2", "appr", "done", "")})
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	pub, err := svc.Publish(ctx, def.ID, task8Owner)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	run, err := svc.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "appr-restart-key")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if run, err = svc.ExecuteRun(ctx, run.ID); err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	if run.State != RunWaitingForApproval {
		t.Fatalf("pre-restart state = %q, want waiting_for_approval", run.State)
	}
	// Restart with a DIFFERENT worker id. The waiting attempt still
	// carries the old worker's live lease, so the new worker must
	// NOT steal it: the first resume stalls (run stays running,
	// node stays pending, nothing is corrupted). Once the lease
	// is stale (clock advanced past it here, so the test never
	// sleeps for the real TTL), the same new worker resumes and
	// the run completes. This is the canonical lease rule, not a
	// gap: Recover never breaks a live foreign lease.
	if err := store.Close(); err != nil {
		t.Fatalf("store Close: %v", err)
	}
	store2, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	engine2, err := NewEngine(EngineOptions{Store: store2, Registry: reg, Backend: be, WorkerID: "w-new"})
	if err != nil {
		t.Fatalf("NewEngine #2: %v", err)
	}
	svc2, err := NewService(store2, engine2, reg, nil)
	if err != nil {
		t.Fatalf("NewService #2: %v", err)
	}
	if err := engine2.Recover(ctx); err != nil {
		t.Fatalf("Recover after restart: %v", err)
	}
	if _, err := svc2.ApproveRun(ctx, run.ID, "appr", task8Approver, "approved"); err != nil {
		t.Fatalf("ApproveRun after restart: %v", err)
	}
	run, err = svc2.ResumeRun(ctx, run.ID, task8Requester)
	if err != nil {
		t.Fatalf("ResumeRun stalled: %v", err)
	}
	if run.State != RunRunning {
		t.Fatalf("stalled resume state = %q, want running (live lease must block, not fail)", run.State)
	}
	stalled := task8NodeStates(t, store2, run.ID)
	if stalled["appr"].State != NodePending {
		t.Fatalf("stalled approval node = %q, want pending", stalled["appr"].State)
	}
	// Lease now stale: the new worker resumes to completion.
	engine3, err := NewEngine(EngineOptions{Store: store2, Registry: reg, Backend: be, WorkerID: "w-new", Now: func() time.Time { return time.Now().Add(2 * time.Hour) }})
	if err != nil {
		t.Fatalf("NewEngine #3: %v", err)
	}
	svc3, err := NewService(store2, engine3, reg, nil)
	if err != nil {
		t.Fatalf("NewService #3: %v", err)
	}
	run, err = svc3.ResumeRun(ctx, run.ID, task8Requester)
	if err != nil {
		t.Fatalf("ResumeRun after lease expiry: %v", err)
	}
	if run.State != RunSucceeded {
		t.Fatalf("resumed run state = %q, want succeeded", run.State)
	}
	svc2 = svc3
	// Cancel after restart: a second waiting run is cancellable by
	// its owner on the restarted engine.
	run2, err := svc2.StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, "appr-restart-key-2")
	if err != nil {
		t.Fatalf("StartRun #2: %v", err)
	}
	if _, err := svc2.ExecuteRun(ctx, run2.ID); err != nil {
		t.Fatalf("ExecuteRun #2: %v", err)
	}
	run2, err = svc2.CancelRun(ctx, run2.ID, task8Requester)
	if err != nil {
		t.Fatalf("CancelRun after restart: %v", err)
	}
	if run2.State != RunCancelled {
		t.Fatalf("cancelled run state = %q, want cancelled", run2.State)
	}
	// A non-owner cannot resume another principal's run. (The
	// test approver identity carries the owner role, so the
	// viewer identity is the non-owner probe here.)
	if _, err := svc2.ResumeRun(ctx, run2.ID, task8Viewer); err == nil {
		t.Fatal("non-owner resume accepted")
	}
}

// ---- PostgreSQL proofs (skip without NEXAROUTE_TEST_PG_DSN) ----

func TestReconciliationPGRunLimitTwoHandles(t *testing.T) {
	db := task8PGDB(t)
	ctx := context.Background()
	defID := fmt.Sprintf("recon-pg-limit-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_runs WHERE definition_id = $1`, defID)
	})
	newSvc := func() *Service {
		store, err := NewPostgresStore(ctx, db)
		if err != nil {
			t.Fatalf("NewPostgresStore: %v", err)
		}
		reg := DefaultRegistry()
		engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: newTask8FakeBackend(), WorkerID: "pg-recon"})
		if err != nil {
			t.Fatalf("NewEngine: %v", err)
		}
		svc, err := NewService(store, engine, reg, nil)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		return svc
	}
	svc1 := newSvc()
	def := reconLimitDef(defID, 2)
	if err := svc1.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	pub, err := svc1.Publish(ctx, def.ID, task8Owner)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Twelve concurrent admissions across two handles sharing one
	// database: the advisory-locked count + insert admits exactly 2.
	svcs := []*Service{svc1, newSvc()}
	admitted := make(chan struct{}, 12)
	limited := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svcs[i%2].StartRun(ctx, pub, task8Requester, map[string]any{"payload": map[string]any{}}, fmt.Sprintf("pg-limit-%d", i))
			if err == nil {
				admitted <- struct{}{}
			} else if errors.Is(err, ErrRunLimit) {
				limited <- struct{}{}
			} else {
				t.Errorf("PG StartRun %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(admitted)
	close(limited)
	if n := len(admitted); n != 2 {
		t.Fatalf("PG concurrent admissions = %d, want exactly 2", n)
	}
	if n := len(limited); n != 10 {
		t.Fatalf("PG concurrent rejections = %d, want 10", n)
	}
}

func TestReconciliationPGTryClaimTwoHandles(t *testing.T) {
	db := task8PGDB(t)
	ctx := context.Background()
	defID := fmt.Sprintf("recon-pg-claim-%d", time.Now().UnixNano())
	runID := fmt.Sprintf("recon-pg-claim-run-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_nodes WHERE run_id = $1`, runID)
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_runs WHERE run_id = $1`, runID)
	})
	store1, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatalf("store1: %v", err)
	}
	store2, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatalf("store2: %v", err)
	}
	if err := store1.CreateRun(ctx, Run{ID: runID, DefinitionID: defID, Definition: Definition{ID: defID}, State: RunCreated}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := store1.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "done", State: NodePending}); err != nil {
		t.Fatalf("AppendNodeExecution: %v", err)
	}
	stores := []Store{store1, store2}
	won := make(chan bool, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := stores[i%2].TryClaim(ctx, runID, "done", fmt.Sprintf("pg-worker-%d", i), time.Hour, time.Now())
			if err != nil {
				t.Errorf("TryClaim %d: %v", i, err)
				return
			}
			won <- ok
		}(i)
	}
	wg.Wait()
	close(won)
	winners := 0
	for ok := range won {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("PG concurrent claims winners = %d, want exactly 1", winners)
	}
}

func TestReconciliationPGCrossProcessRunLimit(t *testing.T) {
	db := task6PG(t)
	defID := task6Unique("recon-pg-xproc-limit")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_runs WHERE definition_id = $1`, defID)
	})
	dir := t.TempDir()
	const children = 6
	const limit = 2
	envs := make([]map[string]string, children)
	outs := make([]string, children)
	for i := 0; i < children; i++ {
		outs[i] = filepath.Join(dir, fmt.Sprintf("r%d.txt", i))
		envs[i] = map[string]string{
			"TASK6_CHILD_MODE":  "run-limit",
			"TASK6_CHILD_DEF":   defID,
			"TASK6_CHILD_RUN":   fmt.Sprintf("%s-run-%d", defID, i),
			"TASK6_CHILD_LIMIT": fmt.Sprintf("%d", limit),
			"TASK6_CHILD_OUT":   outs[i],
		}
	}
	for _, err := range task6RunChildrenConcurrently(t, envs) {
		if err != nil {
			t.Fatal(err)
		}
	}
	admitted := 0
	for _, out := range outs {
		if task6ReadResult(t, out) == "admitted" {
			admitted++
		}
	}
	if admitted != limit {
		t.Fatalf("cross-process PG admissions = %d, want exactly %d", admitted, limit)
	}
}
