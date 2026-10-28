package nexaflow

import (
	"context"
	"fmt"
	"time"
)

// Store is the durable persistence port for drafts, published versions,
// runs, schedule claims, webhook delivery records, and audit events. Every
// method is safe for concurrent use. Implementations must persist a run
// after every step: a crash may lose at most the in-flight step, which
// recovery then marks ambiguous rather than silently re-running.
//
// Draft/version model (Phase 1 lifecycle): a draft is the mutable working
// copy of a definition, keyed by definition id. Publish snapshots the
// draft as a new revision with status published and supersedes any
// previously published revision; revisions are never mutated, and
// rollback publishes an older snapshot as a new revision. Runs pin one
// (definition id, revision) pair.
//
// Supported topology in V1 is a single gateway process. MemoryStore is for
// tests. FileStore is the single-host durable authority: writes are atomic
// (temp file + rename + fsync) and serialized per store instance, which is
// what makes schedule claims and idempotency keys duplicate-safe inside one
// process. A multi-instance store (PostgreSQL claim transactions) is a
// later phase; the Store interface already carries the claim operations it
// will need, but FileStore does not claim cross-process safety.
type Store interface {
	// SaveDraft stores the mutable working copy of a definition.
	SaveDraft(ctx context.Context, d *Definition) error
	GetDraft(ctx context.Context, id string) (*Definition, error)
	// ListDrafts returns drafts in the tenant scope, sorted by id. An
	// empty scope lists all drafts.
	ListDrafts(ctx context.Context, tenantScope string) ([]*Definition, error)
	// Publish snapshots a draft as a new revision with status published,
	// superseding any previously published revision. It returns the
	// version record and the stored snapshot content.
	Publish(ctx context.Context, draft *Definition, by string) (*DefinitionVersion, *Definition, error)
	// GetVersion loads one published revision's content and record.
	GetVersion(ctx context.Context, definitionID string, revision uint64) (*Definition, *DefinitionVersion, error)
	// ListVersions returns all version records, ascending by revision.
	ListVersions(ctx context.Context, definitionID string) ([]*DefinitionVersion, error)
	// LatestPublished returns the currently published revision's content
	// and record, if any.
	LatestPublished(ctx context.Context, definitionID string) (*Definition, *DefinitionVersion, error)
	// ListPublished returns the latest published content of every
	// definition that has one, sorted by definition id.
	ListPublished(ctx context.Context) ([]*Definition, error)

	CreateRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error)
	// UpdateRun persists a run, incrementing its Version. Implementations
	// must reject a step history that shrinks: history is append-only.
	UpdateRun(ctx context.Context, r *Run) error
	FindRunByIdempotency(ctx context.Context, definitionID, key string) (*Run, error)
	// ClaimSchedule atomically claims one scheduled fire slot. It returns
	// true only for the first claim of (scheduleKey, fireTime); later claims
	// for the same slot return false and must not dispatch a run.
	ClaimSchedule(ctx context.Context, scheduleKey string, fireTime time.Time) (bool, error)
	// RecordDelivery records a webhook delivery hash. It returns true only
	// the first time the hash is seen; a replay returns false.
	RecordDelivery(ctx context.Context, hash string) (bool, error)
	// ScheduleState persists per-schedule fire bookkeeping.
	GetScheduleState(ctx context.Context, scheduleKey string) (*ScheduleState, error)
	SaveScheduleState(ctx context.Context, st *ScheduleState) error

	AppendAudit(ctx context.Context, event AuditEvent) error
	ListAudit(ctx context.Context, limit int) ([]AuditEvent, error)
	// PruneRuns deletes terminal runs finished before t, keeping
	// definitions and non-terminal runs untouched. It returns the count.
	PruneRuns(ctx context.Context, t time.Time) (int, error)
	// PruneRunsFor is PruneRuns restricted to one definition.
	PruneRunsFor(ctx context.Context, definitionID string, t time.Time) (int, error)
	// FormatVersion is the on-disk record format version (see file_store.go).
	FormatVersion() int
	// Migrate upgrades stored records to the current format in place,
	// preserving definition content hashes and run histories byte-for-byte
	// apart from added default fields. It is a no-op when already current.
	Migrate(ctx context.Context) error
}

// RunFilter narrows ListRuns. Empty fields match everything.
type RunFilter struct {
	DefinitionID string
	TenantScope  string
	Status       RunStatus
	Limit        int // 0 means no limit
}

// ScheduleState is the durable per-schedule fire bookkeeping used by the
// scheduler to compute the next fire time and to prove a slot fired once.
// ScheduleKey is definitionID + ":" + trigger instance id: it is stable
// across republication, so republishing does not reset the fire clock.
type ScheduleState struct {
	ScheduleKey  string    `json:"schedule_key"`
	DefinitionID string    `json:"definition_id"`
	TriggerNode  string    `json:"trigger_node"`
	LastFire     time.Time `json:"last_fire,omitempty"`
	NextFire     time.Time `json:"next_fire,omitempty"`
}

// ErrNotFound is returned for missing drafts, versions, and runs.
type ErrNotFound struct {
	Kind string
	ID   string
}

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("nexaflow: %s %q not found", e.Kind, e.ID)
}

// NotFound reports whether err is an ErrNotFound.
func NotFound(err error) bool {
	_, ok := err.(*ErrNotFound)
	return ok
}

// CurrentFormatVersion is the record format version FileStore writes.
// Version 1 records lack definition ContentHash on version records and
// lease/version fields on runs; Migrate fills those defaults.
const CurrentFormatVersion = 2
