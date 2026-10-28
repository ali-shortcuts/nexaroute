package nexaflow

import (
	"sync"
	"time"
)

// RunStatus is the state of a whole execution.
type RunStatus string

const (
	// RunPending is a run record created but not yet started.
	RunPending RunStatus = "pending"
	// RunRunning is a run whose executor currently holds its lease.
	RunRunning RunStatus = "running"
	// RunAwaitingApproval is a run suspended at an approval node. It
	// survives restarts: its decisions and completed steps are durable,
	// and it resumes only through an authorized approval decision.
	RunAwaitingApproval RunStatus = "awaiting_approval"
	// RunSucceeded, RunFailed, RunCancelled are terminal.
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
	// RunParked is a run recovered after an interrupted execution (see
	// Engine.Recover). It never re-executes on its own: an operator must
	// resume it with an explicit mode for any ambiguous step, so a
	// side-effect node is never blindly repeated after a crash.
	RunParked RunStatus = "parked"
)

// IsTerminal reports whether no further transition is possible without an
// operator resume (parked runs are resumable, hence not terminal here).
func (s RunStatus) IsTerminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled
}

// StepStatus is the state of one node step record. History is append-only:
// a retry appends a new step instead of overwriting the failed attempt,
// and a skipped node gets exactly one record. "waiting" marks an approval
// node suspended for a decision; "ambiguous" marks a step that was running
// when execution was interrupted and whose outcome is unknown.
type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
	StepCancelled StepStatus = "cancelled"
	StepWaiting   StepStatus = "waiting"
	StepAmbiguous StepStatus = "ambiguous"
)

// IsTerminal reports whether a step record will not change again, except
// that an ambiguous step is resolved by an operator resume.
func (s StepStatus) IsTerminal() bool {
	return s == StepSucceeded || s == StepFailed || s == StepSkipped || s == StepCancelled
}

// ApprovalDecision is one recorded human decision for an approval node.
type ApprovalDecision struct {
	Decision  string    `json:"decision"` // "approved" or "rejected"
	Approver  string    `json:"approver"`
	DecidedAt time.Time `json:"decided_at"`
}

// Step is one node attempt record. Input and Output are the redacted copies
// the store holds; the in-flight run keeps the same redacted values, so a
// resumed run never sees a value the store would not hold. Iteration is -1
// for non-iterated nodes and the zero-based iteration index otherwise.
type Step struct {
	NodeID     string         `json:"node_id"`
	Attempt    int            `json:"attempt"`
	Iteration  int            `json:"iteration"`
	Status     StepStatus     `json:"status"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	DurationMs int64          `json:"duration_ms"`
	Input      map[string]any `json:"input,omitempty"`
	Output     map[string]any `json:"output,omitempty"`
	Error      *NodeError     `json:"error,omitempty"`
}

// Run is a durable execution record, pinned to one published definition
// revision. Republishing never rewrites this record's definition: a new
// revision is a new snapshot, and rollback publishes an older snapshot as
// a newer revision.
type Run struct {
	ID           string `json:"id"`
	DefinitionID string `json:"definition_id"`
	Revision     uint64 `json:"revision"`
	TenantScope  string `json:"tenant_scope,omitempty"`
	Trigger      string `json:"trigger"` // "manual", "webhook", "schedule"
	// TriggerNodeID names the entry trigger instance for webhook and
	// schedule runs; empty for manual runs (all manual triggers are entries).
	TriggerNodeID  string                      `json:"trigger_node_id,omitempty"`
	Actor          string                      `json:"actor"`
	IdempotencyKey string                      `json:"idempotency_key,omitempty"`
	Status         RunStatus                   `json:"status"`
	Input          map[string]any              `json:"input,omitempty"`
	Steps          []Step                      `json:"steps,omitempty"`
	Outputs        map[string]any              `json:"outputs,omitempty"` // latest succeeded output per node id
	Decisions      map[string]ApprovalDecision `json:"decisions,omitempty"`
	Error          *NodeError                  `json:"error,omitempty"`
	// AmbiguousNodeID names the step whose outcome is unknown after an
	// interrupted execution (empty when the interruption happened between
	// steps). Resume requires an explicit mode for this node.
	AmbiguousNodeID string `json:"ambiguous_node_id,omitempty"`
	// HandlerUses counts error-handler invocations per failed node id,
	// bounding handler-driven retry cycles.
	HandlerUses map[string]int `json:"handler_uses,omitempty"`
	// LeaseOwner/LeaseUntil implement the execution lease: only the holder
	// may advance the run, and recovery parks runs whose lease expired.
	LeaseOwner string    `json:"lease_owner,omitempty"`
	LeaseUntil time.Time `json:"lease_until,omitempty"`
	// PolicyApproved records whether the published definition carried
	// operator policy approval for RequiresOperatorPolicy nodes (for
	// example non-loopback HTTP nodes and billable LLM nodes). It is set
	// from the definition at StartRun and threaded into every node
	// execution of the run.
	PolicyApproved bool `json:"policy_approved,omitempty"`
	// Version is an optimistic-concurrency counter incremented on every
	// store update. It is bookkeeping, not a workflow version.
	Version    int        `json:"version"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// LatestStep returns the most recent step record for a node, if any.
func (r *Run) LatestStep(nodeID string) (Step, bool) {
	for i := len(r.Steps) - 1; i >= 0; i-- {
		if r.Steps[i].NodeID == nodeID {
			return r.Steps[i], true
		}
	}
	return Step{}, false
}

// LatestByNode returns the most recent step record per node id.
func (r *Run) LatestByNode() map[string]Step {
	out := make(map[string]Step, len(r.Steps))
	for _, s := range r.Steps {
		out[s.NodeID] = s
	}
	return out
}

// AuditEvent is one authorization-relevant record. It carries no payloads,
// no input values, and no secret material of any kind.
type AuditEvent struct {
	Actor        string    `json:"actor"`
	Action       string    `json:"action"`
	DefinitionID string    `json:"definition_id,omitempty"`
	RunID        string    `json:"run_id,omitempty"`
	Success      bool      `json:"success"`
	Detail       string    `json:"detail,omitempty"`
	At           time.Time `json:"at"`
}

// AuditSink receives audit events. The engine takes a sink; stores
// implement it by persisting events, and tests use MemoryAuditSink.
type AuditSink interface {
	Record(event AuditEvent)
}

// MemoryAuditSink is an in-memory AuditSink for tests. It is safe for
// concurrent use.
type MemoryAuditSink struct {
	mu     sync.Mutex
	events []AuditEvent
}

// Record appends one audit event.
func (s *MemoryAuditSink) Record(event AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

// Events returns a copy of all recorded events.
func (s *MemoryAuditSink) Events() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEvent, len(s.events))
	copy(out, s.events)
	return out
}
