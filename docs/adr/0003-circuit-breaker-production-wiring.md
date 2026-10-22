# ADR 0003: Circuit-breaker production wiring (opt-in breaker sync)

- Status: Implemented behind an opt-in flag (default off); policy questions
  listed at the end remain open for the owner.
- Date: 2026-10-22
- Parent audit: #593; builds on ADR 0002 (state-store contract, #608/#618)
- Scope: wiring `internal/sharedstate` into the gateway runtime

## Context

ADR 0002 landed the shared breaker state-store slice but deliberately did
not wire it into routing or health state: four decisions were left open
(application entry points and cadence, publication set and cadence,
configuration surface, and multi-instance acceptance). The continuation
mission (2026-10-22) requires the wiring with these prescribed semantics:
opt-in configuration with backward-compatible defaults, publication and
application cadence/expiry/clock/conflict/restart semantics fixed by the
store contract, an outage of the shared store must not clear local
circuits or admit unsafe traffic, standalone behavior preserved exactly,
the actual scope exposed through the deployment report/UI, and real
PostgreSQL/Redis two-instance acceptance tests.

## Decision

Implement `internal/breakersync` and wire it in `cmd/gateway`:

1. **Configuration surface** (`control_plane`):
   - `breaker_sync` (bool, default `false`) — the opt-in.
   - `breaker_sync_interval_seconds` (int, 0 = default 10s; validated
     5..3600 when set).
   - `breaker_sync_replica_id` (string, optional; default is a generated
     `hostname-pid` identity at startup).
   - `breaker_sync` without `control_plane.enabled` is a validation
     error, so an operator request is never silently ignored.
2. **Publication**: a background goroutine (never the request path)
   publishes the local markers extracted from `health.Manager.Snapshot()`:
   active cooldowns and retirements only. Healthy/degraded/half-open
   states stay local. Publication is the monotonic merge of ADR 0002:
   it never deletes a marker and local recovery never clears a peer's
   still-active marker; cooldowns end by expiry only.
3. **Application**: each round loads the shared document first. A load
   error (outage, malformed document) aborts the round with local state
   untouched. Remote cooldowns apply through
   `health.Manager.ApplySharedCooldown`, which can only extend a local
   cooldown, never shorten one, never modifies a locally retired
   deployment, and never touches failure counters, EWMA or recovery
   bookkeeping.
4. **Remote retired**: applied only as a bounded cooldown of the store
   cap (30 minutes), re-extended on each round while the origin keeps
   publishing it. A peer can therefore keep a deployment cooled down,
   but can never impose a permanent local retirement.
5. **Clock and conflict semantics**: inherited from ADR 0002 (deadlines
   clamped to now+MaxCooldown at publish, revision CAS with bounded
   retries, malformed documents rejected rather than read as empty).
6. **Scope reporting**: `/api/hello`'s deployment report derives the
   breaker scope from the same configuration: `shared` for
   PostgreSQL/Redis control planes with sync enabled, `shared-host` for
   the file backend, and `per-instance` otherwise — including when sync
   is requested on the memory backend, which cannot cross processes and
   is called out in the report warnings.
7. **Acceptance**: unit tests cover propagation, bounded retired,
   outage, never-shorten and expiry against an in-memory store;
   build-tagged integration tests run two independent manager+syncer
   pairs against real PostgreSQL and Redis through the incident →
   propagation → partition → heal → restart scenario, and are wired
   into the CI integration jobs.

## Consequences

- Default deployments are unchanged: with `breaker_sync` unset, no
  goroutine starts, no store traffic exists, and the deployment report
  keeps reporting per-instance breaker state.
- Enabling sync makes replicas converge on provider incidents within
  one interval plus propagation, at the cost of a small periodic
  read/write per replica against the control-plane store.
- A shared-store outage degrades the fleet to exactly today's
  per-instance behavior; it can never open circuits.

## Open questions for the owner (not settled by this ADR)

1. Should a remote `retired` marker ever be honored as a full local
   retirement (with an un-retire path), instead of the bounded-cooldown
   demotion implemented here?
2. Should breaker sync become default-on when a shared control-plane
   backend (PostgreSQL/Redis) is configured, remain opt-in forever, or
   become default-on only for specific deployment profiles?
3. Is the 10s default cadence the right production default, or should
   the fleet guidance standardize on a different interval (with its
   write-amplification trade-off)?
