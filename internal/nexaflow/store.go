package nexaflow

import (
	"context"
	"errors"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Store errors. ErrNotFound, ErrAlreadyExists, ErrIdempotencyConflict,
// ErrTerminalState and ErrStateConflict are the only error values a
// Store implementation returns for its own contract violations; I/O and
// driver errors pass through unwrapped so callers can see the cause.
var (
	ErrNotFound            = errors.New("nexaflow: not found")
	ErrAlreadyExists       = errors.New("nexaflow: already exists")
	ErrIdempotencyConflict = errors.New("nexaflow: idempotency key already exists")
	ErrApprovalConflict    = errors.New("nexaflow: approval already decided differently")
	ErrNoApprovalRecord    = errors.New("nexaflow: no approval record")
)

// Bounds enforced at the run-record boundary. Definition size and
// node/edge counts are engine bounds (see engine.go); these two bound
// what one run record may hold.
const (
	// MaxRunInputBytes bounds the JSON of a run's input payload.
	MaxRunInputBytes = 64 << 10

	// MaxStoredOutputBytes bounds a single node output kept in a run
	// record. Larger outputs are truncated and marked truncated; the
	// truncation is recorded, never silent.
	MaxStoredOutputBytes = 64 << 10
)

// Run is one durable workflow execution. Definition is the pinned
// published snapshot: it is a copy, never a reference to a mutable
// draft, so later edits cannot alter this run. Principal is the subject
// that created the run (used for approval separation of duties) and
// PrincipalRoles is that identity's role snapshot at creation time:
// node authorization is re-evaluated at every dispatch from this
// snapshot, so a restart resumes with the same authorization the run
// started with. Role revocation takes effect for new runs, not for
// runs already in flight; that boundary is documented, not hidden.
// Input, outputs and Error are stored sanitized and bounded; secrets
// are never written here.
type Run struct {
	ID             string         `json:"id"`
	DefinitionID   string         `json:"definition_id"`
	Revision       uint64         `json:"revision"`
	Definition     Definition     `json:"definition"`
	State          RunState       `json:"state"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Principal      string         `json:"principal,omitempty"`
	PrincipalRoles []authz.Role   `json:"principal_roles,omitempty"`
	Input          map[string]any `json:"input,omitempty"`
	ErrorCode      string         `json:"error_code,omitempty"`
	Error          string         `json:"error,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	StartedAt      time.Time      `json:"started_at,omitempty"`
	FinishedAt     time.Time      `json:"finished_at,omitempty"`
	LeaseOwner     string         `json:"lease_owner,omitempty"`
	LeaseUntil     time.Time      `json:"lease_until,omitempty"`
}

// NodeExecution is one attempt of one node inside a run. Attempts are
// append-only: a retry or a recovery re-queue appends a new attempt row
// and the highest attempt is the current one. Dispatched is set only
// once the worker actually starts executing the node (after the claim);
// it is the recovery signal that distinguishes "claimed but never
// started" (safe to re-queue) from "started, outcome unknown" (parked
// when the side effect is effectful). Ambiguous is set when the engine
// parks the attempt because the external outcome cannot be proven.
// Deadline, when set, is the approval expiry for a waiting attempt:
// passing it fails the node as approval_expired (see Recover and the
// Execute entry check). A zero Deadline means no expiry.
type NodeExecution struct {
	RunID           string         `json:"run_id"`
	NodeID          string         `json:"node_id"`
	Attempt         int            `json:"attempt"`
	State           NodeState      `json:"state"`
	Input           map[string]any `json:"input,omitempty"`
	Output          map[string]any `json:"output,omitempty"`
	OutputTruncated bool           `json:"output_truncated,omitempty"`
	ResumeInput     map[string]any `json:"resume_input,omitempty"`
	ErrorCode       string         `json:"error_code,omitempty"`
	Error           string         `json:"error,omitempty"`
	Dispatched      bool           `json:"dispatched,omitempty"`
	Ambiguous       bool           `json:"ambiguous,omitempty"`
	Deadline        time.Time      `json:"deadline,omitempty"`
	IdempotencyKey  string         `json:"idempotency_key,omitempty"`
	LeaseOwner      string         `json:"lease_owner,omitempty"`
	LeaseUntil      time.Time      `json:"lease_until,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	StartedAt       time.Time      `json:"started_at,omitempty"`
	FinishedAt      time.Time      `json:"finished_at,omitempty"`
}

// Approval is one operator decision for an approval or operator-policy
// gate. A node has at most one approval row per run; a second,
// different decision is a conflict, while repeating the identical
// decision is idempotent.
type Approval struct {
	RunID     string    `json:"run_id"`
	NodeID    string    `json:"node_id"`
	Requester string    `json:"requester,omitempty"`
	Approver  string    `json:"approver,omitempty"`
	Decision  string    `json:"decision,omitempty"` // "approved" or "rejected"
	DecidedAt time.Time `json:"decided_at,omitempty"`
}

// PublishedDefinition pairs a DefinitionVersion with the immutable
// definition snapshot it versions. Phase 1 keeps the version record
// and the definition together in the store, keyed by DefinitionID +
// Revision; this struct is that pair, plus the store-level record of
// who published it and when (the Phase 1 version record itself carries
// no author or timestamp fields).
type PublishedDefinition struct {
	Version     DefinitionVersion `json:"version"`
	Definition  Definition        `json:"definition"`
	PublishedBy string            `json:"published_by,omitempty"`
	PublishedAt time.Time         `json:"published_at,omitempty"`
}

// RunFilter selects runs for listing. Empty fields match everything.
type RunFilter struct {
	DefinitionID string
	State        RunState
	Limit        int // 0 means no limit
}

// Store is the durability contract for the engine. Every method is safe
// for concurrent use. State transitions are conditional: UpdateRunState
// and SaveNodeExecution fail with ErrStateConflict / ErrTerminalState
// instead of overwriting a concurrent writer's result, and a transition
// to the already-current state succeeds as a no-op (idempotent
// re-delivery). Implementations must return deep copies, never aliases
// of stored records, so callers cannot mutate stored state by accident.
//
// Production note (ADR 0008 §D5): execution requires PostgreSQL and
// fails closed without it. MemoryStore and FileStore implement this
// same contract for tests and single-process use; they must never be
// presented as production run history. The production store is
// PostgresStore.
type Store interface {
	// Drafts and revisions.
	SaveDraft(ctx context.Context, def Definition) error
	GetDraft(ctx context.Context, definitionID string) (Definition, error)
	// ListDrafts returns all drafts sorted by definition id.
	ListDrafts(ctx context.Context) ([]Definition, error)
	// Publish validates nothing itself (validation is the caller's
	// pure ValidateDefinition step): it computes the draft's content
	// hash and appends a new published revision, unless an identical
	// hash is already the latest published revision, in which case
	// that revision is returned (idempotent publish). Publishing a
	// new revision supersedes the previous published revision; runs
	// pinned to it keep their snapshot and are unaffected.
	Publish(ctx context.Context, definitionID, publishedBy string) (PublishedDefinition, error)
	// SupersedeRevision moves one published revision to superseded
	// without publishing a new one.
	SupersedeRevision(ctx context.Context, definitionID string, revision uint64) (PublishedDefinition, error)
	GetRevision(ctx context.Context, definitionID string, revision uint64) (PublishedDefinition, error)
	ListRevisions(ctx context.Context, definitionID string) ([]PublishedDefinition, error)

	// Runs.
	CreateRun(ctx context.Context, run Run) error
	GetRun(ctx context.Context, runID string) (Run, error)
	GetRunByIdempotency(ctx context.Context, key string) (Run, error)
	// UpdateRunState moves a run to state only if its current state is
	// expected ("" means any non-terminal current state). The update
	// also refreshes UpdatedAt and sets StartedAt/FinishedAt on the
	// first entry into running / a terminal state.
	UpdateRunState(ctx context.Context, runID string, expected RunState, state RunState, errorCode, errMsg string) (Run, error)
	ListRuns(ctx context.Context, filter RunFilter) ([]Run, error)
	// Prune deletes terminal runs (and their node rows and approvals)
	// finished before cutoff and returns how many runs were removed.
	// It never touches non-terminal runs.
	Prune(ctx context.Context, before time.Time) (int, error)

	// Node executions.
	// AppendNodeExecution adds a new attempt row for (run, node). The
	// attempt number is assigned by the store (previous max + 1, or 0
	// for the first attempt of a node the run created with).
	AppendNodeExecution(ctx context.Context, exec NodeExecution) (NodeExecution, error)
	// SaveNodeExecution replaces the current attempt row for
	// (run, node) after checking the transition from its stored state.
	// The caller must pass the stored state in expected; "" means any
	// non-terminal current state. Terminal rows reject any change.
	SaveNodeExecution(ctx context.Context, exec NodeExecution, expected NodeState) (NodeExecution, error)
	GetNodeExecutions(ctx context.Context, runID string) ([]NodeExecution, error)
	// TryClaim atomically leases the current attempt of (run, node) to
	// owner until now+ttl. It succeeds when the attempt is pending, or
	// carries no live lease held by a different owner (expired lease,
	// or a lease already held by the same owner, which renews). It
	// returns false (not an error) when a live foreign lease blocks.
	// For PostgreSQL the server clock is authoritative and now is
	// advisory; for memory/file stores now is the clock.
	TryClaim(ctx context.Context, runID, nodeID, owner string, ttl time.Duration, now time.Time) (NodeExecution, bool, error)
	// ReleaseLease clears a lease held by owner without changing state.
	// It is a no-op when the lease belongs to someone else.
	ReleaseLease(ctx context.Context, runID, nodeID, owner string) error

	// Approvals.
	RecordApproval(ctx context.Context, appr Approval) (Approval, error)
	GetApproval(ctx context.Context, runID, nodeID string) (Approval, error)
}

// NodeIDKey derives the idempotency key for one node attempt from
// (run, node, attempt). It is stored on the attempt row so a duplicate
// delivery with the same key returns the recorded outcome instead of
// re-executing.
func NodeIDKey(runID, nodeID string, attempt int) string {
	return runID + ":" + nodeID + ":" + itoa(attempt)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// sanitizeNodeOutput redacts secret-shaped values and truncates an
// over-bound output. The second return value reports truncation.
func sanitizeNodeOutput(out map[string]any) (map[string]any, bool) {
	if out == nil {
		return map[string]any{}, false
	}
	cleaned, _ := redactJSONValue(deepCopyValue(out)).(map[string]any)
	if cleaned == nil {
		cleaned = map[string]any{}
	}
	raw := jsonLen(cleaned)
	if raw <= MaxStoredOutputBytes {
		return cleaned, false
	}
	// Keep the output bounded and honest: truncate deterministically by
	// keeping keys in sorted order until the bound is met, and record
	// that truncation happened.
	keys := sortedKeys(cleaned)
	kept := make(map[string]any, len(keys))
	for _, k := range keys {
		kept[k] = cleaned[k]
		if jsonLen(kept) > MaxStoredOutputBytes {
			delete(kept, k)
			break
		}
	}
	kept["_truncated"] = true
	return kept, true
}
