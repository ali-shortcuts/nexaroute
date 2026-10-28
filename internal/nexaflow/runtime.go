package nexaflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Service is the workflow runtime: workflow CRUD, versioning, and
// sequential DAG execution over the canonical registry. Execution is
// sequential in topological order (v1): deterministic, easy to reason
// about, and sufficient for the editor's run view. Parallel execution
// is a future phase, not a silent behaviour change.
//
// Durability: runs are persisted to the Store after every node
// transition. A run interrupted mid-node by a gateway restart is marked
// failed on the next load (its node state is unknown, so it is never
// silently resumed). A run paused at an approval gate survives a
// restart: approving or cancelling it after restart resumes or ends it
// from the persisted record.
//
// Approval semantics: an approval node with no decision pauses the run
// (status awaiting_approval). Approving continues execution; rejecting
// continues with approved=false (the node succeeds with that value —
// downstream nodes decide what a rejection means). Cancelling ends the
// run. A rejected approval is NOT a run failure.

type approvalDecision struct {
	decision string
	approver string
}

type activeRun struct {
	mu           sync.Mutex
	cancel       context.CancelFunc
	approve      chan approvalDecision
	done         chan struct{}
	seedDecision *approvalDecision
}

// Service executes workflows. The zero Backend is allowed: pure nodes
// run, backend-backed nodes fail closed with a backend error.
type Service struct {
	reg     *Registry
	backend Backend
	store   *Store

	mu      sync.Mutex
	actives map[string]*activeRun
	deleted map[string]bool
}

// NewService loads the store and reconciles interrupted runs: runs
// left in "running" by a previous process are failed with a restart
// error, because their in-flight node state is unknown. Runs left in
// "awaiting_approval" are kept: they resume on approve/cancel.
func NewService(reg *Registry, backend Backend, store *Store) (*Service, error) {
	if reg == nil {
		return nil, errors.New("nexaflow: nil registry")
	}
	if store == nil {
		return nil, errors.New("nexaflow: nil store")
	}
	if err := store.Load(); err != nil {
		return nil, err
	}
	s := &Service{reg: reg, backend: backend, store: store, actives: map[string]*activeRun{}, deleted: map[string]bool{}}
	// Reconcile: find running runs across all workflows.
	workflows, err := store.ListWorkflows()
	if err != nil {
		return nil, err
	}
	for _, w := range workflows {
		runs, err := store.ListRuns(w.ID, 0)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			if r.Status == RunRunning || r.Status == RunPending {
				r.Status = RunFailed
				r.Error = "interrupted by gateway restart"
				now := time.Now()
				r.FinishedAt = &now
				for _, nr := range r.Nodes {
					if nr.Status == RunRunning {
						nr.Status = RunFailed
						nr.Error = "interrupted by gateway restart"
						nr.ErrorCode = "interrupted"
						nr.FinishedAt = &now
					}
				}
				if err := store.PutRun(r); err != nil {
					return nil, err
				}
			}
		}
	}
	return s, nil
}

func (s *Service) Registry() *Registry { return s.reg }
func (s *Service) Store() *Store       { return s.store }

func cloneRun(r *Run) *Run {
	if r == nil {
		return nil
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	var out Run
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}

// GetRun returns a detached copy of a run (safe to read while the
// engine mutates the live record).
func (s *Service) GetRun(id string) (*Run, error) {
	live, err := s.store.GetRun(id)
	if err != nil || live == nil {
		return live, err
	}
	s.mu.Lock()
	active := s.actives[id]
	s.mu.Unlock()
	if active != nil {
		active.mu.Lock()
		defer active.mu.Unlock()
		return cloneRun(live), nil
	}
	// No live goroutine: hold the service lock for the clone so a
	// concurrent approve/cancel registration cannot race the copy.
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRun(live), nil
}

// ListRuns returns detached copies, newest first.
func (s *Service) ListRuns(workflowID string, limit int) ([]*Run, error) {
	live, err := s.store.ListRuns(workflowID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*Run, 0, len(live))
	for _, r := range live {
		if c, err := s.GetRun(r.ID); err == nil && c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Service) nameTaken(name, exceptID string) (bool, error) {
	workflows, err := s.store.ListWorkflows()
	if err != nil {
		return false, err
	}
	for _, w := range workflows {
		if w.ID != exceptID && w.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// CreateWorkflow creates an enabled workflow with an empty draft.
func (s *Service) CreateWorkflow(name string, principal authz.Identity) (*Workflow, error) {
	if err := ValidateWorkflowName(name); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if taken, err := s.nameTaken(name, ""); err != nil {
		return nil, err
	} else if taken {
		return nil, fmt.Errorf("workflow name %q already exists", name)
	}
	id, err := newID("wf_")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	w := &Workflow{
		ID: id, Name: name, Enabled: true,
		Draft:     Graph{},
		CreatedBy: principal.Subject, UpdatedBy: principal.Subject,
		CreatedAt: now, UpdatedAt: now, DraftUpdatedAt: now,
	}
	if err := s.store.PutWorkflow(w); err != nil {
		return nil, err
	}
	return w, nil
}

// RenameWorkflow renames a workflow (draft and versions untouched).
func (s *Service) RenameWorkflow(id, name string, principal authz.Identity) (*Workflow, error) {
	if err := ValidateWorkflowName(name); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	w, err := s.store.GetWorkflow(id)
	if err != nil || w == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", id)
		}
		return nil, err
	}
	if taken, err := s.nameTaken(name, id); err != nil {
		return nil, err
	} else if taken {
		return nil, fmt.Errorf("workflow name %q already exists", name)
	}
	w.Name = name
	w.UpdatedBy = principal.Subject
	w.UpdatedAt = time.Now()
	if err := s.store.PutWorkflow(w); err != nil {
		return nil, err
	}
	return w, nil
}

// DuplicateWorkflow copies the draft under a new name. Versions and
// runs are not copied: the duplicate starts unpublished.
func (s *Service) DuplicateWorkflow(id string, principal authz.Identity) (*Workflow, error) {
	src, err := s.store.GetWorkflow(id)
	if err != nil || src == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", id)
		}
		return nil, err
	}
	base := src.Name + " (copy)"
	name := base
	for i := 2; i < 100; i++ {
		if taken, err := s.nameTaken(name, ""); err != nil {
			return nil, err
		} else if !taken {
			break
		}
		name = fmt.Sprintf("%s %d", base, i)
	}
	newID, err := newID("wf_")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	w := &Workflow{
		ID: newID, Name: name, Enabled: true,
		Draft:     CloneGraph(src.Draft),
		CreatedBy: principal.Subject, UpdatedBy: principal.Subject,
		CreatedAt: now, UpdatedAt: now, DraftUpdatedAt: now,
	}
	if err := s.store.PutWorkflow(w); err != nil {
		return nil, err
	}
	return w, nil
}

// UpdateDraft replaces the draft graph. Drafts may be invalid: the
// editor saves work in progress, and validation runs on demand, at
// publish, and at run start. Node positions are preserved verbatim.
func (s *Service) UpdateDraft(id string, draft Graph, principal authz.Identity) (*Workflow, error) {
	w, err := s.store.GetWorkflow(id)
	if err != nil || w == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", id)
		}
		return nil, err
	}
	if len(draft.Nodes) > MaxGraphNodes || len(draft.Edges) > MaxGraphEdges {
		return nil, fmt.Errorf("graph exceeds size limits (%d nodes, %d edges)", MaxGraphNodes, MaxGraphEdges)
	}
	w.Draft = draft
	w.UpdatedBy = principal.Subject
	w.UpdatedAt = time.Now()
	w.DraftUpdatedAt = time.Now()
	if err := s.store.PutWorkflow(w); err != nil {
		return nil, err
	}
	return w, nil
}

// SetEnabled enables or disables a workflow. Disabling blocks new runs;
// in-flight runs continue to completion, cancellation, or approval.
func (s *Service) SetEnabled(id string, enabled bool, principal authz.Identity) (*Workflow, error) {
	w, err := s.store.GetWorkflow(id)
	if err != nil || w == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", id)
		}
		return nil, err
	}
	w.Enabled = enabled
	w.UpdatedBy = principal.Subject
	w.UpdatedAt = time.Now()
	if err := s.store.PutWorkflow(w); err != nil {
		return nil, err
	}
	return w, nil
}

// ValidateDraft validates the current draft without changing anything.
func (s *Service) ValidateDraft(id string) ([]ValidationIssue, error) {
	w, err := s.store.GetWorkflow(id)
	if err != nil || w == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", id)
		}
		return nil, err
	}
	return ValidateGraph(s.reg, w.Draft), nil
}

// PublishWorkflow validates the draft and appends an immutable version.
// Validation errors block publishing; warnings do not.
func (s *Service) PublishWorkflow(id string, principal authz.Identity) (*Workflow, error) {
	w, err := s.store.GetWorkflow(id)
	if err != nil || w == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", id)
		}
		return nil, err
	}
	issues := ValidateGraph(s.reg, w.Draft)
	if HasErrors(issues) {
		return nil, &ValidationError{Issues: issues}
	}
	graph := CloneGraph(w.Draft)
	hash, err := ContentHashOfGraph(graph)
	if err != nil {
		return nil, err
	}
	v := Version{
		Number:      len(w.Versions) + 1,
		Graph:       graph,
		ContentHash: hash,
		PublishedBy: principal.Subject,
		PublishedAt: time.Now(),
	}
	w.Versions = append(w.Versions, v)
	w.UpdatedBy = principal.Subject
	w.UpdatedAt = time.Now()
	if err := s.store.PutWorkflow(w); err != nil {
		return nil, err
	}
	return w, nil
}

// ValidationError carries validation issues as a typed error so the
// HTTP layer can render them structurally instead of as a string.
type ValidationError struct {
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("workflow validation failed with %d error(s)", len(e.Issues))
}

// StartRun starts a run of a published version (versionNumber 0 =
// latest). The run input must be a JSON object; it is bounded, and it
// is never merged into the workflow definition.
func (s *Service) StartRun(workflowID string, versionNumber int, input map[string]any, policyApproved bool, principal authz.Identity) (*Run, error) {
	w, err := s.store.GetWorkflow(workflowID)
	if err != nil || w == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", workflowID)
		}
		return nil, err
	}
	if !w.Enabled {
		return nil, fmt.Errorf("workflow %q is disabled", w.Name)
	}
	var ver *Version
	if versionNumber == 0 {
		if len(w.Versions) == 0 {
			return nil, fmt.Errorf("workflow %q has no published version", w.Name)
		}
		ver = &w.Versions[len(w.Versions)-1]
	} else {
		for i := range w.Versions {
			if w.Versions[i].Number == versionNumber {
				ver = &w.Versions[i]
				break
			}
		}
		if ver == nil {
			return nil, fmt.Errorf("workflow %q has no version %d", w.Name, versionNumber)
		}
	}
	if input == nil {
		input = map[string]any{}
	}
	if raw, err := json.Marshal(input); err != nil || len(raw) > MaxRunInputBytes {
		return nil, fmt.Errorf("run input exceeds %d bytes", MaxRunInputBytes)
	}
	// Run input keys must be declared workflow inputs, trigger inputs,
	// or the manual-trigger payload: anything else is a typo or a
	// stale parameter, and failing fast beats a silently ignored field.
	allowed := make(map[string]bool, len(ver.Graph.Inputs))
	for _, name := range ver.Graph.Inputs {
		allowed[name] = true
	}
	allowed["payload"] = true
	for _, n := range ver.Graph.Nodes {
		if spec, err := s.reg.Lookup(n.Type, SupportedVersion); err == nil && spec.Category == "trigger" {
			for _, f := range spec.InputSchema {
				allowed[f.Name] = true
			}
		}
	}
	for key := range input {
		if !allowed[key] {
			return nil, fmt.Errorf("run input %q is not a declared workflow input", key)
		}
	}
	issues := ValidateGraph(s.reg, ver.Graph)
	if HasErrors(issues) {
		return nil, &ValidationError{Issues: issues}
	}
	id, err := newID("run_")
	if err != nil {
		return nil, err
	}
	roles := make([]string, 0, len(principal.Roles))
	for _, r := range principal.Roles {
		roles = append(roles, string(r))
	}
	run := &Run{
		ID: id, WorkflowID: w.ID, WorkflowName: w.Name,
		VersionNumber: ver.Number, Snapshot: CloneGraph(ver.Graph),
		Status: RunRunning, StartedBy: principal.Subject, StartedRoles: roles,
		PolicyApproved: policyApproved, Input: input,
		Nodes: map[string]*NodeRun{}, StartedAt: time.Now(),
	}
	for _, n := range run.Snapshot.Nodes {
		run.Nodes[n.ID] = &NodeRun{NodeID: n.ID, Type: n.Type, Status: RunPending}
	}
	if err := s.store.PutRun(run); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	active := &activeRun{cancel: cancel, approve: make(chan approvalDecision, 1), done: make(chan struct{})}
	s.mu.Lock()
	s.actives[run.ID] = active
	s.mu.Unlock()
	go s.runLoop(ctx, run, active)
	// Clone under the run lock: the loop's first mutation must not
	// race this return value.
	active.mu.Lock()
	defer active.mu.Unlock()
	return cloneRun(run), nil
}

// CancelRun cancels a running or awaiting run. Awaiting runs that
// survived a restart (no live goroutine) are cancelled directly.
func (s *Service) CancelRun(id string) (*Run, error) {
	live, err := s.store.GetRun(id)
	if err != nil || live == nil {
		if err == nil {
			err = fmt.Errorf("run %q not found", id)
		}
		return nil, err
	}
	s.mu.Lock()
	active := s.actives[id]
	s.mu.Unlock()
	if active == nil {
		if live.Status != RunAwaitingApproval {
			return nil, fmt.Errorf("run %q is %s and cannot be cancelled", id, live.Status)
		}
		s.mu.Lock()
		now := time.Now()
		live.Status = RunCancelled
		live.FinishedAt = &now
		if nr := live.Nodes[live.PendingNodeID]; nr != nil {
			nr.Status = RunCancelled
			nr.FinishedAt = &now
		}
		for _, nr := range live.Nodes {
			if nr.Status == RunPending || nr.Status == RunRunning {
				nr.Status = NodeSkipped
			}
		}
		live.PendingNodeID = ""
		err := s.store.PutRun(live)
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return cloneRun(live), nil
	}
	active.cancel()
	<-active.done
	return s.GetRun(id)
}

// ApproveRun delivers an approval decision to an awaiting run. When
// the run survived a restart, approving resumes execution from the
// persisted record.
func (s *Service) ApproveRun(id, decision, approver string) (*Run, error) {
	if decision != "approved" && decision != "rejected" {
		return nil, fmt.Errorf("decision must be approved or rejected")
	}
	live, err := s.store.GetRun(id)
	if err != nil || live == nil {
		if err == nil {
			err = fmt.Errorf("run %q not found", id)
		}
		return nil, err
	}
	if live.Status != RunAwaitingApproval {
		return nil, fmt.Errorf("run %q is not awaiting approval (status %s)", id, live.Status)
	}
	s.mu.Lock()
	active := s.actives[id]
	s.mu.Unlock()
	if active != nil {
		select {
		case active.approve <- approvalDecision{decision: decision, approver: approver}:
		default:
			return nil, fmt.Errorf("run %q already has a pending decision", id)
		}
		return s.GetRun(id)
	}
	// Restarted run: resume the loop with the decision pre-seeded.
	ctx, cancel := context.WithCancel(context.Background())
	resumed := &activeRun{
		cancel: cancel, approve: make(chan approvalDecision, 1), done: make(chan struct{}),
		seedDecision: &approvalDecision{decision: decision, approver: approver},
	}
	s.mu.Lock()
	s.actives[id] = resumed
	s.mu.Unlock()
	go s.runLoop(ctx, live, resumed)
	return s.GetRun(id)
}

// DeleteWorkflow cancels live runs, then removes the workflow and its
// runs. Engine persistence after deletion is suppressed so a cancelled
// loop cannot resurrect a deleted run.
func (s *Service) DeleteWorkflow(id string) error {
	w, err := s.store.GetWorkflow(id)
	if err != nil || w == nil {
		if err == nil {
			err = fmt.Errorf("workflow %q not found", id)
		}
		return err
	}
	runs, err := s.store.ListRuns(id, 0)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Status == RunRunning || r.Status == RunAwaitingApproval {
			_, _ = s.CancelRun(r.ID)
		}
	}
	s.mu.Lock()
	s.deleted[id] = true
	// Mark run ids deleted too, so late engine writes are dropped.
	for _, r := range runs {
		s.deleted[r.ID] = true
	}
	s.mu.Unlock()
	return s.store.DeleteWorkflow(id)
}

func (s *Service) persistRun(active *activeRun, run *Run) {
	s.mu.Lock()
	deleted := s.deleted[run.ID]
	s.mu.Unlock()
	if deleted {
		return
	}
	active.mu.Lock()
	defer active.mu.Unlock()
	_ = s.store.PutRun(run)
}

func nodeErrorCode(err error) string {
	var ne *NodeError
	if errors.As(err, &ne) {
		return ne.Code
	}
	return "backend_error"
}

func (s *Service) principalFor(run *Run) authz.Identity {
	roles := make([]authz.Role, 0, len(run.StartedRoles))
	for _, r := range run.StartedRoles {
		roles = append(roles, authz.Role(r))
	}
	return authz.Identity{Subject: run.StartedBy, Roles: roles}
}

// buildInput assembles one node's validated input: edge-fed fields
// first, then run-input fields for declared fields still unset.
// Only declared input fields survive: Registry.Execute rejects unknown
// keys fail-closed, so the engine must not forward extras.
func (s *Service) buildInput(run *Run, node GraphNode, spec *NodeSpec, outputs map[string]map[string]any) map[string]any {
	declared := fieldNames(spec.InputSchema)
	input := make(map[string]any)
	for _, e := range run.Snapshot.Edges {
		if e.To != node.ID {
			continue
		}
		src, ok := outputs[e.From]
		if !ok {
			continue
		}
		if v, ok := src[e.FromPort]; ok && declared[e.ToPort] {
			input[e.ToPort] = v
		}
	}
	for _, f := range spec.InputSchema {
		if _, ok := input[f.Name]; !ok {
			if v, ok := run.Input[f.Name]; ok {
				input[f.Name] = v
			}
		}
	}
	if node.Type == "nexaroute.trigger.manual" {
		if _, ok := input["payload"]; !ok {
			if run.Input != nil {
				input["payload"] = run.Input
			} else {
				input["payload"] = map[string]any{}
			}
		}
	}
	return input
}

// runLoop executes a run's snapshot in topological order. It is
// re-entrant for approval resumption: nodes already succeeded keep
// their stored outputs and are skipped.
func (s *Service) runLoop(ctx context.Context, run *Run, active *activeRun) {
	defer func() {
		s.mu.Lock()
		delete(s.actives, run.ID)
		s.mu.Unlock()
		close(active.done)
	}()
	order, err := TopologicalOrder(run.Snapshot)
	if err != nil {
		active.mu.Lock()
		run.Status = RunFailed
		run.Error = "graph cannot be ordered: " + RedactError(err)
		now := time.Now()
		run.FinishedAt = &now
		active.mu.Unlock()
		s.persistRun(active, run)
		return
	}
	principal := s.principalFor(run)
	byID := make(map[string]GraphNode, len(run.Snapshot.Nodes))
	for _, n := range run.Snapshot.Nodes {
		byID[n.ID] = n
	}
	outputs := make(map[string]map[string]any, len(run.Nodes))
	active.mu.Lock()
	for id, nr := range run.Nodes {
		if nr.Status == RunSucceeded && nr.Output != nil {
			outputs[id] = nr.Output
		}
	}
	active.mu.Unlock()
	for _, nodeID := range order {
		node := byID[nodeID]
		spec, err := s.reg.Lookup(node.Type, SupportedVersion)
		if err != nil {
			s.failRun(active, run, nodeID, "node_not_found", "node type no longer registered")
			return
		}
		active.mu.Lock()
		nr := run.Nodes[nodeID]
		if nr.Status == RunSucceeded {
			active.mu.Unlock()
			continue
		}
		active.mu.Unlock()

		// Approval pause: a decision supplied by edge or run input
		// executes immediately; otherwise the run waits for ApproveRun.
		var decision *approvalDecision
		if node.Type == "nexaroute.control.approval" {
			input := s.buildInput(run, node, spec, outputs)
			if d, ok := input["decision"].(string); !ok || strings.TrimSpace(d) == "" {
				if active.seedDecision != nil {
					decision = active.seedDecision
					active.seedDecision = nil
				} else {
					active.mu.Lock()
					run.Status = RunAwaitingApproval
					run.PendingNodeID = nodeID
					nr.Status = NodeAwaitingApproval
					active.mu.Unlock()
					s.persistRun(active, run)
					select {
					case d := <-active.approve:
						decision = &d
					case <-ctx.Done():
						s.cancelRun(active, run)
						return
					}
				}
			}
		}

		input := s.buildInput(run, node, spec, outputs)
		if decision != nil {
			input["decision"] = decision.decision
			input["approver"] = decision.approver
			// Deliberately NOT inferred: the approval node rejects
			// self-approval only "when both are present". If the
			// workflow declares a requester (edge or run input) and
			// the same subject approves, the node fails; otherwise
			// a single-admin deployment can approve its own runs.
		}
		config := node.Config
		if config == nil {
			config = map[string]any{}
		}

		active.mu.Lock()
		nr.Status = RunRunning
		start := time.Now()
		nr.StartedAt = &start
		nr.Attempts = 0
		run.Status = RunRunning
		run.PendingNodeID = ""
		active.mu.Unlock()
		s.persistRun(active, run)

		var out map[string]any
		var runErr error
		maxAttempts := spec.Retry.MaxAttempts
		if maxAttempts < 1 {
			maxAttempts = 1
		}
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			active.mu.Lock()
			nr.Attempts = attempt
			active.mu.Unlock()
			out, runErr = s.reg.Execute(ctx, spec, principal, input, config, s.backend, run.PolicyApproved)
			if runErr == nil {
				break
			}
			if ctx.Err() != nil {
				break
			}
			var ne *NodeError
			retryable := errors.As(runErr, &ne) && ne.Retryable
			if retryable {
				allowed := false
				for _, code := range spec.Retry.RetryableCodes {
					if code == ne.Code {
						allowed = true
						break
					}
				}
				if !allowed || attempt >= maxAttempts {
					break
				}
				continue
			}
			break
		}
		finish := time.Now()
		active.mu.Lock()
		nr.FinishedAt = &finish
		nr.DurationMs = finish.Sub(start).Milliseconds()
		if runErr != nil {
			nr.Status = RunFailed
			nr.Error = RedactError(runErr)
			nr.ErrorCode = nodeErrorCode(runErr)
			if ctx.Err() != nil {
				run.Status = RunCancelled
				run.Error = "run cancelled"
				nr.Status = RunCancelled
				nr.Error = "run cancelled"
				nr.ErrorCode = "cancelled"
			} else {
				run.Status = RunFailed
				run.Error = nr.Error
			}
			run.FinishedAt = &finish
			for _, laterID := range order {
				if later := run.Nodes[laterID]; later != nil && later.Status == RunPending {
					later.Status = NodeSkipped
				}
			}
			active.mu.Unlock()
			s.persistRun(active, run)
			return
		}
		nr.Status = RunSucceeded
		nr.Output = out
		nr.Error = ""
		nr.ErrorCode = ""
		active.mu.Unlock()
		outputs[nodeID] = out
		s.persistRun(active, run)
	}
	active.mu.Lock()
	run.Status = RunSucceeded
	run.Error = ""
	finish := time.Now()
	run.FinishedAt = &finish
	active.mu.Unlock()
	s.persistRun(active, run)
}

func (s *Service) failRun(active *activeRun, run *Run, nodeID, code, msg string) {
	active.mu.Lock()
	if nr := run.Nodes[nodeID]; nr != nil {
		nr.Status = RunFailed
		nr.Error = msg
		nr.ErrorCode = code
		now := time.Now()
		nr.FinishedAt = &now
	}
	run.Status = RunFailed
	run.Error = msg
	now := time.Now()
	run.FinishedAt = &now
	active.mu.Unlock()
	s.persistRun(active, run)
}

func (s *Service) cancelRun(active *activeRun, run *Run) {
	active.mu.Lock()
	run.Status = RunCancelled
	run.Error = "run cancelled"
	now := time.Now()
	run.FinishedAt = &now
	if nr := run.Nodes[run.PendingNodeID]; nr != nil {
		nr.Status = RunCancelled
		nr.Error = "run cancelled"
		nr.ErrorCode = "cancelled"
		nr.FinishedAt = &now
	}
	for _, nr := range run.Nodes {
		if nr.Status == RunPending || nr.Status == RunRunning {
			nr.Status = NodeSkipped
		}
	}
	run.PendingNodeID = ""
	active.mu.Unlock()
	s.persistRun(active, run)
}
