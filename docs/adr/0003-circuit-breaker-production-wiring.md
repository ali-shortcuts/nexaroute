# ADR 0003: Circuit-breaker production wiring (opt-in breaker sync)

- Status: **Accepted.** Implemented behind an opt-in flag (default off);
  the three policy questions at the end were decided 2026-10-22 by the
  supervising engineer under the owner's explicit delegation of the
  remaining decisions ("fix the remaining sections yourself",
  2026-10-22) — see "Decisions on the open questions" below.
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
  two intervals plus store latency (up to one interval for a change to
  be published, up to one more for a peer to observe it), at the cost
  of a small periodic read/write per replica against the control-plane
  store. (Wording corrected 2026-10-23: the original "one interval
  plus propagation" understated the tick-driven worst case.)
- A shared-store outage degrades the fleet to exactly today's
  per-instance behavior; it can never open circuits.

## Decisions on the open questions (2026-10-22)

Decided by the supervising engineer under the owner's explicit
delegation of the remaining decisions ("fix the remaining sections
yourself", 2026-10-22):

1. **Remote `retired` is never honored as a full local retirement.**
   The bounded-cooldown demotion implemented here is the settled
   behavior: a peer can keep a deployment cooled down for at most the
   store cap per round, and only the local operator/config lifecycle
   can retire or un-retire a local deployment. A compromised or buggy
   peer must never be able to permanently remove a local deployment.
2. **Breaker sync remains opt-in.** It does not become default-on for
   shared control-plane backends or for any deployment profile:
   enabling cross-replica state flow is an explicit operator decision,
   and the standalone behavior stays the default exactly as shipped.
3. **The 10s default cadence stands.** It is the settled production
   default; operators with larger fleets can raise it within the
   validated 5..3600 range and accept the slower convergence as the
   documented write-amplification trade-off.
