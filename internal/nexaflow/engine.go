package nexaflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Engine bounds (ADR 0008 §D16 + Phase 2 safety limits). These bound
// what one definition or run may carry; the registry bounds node
// config/input/output separately.
const (
	// EngineMaxNodes bounds the node instances in one definition.
	EngineMaxNodes = 128
	// EngineMaxEdges bounds the edges in one definition.
	EngineMaxEdges = 512
	// EngineMaxDefinitionJSONBytes bounds the JSON encoding of a
	// definition handed to CreateRun.
	EngineMaxDefinitionJSONBytes = 1 << 20
	// EngineDefaultWorkers is the worker pool size per Execute call
	// (ADR 0008 §D16 default 16); EngineMaxWorkers caps it.
	EngineDefaultWorkers = 16
	EngineMaxWorkers     = 32
	// EngineDefaultLeaseTTL is the claim lease (ADR 0008 §D16: 30s).
	EngineDefaultLeaseTTL = 30 * time.Second
)

// Errors returned by the engine in addition to the store errors.
var (
	// ErrRunParked is returned by Execute for a run in
	// needs_manual_action: the engine will not resume it on its own.
	ErrRunParked = errors.New("nexaflow: run needs manual action")
	// ErrRunInFlight is returned when Execute is called for a run that
	// another Execute call is already driving.
	ErrRunInFlight = errors.New("nexaflow: run already being executed")
	// ErrNotWaiting is returned by DecideApproval when the node is not
	// currently waiting for a decision.
	ErrNotWaiting = errors.New("nexaflow: node is not waiting for approval")
)

// AuditEvent is one engine observation delivered to EngineOptions.Audit.
// It is observability only: the store remains the durable record, and
// a failing audit hook never changes run state.
type AuditEvent struct {
	RunID     string
	NodeID    string
	Kind      string // "node_started", "node_finished", "run_finished", "parked", "recovered"
	Attempt   int
	State     string
	ErrorCode string
}

// EngineOptions configures an Engine. Store and Registry are required;
// Backend may be nil when runs only use pure nodes (any node that
// needs a Backend fails closed with backend_unavailable from the
// registry path, exactly as a nil Backend is rejected by nodes that
// need one).
type EngineOptions struct {
	Store    Store
	Registry *Registry
	Backend  Backend

	// WorkerID identifies this engine's claims. Defaults to a random
	// "engine-<hex>" id; set it explicitly for multi-process use so
	// leases are attributable.
	WorkerID string
	// Workers bounds parallel node dispatch inside one Execute call.
	// 0 means EngineDefaultWorkers; values above EngineMaxWorkers are
	// capped, never rejected, so a misconfiguration cannot deadlock.
	Workers int
	// LeaseTTL is the claim lease. 0 means EngineDefaultLeaseTTL.
	LeaseTTL time.Duration
	// ApprovalTimeout bounds how long a waiting node may wait for a
	// decision. 0 means no expiry in this phase (P1.4 fixes the
	// number); a positive value is enforced at Execute entry and in
	// Recover as approval_expired.
	ApprovalTimeout time.Duration
	// Backoff is the delay before a retry attempt. Nil means no delay
	// (deterministic tests); production callers pass a small backoff.
	Backoff func(attempt int) time.Duration
	// Now is the clock, injected for tests. Nil means time.Now.
	Now func() time.Time
	// Audit, when set, receives engine observations. Nil is a no-op.
	Audit func(ev AuditEvent)
}

// Engine drives workflow runs on top of a Store and a Registry. It is
// safe for concurrent use; per-definition concurrency is gated (see
// semaphores) and per-run re-entrancy is rejected.
type Engine struct {
	store           Store
	registry        *Registry
	backend         Backend
	workerID        string
	workers         int
	leaseTTL        time.Duration
	approvalTimeout time.Duration
	backoff         func(attempt int) time.Duration
	now             func() time.Time
	audit           func(ev AuditEvent)

	mu         sync.Mutex
	cancels    map[string]context.CancelFunc
	inFlight   map[string]bool
	semaphores map[string]chan struct{}
}

// NewEngine validates options and returns a ready engine.
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("nexaflow: engine requires a Store")
	}
	if opts.Registry == nil {
		return nil, errors.New("nexaflow: engine requires a Registry")
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = EngineDefaultWorkers
	}
	if workers > EngineMaxWorkers {
		workers = EngineMaxWorkers
	}
	leaseTTL := opts.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = EngineDefaultLeaseTTL
	}
	workerID := opts.WorkerID
	if workerID == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("nexaflow: generate worker id: %w", err)
		}
		workerID = "engine-" + hex.EncodeToString(b[:])
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	backoff := opts.Backoff
	if backoff == nil {
		backoff = func(int) time.Duration { return 0 }
	}
	audit := opts.Audit
	if audit == nil {
		audit = func(AuditEvent) {}
	}
	return &Engine{
		store: opts.Store, registry: opts.Registry, backend: opts.Backend,
		workerID: workerID, workers: workers, leaseTTL: leaseTTL,
		approvalTimeout: opts.ApprovalTimeout, backoff: backoff,
		now: now, audit: audit,
		cancels:    map[string]context.CancelFunc{},
		inFlight:   map[string]bool{},
		semaphores: map[string]chan struct{}{},
	}, nil
}

func (e *Engine) semaphore(definitionID string, maxConcurrent int) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	sem, ok := e.semaphores[definitionID]
	if !ok {
		if maxConcurrent <= 0 {
			maxConcurrent = 1
		}
		sem = make(chan struct{}, maxConcurrent)
		e.semaphores[definitionID] = sem
	}
	return sem
}

// CreateRun validates a published definition snapshot, checks engine
// bounds, and durably creates a run pinned to that snapshot plus its
// initial pending node attempts. It never executes anything: Execute
// drives the run. An idempotency key, when given, deduplicates: a
// second CreateRun with the same key returns the original run, and a
// key reused for a different definition/revision is a conflict.
func (e *Engine) CreateRun(ctx context.Context, pub PublishedDefinition, principal authz.Identity, input map[string]any, idempotencyKey string) (Run, error) {
	def := pub.Definition
	if len(def.Nodes) > EngineMaxNodes {
		return Run{}, fmt.Errorf("nexaflow: definition exceeds %d nodes", EngineMaxNodes)
	}
	if len(def.Edges) > EngineMaxEdges {
		return Run{}, fmt.Errorf("nexaflow: definition exceeds %d edges", EngineMaxEdges)
	}
	if jsonLen(def) > EngineMaxDefinitionJSONBytes {
		return Run{}, fmt.Errorf("nexaflow: definition exceeds %d bytes", EngineMaxDefinitionJSONBytes)
	}
	if err := ValidateDefinition(&def, e.registry); err != nil {
		return Run{}, err
	}
	if input == nil {
		input = map[string]any{}
	}
	if jsonLen(input) > MaxRunInputBytes {
		return Run{}, fmt.Errorf("nexaflow: run input exceeds %d bytes", MaxRunInputBytes)
	}
	if idempotencyKey != "" {
		if existing, err := e.store.GetRunByIdempotency(ctx, idempotencyKey); err == nil {
			if existing.DefinitionID != pub.Version.DefinitionID || existing.Revision != pub.Version.Revision {
				return Run{}, ErrIdempotencyConflict
			}
			return existing, nil
		} else if !errors.Is(err, ErrNotFound) {
			return Run{}, err
		}
	}
	var idb [16]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return Run{}, fmt.Errorf("nexaflow: generate run id: %w", err)
	}
	run := Run{
		ID:             "run_" + hex.EncodeToString(idb[:]),
		DefinitionID:   pub.Version.DefinitionID,
		Revision:       pub.Version.Revision,
		Definition:     cloneDefinition(def),
		State:          RunCreated,
		IdempotencyKey: idempotencyKey,
		Principal:      principal.Subject,
		PrincipalRoles: append([]authz.Role(nil), principal.Roles...),
		Input:          deepCopyMap(input),
	}
	if err := e.store.CreateRun(ctx, run); err != nil {
		return Run{}, err
	}
	for _, nodeID := range TopologicalOrder(def) {
		if _, err := e.store.AppendNodeExecution(ctx, NodeExecution{
			RunID: run.ID, NodeID: nodeID, State: NodePending,
		}); err != nil {
			return Run{}, err
		}
	}
	return e.store.GetRun(ctx, run.ID)
}

// Execute drives one run to a terminal, waiting, or parked state. It
// is idempotent for terminal runs (returns the stored run) and refuses
// parked runs (ErrRunParked) and concurrent duplicate drives
// (ErrRunInFlight). A second Execute after DecideApproval resumes a
// waiting run.
func (e *Engine) Execute(ctx context.Context, runID string) (Run, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if run.State.Terminal() {
		return run, nil
	}
	if run.State == RunNeedsManualAction {
		return run, ErrRunParked
	}

	e.mu.Lock()
	if e.inFlight[runID] {
		e.mu.Unlock()
		return run, ErrRunInFlight
	}
	e.inFlight[runID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.inFlight, runID)
		e.mu.Unlock()
	}()

	maxConcurrent := run.Definition.Settings.MaxConcurrentRuns
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	sem := e.semaphore(run.DefinitionID, maxConcurrent)
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		return run, ErrDefinitionBusy
	}

	runCtx, cancel := context.WithCancel(ctx)
	if secs := run.Definition.Settings.RunTimeoutSeconds; secs > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(runCtx, time.Duration(secs)*time.Second)
		defer cancelTimeout()
	}
	e.mu.Lock()
	e.cancels[runID] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, runID)
		e.mu.Unlock()
		cancel()
	}()

	// Approval expiry is enforced at entry, before any dispatch.
	if err := e.expireApprovals(ctx, run); err != nil {
		return run, err
	}
	run, err = e.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if run.State.Terminal() {
		return run, nil
	}

	switch run.State {
	case RunCreated:
		run, err = e.store.UpdateRunState(ctx, runID, RunCreated, RunRunning, "", "")
	case RunWaitingForApproval:
		run, err = e.store.UpdateRunState(ctx, runID, RunWaitingForApproval, RunRunning, "", "")
	}
	if err != nil {
		return Run{}, err
	}

	// Sweep until a full pass makes no progress: each pass dispatches
	// every ready node, and newly succeeded nodes unblock their
	// successors in the next pass.
	for {
		progress, err := e.sweep(runCtx, run)
		if err != nil {
			if runCtx.Err() != nil {
				return e.finishRun(runCtx, run)
			}
			return run, err
		}
		run, err = e.store.GetRun(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		if run.State.Terminal() {
			return run, nil
		}
		if !progress {
			break
		}
		if runCtx.Err() != nil {
			break
		}
	}

	return e.finishRun(runCtx, run)
}

// expireApprovals fails waiting nodes whose approval deadline passed,
// with code approval_expired, and fails the run when every node is
// terminal and at least one failed. It never touches live leases.
func (e *Engine) expireApprovals(ctx context.Context, run Run) error {
	if e.approvalTimeout <= 0 {
		return nil
	}
	nodes, err := e.store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		return err
	}
	now := e.now()
	changed := false
	for _, cur := range currentAttempts(nodes) {
		if cur.State != NodeWaiting || cur.Deadline.IsZero() || !now.After(cur.Deadline) {
			continue
		}
		next := cur
		next.State = NodeFailed
		next.ErrorCode = "approval_expired"
		next.Error = "approval deadline expired"
		if _, err := e.store.SaveNodeExecution(ctx, next, NodeWaiting); err != nil {
			if errors.Is(err, ErrStateConflict) || errors.Is(err, ErrTerminalState) {
				continue
			}
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return e.recomputeRunState(ctx, run.ID)
}

// recomputeRunState derives the run state from its current node
// attempts without dispatching anything. It is the shared tail of
// finishRun and Recover: parked dominates, then failed, then waiting,
// then succeeded when every node is terminal.
func (e *Engine) recomputeRunState(ctx context.Context, runID string) error {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State.Terminal() {
		return nil
	}
	nodes, err := e.store.GetNodeExecutions(ctx, runID)
	if err != nil {
		return err
	}
	current := currentAttempts(nodes)
	outputs := map[string]map[string]any{}
	for _, cur := range current {
		if cur.State == NodeSucceeded {
			outputs[cur.NodeID] = deepCopyMap(cur.Output)
		}
	}
	anyParked, anyFailed, anyWaiting := false, false, false
	allTerminal := true
	hasRunnable := false
	var firstFailed *NodeExecution
	for _, cur := range current {
		switch cur.State {
		case NodeNeedsManualAction:
			anyParked = true
		case NodeFailed:
			anyFailed = true
			if firstFailed == nil {
				c := cur
				firstFailed = &c
			}
		case NodeWaiting:
			anyWaiting = true
		case NodeClaimed, NodeRunning:
			// Live or foreign-owned work: the run is not settled.
			hasRunnable = true
		case NodePending:
			// A pending node that is ready, or would skip on the
			// next sweep, is unsettled work. A pending node blocked
			// behind a waiting node is not: the run is waiting.
			ready, skip := e.nodeReadiness(run.Definition, cur.NodeID, current, outputs)
			if ready || skip {
				hasRunnable = true
			}
		}
		if !cur.State.Terminal() {
			allTerminal = false
		}
	}
	switch {
	case anyParked:
		_, err = e.store.UpdateRunState(ctx, runID, "", RunNeedsManualAction, "needs_manual_action", "run parked: an ambiguous node outcome needs operator recovery")
		return err
	case anyFailed && allTerminal:
		code, msg := "node_failed", "a node failed"
		if firstFailed != nil {
			code, msg = firstFailed.ErrorCode, firstFailed.Error
		}
		_, err = e.store.UpdateRunState(ctx, runID, "", RunFailed, code, msg)
		return err
	case anyWaiting && !hasRunnable:
		_, err = e.store.UpdateRunState(ctx, runID, "", RunWaitingForApproval, "", "")
		return err
	case allTerminal:
		_, err = e.store.UpdateRunState(ctx, runID, "", RunSucceeded, "", "")
		return err
	default:
		return nil
	}
}

// finishRun settles the run after sweeping stops: cancelled context
// cancels the run; otherwise the state is recomputed from node
// attempts. A run with a node still claimed by a live foreign lease
// stays running.
func (e *Engine) finishRun(ctx context.Context, run Run) (Run, error) {
	if ctx.Err() != nil {
		// The run context is cancelled (CancelRun or the run
		// deadline): store calls below must not inherit it.
		storeCtx := context.WithoutCancel(ctx)
		if _, err := e.store.UpdateRunState(storeCtx, run.ID, "", RunCancelled, "cancelled", "run cancelled"); err != nil && !errors.Is(err, ErrTerminalState) && !errors.Is(err, ErrStateConflict) {
			return run, err
		}
		return e.store.GetRun(storeCtx, run.ID)
	}
	if err := e.recomputeRunState(ctx, run.ID); err != nil {
		return run, err
	}
	final, err := e.store.GetRun(ctx, run.ID)
	if err != nil {
		return run, err
	}
	e.audit(AuditEvent{RunID: run.ID, Kind: "run_finished", State: string(final.State), ErrorCode: final.ErrorCode})
	return final, nil
}

// sweep dispatches every ready node once and applies skip decisions.
// It returns true when any node changed state (progress), so Execute
// loops until quiescence.
func (e *Engine) sweep(ctx context.Context, run Run) (bool, error) {
	nodes, err := e.store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		return false, err
	}
	current := currentAttempts(nodes)
	outputs := map[string]map[string]any{}
	for _, cur := range current {
		if cur.State == NodeSucceeded {
			outputs[cur.NodeID] = deepCopyMap(cur.Output)
		}
	}
	// Apply approval decisions to waiting nodes first: a decided node
	// becomes pending and can be dispatched in this same sweep.
	for _, cur := range current {
		if cur.State != NodeWaiting {
			continue
		}
		appr, err := e.store.GetApproval(ctx, run.ID, cur.NodeID)
		if err != nil {
			if errors.Is(err, ErrNoApprovalRecord) {
				continue
			}
			return false, err
		}
		next := cur
		next.ResumeInput = map[string]any{
			"decision":  appr.Decision,
			"approver":  appr.Approver,
			"requester": appr.Requester,
		}
		next.State = NodePending
		next.Deadline = time.Time{}
		if _, err := e.store.SaveNodeExecution(ctx, next, NodeWaiting); err != nil {
			if errors.Is(err, ErrStateConflict) || errors.Is(err, ErrTerminalState) {
				continue
			}
			return false, err
		}
		current[cur.NodeID] = next
	}
	progress := false
	// Dispatch ready nodes through the worker pool.
	var jobs []engineJob
	for _, nodeID := range TopologicalOrder(run.Definition) {
		cur := current[nodeID]
		if cur.State.Terminal() || cur.State == NodeWaiting || cur.State == NodeClaimed || cur.State == NodeRunning {
			continue
		}
		ready, skip := e.nodeReadiness(run.Definition, nodeID, current, outputs)
		switch {
		case skip:
			next := cur
			next.State = NodeSkipped
			if _, err := e.store.SaveNodeExecution(ctx, next, NodePending); err != nil {
				if errors.Is(err, ErrStateConflict) || errors.Is(err, ErrTerminalState) {
					continue
				}
				return false, err
			}
			current[nodeID] = next
			progress = true
		case ready:
			claimed, ok, err := e.store.TryClaim(ctx, run.ID, nodeID, e.workerID, e.leaseTTL, e.now())
			if err != nil {
				return false, err
			}
			if !ok {
				continue // a live foreign lease owns this node
			}
			node := nodeDefByID(run.Definition, nodeID)
			spec, err := e.registry.Lookup(node.NodeID, node.Version)
			if err != nil {
				// Unknown node: the definition passed validation at
				// creation, so this is a registry change since then.
				// Fail the node rather than dispatching blindly.
				next := claimed
				next.State = NodeFailed
				next.ErrorCode = "node_not_found"
				next.Error = err.Error()
				if _, err := e.store.SaveNodeExecution(ctx, next, NodeClaimed); err != nil {
					return false, err
				}
				current[nodeID] = next
				progress = true
				continue
			}
			input, ierr := e.nodeInput(run, node, spec, outputs, current[nodeID])
			if ierr != nil {
				// Unresolvable mapping: fail the node without
				// dispatching it, never with a guessed input.
				next := claimed
				next.State = NodeFailed
				next.ErrorCode = "input_resolution"
				next.Error = ierr.Error()
				if _, err := e.store.SaveNodeExecution(ctx, next, NodeClaimed); err != nil {
					return false, err
				}
				current[nodeID] = next
				progress = true
				continue
			}
			jobs = append(jobs, engineJob{node: node, spec: spec, exec: claimed, input: input})
		}
	}
	if len(jobs) > 0 {
		progress = true
		e.runJobs(ctx, run, jobs, current, outputs)
	}
	return progress, nil
}

// nodeReadiness implements the AND-join: a node is ready only when
// every incoming edge fired and its source succeeded. It is skipped
// when every incoming edge is resolved but at least one resolved to a
// non-success (failed/cancelled/skipped source, or an unfired labelled
// edge). Otherwise it is not ready yet.
func (e *Engine) nodeReadiness(def Definition, nodeID string, current map[string]NodeExecution, outputs map[string]map[string]any) (ready, skip bool) {
	incoming := incomingEdges(def, nodeID)
	if len(incoming) == 0 {
		return true, false // trigger node
	}
	skipVote := false
	for _, edge := range incoming {
		src, ok := current[edge.From]
		if !ok {
			return false, false
		}
		if !edgeFired(edge, outputs) {
			// The source succeeded on another branch (or failed and
			// produced no output): this edge carries nothing.
			if src.State.Terminal() {
				skipVote = true
				continue
			}
			return false, false
		}
		switch src.State {
		case NodeSucceeded:
			continue
		case NodeFailed, NodeCancelled, NodeSkipped, NodeNeedsManualAction:
			skipVote = true
		default:
			return false, false
		}
	}
	if skipVote {
		return false, true
	}
	return true, false
}

// nodeInput builds one node's input: trigger nodes receive the run
// input verbatim; a node with an Input mapping receives exactly its
// mapped input (projection; see input_mapping.go); other nodes receive
// the merge of their direct predecessors' outputs in edge-declaration
// order, last-wins, plus any approval resume input on top (the
// approval decision wins over a stale merged value). A mapping that
// cannot be resolved returns an error; the caller fails the node with
// input_resolution instead of dispatching it.
// overlayResumeInput applies an approval decision on top of a node's
// input, but only for keys the node's InputSchema declares. Without
// this projection, resuming a policy-gated node (llm.chat,
// video.submit) after approval poisoned its input with
// decision/approver/requester keys that its schema rejects, so the
// node failed validation on every approved resume — the policy gate
// could park but never release. The approval node declares all three
// keys, so its behaviour is unchanged; every other node receives only
// what its schema allows. A nil spec overlays everything (legacy
// fallback for specs the registry cannot describe).
func overlayResumeInput(dst map[string]any, spec *NodeSpec, resume map[string]any) {
	if len(resume) == 0 {
		return
	}
	if spec == nil {
		for k, v := range resume {
			dst[k] = v
		}
		return
	}
	known := make(map[string]bool, len(spec.InputSchema))
	for _, f := range spec.InputSchema {
		known[f.Name] = true
	}
	for k, v := range resume {
		if known[k] {
			dst[k] = v
		}
	}
}

func (e *Engine) nodeInput(run Run, node DefinitionNode, spec *NodeSpec, outputs map[string]map[string]any, cur NodeExecution) (map[string]any, error) {
	if spec != nil && spec.Category == "trigger" {
		merged := deepCopyMap(run.Input)
		overlayResumeInput(merged, spec, cur.ResumeInput)
		return merged, nil
	}
	if spec != nil && spec.ID == approvalNodeID && node.Input == nil {
		// Approval decisions come only from the approval record
		// (see input_mapping.go): never merge predecessor outputs
		// into an approval node, where a transform literal could
		// smuggle a decision past DecideApproval.
		mapped := map[string]any{}
		overlayResumeInput(mapped, spec, cur.ResumeInput)
		return mapped, nil
	}
	if node.Input != nil {
		mapped, err := mappedNodeInput(run, node, outputs)
		if err != nil {
			return nil, err
		}
		overlayResumeInput(mapped, spec, cur.ResumeInput)
		return mapped, nil
	}
	merged := map[string]any{}
	for _, edge := range incomingEdges(run.Definition, node.ID) {
		if !edgeFired(edge, outputs) {
			continue
		}
		for k, v := range outputs[edge.From] {
			merged[k] = deepCopyValue(v)
		}
	}
	overlayResumeInput(merged, spec, cur.ResumeInput)
	return merged, nil
}

// runJobs executes claimed nodes through a bounded worker pool and
// records each outcome. Outcomes are recorded sequentially after the
// pool drains, so store writes never race each other.
// engineJob is one claimed node ready for dispatch.
type engineJob struct {
	node  DefinitionNode
	spec  *NodeSpec
	exec  NodeExecution
	input map[string]any
}

func (e *Engine) runJobs(ctx context.Context, run Run, jobs []engineJob, current map[string]NodeExecution, outputs map[string]map[string]any) {
	sem := make(chan struct{}, e.workers)
	var wg sync.WaitGroup
	type result struct {
		job engineJob
		out map[string]any
		err error
	}
	results := make([]result, len(jobs))
	for i, jb := range jobs {
		wg.Add(1)
		go func(i int, jb engineJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e.audit(AuditEvent{RunID: run.ID, NodeID: jb.node.ID, Kind: "node_started", Attempt: jb.exec.Attempt, State: string(NodeRunning)})
			running := jb.exec
			running.State = NodeRunning
			running.Dispatched = true
			if _, err := e.store.SaveNodeExecution(ctx, running, NodeClaimed); err != nil {
				results[i] = result{job: jb, err: err}
				return
			}
			identity := authz.Identity{Subject: run.Principal, Roles: run.PrincipalRoles}
			policyApproved := false
			if appr, err := e.store.GetApproval(ctx, run.ID, jb.node.ID); err == nil && appr.Decision == "approved" {
				policyApproved = true
			}
			config := triggerConfig(run.Definition, jb.node, jb.spec)
			out, err := e.registry.Execute(ctx, jb.spec, identity, jb.input, config, e.backend, policyApproved)
			results[i] = result{job: jb, out: out, err: err}
		}(i, jb)
	}
	wg.Wait()
	for _, res := range results {
		e.recordOutcome(ctx, run, res.job, res.out, res.err, current, outputs)
	}
}

// recordOutcome classifies one node result and persists it, following
// ADR 0008 §D6. Retryable codes in the node's policy, with attempts
// left, append a fresh pending attempt. A timeout on a billable,
// external, or irreversible node parks the node as ambiguous (the
// upstream call may already have executed). Approval and policy gates
// park the node in waiting. Everything else with a definite outcome
// fails the node. A persist failure after a successful execution parks
// the node as result_not_persisted rather than reporting success the
// store cannot prove.
func (e *Engine) recordOutcome(ctx context.Context, run Run, jb engineJob, out map[string]any, err error, current map[string]NodeExecution, outputs map[string]map[string]any) {
	// Refresh the attempt row: the pool wrote running already.
	cur, gerr := e.store.GetNodeExecutions(ctx, run.ID)
	if gerr == nil {
		for _, c := range cur {
			if c.NodeID == jb.node.ID && c.Attempt == jb.exec.Attempt {
				current[jb.node.ID] = c
			}
		}
	}
	exec := current[jb.node.ID]
	// finish persists an outcome against the refreshed row state, so a
	// concurrent CancelRun that already moved the node to cancelled
	// does not turn into a spurious persist-failure park.
	finish := func(next NodeExecution, expected NodeState) {
		saved, serr := e.store.SaveNodeExecution(ctx, next, expected)
		if serr != nil {
			e.parkPersistFailure(ctx, run, exec)
			return
		}
		current[jb.node.ID] = saved
		if saved.State == NodeSucceeded {
			outputs[jb.node.ID] = deepCopyMap(saved.Output)
		}
		e.audit(AuditEvent{RunID: run.ID, NodeID: jb.node.ID, Kind: "node_finished", Attempt: saved.Attempt, State: string(saved.State), ErrorCode: saved.ErrorCode})
	}
	if err == nil {
		// Canonical loop rule (Task 8): a loop_plan that dropped tail
		// items is a failure, never a silently partial iteration.
		if jb.spec != nil && jb.spec.ID == "nexaroute.control.loop_plan" {
			if tr, _ := out["truncated"].(bool); tr {
				next := exec
				next.State = NodeFailed
				next.ErrorCode = "loop_truncated"
				next.Error = "loop_plan dropped tail items; iteration would be partial"
				finish(next, exec.State)
				return
			}
		}
		cleaned, truncated := sanitizeNodeOutput(out)
		next := exec
		next.State = NodeSucceeded
		next.Output = cleaned
		next.OutputTruncated = truncated
		next.ErrorCode = ""
		next.Error = ""
		finish(next, exec.State)
		return
	}
	var nerr *NodeError
	code := "internal_error"
	retryable := false
	if errors.As(err, &nerr) {
		code, retryable = nerr.Code, nerr.Retryable
	}
	msg := err.Error()
	switch code {
	case "cancelled":
		next := exec
		next.State = NodeCancelled
		next.ErrorCode = code
		next.Error = msg
		finish(next, exec.State)
		return
	case "approval_required", "policy_required":
		next := exec
		next.State = NodeWaiting
		next.ErrorCode = code
		next.Error = msg
		if e.approvalTimeout > 0 {
			next.Deadline = e.now().Add(e.approvalTimeout)
		}
		finish(next, exec.State)
		return
	}
	spec := jb.spec
	if retryable && spec != nil && codeIn(spec.Retry.RetryableCodes, code) && exec.Attempt+1 < spec.Retry.MaxAttempts {
		if d := e.backoff(exec.Attempt); d > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
		appended, aerr := e.store.AppendNodeExecution(ctx, NodeExecution{
			RunID: run.ID, NodeID: jb.node.ID, State: NodePending,
		})
		if aerr != nil {
			e.parkPersistFailure(ctx, run, exec)
			return
		}
		// Mark the failed attempt terminal so the sweep does not
		// re-dispatch it; the new attempt is the current one.
		next := exec
		next.State = NodeFailed
		next.ErrorCode = code
		next.Error = msg
		if _, serr := e.store.SaveNodeExecution(ctx, next, exec.State); serr != nil {
			e.parkPersistFailure(ctx, run, exec)
			return
		}
		current[jb.node.ID] = appended
		return
	}
	if code == "timeout" && spec != nil &&
		(spec.SideEffect == SideEffectBillable || spec.SideEffect == SideEffectExternal || spec.SideEffect == SideEffectIrreversible) {
		next := exec
		next.State = NodeNeedsManualAction
		next.Ambiguous = true
		next.ErrorCode = "ambiguous_timeout"
		next.Error = "node timed out; the external outcome is unknown and the node must not be auto-retried"
		finish(next, exec.State)
		e.audit(AuditEvent{RunID: run.ID, NodeID: jb.node.ID, Kind: "parked", Attempt: exec.Attempt, State: string(NodeNeedsManualAction), ErrorCode: "ambiguous_timeout"})
		return
	}
	next := exec
	next.State = NodeFailed
	next.ErrorCode = code
	next.Error = msg
	finish(next, exec.State)
}

// parkPersistFailure records that a node executed but its outcome
// could not be persisted. The run is parked, never reported as
// succeeded on unproven state.
func (e *Engine) parkPersistFailure(ctx context.Context, run Run, exec NodeExecution) {
	next := exec
	next.State = NodeNeedsManualAction
	next.Ambiguous = true
	next.ErrorCode = "result_not_persisted"
	next.Error = "node outcome could not be persisted; the execution result is unknown to the store"
	if _, err := e.store.SaveNodeExecution(ctx, next, ""); err != nil {
		// Even the park failed: surface it through the audit hook and
		// leave the attempt row as the store has it. The caller still
		// sees an error from the sweep path.
		e.audit(AuditEvent{RunID: run.ID, NodeID: exec.NodeID, Kind: "parked", Attempt: exec.Attempt, State: string(NodeNeedsManualAction), ErrorCode: "result_not_persisted"})
		return
	}
	e.audit(AuditEvent{RunID: run.ID, NodeID: exec.NodeID, Kind: "parked", Attempt: exec.Attempt, State: string(NodeNeedsManualAction), ErrorCode: "result_not_persisted"})
}

// DecideApproval records an operator decision for a waiting node and
// re-queues it. The approver must differ from the run's principal
// (separation of duties, ADR 0008 §D12); a rejected policy gate fails
// the node instead of executing it, while a rejected approval node is
// re-run with the decision as input so downstream logic can react.
func (e *Engine) DecideApproval(ctx context.Context, runID, nodeID, approver, decision string) (Approval, error) {
	if decision != "approved" && decision != "rejected" {
		return Approval{}, errors.New("nexaflow: approval decision must be approved or rejected")
	}
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return Approval{}, err
	}
	if approver != "" && run.Principal != "" && approver == run.Principal {
		return Approval{}, errors.New("nexaflow: approver must not be the requester")
	}
	nodes, err := e.store.GetNodeExecutions(ctx, runID)
	if err != nil {
		return Approval{}, err
	}
	cur := currentAttempts(nodes)[nodeID]
	if cur.State != NodeWaiting {
		return Approval{}, ErrNotWaiting
	}
	appr, err := e.store.RecordApproval(ctx, Approval{
		RunID: runID, NodeID: nodeID, Requester: run.Principal, Approver: approver, Decision: decision,
	})
	if err != nil {
		return Approval{}, err
	}
	node := nodeDefByID(run.Definition, nodeID)
	spec, serr := e.registry.Lookup(node.NodeID, node.Version)
	if decision == "rejected" && serr == nil && spec.RequiresOperatorPolicy {
		next := cur
		next.State = NodeFailed
		next.ErrorCode = "approval_rejected"
		next.Error = "operator policy approval rejected"
		if _, err := e.store.SaveNodeExecution(ctx, next, NodeWaiting); err != nil {
			return Approval{}, err
		}
		_ = e.recomputeRunState(ctx, runID)
		return appr, nil
	}
	next := cur
	next.State = NodePending
	next.Deadline = time.Time{}
	next.ResumeInput = map[string]any{
		"decision":  decision,
		"approver":  approver,
		"requester": run.Principal,
	}
	if _, err := e.store.SaveNodeExecution(ctx, next, NodeWaiting); err != nil {
		return Approval{}, err
	}
	if run.State == RunWaitingForApproval {
		_, _ = e.store.UpdateRunState(ctx, runID, RunWaitingForApproval, RunRunning, "", "")
	}
	return appr, nil
}

// CancelRun cancels a run: the in-flight Execute context (if any) is
// cancelled, the run moves to cancelled, and every non-terminal node
// attempt is cancelled best-effort. Terminal runs are returned
// unchanged.
func (e *Engine) CancelRun(ctx context.Context, runID string) (Run, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if run.State.Terminal() {
		return run, nil
	}
	e.mu.Lock()
	if cancel, ok := e.cancels[runID]; ok {
		cancel()
	}
	e.mu.Unlock()
	nodes, err := e.store.GetNodeExecutions(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	for _, cur := range currentAttempts(nodes) {
		if cur.State.Terminal() {
			continue
		}
		next := cur
		next.State = NodeCancelled
		next.ErrorCode = "cancelled"
		next.Error = "run cancelled"
		_, _ = e.store.SaveNodeExecution(ctx, next, "")
	}
	updated, err := e.store.UpdateRunState(ctx, runID, "", RunCancelled, "cancelled", "run cancelled")
	if err != nil {
		if errors.Is(err, ErrTerminalState) || errors.Is(err, ErrStateConflict) {
			return e.store.GetRun(ctx, runID)
		}
		return Run{}, err
	}
	return updated, nil
}

// ForceFailRun moves a parked (or otherwise non-terminal) run to
// failed with an operator-supplied reason. It never revives a
// terminal run and never resumes a parked one.
func (e *Engine) ForceFailRun(ctx context.Context, runID, code, msg string) (Run, error) {
	if code == "" {
		code = "operator_force_failed"
	}
	updated, err := e.store.UpdateRunState(ctx, runID, "", RunFailed, code, msg)
	if err != nil {
		return Run{}, err
	}
	return updated, nil
}

// Recover reconciles runs interrupted by a process restart. It never
// touches live leases or waiting runs (except expired approval
// deadlines): a claimed attempt whose lease expired and which never
// dispatched is re-queued; a dispatched attempt on a none/read node is
// re-queued; a dispatched attempt on an external/billable/irreversible
// node is parked as ambiguous_recovery, because its outcome is
// unprovable. A run left running with every node terminal and no live
// lease is settled from its node states.
func (e *Engine) Recover(ctx context.Context) error {
	runs, err := e.store.ListRuns(ctx, RunFilter{})
	if err != nil {
		return err
	}
	now := e.now()
	for _, run := range runs {
		if run.State.Terminal() || run.State == RunNeedsManualAction || run.State == RunCreated {
			continue
		}
		nodes, err := e.store.GetNodeExecutions(ctx, run.ID)
		if err != nil {
			return err
		}
		current := currentAttempts(nodes)
		changed := false
		anyLiveLease := false
		for _, cur := range current {
			switch cur.State {
			case NodeWaiting:
				if e.approvalTimeout > 0 && !cur.Deadline.IsZero() && now.After(cur.Deadline) {
					next := cur
					next.State = NodeFailed
					next.ErrorCode = "approval_expired"
					next.Error = "approval deadline expired"
					if _, err := e.store.SaveNodeExecution(ctx, next, NodeWaiting); err == nil {
						changed = true
					}
				}
			case NodeClaimed, NodeRunning:
				if cur.LeaseOwner != "" && !cur.LeaseUntil.IsZero() && cur.LeaseUntil.After(now) {
					anyLiveLease = true
					continue
				}
				if !cur.Dispatched {
					next := cur
					next.State = NodePending
					next.LeaseOwner = ""
					next.LeaseUntil = time.Time{}
					if _, err := e.store.SaveNodeExecution(ctx, next, cur.State); err == nil {
						changed = true
					}
					continue
				}
				node := nodeDefByID(run.Definition, cur.NodeID)
				spec, serr := e.registry.Lookup(node.NodeID, node.Version)
				if serr == nil && (spec.SideEffect == SideEffectNone || spec.SideEffect == SideEffectRead) {
					next := cur
					next.State = NodePending
					next.Dispatched = false
					next.LeaseOwner = ""
					next.LeaseUntil = time.Time{}
					if _, err := e.store.SaveNodeExecution(ctx, next, cur.State); err == nil {
						changed = true
					}
					continue
				}
				next := cur
				next.State = NodeNeedsManualAction
				next.Ambiguous = true
				next.ErrorCode = "ambiguous_recovery"
				next.Error = "run interrupted after dispatch; the external outcome is unknown"
				next.LeaseOwner = ""
				next.LeaseUntil = time.Time{}
				if _, err := e.store.SaveNodeExecution(ctx, next, cur.State); err == nil {
					changed = true
					e.audit(AuditEvent{RunID: run.ID, NodeID: cur.NodeID, Kind: "recovered", Attempt: cur.Attempt, State: string(NodeNeedsManualAction), ErrorCode: "ambiguous_recovery"})
				}
			}
		}
		if changed && !anyLiveLease {
			_ = e.recomputeRunState(ctx, run.ID)
		}
	}
	return nil
}

// currentAttempts collapses attempt rows to the highest attempt per
// node id.
func currentAttempts(all []NodeExecution) map[string]NodeExecution {
	out := map[string]NodeExecution{}
	for _, ex := range all {
		if cur, ok := out[ex.NodeID]; !ok || ex.Attempt > cur.Attempt {
			out[ex.NodeID] = ex
		}
	}
	return out
}

func codeIn(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}
