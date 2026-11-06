package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Agent B (100% coverage mission): engine, file store, Task 8
// service/scheduler/webhook/migration and delivery-ledger gaps.
// Tests only; no production code is touched here.

// agentBStore is a Store wrapper that delegates to an underlying
// store except for the injected faults a test configures. A nil
// hook/field means "delegate".
type agentBStore struct {
	Store
	getRunErr               error
	failGetNodes            bool
	failSave                error
	failSaveAfter           int
	saveCalls               int
	failUpdateRunState      error
	createRunHook           func(ctx context.Context, run Run) error
	listRevisionsOverride   []PublishedDefinition
	listRevisionsErr        error
	saveDraftFailAfter      int
	saveDraftCalls          int
	saveDraftErr            error
	publishRevisionOverride uint64
	publishErr              error
	publishIfUnchangedErr   error
	getRunFailAfter         int
	getRunCalls             int
	getDraftOverride        *Definition
}

func (s *agentBStore) GetRun(ctx context.Context, runID string) (Run, error) {
	if s.getRunErr != nil {
		return Run{}, s.getRunErr
	}
	s.getRunCalls++
	if s.getRunFailAfter > 0 && s.getRunCalls == s.getRunFailAfter {
		return Run{}, errors.New("agentb: get run boom")
	}
	return s.Store.GetRun(ctx, runID)
}

func (s *agentBStore) GetDraft(ctx context.Context, definitionID string) (Definition, error) {
	if s.getDraftOverride != nil {
		return *s.getDraftOverride, nil
	}
	return s.Store.GetDraft(ctx, definitionID)
}

func (s *agentBStore) GetNodeExecutions(ctx context.Context, runID string) ([]NodeExecution, error) {
	if s.failGetNodes {
		return nil, errors.New("agentb: injected GetNodeExecutions failure")
	}
	return s.Store.GetNodeExecutions(ctx, runID)
}

func (s *agentBStore) SaveNodeExecution(ctx context.Context, exec NodeExecution, expected NodeState) (NodeExecution, error) {
	s.saveCalls++
	if s.failSaveAfter > 0 && s.saveCalls == s.failSaveAfter {
		if s.failSave != nil {
			return NodeExecution{}, s.failSave
		}
		return NodeExecution{}, errors.New("agentb: injected SaveNodeExecution failure")
	}
	return s.Store.SaveNodeExecution(ctx, exec, expected)
}

func (s *agentBStore) UpdateRunState(ctx context.Context, runID string, expected RunState, state RunState, errorCode, errMsg string) (Run, error) {
	if s.failUpdateRunState != nil {
		return Run{}, s.failUpdateRunState
	}
	return s.Store.UpdateRunState(ctx, runID, expected, state, errorCode, errMsg)
}

func (s *agentBStore) CreateRun(ctx context.Context, run Run) error {
	if s.createRunHook != nil {
		return s.createRunHook(ctx, run)
	}
	return s.Store.CreateRun(ctx, run)
}

func (s *agentBStore) ListRevisions(ctx context.Context, definitionID string) ([]PublishedDefinition, error) {
	if s.listRevisionsErr != nil {
		return nil, s.listRevisionsErr
	}
	if s.listRevisionsOverride != nil {
		return s.listRevisionsOverride, nil
	}
	return s.Store.ListRevisions(ctx, definitionID)
}

func (s *agentBStore) SaveDraft(ctx context.Context, def Definition) error {
	s.saveDraftCalls++
	if s.saveDraftFailAfter > 0 && s.saveDraftCalls == s.saveDraftFailAfter {
		return errors.New("agentb: injected SaveDraft failure")
	}
	if s.saveDraftErr != nil {
		return s.saveDraftErr
	}
	return s.Store.SaveDraft(ctx, def)
}

func (s *agentBStore) Publish(ctx context.Context, definitionID, publishedBy string) (PublishedDefinition, error) {
	if s.publishErr != nil {
		return PublishedDefinition{}, s.publishErr
	}
	pub, err := s.Store.Publish(ctx, definitionID, publishedBy)
	if err == nil && s.publishRevisionOverride > 0 {
		pub.Version.Revision = s.publishRevisionOverride
	}
	return pub, err
}

func (s *agentBStore) PublishIfUnchanged(ctx context.Context, definitionID, publishedBy, expectedHash string) (PublishedDefinition, error) {
	if s.publishIfUnchangedErr != nil {
		return PublishedDefinition{}, s.publishIfUnchangedErr
	}
	pub, ok := s.Store.(ConditionalPublisher)
	if !ok {
		return PublishedDefinition{}, errors.New("agentb: store does not support PublishIfUnchanged")
	}
	return pub.PublishIfUnchanged(ctx, definitionID, publishedBy, expectedHash)
}

// agentBErrLedger is a DeliveryLedger whose Claim always fails.
type agentBErrLedger struct{ err error }

func (l *agentBErrLedger) Claim(_ context.Context, _ string) (bool, error) {
	return false, l.err
}

// agentBSeedRun creates a run directly in the store (bypassing engine
// validation, so definitions with unresolvable mappings or unknown
// nodes can be staged) and appends one attempt per node in states.
func agentBSeedRun(t *testing.T, store Store, def Definition, state RunState, nodes map[string]NodeState) Run {
	t.Helper()
	ctx := context.Background()
	run := Run{
		ID: agentBRunID(), DefinitionID: def.ID, Definition: def,
		State: state, Principal: "alice", Input: runInput(),
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("seed CreateRun: %v", err)
	}
	for _, nodeID := range TopologicalOrder(def) {
		st, ok := nodes[nodeID]
		if !ok {
			continue
		}
		if _, err := store.AppendNodeExecution(ctx, NodeExecution{
			RunID: run.ID, NodeID: nodeID, State: st, AppendOperationID: "initial",
		}); err != nil {
			t.Fatalf("seed AppendNodeExecution %s: %v", nodeID, err)
		}
	}
	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("seed GetRun: %v", err)
	}
	return got
}

var agentBRunSeq int

func agentBRunID() string {
	agentBRunSeq++
	return fmt.Sprintf("run-agentb-%d", agentBRunSeq)
}

// ---- engine.go ----

func TestAgentBEngineSemaphoreDefaultsToOne(t *testing.T) {
	eng := mustEngine(t, NewMemoryStore(), testRegistry(t))
	if got := cap(eng.semaphore("def-zero", 0)); got != 1 {
		t.Fatalf("semaphore(0) capacity = %d, want 1", got)
	}
	if got := cap(eng.semaphore("def-neg", -5)); got != 1 {
		t.Fatalf("semaphore(-5) capacity = %d, want 1", got)
	}
	if got := cap(eng.semaphore("def-three", 3)); got != 3 {
		t.Fatalf("semaphore(3) capacity = %d, want 3", got)
	}
}

func TestAgentBEngineWaitForInitializedRunCancelledContext(t *testing.T) {
	store := NewMemoryStore()
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	run := agentBSeedRun(t, store, def, RunCreated, map[string]NodeState{})
	eng := mustEngine(t, store, testRegistry(t, fixedSpec("test.echo", nil, map[string]any{"v": "x"})))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := eng.waitForInitializedRun(ctx, run)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForInitializedRun on cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestAgentBEngineCreateRunIdempotencyRaceResolves(t *testing.T) {
	base := NewMemoryStore()
	reg := testRegistry(t, fixedSpec("test.echo", nil, map[string]any{"v": "x"}))
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	def.ID = "wf-race"
	pub := publishDef(t, base, def)
	input := runInput()
	wrapped := &agentBStore{Store: base}
	wrapped.createRunHook = func(ctx context.Context, run Run) error {
		seed := Run{
			ID: "run-race-winner", DefinitionID: pub.Version.DefinitionID,
			Revision: pub.Version.Revision, Definition: pub.Definition,
			State: RunCreated, IdempotencyKey: "race-key-1",
			Principal: "alice", Input: input,
		}
		if err := base.CreateRun(ctx, seed); err != nil {
			return err
		}
		for _, nodeID := range TopologicalOrder(pub.Definition) {
			if _, err := base.AppendNodeExecution(ctx, NodeExecution{
				RunID: seed.ID, NodeID: nodeID, State: NodePending, AppendOperationID: "initial",
			}); err != nil {
				return err
			}
		}
		return ErrIdempotencyConflict
	}
	eng := mustEngine(t, wrapped, reg)
	got, err := eng.CreateRun(context.Background(), pub, principal(), input, "race-key-1")
	if err != nil {
		t.Fatalf("CreateRun idempotency race: %v", err)
	}
	if got.ID != "run-race-winner" {
		t.Fatalf("CreateRun returned run %q, want the raced winner run-race-winner", got.ID)
	}
	if got.State != RunCreated {
		t.Fatalf("winner state = %q, want created", got.State)
	}
}

func TestAgentBEngineCreateRunIdempotencyRaceWaitsForInit(t *testing.T) {
	base := NewMemoryStore()
	reg := testRegistry(t, fixedSpec("test.echo", nil, map[string]any{"v": "x"}))
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	def.ID = "wf-race-incomplete"
	pub := publishDef(t, base, def)
	input := runInput()
	wrapped := &agentBStore{Store: base}
	wrapped.createRunHook = func(ctx context.Context, run Run) error {
		seed := Run{
			ID: "run-race-incomplete", DefinitionID: pub.Version.DefinitionID,
			Revision: pub.Version.Revision, Definition: pub.Definition,
			State: RunCreated, IdempotencyKey: "race-key-2",
			Principal: "alice", Input: input,
		}
		if err := base.CreateRun(ctx, seed); err != nil {
			return err
		}
		// Deliberately no initial node attempts: the winner is not
		// initialized yet, so CreateRun must surface that instead
		// of returning a half-created run.
		return ErrIdempotencyConflict
	}
	eng := mustEngine(t, wrapped, reg)
	start := time.Now()
	_, err := eng.CreateRun(context.Background(), pub, principal(), input, "race-key-2")
	if !errors.Is(err, ErrRunInitializationIncomplete) {
		t.Fatalf("CreateRun race with uninitialized winner = %v, want ErrRunInitializationIncomplete", err)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Fatalf("waited only %v for initialization, want the bounded ~2s wait", elapsed)
	}
}

func TestAgentBEngineExpireApprovalsStoreError(t *testing.T) {
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base, failGetNodes: true}
	eng := mustEngine(t, wrapped, testRegistry(t), func(o *EngineOptions) {
		o.ApprovalTimeout = time.Minute
	})
	if err := eng.expireApprovals(context.Background(), Run{ID: "run-x"}); err == nil {
		t.Fatal("expireApprovals with failing GetNodeExecutions returned nil error")
	} else if !strings.Contains(err.Error(), "injected GetNodeExecutions") {
		t.Fatalf("expireApprovals error = %v", err)
	}
}

func TestAgentBEngineRecomputeRunStateStoreErrors(t *testing.T) {
	ctx := context.Background()
	// GetRun fails.
	wrapped := &agentBStore{Store: NewMemoryStore(), getRunErr: errors.New("agentb: no such run")}
	eng := mustEngine(t, wrapped, testRegistry(t))
	if err := eng.recomputeRunState(ctx, "run-missing"); err == nil {
		t.Fatal("recomputeRunState with failing GetRun returned nil error")
	}
	// GetRun succeeds, GetNodeExecutions fails.
	base := NewMemoryStore()
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{"start": NodeSucceeded, "a": NodePending})
	wrapped2 := &agentBStore{Store: base, failGetNodes: true}
	eng2 := mustEngine(t, wrapped2, testRegistry(t))
	if err := eng2.recomputeRunState(ctx, run.ID); err == nil {
		t.Fatal("recomputeRunState with failing GetNodeExecutions returned nil error")
	} else if !strings.Contains(err.Error(), "injected GetNodeExecutions") {
		t.Fatalf("recomputeRunState error = %v", err)
	}
}

func TestAgentBEngineFinishRunRecomputeError(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{"start": NodeSucceeded, "a": NodePending})
	wrapped := &agentBStore{Store: base, failGetNodes: true}
	eng := mustEngine(t, wrapped, testRegistry(t))
	_, err := eng.finishRun(ctx, run)
	if err == nil {
		t.Fatal("finishRun with failing recompute returned nil error")
	} else if !strings.Contains(err.Error(), "injected GetNodeExecutions") {
		t.Fatalf("finishRun error = %v", err)
	}
}

func agentBSweepSetup(t *testing.T) (*MemoryStore, *agentBStore, *Engine, Run, Definition) {
	t.Helper()
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-sweep", Name: "sweep",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "w", NodeID: "test.w", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e1", From: "start", To: "w"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual}},
	}
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{
		"start": NodePending, "w": NodeWaiting,
	})
	if _, err := base.RecordApproval(context.Background(), Approval{
		RunID: run.ID, NodeID: "w", Requester: "alice", Approver: "bob", Decision: "approved",
	}); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	reg := testRegistry(t, fixedSpec("test.w", nil, map[string]any{"ok": true}))
	eng := mustEngine(t, wrapped, reg)
	return base, wrapped, eng, run, def
}

func TestAgentBEngineSweepApprovalConflictContinues(t *testing.T) {
	base, wrapped, eng, run, _ := agentBSweepSetup(t)
	wrapped.failSave = ErrStateConflict
	wrapped.failSaveAfter = 1
	progress, err := eng.sweep(context.Background(), run)
	if err != nil {
		t.Fatalf("sweep with conflicted approval save = %v, want nil (conflict is skipped)", err)
	}
	if !progress {
		t.Fatal("sweep made no progress: the trigger node should still have dispatched")
	}
	states := nodeStates(t, base, run.ID)
	if states["w"].State != NodeWaiting {
		t.Fatalf("waiting node state = %q, want waiting (conflicted save must not move it)", states["w"].State)
	}
}

func TestAgentBEngineSweepApprovalSaveErrorAborts(t *testing.T) {
	_, wrapped, eng, run, _ := agentBSweepSetup(t)
	wrapped.failSave = errors.New("agentb: save boom")
	wrapped.failSaveAfter = 1
	progress, err := eng.sweep(context.Background(), run)
	if err == nil {
		t.Fatal("sweep with failing approval save returned nil error")
	}
	if !strings.Contains(err.Error(), "save boom") {
		t.Fatalf("sweep error = %v, want the injected save failure", err)
	}
	if progress {
		t.Fatal("sweep reported progress after aborting on a save error")
	}
}

func TestAgentBEngineSweepUnknownNodeSaveError(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-ghost", Name: "ghost",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "g", NodeID: "test.ghost", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e1", From: "start", To: "g"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual}},
	}
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{
		"start": NodeSucceeded, "g": NodePending,
	})
	// The start node already succeeded (output recorded so the ghost
	// is ready); the first SaveNodeExecution in this sweep is the
	// fail-the-ghost save after the registry lookup misses.
	if execs, err := base.GetNodeExecutions(ctx, run.ID); err == nil {
		for _, e := range execs {
			if e.NodeID == "start" {
				e.Output = map[string]any{"payload": map[string]any{"x": 1}}
				if _, err := base.SaveNodeExecution(ctx, e, NodeSucceeded); err != nil {
					t.Fatalf("seed start output: %v", err)
				}
			}
		}
	}
	wrapped.failSave = errors.New("agentb: save boom")
	wrapped.failSaveAfter = 1
	eng := mustEngine(t, wrapped, testRegistry(t))
	progress, err := eng.sweep(ctx, run)
	if err == nil {
		t.Fatal("sweep with failing unknown-node save returned nil error")
	}
	if !strings.Contains(err.Error(), "save boom") {
		t.Fatalf("sweep error = %v, want the injected save failure", err)
	}
	if progress {
		t.Fatal("sweep reported progress after aborting on a save error")
	}
}

func TestAgentBEngineSweepInputResolutionSaveError(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base}
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-badinput", Name: "badinput",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "n", NodeID: "test.echo", Version: SupportedVersion,
				Input: map[string]any{"v": map[string]any{"$ref": "missing.path"}}},
		},
		Edges:    []DefinitionEdge{{ID: "e1", From: "start", To: "n"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual}},
	}
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{
		"start": NodeSucceeded, "n": NodePending,
	})
	if execs, err := base.GetNodeExecutions(ctx, run.ID); err == nil {
		for _, e := range execs {
			if e.NodeID == "start" {
				e.Output = map[string]any{"payload": map[string]any{"x": 1}}
				if _, err := base.SaveNodeExecution(ctx, e, NodeSucceeded); err != nil {
					t.Fatalf("seed start output: %v", err)
				}
			}
		}
	}
	wrapped.failSave = errors.New("agentb: save boom")
	wrapped.failSaveAfter = 1
	eng := mustEngine(t, wrapped, testRegistry(t, fixedSpec("test.echo", nil, map[string]any{"v": "x"})))
	progress, err := eng.sweep(ctx, run)
	if err == nil {
		t.Fatal("sweep with failing input-resolution save returned nil error")
	}
	if !strings.Contains(err.Error(), "save boom") {
		t.Fatalf("sweep error = %v, want the injected save failure", err)
	}
	if progress {
		t.Fatal("sweep reported progress after aborting on a save error")
	}
	// The node must still be claimed: the failed save never moved it.
	if states := nodeStates(t, base, run.ID); states["n"].State != NodeClaimed {
		t.Fatalf("node state = %q, want claimed (failed save must not move it)", states["n"].State)
	}
}

func TestAgentBEngineNodeReadinessMissingSource(t *testing.T) {
	eng := mustEngine(t, NewMemoryStore(), testRegistry(t))
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-ready", Name: "ready",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "a", NodeID: "test.echo", Version: SupportedVersion},
			{ID: "b", NodeID: "test.echo", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e1", From: "start", To: "a"},
			{ID: "e2", From: "a", To: "b"},
		},
	}
	current := map[string]NodeExecution{
		"start": {NodeID: "start", State: NodeSucceeded},
		"b":     {NodeID: "b", State: NodePending},
	}
	ready, skip := eng.nodeReadiness(def, "b", current, nil)
	if ready || skip {
		t.Fatalf("nodeReadiness with missing predecessor = ready=%v skip=%v, want false,false", ready, skip)
	}
}

func TestAgentBEngineOverlayResumeInputNilSpec(t *testing.T) {
	dst := map[string]any{"kept": 1}
	overlayResumeInput(dst, nil, map[string]any{"decision": "approved", "approver": "bob"})
	if dst["decision"] != "approved" || dst["approver"] != "bob" || dst["kept"] != 1 {
		t.Fatalf("overlay with nil spec = %v, want all resume keys copied over the original", dst)
	}
}

func TestAgentBEngineNodeInputSkipsUnfiredEdge(t *testing.T) {
	eng := mustEngine(t, NewMemoryStore(), testRegistry(t))
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "wf-merge", Name: "merge",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "br", NodeID: "nexaroute.control.branch", Version: SupportedVersion},
			{ID: "join", NodeID: "test.join", Version: SupportedVersion},
		},
		Edges: []DefinitionEdge{
			{ID: "e1", From: "start", To: "br"},
			{ID: "e2", From: "start", To: "join"},
			{ID: "e3", From: "br", To: "join", Branch: "fast"},
		},
	}
	run := Run{Definition: def, Input: runInput()}
	outputs := map[string]map[string]any{
		"start": {"payload": map[string]any{"x": 1}},
		"br":    {"selected_branch": "slow"},
	}
	spec := testSpec("test.join", nil, func(context.Context, *RunContext) (map[string]any, error) {
		return nil, nil
	})
	merged, err := eng.nodeInput(run, DefinitionNode{ID: "join", NodeID: "test.join"}, spec, outputs, NodeExecution{})
	if err != nil {
		t.Fatalf("nodeInput: %v", err)
	}
	if _, ok := merged["selected_branch"]; ok {
		t.Fatalf("merged input = %v, want no value from the unfired branch edge", merged)
	}
	payload, ok := merged["payload"].(map[string]any)
	if !ok || payload["x"] != 1 {
		t.Fatalf("merged input = %v, want the fired predecessor's payload", merged)
	}
}

func TestAgentBEngineRecordOutcomeBackoffCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base := NewMemoryStore()
	var parked []AuditEvent
	eng := mustEngine(t, base, testRegistry(t), func(o *EngineOptions) {
		o.Backoff = func(int) time.Duration { return time.Second }
		o.Audit = func(ev AuditEvent) { parked = append(parked, ev) }
	})
	spec := testSpec("test.flaky", []Field{{Name: "payload", Type: "object"}},
		func(context.Context, *RunContext) (map[string]any, error) { return nil, nil })
	spec.Retry = RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"backend_unavailable"}}
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"f", "test.flaky", nil})
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{"f": NodeRunning})
	current := nodeStates(t, base, run.ID)
	job := engineJob{
		node: def.Nodes[1], spec: spec, exec: current["f"], input: runInput(),
	}
	eng.recordOutcome(ctx, run, job, nil, NewNodeError("backend_unavailable", true, "transient"), current, map[string]map[string]any{})
	// The cancelled backoff must not have slept: the call returns
	// promptly and the retry attempt is appended.
	execs, err := base.GetNodeExecutions(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	var attempts []NodeExecution
	for _, e := range execs {
		if e.NodeID == "f" {
			attempts = append(attempts, e)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("flaky attempts = %d, want 2 (original + retry)", len(attempts))
	}
	if attempts[1].State != NodePending {
		t.Fatalf("retry attempt state = %q, want pending", attempts[1].State)
	}
	found := false
	for _, ev := range parked {
		if ev.Kind == "parked" && ev.ErrorCode == "result_not_persisted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit events = %+v, want a result_not_persisted parked event (the old attempt cannot be marked failed once the retry row is current)", parked)
	}
}

// ---- file_store.go ----

func TestAgentBFileStoreDocToStateNilAndNulls(t *testing.T) {
	if st := docToState(nil); st == nil {
		t.Fatal("docToState(nil) returned nil state")
	}
	r2 := Run{ID: "r2"}
	n2 := NodeExecution{RunID: "r2", NodeID: "n"}
	a2 := Approval{RunID: "r2", NodeID: "n", Decision: "approved"}
	doc := &fileStateDoc{
		Drafts:    map[string]Definition{},
		Revisions: map[string][]PublishedDefinition{},
		Runs:      map[string]*Run{"r1": nil, "r2": &r2},
		RunByIdem: map[string]string{},
		Nodes:     map[string][]*NodeExecution{"r2": {nil, &n2}},
		Approvals: map[string]*Approval{"k1": nil, "k2": &a2},
	}
	st := docToState(doc)
	if _, ok := st.runs["r1"]; ok {
		t.Fatal("docToState kept a nil run")
	}
	if _, ok := st.runs["r2"]; !ok {
		t.Fatal("docToState dropped a valid run")
	}
	if len(st.nodes["r2"]) != 1 || st.nodes["r2"][0].NodeID != "n" {
		t.Fatalf("docToState nodes = %+v, want only the non-nil node (nil entries compacted out)", st.nodes["r2"])
	}
	if _, ok := st.approvals["k1"]; ok {
		t.Fatal("docToState kept a nil approval")
	}
	if _, ok := st.approvals["k2"]; !ok {
		t.Fatal("docToState dropped a valid approval")
	}
}

func TestAgentBFileStoreOpenErrors(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	// MkdirAll fails: the parent path is a regular file.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileStore(filepath.Join(blocker, "store.json")); err == nil {
		t.Fatal("OpenFileStore under a regular file succeeded")
	} else if !strings.Contains(err.Error(), "create file store dir") {
		t.Fatalf("OpenFileStore mkdir error = %v", err)
	}
	// ReadFile fails with a non-NotExist error: the path is a directory.
	dir := t.TempDir()
	if _, err := OpenFileStore(dir); err == nil {
		t.Fatal("OpenFileStore on a directory succeeded")
	} else if !strings.Contains(err.Error(), "read file store") {
		t.Fatalf("OpenFileStore read error = %v", err)
	}
}

func TestAgentBFileStoreNowUsesClock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s.Clock = func() time.Time { return fixed }
	if got := s.now(); !got.Equal(fixed) {
		t.Fatalf("FileStore.now() = %v, want the injected clock %v", got, fixed)
	}
}

func TestAgentBFileStorePersistMarshalError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "bad-marshal",
		Nodes: []DefinitionNode{{ID: "n", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion,
			Config: map[string]any{"fn": func() {}}}},
	}
	if err := s.SaveDraft(ctx, def); err == nil {
		t.Fatal("SaveDraft with an unmarshallable config succeeded")
	} else if !strings.Contains(err.Error(), "marshal file store") {
		t.Fatalf("SaveDraft marshal error = %v", err)
	}
}

func TestAgentBFileStorePersistCreateTempError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ops := defaultFileStoreFS()
	ops.createTemp = func(string, string) (fileStoreTemp, error) {
		return nil, errors.New("agentb: no temp file")
	}
	s.fs = ops
	if err := s.SaveDraft(ctx, Definition{ID: "x"}); err == nil {
		t.Fatal("SaveDraft with failing temp creation succeeded")
	} else if !strings.Contains(err.Error(), "create file store temp") {
		t.Fatalf("SaveDraft temp error = %v", err)
	}
}

// agentBBadNameTemp is a temp file whose Name lies in a missing
// directory, so the persist chmod step fails deterministically.
type agentBBadNameTemp struct {
	fileStoreTemp
	badName string
}

func (t agentBBadNameTemp) Name() string { return t.badName }

func TestAgentBFileStorePersistChmodError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ops := defaultFileStoreFS()
	create := ops.createTemp
	ops.createTemp = func(d, pattern string) (fileStoreTemp, error) {
		f, err := create(d, pattern)
		if err != nil {
			return nil, err
		}
		return agentBBadNameTemp{fileStoreTemp: f, badName: filepath.Join(d, "agentb-missing-dir", "x")}, nil
	}
	s.fs = ops
	if err := s.SaveDraft(ctx, Definition{ID: "x"}); err == nil {
		t.Fatal("SaveDraft with failing temp chmod succeeded")
	} else if !strings.Contains(err.Error(), "chmod file store temp") {
		t.Fatalf("SaveDraft chmod error = %v", err)
	}
}

func TestAgentBFileStoreAppendAndClaimNotFound(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AppendNodeExecution(ctx, NodeExecution{RunID: "nope", NodeID: "n"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AppendNodeExecution on missing run = %v, want ErrNotFound", err)
	}
	if _, _, err := s.TryClaim(ctx, "nope", "n", "w", time.Second, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TryClaim on missing run = %v, want ErrNotFound", err)
	}
}

// ---- task8_service.go ----

func agentBService(t *testing.T, wrapped *agentBStore) *Service {
	t.Helper()
	reg := DefaultRegistry()
	eng, err := NewEngine(EngineOptions{Store: wrapped, Registry: reg, WorkerID: "agentb"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(wrapped, eng, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func agentBSingleTriggerDef(id string) Definition {
	return Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: id, Name: id,
		Nodes:    []DefinitionNode{{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual}},
	}
}

func TestAgentBServiceSaveDraftStoreError(t *testing.T) {
	wrapped := &agentBStore{Store: NewMemoryStore(), saveDraftErr: errors.New("agentb: save boom")}
	svc := agentBService(t, wrapped)
	if err := svc.SaveDraft(context.Background(), agentBSingleTriggerDef("svc-draft")); err == nil {
		t.Fatal("SaveDraft with failing store returned nil error")
	} else if !strings.Contains(err.Error(), "save boom") {
		t.Fatalf("SaveDraft error = %v", err)
	}
}

func TestAgentBServicePublishIfUnchangedError(t *testing.T) {
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base}
	svc := agentBService(t, wrapped)
	ctx := context.Background()
	def := agentBSingleTriggerDef("svc-pub")
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	wrapped.publishIfUnchangedErr = errors.New("agentb: publish boom")
	if _, err := svc.Publish(ctx, def.ID, task8Owner); err == nil {
		t.Fatal("Publish with failing PublishIfUnchanged returned nil error")
	} else if !strings.Contains(err.Error(), "publish boom") {
		t.Fatalf("Publish error = %v", err)
	}
}

func TestAgentBServiceResumeRunEngineError(t *testing.T) {
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base}
	svc := agentBService(t, wrapped)
	ctx := context.Background()
	def := agentBSingleTriggerDef("svc-resume")
	run := Run{ID: "run-parked", DefinitionID: def.ID, Definition: def, State: RunNeedsManualAction, Principal: "alice"}
	if err := base.CreateRun(ctx, run); err != nil {
		t.Fatalf("seed CreateRun: %v", err)
	}
	_, err := svc.ResumeRun(ctx, "run-parked", authz.Identity{Subject: "alice"})
	if !errors.Is(err, ErrRunParked) {
		t.Fatalf("ResumeRun on parked run = %v, want ErrRunParked", err)
	}
}

func TestAgentBServiceCancelRunEngineError(t *testing.T) {
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base}
	svc := agentBService(t, wrapped)
	ctx := context.Background()
	def := agentBSingleTriggerDef("svc-cancel")
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{"start": NodePending})
	wrapped.failUpdateRunState = errors.New("agentb: update boom")
	_, err := svc.CancelRun(ctx, run.ID, authz.Identity{Subject: "alice"})
	if err == nil {
		t.Fatal("CancelRun with failing UpdateRunState returned nil error")
	} else if !strings.Contains(err.Error(), "update boom") {
		t.Fatalf("CancelRun error = %v", err)
	}
}

// ---- task8_scheduler.go ----

func TestAgentBNewSchedulerRequiresStart(t *testing.T) {
	if _, err := NewScheduler(NewMemoryDeliveryLedger(), nil); err == nil {
		t.Fatal("NewScheduler with nil start succeeded")
	} else if !strings.Contains(err.Error(), "start function") {
		t.Fatalf("NewScheduler error = %v", err)
	}
}

func TestAgentBSchedulerTickClaimError(t *testing.T) {
	sch, err := NewScheduler(&agentBErrLedger{err: errors.New("agentb: claim boom")}, func(context.Context, string, string) (string, error) {
		return "run-1", nil
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	res := sch.Tick(context.Background(), []Schedule{{DefinitionID: "wf-s", IntervalSeconds: 3600, Enabled: true}}, now)
	if len(res) != 1 {
		t.Fatalf("Tick results = %d, want 1", len(res))
	}
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "schedule claim") {
		t.Fatalf("Tick result err = %v, want a wrapped claim failure", res[0].Err)
	}
	if res[0].Started {
		t.Fatal("Tick with a failed claim reported Started")
	}
}

func TestAgentBSchedulerTickStartError(t *testing.T) {
	sch, err := NewScheduler(NewMemoryDeliveryLedger(), func(context.Context, string, string) (string, error) {
		return "", errors.New("agentb: start boom")
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	res := sch.Tick(context.Background(), []Schedule{{DefinitionID: "wf-s", IntervalSeconds: 3600, Enabled: true}}, now)
	if len(res) != 1 {
		t.Fatalf("Tick results = %d, want 1", len(res))
	}
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "start boom") {
		t.Fatalf("Tick result err = %v, want the start failure", res[0].Err)
	}
	if res[0].Started || res[0].RunID != "" {
		t.Fatalf("Tick with a failed start = %+v, want not started", res[0])
	}
}

// ---- task8_webhook.go ----

func agentBWebhookService(t *testing.T, ledger DeliveryLedger) *WebhookService {
	t.Helper()
	svc, err := NewWebhookService(NewWebhookVerifier(), ledger, func(context.Context, string, string, []byte) (string, error) {
		return "run-1", nil
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	return svc
}

func TestAgentBNewWebhookServiceRequiresStart(t *testing.T) {
	if _, err := NewWebhookService(NewWebhookVerifier(), NewMemoryDeliveryLedger(), nil); err == nil {
		t.Fatal("NewWebhookService with nil start succeeded")
	} else if !strings.Contains(err.Error(), "start function") {
		t.Fatalf("NewWebhookService error = %v", err)
	}
}

func TestAgentBWebhookHandleMissingIDs(t *testing.T) {
	svc := agentBWebhookService(t, NewMemoryDeliveryLedger())
	if _, err := svc.Handle(context.Background(), "", "hook", "0", "00", "{}", "secret"); err == nil {
		t.Fatal("Handle with empty definition id succeeded")
	} else if !strings.Contains(err.Error(), "without definition or trigger") {
		t.Fatalf("Handle error = %v", err)
	}
	if _, err := svc.Handle(context.Background(), "wf-1", "", "0", "00", "{}", "secret"); err == nil {
		t.Fatal("Handle with empty trigger id succeeded")
	}
}

func TestAgentBWebhookHandleClaimError(t *testing.T) {
	svc := agentBWebhookService(t, &agentBErrLedger{err: errors.New("agentb: claim boom")})
	ctx := context.Background()
	secret := "test-webhook-secret"
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := `{"event":"ping"}`
	sig := task8SignWebhook(secret, ts, body)
	if _, err := svc.Handle(ctx, "wf-1", "hook", ts, sig, body, secret); err == nil {
		t.Fatal("Handle with failing ledger claim succeeded")
	} else if !strings.Contains(err.Error(), "delivery claim") {
		t.Fatalf("Handle error = %v, want a wrapped claim failure", err)
	}
}

// ---- task8_delivery.go (file/memory Claim) ----

func TestAgentBFileDeliveryClaimOpenError(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	ledger, err := NewFileDeliveryLedger(ledgerDir)
	if err != nil {
		t.Fatalf("NewFileDeliveryLedger: %v", err)
	}
	if err := os.RemoveAll(ledgerDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Claim(context.Background(), "key-1"); err == nil {
		t.Fatal("Claim with an unopenable claim file succeeded")
	} else if !strings.Contains(err.Error(), "claim delivery") {
		t.Fatalf("Claim error = %v", err)
	}
}

func TestAgentBFileDeliveryClaimWriteError(t *testing.T) {
	// A zero RLIMIT_FSIZE makes the claim file's WriteString fail
	// with EFBIG: the only deterministic local trigger for the
	// write-error branch (disk-full cannot be arranged locally).
	// SIGXFSZ must be ignored or the process would die on the write.
	var oldLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &oldLimit); err != nil {
		t.Skipf("cannot read RLIMIT_FSIZE: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	ledger, err := NewFileDeliveryLedger(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileDeliveryLedger: %v", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: oldLimit.Max}); err != nil {
		t.Skipf("cannot set RLIMIT_FSIZE: %v", err)
	}
	_, claimErr := ledger.Claim(context.Background(), "key-write-fail")
	restoreErr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &oldLimit)
	if restoreErr != nil {
		t.Fatalf("cannot restore RLIMIT_FSIZE: %v", restoreErr)
	}
	if claimErr == nil {
		t.Fatal("Claim with a zero file-size limit succeeded")
	}
	if !strings.Contains(claimErr.Error(), "claim delivery") {
		t.Fatalf("Claim error = %v, want a wrapped write failure", claimErr)
	}
}

// ---- task8_migrate.go ----

func agentBMigrateSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	task8WriteLegacyStore(t, src, false, false)
	return src
}

func agentBReadJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func agentBWriteJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestAgentBMigrateDestinationHoldsRun(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	dst := NewMemoryStore()
	if err := dst.CreateRun(ctx, Run{ID: "run-mig-1", DefinitionID: "mig-wf"}); err != nil {
		t.Fatalf("seed destination run: %v", err)
	}
	if _, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry()); err == nil {
		t.Fatal("migration into a destination holding the run succeeded")
	} else if !strings.Contains(err.Error(), "already holds run") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestAgentBMigrateDestinationGetRunError(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	wrapped := &agentBStore{Store: NewMemoryStore(), getRunErr: errors.New("agentb: get boom")}
	if _, err := MigratePhase8Dir(ctx, src, wrapped, DefaultRegistry()); err == nil {
		t.Fatal("migration with failing destination GetRun succeeded")
	} else if !strings.Contains(err.Error(), "get boom") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestAgentBMigrateDestinationHoldsRevisions(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	wrapped := &agentBStore{
		Store:                 NewMemoryStore(),
		listRevisionsOverride: []PublishedDefinition{{Version: DefinitionVersion{DefinitionID: "mig-wf", Revision: 1}}},
	}
	if _, err := MigratePhase8Dir(ctx, src, wrapped, DefaultRegistry()); err == nil {
		t.Fatal("migration into a destination holding revisions succeeded")
	} else if !strings.Contains(err.Error(), "already holds revisions") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestAgentBMigrateSaveVersionDraftError(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	wrapped := &agentBStore{Store: NewMemoryStore(), saveDraftFailAfter: 2}
	rep, err := MigratePhase8Dir(ctx, src, wrapped, DefaultRegistry())
	if err == nil {
		t.Fatal("migration with failing version-draft save succeeded")
	}
	if !strings.Contains(err.Error(), "save version draft") {
		t.Fatalf("migration error = %v", err)
	}
	if rep.Drafts != 1 {
		t.Fatalf("report drafts = %d, want 1 (the plain draft saved before the version draft failed)", rep.Drafts)
	}
}

func TestAgentBMigratePublishRevisionMismatch(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	wrapped := &agentBStore{Store: NewMemoryStore(), publishRevisionOverride: 99}
	if _, err := MigratePhase8Dir(ctx, src, wrapped, DefaultRegistry()); err == nil {
		t.Fatal("migration with a mismatched published revision succeeded")
	} else if !strings.Contains(err.Error(), "produced revision 99, want 1") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestAgentBMigrateSortsMultipleDrafts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d1 := task8BranchDef()
	d1.ID = "mig-z"
	d2 := task8BranchDef()
	d2.ID = "mig-a"
	// Write in reverse id order so the sort comparator must run.
	for _, def := range []Definition{d1, d2} {
		path := filepath.Join(root, "drafts", def.ID+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(def)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := MigratePhase8Dir(ctx, root, NewMemoryStore(), DefaultRegistry())
	if err != nil {
		t.Fatalf("MigratePhase8Dir: %v", err)
	}
	if rep.Drafts != 2 {
		t.Fatalf("report drafts = %d, want 2", rep.Drafts)
	}
}

func TestAgentBMigrateVersionReadDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("readdir permission denial is ineffective as root; exercised in CI as a non-root user")
	}
	ctx := context.Background()
	src := agentBMigrateSource(t)
	verDir := filepath.Join(src, "versions", "mig-wf")
	if err := os.Chmod(verDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(verDir, 0o750) })
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil {
		t.Fatal("migration with an unreadable version directory succeeded")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("migration error = %v, want a readdir permission failure", err)
	}
}

func TestAgentBMigrateVersionCorruptJSON(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	if err := os.WriteFile(filepath.Join(src, "versions", "mig-wf", "1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil {
		t.Fatal("migration with a corrupt version file succeeded")
	} else if !strings.Contains(err.Error(), "corrupt JSON") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestAgentBMigrateVersionLegacyExtension(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	path := filepath.Join(src, "versions", "mig-wf", "1.json")
	m := agentBReadJSONFile(t, path)
	defRaw := m["definition"].(map[string]any)
	defRaw["on_error"] = map[string]any{"node": "eh"}
	agentBWriteJSONFile(t, path, m)
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil {
		t.Fatal("migration with an on_error version definition succeeded")
	} else if !strings.Contains(err.Error(), "on_error") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestAgentBMigrateVersionInvalidDefinition(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	path := filepath.Join(src, "versions", "mig-wf", "1.json")
	m := agentBReadJSONFile(t, path)
	nodes := m["definition"].(map[string]any)["nodes"].([]any)
	nodes[1].(map[string]any)["node_id"] = "test.missing"
	agentBWriteJSONFile(t, path, m)
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil {
		t.Fatal("migration with an invalid version definition succeeded")
	} else if !strings.Contains(err.Error(), "test.missing") {
		t.Fatalf("migration error = %v", err)
	}
}

func TestAgentBMigrateVersionsRootNotDirectory(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	if err := os.RemoveAll(filepath.Join(src, "versions")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "versions"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry()); err == nil {
		t.Fatal("migration with a non-directory versions root succeeded")
	}
}

func TestAgentBMigrateSortsMultipleRuns(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	// A second terminal run pinned to the same revision, written
	// after the first so the runs sort comparator must run.
	run2 := map[string]any{
		"id": "run-mig-2", "definition_id": "mig-wf", "revision": 2,
		"actor": "alice", "status": "succeeded",
		"input": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}},
		"steps": []map[string]any{
			{"node_id": "start", "attempt": 0, "iteration": 0, "status": "succeeded",
				"output": map[string]any{"payload": map[string]any{"health": map[string]any{"status": "ok"}}}},
			{"node_id": "chk", "attempt": 0, "iteration": 0, "status": "succeeded",
				"output": map[string]any{"result": true, "field": "health.status"}},
		},
		"created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:01:00Z",
		"finished_at": "2026-10-01T00:01:00Z",
	}
	agentBWriteJSONFile(t, filepath.Join(src, "runs", "run-mig-2.json"), run2)
	rep, err := MigratePhase8Dir(ctx, src, NewMemoryStore(), DefaultRegistry())
	if err != nil {
		t.Fatalf("MigratePhase8Dir: %v", err)
	}
	if rep.Runs != 2 {
		t.Fatalf("report runs = %d, want 2", rep.Runs)
	}
}

func TestAgentBMigrateRunWithoutInput(t *testing.T) {
	ctx := context.Background()
	src := agentBMigrateSource(t)
	path := filepath.Join(src, "runs", "run-mig-1.json")
	m := agentBReadJSONFile(t, path)
	delete(m, "input")
	agentBWriteJSONFile(t, path, m)
	dst := NewMemoryStore()
	rep, err := MigratePhase8Dir(ctx, src, dst, DefaultRegistry())
	if err != nil {
		t.Fatalf("MigratePhase8Dir: %v", err)
	}
	if rep.Runs != 1 {
		t.Fatalf("report runs = %d, want 1", rep.Runs)
	}
	run, err := dst.GetRun(ctx, "run-mig-1")
	if err != nil {
		t.Fatalf("GetRun migrated: %v", err)
	}
	// writeMigratedRun 450-451 (input == nil default) is unreachable:
	// r.Input is typed map[string]any, so deepCopyValue's map case
	// allocates a fresh (empty, non-nil) map even for a nil input, and
	// redactJSONValue preserves that map — the type assertion at 448
	// can never yield a nil map. Assert the observable consequence.
	if run.Input == nil || len(run.Input) != 0 {
		t.Fatalf("migrated run input = %#v, want a non-nil empty map", run.Input)
	}
}

func TestAgentBEngineRecomputeRunStateTerminalRun(t *testing.T) {
	// engine.go 473-474: recomputeRunState returns early (nil) for a
	// terminal run without reading node executions.
	base := NewMemoryStore()
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	def.ID = "wf-term-recompute"
	run := agentBSeedRun(t, base, def, RunSucceeded, map[string]NodeState{"start": NodeSucceeded, "a": NodeSucceeded})
	eng, err := NewEngine(EngineOptions{Store: base, Registry: DefaultRegistry(), WorkerID: "agentb"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := eng.recomputeRunState(context.Background(), run.ID); err != nil {
		t.Fatalf("recomputeRunState(terminal) = %v, want nil", err)
	}
	got, err := base.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != RunSucceeded {
		t.Fatalf("run state = %q, want %q", got.State, RunSucceeded)
	}
}

func TestAgentBEngineFinishRunGetRunError(t *testing.T) {
	// engine.go 559-560: finishRun's GetRun AFTER a successful
	// recompute fails. The wrapper fails the second GetRun call:
	// call 1 is recomputeRunState's read, call 2 is finishRun's.
	base := NewMemoryStore()
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.echo", nil})
	def.ID = "wf-finish-getrun"
	run := agentBSeedRun(t, base, def, RunRunning, map[string]NodeState{"start": NodeSucceeded, "a": NodeSucceeded})
	wrapped := &agentBStore{Store: base, getRunFailAfter: 2}
	eng, err := NewEngine(EngineOptions{Store: wrapped, Registry: DefaultRegistry(), WorkerID: "agentb"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	got, err := eng.finishRun(context.Background(), run)
	if err == nil {
		t.Fatal("finishRun with failing second GetRun returned nil error")
	}
	if !strings.Contains(err.Error(), "get run boom") {
		t.Fatalf("finishRun error = %v, want the injected GetRun failure", err)
	}
	if got.ID != run.ID {
		t.Fatalf("finishRun run = %q, want %q (the input run is returned on error)", got.ID, run.ID)
	}
}

func TestAgentBServicePublishContentHashError(t *testing.T) {
	// task8_service.go 198-199: Publish's ContentHash fails when the
	// draft is not JSON-encodable. Config validation is shallow:
	// checkFieldType only asserts an array field is []any, so a func
	// element inside the MCP allowlist passes ValidateDefinition
	// (unknown keys are rejected, but array elements are not walked)
	// and then fails json.Marshal inside ContentHash.
	base := NewMemoryStore()
	wrapped := &agentBStore{Store: base}
	svc := agentBService(t, wrapped)
	def := agentBSingleTriggerDef("svc-hash")
	def.Nodes = append(def.Nodes, DefinitionNode{ID: "mcp", NodeID: "nexaroute.mcp.call",
		Version: SupportedVersion, Config: map[string]any{"allowlist": []any{func() {}}}})
	def.Edges = []DefinitionEdge{{ID: "e1", From: "start", To: "mcp"}}
	wrapped.getDraftOverride = &def
	if _, err := svc.Publish(context.Background(), def.ID, task8Owner); err == nil {
		t.Fatal("Publish with unmarshalable draft returned nil error")
	} else if !strings.Contains(err.Error(), "not JSON-encodable") {
		t.Fatalf("Publish error = %v, want the ContentHash failure", err)
	}
}
