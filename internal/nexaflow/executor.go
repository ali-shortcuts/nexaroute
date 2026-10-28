package nexaflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// ServicePrincipal is the identity trigger-originated runs (schedule,
// webhook) execute as. It carries the operator role: enough to run the
// node catalog (routing read, providers read, usage read, evaluation run,
// video manage) and nothing that writes gateway configuration. Node
// permissions are still enforced per node by Registry.Execute; a workflow
// whose nodes need more than this principal holds fails at that node.
var ServicePrincipal = authz.Identity{Subject: "system:nexaflow", Issuer: "nexaflow", Roles: []authz.Role{authz.RoleOperator}}

// MaxWorkflowInputBytes bounds the workflow input document.
const MaxWorkflowInputBytes = 64 * 1024

// EngineOptions configures an Engine. Zero values take documented defaults.
type EngineOptions struct {
	// MaxConcurrentNodes bounds parallel nodes inside one run (default 4).
	MaxConcurrentNodes int
	// MaxStepsPerRun bounds total step records per run (default 5000).
	MaxStepsPerRun int
	// MaxConcurrentRuns bounds runs executing at once per engine (default 16).
	MaxConcurrentRuns int
	// BackoffBase is the base delay between node retries (default 50ms).
	BackoffBase time.Duration
	// LeaseTTL is the execution lease duration (default 30s).
	LeaseTTL time.Duration
	// RetentionDays is the default run retention for Prune (default 90).
	RetentionDays int
	// Now is the clock, overridable in tests.
	Now func() time.Time
	// ID generates run ids, overridable in tests.
	ID func() string
}

// Engine executes published workflow definitions durably: every step is
// persisted before and after node execution, so a crash loses at most the
// in-flight step, and recovery parks (never blindly re-runs) the run.
//
// Data flow (Phase 1 contract, Phase 8 mapping extension): a node's input
// is the merge of its direct predecessors' outputs, in edge-declaration
// order, later keys overwriting earlier ones — unless the node carries an
// explicit Input mapping, in which case the mapping fully determines the
// input (it replaces, not overlays, the merge). Iterated node outputs are
// arrays and never merge into a map input; they are reachable through
// mappings only.
type Engine struct {
	reg     *Registry
	store   Store
	backend Backend
	audit   AuditSink
	opts    EngineOptions

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	// Striped per-run persist locks: a cached map of one mutex per run
	// id would grow without bound over a gateway's lifetime, and
	// deleting entries would break mutual exclusion. Striping bounds
	// the memory; distinct runs sharing a stripe merely serialize
	// their persists briefly.
	locks           [64]sync.Mutex
	sem             chan struct{}
	owner           string
	webhookVerifier *WebhookVerifier
}

// NewEngine builds an Engine. A nil backend means nodes that need one fail
// closed with a backend error; pure control nodes still execute.
func NewEngine(reg *Registry, store Store, backend Backend, audit AuditSink, opts EngineOptions) *Engine {
	if reg == nil {
		reg = DefaultRegistry()
	}
	if opts.MaxConcurrentNodes <= 0 {
		opts.MaxConcurrentNodes = 4
	}
	if opts.MaxStepsPerRun <= 0 {
		opts.MaxStepsPerRun = 5000
	}
	if opts.MaxConcurrentRuns <= 0 {
		opts.MaxConcurrentRuns = 16
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = 50 * time.Millisecond
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 30 * time.Second
	}
	if opts.RetentionDays <= 0 {
		opts.RetentionDays = 90
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ID == nil {
		opts.ID = func() string {
			var b [16]byte
			_, _ = rand.Read(b[:])
			return "run_" + hex.EncodeToString(b[:])
		}
	}
	if audit == nil {
		audit = &MemoryAuditSink{}
	}
	return &Engine{
		reg: reg, store: store, backend: backend, audit: audit, opts: opts,
		inflight:        make(map[string]context.CancelFunc),
		sem:             make(chan struct{}, opts.MaxConcurrentRuns),
		owner:           "engine_" + opts.ID(),
		webhookVerifier: NewWebhookVerifier(),
	}
}

func (e *Engine) now() time.Time { return e.opts.Now() }

func (e *Engine) runLock(id string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return &e.locks[h.Sum32()%uint32(len(e.locks))]
}

func (e *Engine) emit(actor, action, definitionID, runID string, success bool, detail string) {
	e.audit.Record(AuditEvent{
		Actor: actor, Action: action, DefinitionID: definitionID, RunID: runID,
		Success: success, Detail: detail, At: e.now(),
	})
}

func (e *Engine) persist(ctx context.Context, r *Run) error {
	lock := e.runLock(r.ID)
	lock.Lock()
	defer lock.Unlock()
	r.UpdatedAt = e.now()
	return e.store.UpdateRun(ctx, r)
}

// StartRun creates and executes a run of a published definition revision.
// Revision 0 means the currently published revision. TriggerKind is
// "manual", "webhook", or "schedule"; for webhook and schedule runs
// triggerNodeID names the entry trigger instance, and for manual runs it
// is empty (all manual trigger instances are entries). The run executes
// synchronously until it reaches a terminal status, awaits approval, or
// the context ends; HTTP layers run it in a goroutine and return the run
// id immediately. An idempotency key, when given, makes a second StartRun
// with the same definition and key return the original run instead of
// dispatching a duplicate.
// prepareRun validates a dispatch and persists the run record without
// executing it. It returns execute=false for an idempotency replay, in
// which case the caller returns the original run untouched.
func (e *Engine) prepareRun(ctx context.Context, principal authz.Identity, definitionID string, revision uint64, triggerKind, triggerNodeID, idempotencyKey string, input map[string]any) (*Run, *Definition, map[string]bool, bool, error) {
	if !principal.Authenticated() {
		return nil, nil, nil, false, NewNodeError("permission_denied", false, "anonymous principal cannot start a workflow run")
	}
	var def *Definition
	var ver *DefinitionVersion
	var err error
	if revision > 0 {
		def, ver, err = e.store.GetVersion(ctx, definitionID, revision)
	} else {
		def, ver, err = e.store.LatestPublished(ctx, definitionID)
	}
	if err != nil {
		return nil, nil, nil, false, err
	}
	if ver.Status != DefinitionPublished {
		return nil, nil, nil, false, NewNodeError("invalid_state", false, "definition %q revision %d is %s, not published", definitionID, ver.Revision, ver.Status)
	}
	if err := ValidateDefinitionExtended(def, e.reg); err != nil {
		return nil, nil, nil, false, err
	}
	if raw, err := json.Marshal(input); err != nil || len(raw) > MaxWorkflowInputBytes {
		return nil, nil, nil, false, NewNodeError("input_too_large", false, "workflow input exceeds %d bytes", MaxWorkflowInputBytes)
	}
	if idempotencyKey != "" {
		if prev, err := e.store.FindRunByIdempotency(ctx, definitionID, idempotencyKey); err == nil && prev != nil {
			return prev, nil, nil, false, nil
		} else if err != nil {
			return nil, nil, nil, false, err
		}
	}
	maxConcurrent := def.Settings.MaxConcurrentRuns
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	active, err := e.store.ListRuns(ctx, RunFilter{DefinitionID: definitionID})
	if err != nil {
		return nil, nil, nil, false, err
	}
	activeCount := 0
	for _, r := range active {
		if r.Status == RunRunning || r.Status == RunAwaitingApproval {
			activeCount++
		}
	}
	if activeCount >= maxConcurrent {
		return nil, nil, nil, false, NewNodeError("run_conflict", false, "definition %q already has %d active run(s)", definitionID, activeCount)
	}
	// Resolve entry trigger instances.
	entryTriggers := map[string]bool{}
	switch triggerKind {
	case TriggerManual:
		for _, t := range def.Triggers {
			if t.Kind == TriggerManual {
				entryTriggers[t.Name] = true
			}
		}
		if len(entryTriggers) == 0 {
			return nil, nil, nil, false, NewNodeError("validation_error", false, "definition %q has no manual trigger", definitionID)
		}
	case TriggerWebhook, TriggerSchedule:
		if triggerNodeID == "" {
			return nil, nil, nil, false, NewNodeError("validation_error", false, "trigger node id is required for %s runs", triggerKind)
		}
		decl, ok := def.TriggerDecl(triggerNodeID)
		if !ok || decl.Kind != triggerKind {
			return nil, nil, nil, false, NewNodeError("validation_error", false, "trigger node %q is not a %s trigger", triggerNodeID, triggerKind)
		}
		entryTriggers[triggerNodeID] = true
	default:
		return nil, nil, nil, false, NewNodeError("validation_error", false, "unknown trigger kind %q", triggerKind)
	}
	now := e.now()
	run := &Run{
		ID: runIDOr(e.opts.ID()), DefinitionID: definitionID, Revision: ver.Revision,
		TenantScope: def.TenantScope, Trigger: triggerKind, TriggerNodeID: triggerNodeID,
		Actor: principal.Subject, IdempotencyKey: idempotencyKey,
		Status: RunRunning, Input: cloneMap(input), Outputs: map[string]any{},
		Decisions: map[string]ApprovalDecision{}, HandlerUses: map[string]int{},
		PolicyApproved: def.Settings.PolicyApproved,
		LeaseOwner:     e.owner, LeaseUntil: now.Add(e.opts.LeaseTTL),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := e.store.CreateRun(ctx, run); err != nil {
		return nil, nil, nil, false, err
	}
	e.emit(principal.Subject, "run_started", definitionID, run.ID, true, "trigger="+triggerKind)
	return run, def, entryTriggers, true, nil
}

// StartRun creates and executes a run of a published definition revision.
// Revision 0 means the currently published revision. TriggerKind is
// "manual", "webhook", or "schedule"; for webhook and schedule runs
// triggerNodeID names the entry trigger instance, and for manual runs it
// is empty (all manual trigger instances are entries). The run executes
// synchronously until it reaches a terminal status, awaits approval, or
// the context ends; HTTP layers run it in a goroutine and return the run
// id immediately. An idempotency key, when given, makes a second StartRun
// with the same definition and key return the original run instead of
// dispatching a duplicate.
func (e *Engine) StartRun(ctx context.Context, principal authz.Identity, definitionID string, revision uint64, triggerKind, triggerNodeID, idempotencyKey string, input map[string]any) (*Run, error) {
	run, def, entryTriggers, execute, err := e.prepareRun(ctx, principal, definitionID, revision, triggerKind, triggerNodeID, idempotencyKey, input)
	if err != nil || !execute {
		return run, err
	}
	return e.execute(ctx, run, def, principal, entryTriggers)
}

// StartRunAsync validates and persists the run, then executes it on a
// detached background context and returns immediately. The run record in
// the store is the source of truth for progress: execution failures land
// in the run's terminal status, and CancelRun still interrupts the run
// through the engine's in-flight registry. Validation, idempotency, and
// concurrency failures are returned synchronously before dispatch.
// The returned run is the pre-execution record and aliases the
// executing run: only its immutable fields (notably ID) may be read
// after return. Poll the store for status.
func (e *Engine) StartRunAsync(ctx context.Context, principal authz.Identity, definitionID string, revision uint64, triggerKind, triggerNodeID, idempotencyKey string, input map[string]any) (*Run, error) {
	run, def, entryTriggers, execute, err := e.prepareRun(ctx, principal, definitionID, revision, triggerKind, triggerNodeID, idempotencyKey, input)
	if err != nil || !execute {
		return run, err
	}
	go func() {
		_, _ = e.execute(context.Background(), run, def, principal, entryTriggers)
	}()
	return run, nil
}

func runIDOr(id string) string {
	if id == "" {
		return "run_unknown"
	}
	return id
}

// execute drives one run to terminal, awaiting, or cancelled status. It is
// re-entrant: state is derived from the persisted run record, so resuming
// a suspended or parked run re-enters the same loop and reuses completed
// steps. Trigger instances outside entryTriggers are skipped up front;
// their exclusive subtrees are then skipped by propagation.
func (e *Engine) execute(ctx context.Context, run *Run, def *Definition, principal authz.Identity, entryTriggers map[string]bool) (*Run, error) {
	// Runs loaded from a store (or hand-crafted by recovery tooling) may
	// carry nil maps; every write path below assigns into them, so
	// initialize them here rather than panicking mid-execution.
	if run.Outputs == nil {
		run.Outputs = map[string]any{}
	}
	if run.Decisions == nil {
		run.Decisions = map[string]ApprovalDecision{}
	}
	if run.HandlerUses == nil {
		run.HandlerUses = map[string]int{}
	}
	// The global run slot enforces EngineOptions.MaxConcurrentRuns
	// across concurrent StartRun/ApproveRun/ResumeRun callers. It is
	// acquired here, the single choke point, rather than in each caller.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return run, NewNodeError("cancelled", false, "run cancelled while waiting for an execution slot")
	}
	// A run already terminal (for example cancelled between creation and
	// loop start) is never re-executed.
	if run.Status.IsTerminal() {
		return run, nil
	}
	// Defensive: a leftover running step means a previous execution died
	// mid-step. Park instead of blindly continuing.
	for i := range run.Steps {
		if run.Steps[i].Status == StepRunning {
			run.Steps[i].Status = StepAmbiguous
			run.AmbiguousNodeID = run.Steps[i].NodeID
			run.Status = RunParked
			run.LeaseOwner = ""
			if err := e.persist(ctx, run); err != nil {
				return run, err
			}
			e.emit(principal.Subject, "run_parked", run.DefinitionID, run.ID, false, "interrupted step "+run.AmbiguousNodeID)
			return run, nil
		}
	}
	if entryTriggers != nil {
		for _, n := range def.Nodes {
			if !def.IsTriggerNode(n.ID, e.reg) {
				continue
			}
			if entryTriggers[n.ID] {
				continue
			}
			if _, ok := run.LatestStep(n.ID); !ok {
				e.appendStep(run, n.ID, -1, StepSkipped, nil, nil, nil)
			}
		}
		if err := e.persist(ctx, run); err != nil {
			return run, err
		}
	}
	// Handler and fallback nodes are out-of-band: they run only through
	// handleFailure, never through graph scheduling. Without this they
	// would execute on the success path with an input that lacks
	// error_code, or block completion. The skipped record is the honest
	// "not invoked" history entry; handleFailure appends the real step
	// afterwards when the handler actually runs.
	if def.OnError != nil {
		for _, id := range []string{def.OnError.HandlerNodeID, def.OnError.FallbackNodeID} {
			if id == "" {
				continue
			}
			if _, ok := run.LatestStep(id); !ok {
				e.appendStep(run, id, -1, StepSkipped, nil, nil, nil)
			}
		}
		if err := e.persist(ctx, run); err != nil {
			return run, err
		}
	}

	runCtx := ctx
	var cancel context.CancelFunc = func() {}
	if def.Settings.RunTimeoutSeconds > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(def.Settings.RunTimeoutSeconds)*time.Second)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	e.mu.Lock()
	e.inflight[run.ID] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.inflight, run.ID)
		e.mu.Unlock()
		cancel()
	}()

	for {
		if err := runCtx.Err(); err != nil {
			if err == context.DeadlineExceeded {
				return e.finishFailed(runCtx, run, principal, NewNodeError("timeout", false, "run exceeded %d seconds", def.Settings.RunTimeoutSeconds))
			}
			return e.finishCancelled(runCtx, run, principal)
		}
		if len(run.Steps) > e.opts.MaxStepsPerRun {
			return e.finishFailed(runCtx, run, principal, NewNodeError("limit_exceeded", false, "run exceeded %d steps", e.opts.MaxStepsPerRun))
		}
		// A failed step left over from a previous execution (for example a
		// crash between a node failure and its handler) is resolved here
		// before scheduling anything else, so a resumed run never dead-ends.
		if done, out, err := e.resolvePendingFailure(runCtx, run, def, principal); done {
			return out, err
		}
		if e.propagateSkips(run, def) {
			if err := e.persist(runCtx, run); err != nil {
				return run, err
			}
			continue
		}
		ready := e.readyNodes(run, def)
		if len(ready) == 0 {
			if e.allTerminal(run, def) {
				return e.finishSucceeded(runCtx, run, principal)
			}
			return e.finishFailed(runCtx, run, principal, NewNodeError("internal_error", false, "no executable node remains but the run is not complete"))
		}
		results := e.executeBatch(runCtx, run, def, principal, ready)
		suspend, failed, ferr := e.applyBatch(runCtx, run, def, principal, results)
		if err := e.persist(runCtx, run); err != nil {
			return run, err
		}
		if suspend {
			run.Status = RunAwaitingApproval
			run.LeaseOwner = ""
			if err := e.persist(runCtx, run); err != nil {
				return run, err
			}
			e.emit(principal.Subject, "run_awaiting_approval", run.DefinitionID, run.ID, true, "")
			return run, nil
		}
		if failed {
			if ferr != nil && ferr.Code == "cancelled" {
				return e.finishCancelled(runCtx, run, principal)
			}
			return e.finishFailed(runCtx, run, principal, ferr)
		}
	}
}

func (e *Engine) allTerminal(run *Run, def *Definition) bool {
	latest := run.LatestByNode()
	for _, n := range def.Nodes {
		s, ok := latest[n.ID]
		if !ok || !s.Status.IsTerminal() {
			return false
		}
	}
	return true
}

// satisfiedPredecessor reports whether a predecessor's output may be used
// downstream: it succeeded, or it failed but the error handler substituted
// a fallback output for it. The failed steps stay in history; only the
// output stands in. This is what keeps a fallback-substituted node from
// deadlocking its downstream nodes in the same execution loop.
func satisfiedPredecessor(run *Run, pred Step) bool {
	if pred.Status == StepSucceeded {
		return true
	}
	if pred.Status == StepFailed {
		_, ok := run.Outputs[pred.NodeID]
		return ok
	}
	return false
}

// edgeInactive reports whether an edge cannot carry input: its predecessor
// was skipped, or it is a labelled edge from a branch node whose selected
// branch differs. Unlabelled edges from branch nodes are unconditional
// (Phase 1 rule) and never inactive for branch reasons.
func (e *Engine) edgeInactive(run *Run, def *Definition, edge DefinitionEdge, latest map[string]Step) bool {
	pred, ok := latest[edge.From]
	if !ok {
		return false // predecessor not yet run: pending, not inactive
	}
	if pred.Status == StepSkipped {
		return true
	}
	if pred.Status == StepSucceeded {
		from, ok := def.NodeByID(edge.From)
		if ok && from.NodeID == "nexaroute.control.branch" && edge.Branch != "" {
			if out, ok := run.Outputs[edge.From].(map[string]any); ok {
				if sel, ok := out["selected_branch"].(string); ok && sel != edge.Branch {
					return true
				}
			}
		}
	}
	return false
}

// propagateSkips marks nodes skipped when every incoming edge is inactive.
// It returns true when it skipped at least one node.
func (e *Engine) propagateSkips(run *Run, def *Definition) bool {
	latest := run.LatestByNode()
	changed := false
	for _, n := range def.Nodes {
		if _, ok := latest[n.ID]; ok {
			continue
		}
		incoming := def.Incoming(n.ID)
		if len(incoming) == 0 {
			continue
		}
		allInactive := true
		for _, edge := range incoming {
			if !e.edgeInactive(run, def, edge, latest) {
				allInactive = false
				break
			}
		}
		if allInactive {
			e.appendStep(run, n.ID, -1, StepSkipped, nil, nil, nil)
			latest[n.ID] = run.Steps[len(run.Steps)-1]
			changed = true
		}
	}
	return changed
}

// readyNodes returns nodes with at least one active incoming edge (or no
// incoming edges, for entry triggers) whose active predecessors all
// succeeded, in definition order.
func (e *Engine) readyNodes(run *Run, def *Definition) []DefinitionNode {
	latest := run.LatestByNode()
	var ready []DefinitionNode
	for _, n := range def.Nodes {
		if s, ok := latest[n.ID]; ok {
			if s.Status.IsTerminal() {
				continue
			}
			// A waiting node (approval pause) becomes ready only once
			// its decision has been recorded; otherwise it stays
			// suspended and its downstream nodes stay blocked.
			if s.Status == StepWaiting {
				if _, decided := run.Decisions[n.ID]; !decided {
					continue
				}
			}
		}
		incoming := def.Incoming(n.ID)
		if len(incoming) == 0 {
			ready = append(ready, n)
			continue
		}
		hasActive := false
		blocked := false
		for _, edge := range incoming {
			if e.edgeInactive(run, def, edge, latest) {
				continue
			}
			hasActive = true
			pred, ok := latest[edge.From]
			if !ok || !satisfiedPredecessor(run, pred) {
				blocked = true
				break
			}
		}
		if hasActive && !blocked {
			ready = append(ready, n)
		}
	}
	return ready
}

func (e *Engine) appendStep(run *Run, nodeID string, iteration int, status StepStatus, input, output map[string]any, err *NodeError) Step {
	now := e.now()
	s := Step{
		NodeID: nodeID, Attempt: 1, Iteration: iteration, Status: status,
		StartedAt: now, Input: cloneMap(input), Output: cloneMap(output), Error: err,
	}
	if status.IsTerminal() || status == StepWaiting || status == StepAmbiguous {
		fin := now
		s.FinishedAt = &fin
		s.DurationMs = 0
	}
	run.Steps = append(run.Steps, s)
	return s
}

// batchResult is one node's execution outcome.
type batchResult struct {
	node    DefinitionNode
	output  any // map[string]any, or []any for iterated nodes
	steps   []Step
	err     *NodeError
	suspend bool
}

func (e *Engine) executeBatch(ctx context.Context, run *Run, def *Definition, principal authz.Identity, ready []DefinitionNode) []batchResult {
	sem := make(chan struct{}, e.opts.MaxConcurrentNodes)
	results := make([]batchResult, len(ready))
	var wg sync.WaitGroup
	for i, node := range ready {
		wg.Add(1)
		go func(i int, node DefinitionNode) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = batchResult{node: node, err: NewNodeError("cancelled", false, "run cancelled")}
				return
			}
			results[i] = e.executeNode(ctx, run, def, node, principal)
		}(i, node)
	}
	wg.Wait()
	return results
}

// executeNode runs one node (with retries and ForEach expansion) and
// returns its outcome. Steps are built in memory so a concurrent batch
// never interleaves history writes; applyBatch appends them in
// definition order.
func (e *Engine) executeNode(ctx context.Context, run *Run, def *Definition, node DefinitionNode, principal authz.Identity) batchResult {
	spec, err := e.reg.Lookup(node.NodeID, node.Version)
	if err != nil {
		return batchResult{node: node, err: asNodeError(err)}
	}
	// Approval suspension: without a recorded decision the node cannot run.
	// The waiting step carries no FinishedAt: it is still waiting.
	if node.NodeID == "nexaroute.control.approval" {
		if _, decided := run.Decisions[node.ID]; !decided {
			step := Step{NodeID: node.ID, Attempt: 1, Iteration: -1, Status: StepWaiting, StartedAt: e.now()}
			return batchResult{node: node, steps: []Step{step}, suspend: true}
		}
	}
	ctxObj := e.contextObject(run)
	if node.ForEach != nil {
		return e.executeForEach(ctx, run, def, node, spec, principal, ctxObj)
	}
	input, ierr := e.nodeInput(run, def, node, spec, ctxObj, nil)
	if ierr != nil {
		return batchResult{node: node, err: ierr}
	}
	if node.NodeID == "nexaroute.control.approval" {
		dec := run.Decisions[node.ID]
		if input == nil {
			input = map[string]any{}
		}
		input["decision"] = dec.Decision
		input["approver"] = dec.Approver
		input["requester"] = run.Actor
	}
	return e.runWithRetries(ctx, run, def, node, spec, principal, input, -1)
}

// nodeInput builds one node's input. Trigger nodes get trigger-specific
// inputs; other nodes get the predecessor merge, or their explicit Input
// mapping when present (the mapping replaces the merge entirely).
// loopCtx, when non-nil, adds "loop" {item, index} to the mapping context.
//
// The merge is filtered to the node's declared InputSchema fields. Without
// this, every downstream node would inherit the manual trigger's "payload"
// key (and every other predecessor's full output), and the registry's
// strict input validation would reject nodes whose schema does not list
// those keys — making the pure-merge flow unusable for any node without
// an explicit mapping. A node with no declared input fields (cost.usage)
// receives an empty input. Explicit mappings are NOT filtered: the author
// chose those keys, and the registry still validates them strictly.
func (e *Engine) nodeInput(run *Run, def *Definition, node DefinitionNode, spec *NodeSpec, ctxObj map[string]any, loopCtx map[string]any) (map[string]any, *NodeError) {
	if def.IsTriggerNode(node.ID, e.reg) {
		switch node.NodeID {
		case "nexaroute.trigger.manual":
			return map[string]any{"payload": cloneMap(run.Input)}, nil
		case "nexaroute.trigger.webhook":
			return map[string]any{
				"timestamp": strVal(run.Input["timestamp"]),
				"signature": strVal(run.Input["signature"]),
				"body":      strVal(run.Input["body"]),
			}, nil
		case "nexaroute.trigger.schedule":
			return map[string]any{"last_run": strVal(run.Input["last_run"])}, nil
		}
	}
	if node.Input != nil {
		obj := ctxObj
		if loopCtx != nil {
			obj = cloneMap(ctxObj)
			obj["loop"] = loopCtx
		}
		return e.resolveInput(node.Input, obj)
	}
	allowed := map[string]bool{}
	if spec != nil {
		for _, f := range spec.InputSchema {
			allowed[f.Name] = true
		}
	}
	merged := map[string]any{}
	for _, edge := range def.Incoming(node.ID) {
		if e.edgeInactive(run, def, edge, run.LatestByNode()) {
			continue
		}
		pred, ok := run.LatestStep(edge.From)
		if !ok || !satisfiedPredecessor(run, pred) {
			continue
		}
		if out, ok := run.Outputs[edge.From].(map[string]any); ok {
			for k, v := range out {
				if !allowed[k] {
					continue
				}
				merged[k] = v
			}
		}
	}
	return merged, nil
}

func strVal(v any) string {
	s, _ := v.(string)
	return s
}

// executeForEach expands a node over a bounded array.
func (e *Engine) executeForEach(ctx context.Context, run *Run, def *Definition, node DefinitionNode, spec *NodeSpec, principal authz.Identity, ctxObj map[string]any) batchResult {
	raw, err := resolvePath(ctxObj, node.ForEach.Source)
	if err != nil {
		return batchResult{node: node, err: NewNodeError("input_resolution", false, "for_each source %q: %v", node.ForEach.Source, err)}
	}
	items, ok := raw.([]any)
	if !ok {
		return batchResult{node: node, err: NewNodeError("validation_error", false, "for_each source %q is not an array", node.ForEach.Source)}
	}
	max := node.ForEach.Max
	if max == 0 {
		max = MaxLoopItems
	}
	if len(items) > max {
		items = items[:max]
	}
	var steps []Step
	outputs := make([]any, 0, len(items))
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return batchResult{node: node, steps: steps, err: NewNodeError("cancelled", false, "run cancelled")}
		}
		loopCtx := map[string]any{"item": item, "index": float64(i)}
		input, ierr := e.nodeInput(run, def, node, spec, ctxObj, loopCtx)
		if ierr != nil {
			return batchResult{node: node, steps: steps, err: ierr}
		}
		res := e.runWithRetries(ctx, run, def, node, spec, principal, input, i)
		steps = append(steps, res.steps...)
		if res.err != nil {
			return batchResult{node: node, steps: steps, err: res.err}
		}
		outputs = append(outputs, res.output)
	}
	return batchResult{node: node, steps: steps, output: outputs}
}

// runWithRetries executes one node invocation with the spec's retry policy:
// attempts are bounded by MaxAttempts, and only retryable codes in
// RetryableCodes (or any retryable code when the list is empty) are retried.
func (e *Engine) runWithRetries(ctx context.Context, run *Run, def *Definition, node DefinitionNode, spec *NodeSpec, principal authz.Identity, input map[string]any, iteration int) batchResult {
	maxAttempts := spec.Retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var steps []Step
	var lastErr *NodeError
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := e.now()
		step := Step{NodeID: node.ID, Attempt: attempt, Iteration: iteration, Status: StepRunning, StartedAt: start, Input: redactedCopy(input)}
		out, err := e.reg.Execute(ctx, spec, principal, input, def.EffectiveConfig(node), e.backend, run.PolicyApproved)
		fin := e.now()
		step.FinishedAt = &fin
		step.DurationMs = fin.Sub(start).Milliseconds()
		if err != nil {
			ne := asNodeError(err)
			step.Status = StepFailed
			step.Error = ne
			steps = append(steps, step)
			lastErr = ne
			if ctx.Err() != nil || ne.Code == "cancelled" {
				return batchResult{node: node, steps: steps, err: ne}
			}
			// An empty RetryableCodes list means any retryable code
			// (the registry helper alone would reject all of them).
			if ne.Retryable && (len(spec.Retry.RetryableCodes) == 0 || retryableCode(spec.Retry.RetryableCodes, ne.Code)) && attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return batchResult{node: node, steps: steps, err: NewNodeError("cancelled", false, "run cancelled")}
				case <-time.After(e.opts.BackoffBase * time.Duration(attempt)):
				}
				continue
			}
			return batchResult{node: node, steps: steps, err: ne}
		}
		step.Status = StepSucceeded
		step.Output = redactedCopy(out)
		steps = append(steps, step)
		return batchResult{node: node, steps: steps, output: out}
	}
	return batchResult{node: node, steps: steps, err: lastErr}
}

// applyBatch appends batch steps in definition order, updates outputs, and
// handles suspension, failure (with the error handler), and cancellation.
// It returns (suspend, failed, failure).
func (e *Engine) applyBatch(ctx context.Context, run *Run, def *Definition, principal authz.Identity, results []batchResult) (bool, bool, *NodeError) {
	// A suspension must not discard the rest of the batch: nodes that
	// already executed alongside the suspending node keep their steps
	// and outputs, so resuming the run never re-executes them. A failure
	// disposition later in the same batch still terminates the run.
	suspended := false
	for _, res := range results {
		for _, s := range res.steps {
			run.Steps = append(run.Steps, s)
		}
		if res.suspend {
			suspended = true
			continue
		}
		if res.err != nil {
			if ctx.Err() != nil || res.err.Code == "cancelled" {
				return false, true, res.err
			}
			curErr := res.err
			for {
				disposition, _ := e.handleFailure(ctx, run, def, principal, res.node, curErr)
				switch disposition {
				case dispositionFail:
					return false, true, curErr
				case dispositionFallback:
					goto nodeDone
				case dispositionRetry:
					reres := e.executeNode(ctx, run, def, res.node, principal)
					for _, s := range reres.steps {
						run.Steps = append(run.Steps, s)
					}
					if reres.suspend {
						suspended = true
						goto nodeDone
					}
					if reres.err != nil {
						if ctx.Err() != nil || reres.err.Code == "cancelled" {
							return false, true, reres.err
						}
						curErr = reres.err
						continue
					}
					run.Outputs[res.node.ID] = reres.output
					goto nodeDone
				}
			}
		nodeDone:
			continue
		}
		if res.node.ForEach != nil {
			run.Outputs[res.node.ID] = res.output
		} else {
			out, _ := res.output.(map[string]any)
			run.Outputs[res.node.ID] = out
		}
	}
	if suspended {
		return true, false, nil
	}
	return false, false, nil
}

type disposition int

const (
	dispositionFail disposition = iota
	dispositionRetry
	dispositionFallback
)

// resolvePendingFailure handles a failed latest step left over from a
// previous execution (for example a crash between a node failure and its
// error handler). It returns done=true when the run reached a terminal
// outcome or must stop; done=false means the loop may continue.
func (e *Engine) resolvePendingFailure(ctx context.Context, run *Run, def *Definition, principal authz.Identity) (bool, *Run, error) {
	latest := run.LatestByNode()
	for _, node := range def.Nodes {
		s, ok := latest[node.ID]
		if !ok || s.Status != StepFailed || s.Error == nil {
			continue
		}
		if _, recovered := run.Outputs[node.ID]; recovered {
			continue
		}
		disposition, _ := e.handleFailure(ctx, run, def, principal, node, s.Error)
		switch disposition {
		case dispositionFail:
			out, err := e.finishFailed(ctx, run, principal, s.Error)
			return true, out, err
		case dispositionFallback:
			if err := e.persist(ctx, run); err != nil {
				return true, run, err
			}
			return false, run, nil
		case dispositionRetry:
			reres := e.executeNode(ctx, run, def, node, principal)
			for _, st := range reres.steps {
				run.Steps = append(run.Steps, st)
			}
			if reres.suspend {
				run.Status = RunAwaitingApproval
				run.LeaseOwner = ""
				if err := e.persist(ctx, run); err != nil {
					return true, run, err
				}
				return true, run, nil
			}
			if reres.err != nil {
				if err := e.persist(ctx, run); err != nil {
					return true, run, err
				}
				return false, run, nil
			}
			run.Outputs[node.ID] = reres.output
			if err := e.persist(ctx, run); err != nil {
				return true, run, err
			}
			return false, run, nil
		}
	}
	return false, run, nil
}

// handleFailure runs the workflow's error handler for a failed node, if one
// is configured and its per-node budget remains. It returns the disposition
// and, for fallback, sets the fallback output in run.Outputs.
func (e *Engine) handleFailure(ctx context.Context, run *Run, def *Definition, principal authz.Identity, node DefinitionNode, failure *NodeError) (disposition, *NodeError) {
	if def.OnError == nil {
		return dispositionFail, nil
	}
	uses := run.HandlerUses[node.ID]
	if uses >= 2 {
		return dispositionFail, nil
	}
	handler, ok := def.NodeByID(def.OnError.HandlerNodeID)
	if !ok {
		return dispositionFail, nil
	}
	spec, err := e.reg.Lookup(handler.NodeID, handler.Version)
	if err != nil {
		return dispositionFail, nil
	}
	run.HandlerUses[node.ID] = uses + 1
	input := map[string]any{"error_code": failure.Code, "message": failure.Message}
	start := e.now()
	out, err := e.reg.Execute(ctx, spec, principal, input, def.EffectiveConfig(handler), e.backend, run.PolicyApproved)
	step := Step{NodeID: handler.ID, Attempt: 1, Iteration: -1, Status: StepRunning, StartedAt: start, Input: redactedCopy(input)}
	fin := e.now()
	step.FinishedAt = &fin
	step.DurationMs = fin.Sub(start).Milliseconds()
	if err != nil {
		step.Status = StepFailed
		step.Error = asNodeError(err)
		run.Steps = append(run.Steps, step)
		return dispositionFail, nil
	}
	step.Status = StepSucceeded
	step.Output = redactedCopy(out)
	run.Steps = append(run.Steps, step)
	run.Outputs[handler.ID] = out
	action, _ := out["action"].(string)
	switch action {
	case "retry":
		return dispositionRetry, nil
	case "fallback":
		if def.OnError.FallbackNodeID == "" {
			return dispositionFail, nil
		}
		fallback, ok := def.NodeByID(def.OnError.FallbackNodeID)
		if !ok {
			return dispositionFail, nil
		}
		fspec, err := e.reg.Lookup(fallback.NodeID, fallback.Version)
		if err != nil {
			return dispositionFail, nil
		}
		fctx := e.contextObject(run)
		finput, ierr := e.nodeInput(run, def, fallback, fspec, fctx, nil)
		if ierr != nil {
			return dispositionFail, nil
		}
		fres := e.runWithRetries(ctx, run, def, fallback, fspec, principal, finput, -1)
		for _, s := range fres.steps {
			run.Steps = append(run.Steps, s)
		}
		if fres.err != nil {
			return dispositionFail, nil
		}
		run.Outputs[node.ID] = fres.output
		run.Outputs[fallback.ID] = fres.output
		return dispositionFallback, nil
	default:
		return dispositionFail, nil
	}
}

// contextObject builds the mapping context: workflow input plus every
// succeeded node's output, keyed by node instance id.
func (e *Engine) contextObject(run *Run) map[string]any {
	obj := map[string]any{"input": cloneMap(run.Input)}
	for id, out := range run.Outputs {
		obj[id] = out
	}
	return obj
}

// resolveInput resolves a node input mapping against the context object.
// A reference to a missing value fails the node with input_resolution:
// skip propagation over edges already handles the branch case, and any
// other missing reference is an authoring error, not a silent zero value.
func (e *Engine) resolveInput(mapping map[string]any, ctxObj map[string]any) (map[string]any, *NodeError) {
	if mapping == nil {
		return map[string]any{}, nil
	}
	out, err := resolveValue(mapping, ctxObj)
	if err != nil {
		return nil, NewNodeError("input_resolution", false, "%v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, NewNodeError("input_resolution", false, "input mapping must resolve to an object")
	}
	return m, nil
}

func resolveValue(v any, ctxObj map[string]any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if ref, ok := t["$ref"].(string); ok {
				return resolvePath(ctxObj, ref)
			}
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			resolved, err := resolveValue(val, ctxObj)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			resolved, err := resolveValue(val, ctxObj)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	case string:
		if strings.HasPrefix(t, "$.") {
			return resolvePath(ctxObj, strings.TrimPrefix(t, "$."))
		}
		return t, nil
	default:
		return t, nil
	}
}

// resolvePath walks a dotted path through maps and arrays.
func resolvePath(ctxObj map[string]any, path string) (any, error) {
	parts := strings.Split(path, ".")
	cur := any(ctxObj)
	for _, part := range parts {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[part]
			if !ok {
				return nil, fmt.Errorf("path %q not found", path)
			}
			cur = v
		case []any:
			var idx int
			if _, err := fmt.Sscanf(part, "%d", &idx); err != nil || idx < 0 || idx >= len(c) {
				return nil, fmt.Errorf("path %q not found", path)
			}
			cur = c[idx]
		default:
			return nil, fmt.Errorf("path %q not found", path)
		}
	}
	return cur, nil
}

// redactedCopy returns a redacted deep copy for storage.
func redactedCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	v := redactJSONValue(cloneMap(m))
	out, _ := v.(map[string]any)
	return out
}

// ApproveRun records an authorized approval decision and resumes the run.
// The approver must differ from the run's actor (self-approval is
// rejected); the approval node itself re-checks this when it executes.
func (e *Engine) ApproveRun(ctx context.Context, principal authz.Identity, runID, decision string) (*Run, error) {
	if !principal.Authenticated() {
		return nil, NewNodeError("permission_denied", false, "anonymous principal cannot approve a run")
	}
	if decision != "approved" && decision != "rejected" {
		return nil, NewNodeError("validation_error", false, "decision must be approved or rejected")
	}
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != RunAwaitingApproval {
		return nil, NewNodeError("invalid_state", false, "run %q is %s, not awaiting approval", runID, run.Status)
	}
	if principal.Subject == run.Actor {
		return nil, NewNodeError("permission_denied", false, "approver must differ from the run requester")
	}
	var waitingNode string
	for i := len(run.Steps) - 1; i >= 0; i-- {
		if run.Steps[i].Status == StepWaiting {
			waitingNode = run.Steps[i].NodeID
			break
		}
	}
	if waitingNode == "" {
		return nil, NewNodeError("internal_error", false, "run %q awaits approval but has no waiting step", runID)
	}
	// The waiting step is NOT rewritten: it stays as the honest pause
	// record, and the decision execution appends its own step. Marking
	// it skipped would make the approval node terminal, so readyNodes
	// would never schedule its execution and the run would "succeed"
	// without the decision ever being evaluated.
	if run.Decisions == nil {
		run.Decisions = map[string]ApprovalDecision{}
	}
	run.Decisions[waitingNode] = ApprovalDecision{Decision: decision, Approver: principal.Subject, DecidedAt: e.now()}
	run.Status = RunRunning
	run.LeaseOwner = e.owner
	run.LeaseUntil = e.now().Add(e.opts.LeaseTTL)
	if err := e.persist(ctx, run); err != nil {
		return nil, err
	}
	e.emit(principal.Subject, "run_approved", run.DefinitionID, run.ID, true, "decision="+decision)
	def, _, err := e.store.GetVersion(ctx, run.DefinitionID, run.Revision)
	if err != nil {
		return nil, err
	}
	// Approval resumes never re-skip trigger nodes: completed steps are
	// reused, and the pre-skip gate only applies to nodes without steps.
	// Passing nil skips that gate on resume.
	return e.execute(ctx, run, def, principal, nil)
}

// ResumeMode selects how a parked run's ambiguous step is treated.
type ResumeMode string

const (
	// ResumeContinue resumes a parked run with no ambiguous step.
	ResumeContinue ResumeMode = "continue"
	// ResumeRetryAmbiguous re-executes the ambiguous node from scratch.
	ResumeRetryAmbiguous ResumeMode = "retry_ambiguous"
	// ResumeFailAmbiguous marks the ambiguous node failed and applies the
	// error handler (or fails the run when none is configured).
	ResumeFailAmbiguous ResumeMode = "fail_ambiguous"
	// ResumeSkipAmbiguous marks the ambiguous node skipped; downstream
	// nodes that depend on its output are skipped in turn.
	ResumeSkipAmbiguous ResumeMode = "skip_ambiguous"
)

// ResumeRun resumes a parked run with an explicit mode for its ambiguous
// step. Without an operator decision, a parked run never re-executes. A
// fail_ambiguous node is handled by the execute loop's pending-failure
// resolution (handler or run failure).
func (e *Engine) ResumeRun(ctx context.Context, principal authz.Identity, runID string, mode ResumeMode) (*Run, error) {
	if !principal.Authenticated() {
		return nil, NewNodeError("permission_denied", false, "anonymous principal cannot resume a run")
	}
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != RunParked {
		return nil, NewNodeError("invalid_state", false, "run %q is %s, not parked", runID, run.Status)
	}
	amb := run.AmbiguousNodeID
	switch mode {
	case ResumeContinue:
		if amb != "" {
			return nil, NewNodeError("invalid_state", false, "run %q has ambiguous node %q; choose retry_ambiguous, fail_ambiguous, or skip_ambiguous", runID, amb)
		}
	case ResumeRetryAmbiguous:
		if amb == "" {
			return nil, NewNodeError("invalid_state", false, "run %q has no ambiguous node to retry", runID)
		}
		run.AmbiguousNodeID = ""
	case ResumeFailAmbiguous:
		if amb == "" {
			return nil, NewNodeError("invalid_state", false, "run %q has no ambiguous node to fail", runID)
		}
		for i := range run.Steps {
			if run.Steps[i].NodeID == amb && run.Steps[i].Status == StepAmbiguous {
				run.Steps[i].Status = StepFailed
				run.Steps[i].Error = NewNodeError("ambiguous_outcome", false, "node outcome unknown after interrupted execution")
				fin := e.now()
				run.Steps[i].FinishedAt = &fin
				break
			}
		}
		run.AmbiguousNodeID = ""
	case ResumeSkipAmbiguous:
		if amb == "" {
			return nil, NewNodeError("invalid_state", false, "run %q has no ambiguous node to skip", runID)
		}
		for i := range run.Steps {
			if run.Steps[i].NodeID == amb && run.Steps[i].Status == StepAmbiguous {
				run.Steps[i].Status = StepSkipped
				fin := e.now()
				run.Steps[i].FinishedAt = &fin
				break
			}
		}
		run.AmbiguousNodeID = ""
	default:
		return nil, NewNodeError("validation_error", false, "unknown resume mode %q", mode)
	}
	run.Status = RunRunning
	run.LeaseOwner = e.owner
	run.LeaseUntil = e.now().Add(e.opts.LeaseTTL)
	if err := e.persist(ctx, run); err != nil {
		return nil, err
	}
	e.emit(principal.Subject, "run_resumed", run.DefinitionID, run.ID, true, "mode="+string(mode))
	def, _, err := e.store.GetVersion(ctx, run.DefinitionID, run.Revision)
	if err != nil {
		return nil, err
	}
	return e.execute(ctx, run, def, principal, nil)
}

// CancelRun cancels a run. In-flight executions in this process are
// interrupted via context; runs that are parked, awaiting, or executing
// elsewhere are marked cancelled directly.
func (e *Engine) CancelRun(ctx context.Context, principal authz.Identity, runID string) (*Run, error) {
	if !principal.Authenticated() {
		return nil, NewNodeError("permission_denied", false, "anonymous principal cannot cancel a run")
	}
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status.IsTerminal() {
		return nil, NewNodeError("invalid_state", false, "run %q is already %s", runID, run.Status)
	}
	e.mu.Lock()
	cancel, inflight := e.inflight[runID]
	e.mu.Unlock()
	if inflight {
		cancel()
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			updated, err := e.store.GetRun(ctx, runID)
			if err != nil {
				return nil, err
			}
			if updated.Status.IsTerminal() {
				return updated, nil
			}
		}
		return e.store.GetRun(ctx, runID)
	}
	run.Status = RunCancelled
	run.LeaseOwner = ""
	fin := e.now()
	run.FinishedAt = &fin
	for i := range run.Steps {
		if run.Steps[i].Status == StepWaiting || run.Steps[i].Status == StepRunning {
			run.Steps[i].Status = StepCancelled
			run.Steps[i].FinishedAt = &fin
		}
	}
	if err := e.persist(ctx, run); err != nil {
		return nil, err
	}
	e.emit(principal.Subject, "run_cancelled", run.DefinitionID, run.ID, true, "")
	return run, nil
}

// Recover parks every running run whose lease expired (or is missing) and
// which this engine is not currently executing. Awaiting runs are
// untouched: their decisions and completed steps are durable. Parked runs
// resume only through ResumeRun with an explicit mode.
func (e *Engine) Recover(ctx context.Context) ([]*Run, error) {
	runs, err := e.store.ListRuns(ctx, RunFilter{Status: RunRunning})
	if err != nil {
		return nil, err
	}
	now := e.now()
	var parked []*Run
	for _, run := range runs {
		e.mu.Lock()
		_, inflight := e.inflight[run.ID]
		e.mu.Unlock()
		if inflight {
			continue
		}
		if !run.LeaseUntil.IsZero() && run.LeaseUntil.After(now) {
			continue
		}
		for i := range run.Steps {
			if run.Steps[i].Status == StepRunning {
				run.Steps[i].Status = StepAmbiguous
				run.AmbiguousNodeID = run.Steps[i].NodeID
				fin := now
				run.Steps[i].FinishedAt = &fin
				break
			}
		}
		run.Status = RunParked
		run.LeaseOwner = ""
		if err := e.persist(ctx, run); err != nil {
			return nil, err
		}
		e.emit("system:nexaflow", "run_parked", run.DefinitionID, run.ID, false, "recovered after interrupted execution")
		parked = append(parked, run)
	}
	return parked, nil
}

// Prune removes terminal runs finished before each definition's retention
// (its Settings.RetentionDays, or the engine default when unset).
func (e *Engine) Prune(ctx context.Context) (int, error) {
	defs, err := e.store.ListPublished(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	now := e.now()
	for _, def := range defs {
		days := def.Settings.RetentionDays
		if days == 0 {
			days = e.opts.RetentionDays
		}
		n, err := e.store.PruneRunsFor(ctx, def.ID, now.AddDate(0, 0, -days))
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (e *Engine) finishSucceeded(ctx context.Context, run *Run, principal authz.Identity) (*Run, error) {
	run.Status = RunSucceeded
	run.LeaseOwner = ""
	fin := e.now()
	run.FinishedAt = &fin
	if err := e.persist(ctx, run); err != nil {
		return run, err
	}
	e.emit(principal.Subject, "run_finished", run.DefinitionID, run.ID, true, "status=succeeded")
	return run, nil
}

func (e *Engine) finishFailed(ctx context.Context, run *Run, principal authz.Identity, failure *NodeError) (*Run, error) {
	run.Status = RunFailed
	run.Error = failure
	run.LeaseOwner = ""
	fin := e.now()
	run.FinishedAt = &fin
	if err := e.persist(ctx, run); err != nil {
		return run, err
	}
	e.emit(principal.Subject, "run_finished", run.DefinitionID, run.ID, false, "status=failed code="+failure.Code)
	return run, nil
}

func (e *Engine) finishCancelled(ctx context.Context, run *Run, principal authz.Identity) (*Run, error) {
	run.Status = RunCancelled
	run.LeaseOwner = ""
	fin := e.now()
	run.FinishedAt = &fin
	for i := range run.Steps {
		if run.Steps[i].Status == StepRunning || run.Steps[i].Status == StepWaiting {
			run.Steps[i].Status = StepCancelled
			run.Steps[i].FinishedAt = &fin
		}
	}
	if err := e.persist(ctx, run); err != nil {
		return run, err
	}
	e.emit(principal.Subject, "run_finished", run.DefinitionID, run.ID, false, "status=cancelled")
	return run, nil
}
