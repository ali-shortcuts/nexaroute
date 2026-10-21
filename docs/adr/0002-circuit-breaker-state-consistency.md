# ADR 0002: Shared circuit-breaker state consistency contract

- Status: Accepted for the state-store slice; production wiring remains BLOCKED on owner decisions.
- Date: 2026-10-21
- Issue: #608 (parent audit #593)
- Scope: the first safe shared circuit-breaker state slice only

## Context

NexaRoute's deployment breaker, cooldown and probe state lives in the local
`health.Manager` (`internal/health/manager.go`) and is process-local. The
existing control-plane abstraction already provides atomic revision-checked
documents over file, memory, PostgreSQL and Redis
(`internal/controlplane/types.go`, `sqlstore.go`, `redisstore.go`). It has
real-service integration coverage for concurrent writers and restart
persistence.

Issue #608 asks for the smallest safe first slice, not a routing rewrite and
not a distributed-support claim. The slice therefore defines and implements a
state-store contract that can carry only bounded breaker markers. It does not
wire the gateway data plane to the store in this change.

## Decision

Implement `internal/sharedstate` as an explicit, reusable state-store slice:

1. A narrow `Store` contract with `Load` and revision-guarded
   `CompareAndSwap` only. No delete, list, watch, TTL or backend-specific
   behaviour is assumed.
2. A `ControlPlaneStore` adapter over the existing `controlplane.Store`.
   This adds no table, migration, driver, namespace rewrite or config key; it
   reuses the existing revision/CAS semantics.
3. A versioned breaker document (`nexaroute:breaker/v1`, schema `1`) keyed by
   deployment ID, with only `cooldown` and `retired` markers.
4. A pure monotonic merge and a high-level `BreakerStore` that performs
   read/merge/compare-and-swap with bounded conflict retries.

## Consistency and atomicity contract

- **Unit of atomicity:** one namespace document. Writers never patch one key
  independently; they load the document, merge locally, and write the complete
  next document with `expectedRevision`.
- **Conflict handling:** a stale revision fails with `ErrConflict`. The
  high-level store re-reads and re-merges up to `Options.Retries` times
  (default 3 retries after the initial attempt), then returns `ErrConflict`.
  There is no unbounded retry and no busy loop.
- **Merge rules:** `retired` is sticky and is never removed by an incoming
  cooldown. Two cooldowns keep the later `cooldown_until`. An absent marker in
  the incoming view never deletes an existing marker. Expired cooldowns are
  pruned. Ties keep the existing entry, which makes repeated writes
  deterministic.
- **Idempotency:** publishing the same active markers twice produces the same
  document; the second call does not write and returns the existing revision.
- **Clock safety:** every persisted cooldown deadline is clamped to
  `now + Options.MaxCooldown` (default 30 minutes). A replica with a skewed
  clock can therefore over-exclude for at most that configured cap, never
  indefinitely.
- **Malformed data:** unknown document schemas, oversized documents and
  over-limit entry counts are rejected as `ErrMalformed`; they are never
  interpreted as empty state. Individual malformed remote entries are skipped
  while valid entries in the same document remain usable.

## TTL and expiration contract

- A cooldown's TTL is its `cooldown_until` deadline. The store itself does not
  perform TTL eviction.
- Expired cooldowns are filtered from `Load` results immediately and removed
  from the persisted document by the next successful `Publish`.
- `retired` has no TTL and remains sticky. This store slice intentionally has
  no delete/unretire operation; a future operator/config lifecycle must define
  explicit invalidation. Remote `retired` handling is deliberately not wired
  in this slice (see BLOCKED below).
- Successful traffic is not published. Local recovery must not clear another
  replica's still-active cooldown.

## Outage and failure contract

- `Load` and `Publish` return `ErrUnavailable` when the backing store is
  unreachable. Context cancellation/deadline errors are preserved.
- On any store error the caller must keep its current local breaker state.
  A failed load is not an empty load, and a failed publish is not a recovery
  signal.
- There is no fallback that closes circuits, admits traffic, clears markers or
  writes local state into another namespace. Outage behaviour is therefore
  "keep local truth", not "fail open".
- The store is not on the request hot path in this slice because no gateway
  wiring is included. Any future wiring must keep store I/O off-request and
  best-effort.

## Startup and recovery contract

- On a normal start the high-level store loads the namespace. `ErrNotFound`
  means a new, empty namespace and is the only condition that yields an empty
  view without an error.
- A restart over the same durable store recovers committed markers and their
  revisions. File-backed restart is covered by contract tests; PostgreSQL and
  Redis integration tests exercise the same adapter against real services.
- A malformed or unsupported document fails the load. It does not reset local
  circuits to closed and it does not overwrite the document.
- If a write conflicts during startup/recovery, the normal bounded retry path
  applies.

## Backward-compatible defaults

- No configuration field is added in this slice.
- The gateway does not construct or call `internal/sharedstate`.
- No existing control-plane namespace is read or written, and no migration is
  applied.
- A single-process deployment with no shared store behaves exactly as before;
  this change is additive library code plus tests.

## Explicit non-goals and remaining BLOCKED decisions

This slice does **not** provide distributed circuit-breaker support. It stores
and merges markers only. The following require an owner decision before any
production behaviour change:

1. **Applying remote markers to `health.Manager`** (which entry points,
   application cadence, and how remote `retired` is demoted or bounded).
2. **Publishing local transitions** (which transitions publish, how often
   active cooldowns refresh, and how to avoid write amplification).
3. **Configuration surface** (opt-in key names/defaults, namespace override,
   timeout/backoff values).
4. **Multi-instance acceptance** (a real two-replica gateway service test with
   incident, partition and heal scenarios). No such test exists here, so this
   PR is not distributed acceptance.

Provider-level and scope-level circuits, affinity hints, metrics, admin
surfaces and backup/restore integration are also out of scope for this slice.

## Consequences

- The project gains one explicit, backend-agnostic state-store contract with
  deterministic contract tests and real PostgreSQL/Redis adapter tests.
- The hard consistency choices required for live multi-replica routing remain
  visible and unclaimed rather than being silently assumed.
- Adding production wiring later can build on the tested store/merge layer
  without changing its on-disk schema or backward-compatible defaults.
