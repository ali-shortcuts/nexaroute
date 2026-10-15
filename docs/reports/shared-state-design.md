# Design: cross-replica breaker + affinity state (DESIGN ONLY)

Status: design only — no code, no config changes in this run.
Scope: `docs/KNOWN_GAPS.md` ("State and HA" and "Routing boundaries") records
that cross-replica breaker synchronization and shared/distributed affinity and
breaker state across multiple NexaRoute processes are not implemented.
This document inventories today's single-process state with file+type evidence,
proposes a sharing design that reuses only store capabilities that exist in the
codebase, and gives a phased implementation plan in which Phase 1 is
independently useful.

Zero-service default preserved: a single process with no external store
configured must behave exactly as today. Sharing is opt-in, best-effort, and
fails toward today's single-process behavior — never fail-closed on the data
plane.

## 1. Current-state inventory (all in-memory, single-process)

### 1.1 Session-affinity state

| Claim | Evidence |
|---|---|
| Pin struct is `{Deployment string; Expires time.Time}`, cap 10000 entries | `internal/router/router.go:95-100` — `type sessionPin struct`, `const maxSessionPins = 10000` |
| Store is a process-local map guarded by its own RWMutex | `internal/router/router.go:102-112` — `type Router struct { ... sessionMu sync.RWMutex; sessions map[string]sessionPin }` |
| Bucket key is `hex(sha256(SessionKey)[:16]) + model + providerType + scopes + privacy-class`; raw session key never stored as map key | `internal/router/router.go:328-338` — `func (r *Router) affinityBucket` |
| Read path: `pinned()` (TTL-checked, lazy expiry of the single key); no-op when `SessionAffinity` is false or key is empty | `internal/router/router.go:339-372` — `func (r *Router) pinned` |
| Decision-plane-safe read returns only the deployment ID | `internal/router/router.go:376-381` — `func (r *Router) PinnedDeploymentID` |
| Write path: `ObserveSession()` sets/refreshes one pin with TTL; bounded-map eviction deletes one arbitrary entry under flood | `internal/router/router.go:382-406` — `func (r *Router) ObserveSession` |
| Expired-pin sweep + count | `internal/router/router.go:407-417` — `func (r *Router) SessionCount` |
| `Reload()` clears all pins when affinity is disabled, else prunes pins pointing at removed deployments or past expiry | `internal/router/router.go:186-197` — `func (r *Router) Reload` |
| Pin only promotes the pinned candidate within the best priority tier (`cost_aware`, `ready_mesh`); never crosses priority | `internal/router/router.go:490-521` (`orderCostAware`), `523-575` (`orderReadyMesh`) |
| Config knobs: `routing.session_affinity bool`, `routing.session_ttl_seconds int` (default `true`/`3600`); validation `1 .. 30*24*60*60` | `internal/config/config.go:397-398`, `915` (defaults), `1186-1187` (defaulting), `1730` (validation) |
| Session-key ingestion: headers `x-claude-code-session-id`, `x-litellm-session-id`, `x-litellm-trace-id`, `x-session-id`, else body session key; values length-bounded | `internal/httpapi/routing_helpers.go:322-334` — `func sessionKeyFromRequestParts`, `func sessionKeyFromRequest` |
| `prepareRequirement` attaches `SessionKey` + `SelectionKey` (`x-request-id`) + per-provider load closure per request | `internal/httpapi/routing_helpers.go:397-415` — `func (s *Server) prepareRequirement` |
| **Write cadence (production): exactly one map write per successful proxied response** via `recordRouteSuccess` (health + scope + affinity together) | `internal/httpapi/routing_helpers.go:416-421` — `func (s *Server) recordRouteSuccess` |
| **Read cadence (production): every candidate ordering** consults `pinned()` (`orderCostAware`/`orderReadyMesh`); affinity re-checks also flow through `eligibleDeployment` (see privacy gate comment) | `internal/router/router.go:274-326` (`eligibleDeployment` comment at `286-287`), `505-519`, `533-544` |
| Privacy: `affinityBucket` isolates `no_training` vs `any`; the eligibility gate `eligibleDeployment` fail-closes `RequireNoTraining` before any pin can promote an ineligible deployment | `internal/router/router.go:289-291`, `328-338`; `internal/router/privacy_test.go:142-143` |

There is no persistence, no replication, no cross-process invalidation for
affinity state. Restart or a second replica starts with an empty map.

### 1.2 Circuit-breaker / health state ("breaker" in the issue = this)

| Claim | Evidence |
|---|---|
| Per-deployment state struct (counters + EWMA gauges + cooldown deadline + scoped circuits) | `internal/health/manager.go:42-59` — `type State struct { Deployment, Status, Successes, Failures, ConsecutiveFailures, EWMALatencyMS, EWMATTFTMS, EWMAFailureRate, LastChecked, LastSuccess, LastFailure, LastError, LastErrorClass, CooldownUntil, RecoveryFailures, Scopes map[string]ScopeState }` |
| Per-capability (scope) circuit struct | `internal/health/manager.go:19-29` — `type ScopeState struct` |
| Provider-level circuit struct + per-provider multi-deployment evidence map | `internal/health/manager.go:31-40` — `type ProviderState struct`; `61-76` — `type Manager struct { ... providers map[string]ProviderState; providerEvidence map[string]map[string]time.Time; ... }` |
| Manager is a mutex-guarded in-memory map set; thresholds/cooldowns held in the same struct | `internal/health/manager.go:61-76`, `129-148` (`New`), `733-777` (`Configure`, `ConfigureAdvanced`, `ConfigureProviderIncidents`) |
| Status vocabulary | `internal/health/manager.go:8-17` — `Unknown, Healthy, Degraded, HalfOpen, Cooldown, Retired` |
| Transition: success resets to `Healthy` (except `Retired` is sticky; observations inside an active hard cooldown do not re-admit) | `internal/health/manager.go:288-319` — `func (m *Manager) RecordSuccess` |
| Transition: failure increments counters/EWMA; `HalfOpen` or `ConsecutiveFailures >= threshold` opens `Cooldown` with `CooldownUntil = now + cooldown`, else `Degraded` (no deadline); in-flight observations inside an active cooldown never downgrade back to `Degraded` | `internal/health/manager.go:321-357` — `func (m *Manager) RecordFailure` |
| Hard-cooldown entry points (429/`Retry-After` path) and soft quarantine (degrade without deadline) | `internal/health/manager.go:568-606` (`EnterCooldown`, `ForceCooldown`), `524-544` (`Quarantine`), `546-566` (`RecordRecoveryFailure`) |
| Terminal retirement (EOL/model-removed); success/failure while `Retired` never re-admits | `internal/health/manager.go:608-624` (`Retire`); `299-303`, `326-329` guards |
| Scoped (capability) circuits: `scopeThreshold`/`scopeCooldown` per scope; `Cooldown` → `Unknown` after deadline | `internal/health/manager.go:626-694` (`RecordScopeSuccess`, `RecordScopeFailure`), `172-183` (`normalizeScopes`) |
| Provider circuits: `providerThreshold`/`providerWindow`/`providerCooldown`; evidence deduplicated per deployment inside the window; `HalfOpen` failure re-opens `Cooldown`; success inside active cooldown is ignored as stale | `internal/health/manager.go:400-477` (`RecordProviderFailure`, `RecordProviderSuccess`), `369-398` (`ProviderAvailable`) |
| Time-based normalization is lazy on read (`Cooldown` → `HalfOpen` globally, scope `Cooldown` → `Unknown`) | `internal/health/manager.go:161-195` (`normalizeGlobal`, `normalizeScopes`, `stateNeedsNormalization`), `206-241` (`GetWithScopes`), `492-522` (`Snapshot`), `359-367`+`479-490` (provider equivalents) |
| Read entry points used by routing | `internal/health/manager.go:206-241` (`GetWithScopes`), `238-241` (`Get`), `369-398` (`ProviderAvailable`), `688-694` (`ScopesReady`), `492-522` (`Snapshot`), `479-490` (`ProviderSnapshot`) |
| Routing gate: provider-gated, then `Retired` always ineligible; `ready_mesh`/`cost_aware`/`ready_queue` admit only `Healthy`; other strategies exclude only `Cooldown`; `ready_mesh` additionally requires scope readiness | `internal/router/router.go:274-326` (`eligibleDeployment`), `215-226` (`healthRank`), `734-752` (`Readiness`) |
| Config defaults/thresholds | `internal/config/config.go:402-408` (fields), `1202-1258` (defaults: failure 5, cooldown 1800s, provider 3/20s/30s, capability 2/300s), `1736-1754` (validation), `3027-3034` (`Cooldown()`, `ProviderCooldown()`) |
| **Data-plane write cadence (production): per upstream attempt.** Failure/quarantine/hard-cooldown/scope-failure recorded per attempt in each protocol path; success recorded once per served response via `recordRouteSuccess` | `internal/httpapi/openai.go:261-274,330-342,432-457`; `internal/httpapi/anthropic.go:266-279,335-347,459-484`; `internal/httpapi/canonical_path.go:556-561,604-616,666-669`; `internal/httpapi/media.go:250,285`; `internal/httpapi/routing_helpers.go:416-421`, `423-427` (`recordProviderFailure` gated by `policy.SignalProvider`) |
| Failure classification feeding those calls (which errors quarantine / hard-cool / scope-trip) | `internal/httpapi/routing_helpers.go:109-134` (`upstreamFailurePolicy`) |
| **Probe-plane write cadence: event-driven sweeps + dedicated recovery loops** (startup, new/unverified deployments, recovery tasks). Recovery success → `RecordSuccess`; recovery failure → `RecordRecoveryFailure`; exhausted attempts → `EnterCooldown`; sweep failures/successes → `RecordFailure`/`RecordSuccess` | `internal/probe/engine.go:467-511` (recovery task), `788-802` (sweep); cadence/selectivity policy in `docs/KNOWN_GAPS.md:194-198` ("Probe cadence and model quality") |
| Retention across config: `Retain`/`RetainProviders` drop state for removed deployments; `Invalidate` resets one deployment to `Unknown` | `internal/health/manager.go:702-731` |

Like affinity, all of this is process-local. Two replicas observing the same
failing deployment each count only their own failures, open their own
cooldowns on their own schedules, and pin sessions only to deployments they
personally served.

### 1.3 Existing shared-store building blocks (only capabilities cited are those implemented)

#### A. Control-plane `Store` abstraction (M4-1/M4-2) — the proposed carrier

| Claim | Evidence |
|---|---|
| Revision-aware KV contract: `Get`, `Put(namespace, expectedRevision, payload)`, `Delete(namespace, expectedRevision)`; sentinel errors `ErrNotFound/ErrConflict/ErrUnavailable/ErrMalformedSnapshot`; `Snapshot{Revision, Schema, Payload, UpdatedAt}`; `FailureMode{fail_closed, fail_open, last_known_good}` | `internal/controlplane/types.go:9-65` |
| PostgreSQL backend: `pg_advisory_xact_lock` per namespace + `SELECT ... FOR UPDATE`, deterministic `ErrConflict` on revision mismatch (including the first-write race), `Delete` distinguishes `ErrNotFound` (never-created namespace) from `ErrConflict` | `internal/controlplane/sqlstore.go:37-115` |
| Redis backend: one hash per namespace (`revision,schema,payload,updated_at`), atomic compare-and-set Lua scripts, decimal-string revision arithmetic so all uint64 values are exact despite Lua floats; client-agnostic via `RedisHashClient{HGetAll,Eval}` | `internal/controlplane/redisstore.go:11-63` (interface + struct + `Get`), `100-279` (Lua scripts; decimal-string rationale at `100-102`, `25-30`) |
| Explicit non-promise: "Redis is only as durable as its own configuration … this store does not promise cross-replica safety" — durability/replication/failover are operator-owned | `internal/controlplane/redisstore.go:31-34`; `docs/KNOWN_GAPS.md:156-162` |
| `Manager` layer: last-known-good cache, `Load/Save/Reconcile` (reconcile never overwrites a newer revision), `last_known_good` fallback only when configured, `Health{loads,saves,unavailable,conflicts}` counters | `internal/controlplane/manager.go:10-110` |
| Startup snapshot reconciliation over the selectable file/memory/PostgreSQL/Redis store; database backends fail closed when unavailable | `docs/KNOWN_GAPS.md:156-162` |
| Backend contract + real-service integration coverage (Postgres/Redis), adapter tests only | `internal/controlplane/sqlstore_contract_test.go`, `internal/controlplane/postgres_integration_test.go`, `internal/controlplane/redis_integration_test.go`, `internal/controlplane/redisstore_test.go`, `internal/controlplane/sqlstore_test.go` |

#### B. Security-store `Backend` abstraction (M4-2a/M4-2b) — explicitly NOT the carrier

| Claim | Evidence |
|---|---|
| Contract covers admin sessions + tamper-evident audit chain only: `CreateSession/RotateSession/Session/TouchSession/RevokeSession/AppendAudit/AuditRecords/VerifyAuditChain` | `internal/securitystore/backend.go:22-33` — `type Backend interface`; scope comment `18-21` ("only sessions and audit records live behind this interface … health, caches … remain process-local by design") |
| Fail-closed selection: unconstructable/unreachable adapter is an error, never a silent local fallback | `internal/securitystore/backend.go:55-90` (`OpenBackend`) |
| PostgreSQL serializes audit-chain writes and verifies the chain in transactions | `internal/securitystore/postgres.go:111-260` (tx + chain verify/append); `docs/KNOWN_GAPS.md:164-168` |
| Redis uses version-checked Lua compare-and-set with bounded retry (`redis concurrent modification; retry budget exhausted`) | `internal/securitystore/redis.go:321-333`, `490`; `docs/KNOWN_GAPS.md:164-168` |
| Real-service integration coverage including concurrent appends from two instances | `internal/securitystore/redis_integration_test.go`, `internal/securitystore/postgres_integration_test.go`, `internal/securitystore/backend_contract_test.go`; `docs/KNOWN_GAPS.md:164-168` |

The security store proves the team can do concurrent, revision-checked sharing
against both Postgres and Redis — but its contract (fail-closed sessions/audit
with chain verification) is the wrong semantics for data-plane routing hints,
so this design does not route breaker/affinity traffic through it.

## 2. Proposed design

### 2.1 What is shared vs what stays local

Shared (new, versioned documents over the existing `controlplane.Store`):

1. **Breaker markers** — per-deployment `{status ∈ {Cooldown, Retired}, cooldown_until (RFC3339Nano UTC), reason_class, origin_replica, observed_at, revision}` plus per-provider `{provider, cooldown_until, ...}`. Only terminal-or-bounded markers cross the wire: `Cooldown` with its deadline and `Retired` (sticky). `Healthy/Degraded/HalfOpen/Unknown` are never published: they are local inferences that flap at request cadence and would churn the store.
2. **Affinity hints** — per-bucket `{bucket, deployment, expires_at, origin_replica}` where `bucket` is exactly today's `affinityBucket()` output (hash + model + providerType + scopes + privacy class). Raw session keys never leave the process. The shared set is a bounded, TTL-scoped hint cache, not a full replica of the 10k-entry local map.

Stays local (never published, never consumed from the store):

- EWMA latency/TTFT/failure-rate gauges, `Successes/Failures/ConsecutiveFailures` counters, histograms (`internal/health/manager.go:75`, `92-109`), `RecoveryFailures`, scope-`Degraded` states, per-provider `providerEvidence` windows, in-flight quota reservations and capacity pressure (`internal/router/router.go:228-249`, `44-53`), response/semantic caches, scorecards. Rationale: these are high-cadence local signals; sharing them would multiply store writes per request and couple routing latency to store latency. Replicas keep independent gauges; only the coarse markers above are exchanged.

### 2.2 Carrier: the existing `controlplane.Store`, two new namespaces

- New snapshot namespaces (examples; exact strings fixed in Phase 1 spec):
  `nexaroute:breaker/v1` and `nexaroute:affinity/v1` as `controlplane.Store`
  namespaces backed by the already-selectable file/memory/PostgreSQL/Redis
  implementations (`internal/controlplane/types.go:34-38`). Payload is a
  versioned JSON document (`schema: 1`) mapping key → entry. No new backend,
  no new driver, no new Lua: reuse `SQLStore`'s advisory-lock + compare
  (`internal/controlplane/sqlstore.go:51-87`) and `RedisStore`'s atomic CAS Lua
  (`internal/controlplane/redisstore.go:100-279`).
- Why not the security store: its fail-closed, chain-verified audit/session
  semantics (`internal/securitystore/backend.go:55-90`, `postgres.go:111-260`)
  would deny or stall data-plane routing when the store is slow — the opposite
  of the required behavior in §2.4. The control-plane store already has the
  needed revision-conflict primitive (`ErrConflict`) and the
  `last_known_good` read pattern (`internal/controlplane/manager.go:31-56`).
- Why not a new gossip/mesh protocol: no membership, encryption, or NAT story
  exists in the codebase; adding one would dwarf the state being shared and
  has no cited building block. The store indirection keeps the zero-service
  default (no store configured → no sharing code runs).

### 2.3 Consistency model

- **Breaker markers: per-key last-writer-wins with monotonic merge under a
  revision guard.** Writers `Get` the namespace snapshot, merge locally,
  `Put` with `expectedRevision`; on `ErrConflict` re-`Get`, re-merge, retry up
  to N=3, then drop (local state is authoritative for this replica's routing).
  Merge is element-wise and monotonic:
  - `Retired` dominates everything and is never un-retired by merge (only a
    config/model-identity change invalidates it, matching today's
    `eligibleDeployment` + `Invalidate`/`Retain` behavior at
    `internal/router/router.go:310-313` and
    `internal/health/manager.go:696-710`).
  - For `Cooldown`, `max(cooldown_until)` wins; a live `Retired` beats any
    `Cooldown`; `Cooldown` never overwrites `Retired`; absence (or expiry
    `cooldown_until < now`) means "no remote opinion".
  - Malformed remote entries (`ErrMalformedSnapshot` analogues, unparseable
    timestamps, unknown status) are ignored entry-wise, never whole-document
    fatal: one bad replica must not poison routing for all.
- **Affinity hints: per-bucket LWW, lost updates acceptable.** Merge keeps the
  entry with the later `expires_at`; ties break deterministically (smaller
  `deployment` ID wins) so two replicas converging on the same snapshot agree.
  Stale entries (`expires_at < now`) are pruned on merge and on read. Because
  affinity is an optimization (eviction is already arbitrary under flood at
  `internal/router/router.go:394-403`), losing a pin to a concurrent writer is
  a cache miss, not a correctness violation.
- **Read-your-own-state:** local in-memory `Manager`/`Router` maps remain the
  routing authority on every request. Remote state is applied as advisory
  input on a bounded staleness budget (see §2.5 cadence): a remote `Cooldown`
  whose deadline already passed locally, or a remote pin for a deployment that
  is locally `Retired`/ineligible, is discarded at apply time via the existing
  `eligibleDeployment` gate — remote state can only narrow (exclude/pin
  within) the locally eligible set, never widen it. This preserves the
  fail-closed privacy gate (`RequireNoTraining` at
  `internal/router/router.go:289-291`) and priority-tier discipline
  (`orderCostAware`/`orderReadyMesh`) across replicas.

### 2.4 Failure behavior: fail toward today's single-process behavior

- Store unreachable/slow at startup: gateway starts with empty remote state
  (identical to today). No fail-closed refusal (contrast: control-plane DB
  backends fail closed at `docs/KNOWN_GAPS.md:156-162` and security stores fail
  closed at `internal/securitystore/backend.go:55-90` — this design explicitly
  does not extend that posture to the data plane).
- Store failure at runtime (timeout, `ErrUnavailable`, conflict-retry
  exhaustion, malformed payload): log + metric, keep serving from local
  `Manager`/`Router` state. Publish is best-effort background work, never on
  the request hot path; a publish failure never changes routing. Reads use the
  last successfully merged remote snapshot (local `last_known_good` in the
  sharing layer, mirroring `internal/controlplane/manager.go:31-56`) with its
  own TTL; past that TTL the replica simply acts standalone.
- Slow store: all sharing I/O happens off-request in a bounded loop with a
  short timeout and jitter; request latency never includes a store round trip.
  If p99 publish/refresh latency exceeds budget, the loop backs off and (if
  configured) disables itself until the next window — degrading to standalone
  rather than shedding data-plane traffic.
- Partition (replicas diverged): each side keeps serving with local +
  last-known remote state; on heal, monotonic merge (§2.3) converges without
  operator action. No fencing, no leader, no quorum: the shared markers are
  advisory exclusions/hints, so divergence costs extra upstream failures (as
  today), never wrong answers.
- Never fail-closed on the data plane: there is no configuration in this
  design under which "shared store unavailable" removes a locally healthy
  deployment from candidacy or rejects a request. The only store-driven
  routing effect is *additional* exclusion (a remotely cooled-down deployment
  is skipped while its deadline holds) or *additional* stickiness (a remotely
  observed pin is honored while it is locally eligible and unexpired).

### 2.5 Update cadence and bounding (no per-request store I/O)

- Publish triggers (breaker): only transitions into `Cooldown`
  (`RecordFailure` crossing threshold, `EnterCooldown`/`ForceCooldown`) and
  into/out-of `Retired`, plus periodic refresh of still-active cooldowns with
  `cooldown_until` in the future. Successes, `Degraded` flaps, EWMA updates,
  and scope-`Degraded` are never published. Expected steady-state write rate:
  ~0 plus one small document write per incident per replica — not per request.
- Publish triggers (affinity): sampled, not exhaustive — at most one hint
  batch per refresh window (e.g. 5–15s, jittered) containing at most K
  (e.g. 100) most-recently-refreshed pins. Success-path `ObserveSession`
  (`internal/httpapi/routing_helpers.go:416-421`) stays synchronous and local;
  sharing copies from the local map on the background tick.
- Refresh (consume): one `Get` per namespace per window per replica (same
  5–15s jittered tick, or the existing probe sweep interval if the
  implementation wires it there — decision deferred to Phase 1 spec, with the
  constraint that it must not increase probe traffic). Apply = merge +
  prune-expired + feed through `eligibleDeployment`/priority-tier rules.
- Size bounds: breaker document scales with deployment count (tens, not
  thousands); affinity document is top-K sampled with per-entry TTL ≤
  `routing.session_ttl_seconds` (`internal/config/config.go:1186-1187`,
  max at `1730`). Documents carry `origin_replica` (hostname/PID/UUID fixed at
  startup) and wall-clock `observed_at`; receivers never trust remote clocks
  for local deadlines beyond `min(remote_cooldown_until, now_local + max_cooldown_cap)` where the cap is today's configured `Cooldown()`
  (`internal/config/config.go:3027-3028`) — bounding the blast radius of a
  skewed replica clock.

### 2.6 Conflict/merge semantics for two concurrent replicas (worked example)

Initial: snapshot rev 7 holds `A: Cooldown until T+30m (replica-1)`.
Replica-1's deployment B fails and it publishes `B: Cooldown until T+30m`;
replica-2's deployment A recovers locally (its own `RecordSuccess` → local
`Healthy`) but has not published. Both `Get` rev 7; replica-1's `Put(rev 7 → 8)`
wins; replica-2's `Put(rev 7)` gets `ErrConflict` (`internal/controlplane/sqlstore.go:70-72`,
Redis CAS equivalent). Replica-2 re-`Get`s rev 8, re-merges: its local "A is
healthy" is *not* a publishable event (only Cooldown/Retired publish), so the
merged document keeps `A: Cooldown until T+30m` (max-wins; replica-2's local
health does not cancel a remote cooldown) plus its own new marker if any, and
retries `Put(rev 8 → 9)`. Meanwhile replica-2 still routes to A locally once
its own `CooldownUntil` passes — remote state only excludes while the remote
deadline holds and only if locally the deployment is otherwise eligible.
Affinity race: both replicas publish a pin for the same bucket with different
deployments; merge keeps the later `expires_at` (deterministic tie-break);
the loser re-reads and adopts it as a hint, subject to local eligibility. No
deadlock, no rollback: at most one retry loop of 3, then drop and continue
standalone.

## 3. Phased implementation plan (each item = one normal single-issue PR)

Conventions for every phase: docs-only exceptions none — each phase ships code
+ tests; each phase lists its acceptance tests explicitly; no phase may change
the zero-service default (sharing code paths run only when a store namespace
for sharing is configured); every phase must keep `scripts/verify.sh` green.

### Phase 1 — Cross-replica breaker markers over `controlplane.Store` (independently useful; fileable verbatim)

**Goal:** replicas skip deployments that another replica already cooled down
or retired, within the remote deadline. This alone removes the worst
multi-replica failure mode (every replica independently burning quota/latency
against the same dead deployment) and needs no later phase to be valuable.

**Build (new package, e.g. `internal/sharedstate`, plus a thin wiring):**

1. Versioned codec: `BreakerSnapshot{Schema:1, Entries map[deploymentID]BreakerEntry, Providers map[providerID]ProviderEntry}` with
   `BreakerEntry{Status, CooldownUntil, ReasonClass, OriginReplica, ObservedAt}`;
   JSON marshal/unmarshal with entry-wise leniency (bad entry skipped, rest
   honored); size cap (reject documents > e.g. 1 MiB or entries > 10k with a
   metric, never a panic).
2. Pure merge function `MergeBreaker(local-view, remote-snapshot, now)` implementing §2.3 monotonic rules (Retired sticky, max-CooldownUntil, expiry prune, malformed-entry skip, remote-clock cap via configured max cooldown).
3. Revision-guarded `PublishBreaker(ctx, store, namespace, buildLocalMarkers, retries=3)` implementing Get→merge→Put-retry on `ErrConflict`, best-effort error mapping (`ErrUnavailable`/timeout/malformed → metric + nil-effect, never an error to the caller that could fail a request).
4. Background `Refresher` (goroutine, jittered tick 5–15s, short ctx timeout, backoff-then-standalone on repeated failure) that publishes local Cooldown/Retired transitions and applies remote snapshots to the local `health.Manager` via the *existing* `EnterCooldown`/`ForceCooldown`/`Retire` entry points (remote cooldown applied as `EnterCooldown(id, "remote:<origin>", remaining)`; remote `Retired` **not** auto-applied as terminal in Phase 1 — applied as a long `EnterCooldown` capped at the configured cooldown, so a rogue replica cannot permanently retire another replica's deployments; true distributed retire is deferred to Phase 4 hardening).
5. Wiring: construct the refresher only when sharing is configured (new
   `routing.shared_state`-style config section, default off; exact field
   names fixed in the implementation issue); when unconfigured, zero behavior
   change. Metrics: `sharedstate_publishes`, `sharedstate_conflicts`,
   `sharedstate_apply_remote_cooldowns`, `sharedstate_store_unavailable`
   (names illustrative).
6. Privacy/correctness guardrails: remote state may only narrow the locally
   eligible set (applied through the unchanged `eligibleDeployment` gate);
   `RequireNoTraining` and priority-tier rules untouched.

**Acceptance tests (all must pass; representative names):**

- `TestBreakerCodecRoundTrip` — marshal/unmarshal fixture incl. unknown-field tolerance.
- `TestBreakerCodecSkipsMalformedEntry` — one bad entry (bad timestamp, unknown status) skipped, good entries honored.
- `TestMergeBreakerMaxCooldownWins` — two concurrent cooldowns for one deployment merge to later deadline.
- `TestMergeBreakerRetiredSticky` — Retired survives merge against any Cooldown; non-retire merge never un-retires.
- `TestMergeBreakerExpiryPruned` — expired `cooldown_until` dropped at merge and at apply.
- `TestMergeBreakerRemoteClockCapped` — far-future remote deadline clamped to local max cooldown.
- `TestPublishBreakerConflictRetry` — fake `Store` injecting one `ErrConflict` then success; assert second attempt re-`Get`s and merges (covers the §2.6 race).
- `TestPublishBreakerUnavailableIsBestEffort` — `ErrUnavailable` store → publish returns without error, metric incremented, local routing unchanged.
- `TestRefreshAppliesRemoteCooldownLocally` — seed remote snapshot with active cooldown for `p/m`, run one refresh tick, assert local `health.Manager.Get("p/m").Status == Cooldown` and `Candidates` excludes it under `ready_mesh`, while a second deployment stays routable.
- `TestRemoteRetiredAppliesAsBoundedCooldownNotTerminal` — remote `Retired` becomes local `Cooldown` with capped deadline, never local `Retired`.
- `TestSharingDisabledIsNoop` — default config constructs no goroutine, performs zero store calls (assert via counting fake), routing identical to today.
- Contract runs: the new namespace payload round-trips through the existing `SQLStore` and `RedisStore` revision paths (reuse `internal/controlplane/sqlstore_contract_test.go`-style cases against real services where available; miniredis/fake otherwise) — proving no new store capability is assumed.
- `scripts/verify.sh` green; no change to `COMPLETION_CHECKLIST.md`.

**Phase 1 exit:** with sharing configured and two replicas against one
Redis/Postgres, cooling down deployment X on replica A causes replica B to
skip X (under `ready_mesh`) within two refresh windows; with sharing
unconfigured, behavior is byte-identical to today.

### Phase 2 — Affinity-hint sharing (best-effort stickiness across replicas)

**Goal:** a session pinned by replica A stays pinned when the next turn lands
on replica B (when the pinned deployment is locally eligible), reducing
cross-replica session flapping.

**Build:**

1. `AffinitySnapshot{Schema:1, Hints map[bucket]AffinityHint}` with
   `AffinityHint{Bucket, Deployment, ExpiresAt, OriginReplica}`; bucket format
   identical to `affinityBucket()` output (`internal/router/router.go:328-338`).
2. `MergeAffinity` (later-`expires_at` wins, deterministic tie-break, prune expired, cap K entries).
3. Publish samples at most K most-recently-refreshed local pins per window
   (read under `sessionMu`, never holding the lock across I/O); consume applies
   hints via the existing `ObserveSession`-equivalent path only if the
   deployment is locally eligible (`Eligible(id, req)`-style check at apply or,
   cheaper, at next-request `pinned()` validation — implementation choice, but
   the privacy/priority gates at `internal/router/router.go:289-291,505-519`
   must still hold).
4. Raw session keys never serialized (bucket is already a truncated hash);
   document the privacy argument explicitly in the implementation PR.

**Acceptance tests:**

- `TestAffinityBucketFormatMatchesRouter` — codec fixture uses live `affinityBucket` vectors (incl. `no_training` class split).
- `TestMergeAffinityLaterExpiryWins` + tie-break determinism test.
- `TestAffinityHintsBoundedAndSampled` — 10k local pins → published document ≤ K entries.
- `TestAffinityHintHonoredAcrossReplicas` — replica A publishes pin, replica B applies, same `Requirement` (same session/model/scopes) routes to the same deployment when eligible.
- `TestAffinityHintNeverWidensEligibility` — hint for a locally `Cooldown`/`Retired`/privacy-ineligible deployment is ignored (matrix over statuses × `RequireNoTraining`).
- `TestAffinityUnavailableIsNoop` — store down → local affinity behaves exactly as today.
- `scripts/verify.sh` green.

### Phase 3 — Provider-level + scope-level circuit sharing

**Goal:** extend Phase 1 markers to provider circuits (`ProviderState`) and
capability scope circuits (`ScopeState`), which today also diverge per
replica.

**Build:** `ProviderEntry{Provider, CooldownUntil, ...}`,
`ScopeEntry{Deployment, Scope, CooldownUntil, ...}` in the same breaker
namespace (schema bump to 2 with backward-tolerant reader: v1 documents still
parse); publish on `RecordProviderFailure`-driven cooldowns and
`RecordScopeFailure`-driven scope cooldowns (thresholds from
`internal/config/config.go:1210-1258`); apply via existing
`RecordProviderFailure`-equivalent cooldown entry and scope-cooldown apply
path; same monotonic max-wins merge and clock cap (provider cap =
`ProviderCooldown()`, scope cap = capability cooldown).

**Acceptance tests:** `TestMergeProviderCooldownMaxWins`,
`TestProviderCooldownAppliesAndExpires` (local `ProviderAvailable` flips and
recovers), `TestScopeCooldownAppliesPerScope` (only the cooled scope excluded
under `ready_mesh`; other scopes routable — mirrors
`internal/router/router_test.go:304-307` scope setup),
`TestSchemaV1BackwardCompatible`, `scripts/verify.sh` green.

### Phase 4 — Hardening, observability, and multi-replica proof

**Goal:** make sharing operable and mark honestly what remains unverified.

**Build:**

1. Admin/debug surfaces: read-only snapshot-inspection endpoint (or CLI dump)
   showing local vs last-remote breaker/affinity views + `Health()`-style
   sharing counters (model on `internal/controlplane/manager.go:106-108`);
   structured events on publish/apply/conflict/unavailable (following the
   existing `recovery_cooldown`/`model_cooldown` event pattern at
   `internal/probe/engine.go:507-509`).
2. Metrics/dashboards + alerts (conflict rate, staleness age, unavailable
   ratio); config validation for the new section (bounds on tick, timeout, K,
   namespace strings — same style as `internal/config/config.go:1730-1754`).
3. Optional backup/restore inclusion of the two namespaces (extends the M4-3
   `nexaroute backup/restore` application-level mechanism, explicitly not a
   durability upgrade beyond today's documented posture).
4. Two-replica compose harness (gateway × 2 + Redis/Postgres) with a scripted
   incident (kill deployment X, observe both replicas skip within N windows;
   partition the store, observe standalone-degraded serving, heal, observe
   convergence) — kept out of default CI if flaky-prone, but runnable via an
   explicit script like the existing `scripts/stress.sh`/`scripts/soak.sh`
   opt-ins noted in `docs/KNOWN_GAPS.md:313-326`.
5. Docs: update `docs/KNOWN_GAPS.md` State-and-HA + Routing-boundaries entries
   to describe what is now shared, with the remaining bounds from §4 below.

**Acceptance tests:** admin-surface golden test, metrics-counter test,
config-validation table test, harness run evidence attached to the PR (logs
showing cross-replica skip + partition-standalone + heal-converge),
`scripts/verify.sh` green.

## 4. Explicit non-goals and risks

**Non-goals (will not be built in this track):**

- Exactly-once delivery, total ordering, consensus/leader-election, or
  quorum reads. The design is AP-hints, not CP-state: brief divergence is
  accepted and bounded by cooldown TTLs.
- Sharing EWMA gauges, counters, histograms, quota reservations, caches,
  scorecards, OIDC pending transactions, or video-gateway job/ledger state —
  all stay process-local per their existing documented boundaries
  (`internal/securitystore/backend.go:18-21`, `docs/KNOWN_GAPS.md:154-268`).
- True distributed `Retired` (terminal across replicas) — Phase 1 deliberately
  demotes remote-retire to a bounded cooldown; permanent cross-replica
  retirement needs human/operator action or a later agreed-upon authority
  protocol.
- Gossip/mesh transport, new backends/drivers, new crypto, or changes to the
  `controlplane.Store`/`securitystore.Backend` contracts. New namespaces only.
- Any change to the zero-service default: unconfigured sharing must be
  unobservable in behavior, metrics cardinality, and goroutine count.

**Risks and mitigations:**

- Store latency/availability coupled to routing freshness → mitigated by
  off-hot-path background I/O, short timeouts, jitter, backoff-to-standalone,
  and the never-fail-closed rule (§2.4).
- Thundering-herd re-admission when a shared cooldown expires simultaneously
  on all replicas → mitigated by jittered refresh, existing `HalfOpen`
  semantics (`internal/health/manager.go:347-349`), and probe-gated recovery
  (`internal/probe/engine.go:450-512`); residual herd is no worse than today.
- Clock skew across replicas inflating remote cooldowns → mitigated by the
  observation-time + max-cooldown cap (§2.5); document the cap as the
  worst-case exclusion window.
- Write amplification / store cost → bounded by incident-only breaker writes
  and K-sampled affinity batches; counters in Phase 1 prove the rate.
- Privacy leakage via affinity → mitigated structurally: buckets are truncated
  hashes already (`internal/router/router.go:328-338`); raw keys never
  serialized; `no_training` class separation preserved in the bucket string.
- Poisoned snapshots (buggy/malicious replica) → entry-wise leniency +
  status allowlist + clock cap + remote-retire demotion; a bad writer can at
  worst cause bounded over-exclusion, never request rejection or widening.
- Scope creep into fail-closed posture → explicit rule: any reviewer may veto
  a sharing-path change that denies a request or removes a locally healthy
  deployment solely because the store is unavailable.

**What stays UNVERIFIED without a real multi-replica deployment:**

- Convergence time and skip-rate improvement under realistic incident shapes
  (single-deployment outage, provider-wide outage, flapping deployment).
- Behavior under store partition (divergence window, heal convergence,
  duplicate-publish storms on reconnect).
- Clock-skew impact beyond unit-test caps; wall-clock vs monotonic interactions.
- Performance overhead (background I/O, merge CPU, document sizes at fleet
  scale) and Redis/Postgres sizing for the two namespaces.
- Live external-IdP/session interactions are unaffected by construction (admin
  sessions stay in the security store), but multi-instance admin behavior
  remains bounded by the existing WP4/M4-2 statements, not re-proven here.
- Deployment-level persistence/replication/failover/recovery remain
  operator-owned per `docs/KNOWN_GAPS.md:156-177`; this design adds shared
  routing hints, not durable HA.

## Appendix: key file+type index

- `internal/router/router.go` — `type sessionPin`, `const maxSessionPins`, `type Router`, `func affinityBucket/pinned/PinnedDeploymentID/ObserveSession/SessionCount/Reload`, `func eligibleDeployment/orderCostAware/orderReadyMesh/Candidates/Eligible`, `func healthRank/capacityPressure`, `type Requirement/ProviderLoad/Scored/Deployment`
- `internal/health/manager.go` — `type Status/State/ScopeState/ProviderState/Manager`, `func RecordSuccess/RecordFailure/RecordScopeSuccess/RecordScopeFailure/RecordProviderSuccess/RecordProviderFailure/EnterCooldown/ForceCooldown/Quarantine/RecordRecoveryFailure/Retire`, `func GetWithScopes/Get/ProviderAvailable/ScopesReady/Snapshot/ProviderSnapshot/Retain/RetainProviders/Invalidate`, `func Configure/ConfigureAdvanced/ConfigureProviderIncidents`
- `internal/config/config.go` — `Routing{Strategy, SessionAffinity, SessionTTLSeconds, FailureThreshold, CooldownSeconds, ProviderFailureThreshold/ProviderFailureWindowSeconds/ProviderCooldownSeconds, CapabilityFailureThreshold/CapabilityCooldownSeconds}`, defaults `~915-931,1186-1258`, validation `~1730-1754`, `func Cooldown/ProviderCooldown`
- `internal/httpapi/routing_helpers.go` — `func sessionKeyFromRequestParts/prepareRequirement/recordRouteSuccess/recordProviderFailure`, `type upstreamFailurePolicy`
- `internal/httpapi/openai.go`, `internal/httpapi/anthropic.go`, `internal/httpapi/canonical_path.go`, `internal/httpapi/media.go` — per-attempt failure/success recording call sites
- `internal/probe/engine.go` — recovery-task and sweep recording (`RecordSuccess/RecordRecoveryFailure/EnterCooldown/RecordFailure`)
- `internal/controlplane/types.go` — `type Store/Snapshot/FailureMode/Availability/ReconcileResult/Health`, `ErrNotFound/ErrConflict/ErrUnavailable/ErrMalformedSnapshot`
- `internal/controlplane/sqlstore.go`, `internal/controlplane/redisstore.go`, `internal/controlplane/manager.go` — revision-guarded backends + last-known-good layer
- `internal/securitystore/backend.go` — `type Backend`, `func OpenBackend` (fail-closed; not used by this design)
- `docs/KNOWN_GAPS.md` — State and HA + Routing boundaries (problem statement); probe cadence; stress/soak opt-ins
