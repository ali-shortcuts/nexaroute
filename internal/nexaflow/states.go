package nexaflow

import "errors"

// RunState is the lifecycle of one workflow run, exactly as fixed by
// ADR 0008 §D4: created -> running <-> waiting_for_approval |
// waiting_timer -> succeeded | failed | cancelled |
// needs_manual_action. There is no "queued" state in v1: a created run
// moves straight to running when Execute drives it.
// needs_manual_action is the terminal parking state for ambiguous
// outcomes: the engine never auto-resumes it, but an operator may
// still force-fail or cancel it.
type RunState string

const (
	RunCreated            RunState = "created"
	RunRunning            RunState = "running"
	RunWaitingForApproval RunState = "waiting_for_approval"
	RunSucceeded          RunState = "succeeded"
	RunFailed             RunState = "failed"
	RunCancelled          RunState = "cancelled"
	RunNeedsManualAction  RunState = "needs_manual_action"
	// RunWaitingTimer is reserved for a future timer state. The Phase 2
	// engine keeps a run in running while a wait node sleeps; it never
	// emits waiting_timer, so no consumer may depend on it yet.
	RunWaitingTimer RunState = "waiting_timer"
)

// Terminal reports whether no further engine transition leaves this state
// except the operator decisions on ForceFailRun/CancelRun.
func (s RunState) Terminal() bool {
	switch s {
	case RunSucceeded, RunFailed, RunCancelled:
		return true
	default:
		return false
	}
}

// NodeState is the lifecycle of one node attempt inside a run, exactly
// as fixed by ADR 0008 §D4: pending -> claimed -> running <-> waiting
// -> succeeded | failed | skipped | cancelled | needs_manual_action.
// The node waiting state is "waiting" (the run-level state is
// "waiting_for_approval"); both are produced by approval and
// operator-policy gates.
type NodeState string

const (
	NodePending           NodeState = "pending"
	NodeClaimed           NodeState = "claimed"
	NodeRunning           NodeState = "running"
	NodeWaiting           NodeState = "waiting"
	NodeSucceeded         NodeState = "succeeded"
	NodeFailed            NodeState = "failed"
	NodeSkipped           NodeState = "skipped"
	NodeCancelled         NodeState = "cancelled"
	NodeNeedsManualAction NodeState = "needs_manual_action"
)

// Terminal reports whether the node attempt will never run again.
func (s NodeState) Terminal() bool {
	switch s {
	case NodeSucceeded, NodeFailed, NodeSkipped, NodeCancelled:
		return true
	default:
		return false
	}
}

// ErrTerminalState is returned when a transition would move a terminal
// run or node attempt to a different state. Same-state transitions are
// idempotent and always succeed.
var ErrTerminalState = errors.New("nexaflow: state is terminal")

// ErrStateConflict is returned when a conditional transition's expected
// current state does not match the stored state (a concurrent writer
// won the race). Callers re-read and retry or report the conflict;
// they must never blindly overwrite.
var ErrStateConflict = errors.New("nexaflow: state conflict")

// ErrDefinitionBusy is returned when Execute is called for a definition
// that already runs the maximum concurrent runs allowed by its
// settings (default 1 in v1).
var ErrDefinitionBusy = errors.New("nexaflow: definition already runs the maximum concurrent runs")

// validRunTransition enforces the run state machine. Same-state is
// always allowed (idempotent re-delivery); terminal states reject any
// different target.
func validRunTransition(from, to RunState) bool {
	if from == to {
		return true
	}
	if from.Terminal() {
		return false
	}
	switch from {
	case RunCreated:
		return to == RunRunning || to == RunCancelled
	case RunRunning:
		return to == RunSucceeded || to == RunFailed || to == RunCancelled ||
			to == RunWaitingForApproval || to == RunNeedsManualAction
	case RunWaitingForApproval:
		return to == RunRunning || to == RunCancelled || to == RunFailed
	case RunNeedsManualAction:
		return to == RunCancelled || to == RunFailed
	default:
		return false
	}
}

// validNodeTransition enforces the node state machine with the same
// idempotency rule as runs.
func validNodeTransition(from, to NodeState) bool {
	if from == to {
		return true
	}
	if from.Terminal() {
		return false
	}
	switch from {
	case NodePending:
		return to == NodeClaimed || to == NodeSkipped || to == NodeCancelled
	case NodeClaimed:
		// Claimed -> Failed is the fail-without-dispatch path: sweep
		// fails a claimed node it cannot run (unknown node, or an
		// input mapping that cannot be resolved) without ever
		// dispatching it, so no side effect has happened.
		return to == NodeRunning || to == NodeCancelled || to == NodeNeedsManualAction ||
			to == NodeWaiting || to == NodePending || to == NodeFailed
	case NodeRunning:
		// Running -> pending is the recovery-only requeue: a lease
		// expired after dispatch on a side-effect-free node (or before
		// dispatch was proven) restarts the attempt. Execute never
		// emits it; only Recover does, and only for expired leases.
		return to == NodeSucceeded || to == NodeFailed || to == NodeCancelled ||
			to == NodeNeedsManualAction || to == NodeWaiting || to == NodePending
	case NodeWaiting:
		return to == NodePending || to == NodeCancelled || to == NodeFailed
	case NodeNeedsManualAction:
		return to == NodeCancelled || to == NodeFailed
	default:
		return false
	}
}
