# HA acceptance follow-up — 2026-10-28 (issue #652, follow-up to PR #679)

## Status line

- Issue #652 stays OPEN. This report is same-host multi-process evidence only.
  Genuine multi-host acceptance is BLOCKED_EXTERNAL (no second host, no
  container runtime, no network-namespace permission in this sandbox).
- No merge, no push, no tag, no release was made from this work. All commits
  below are local-only on `supervisor/issue652-ha-followup`.
- No production system was touched. All services below are disposable:
  PostgreSQL 16 (apt) on 127.0.0.1:55433, Redis (apt) on 127.0.0.1:6390,
  stub upstream on 127.0.0.1:19091, gateway processes on 127.0.0.1:18091–18093.
  No credentials, DSNs with secrets, client keys, or tokens appear in this report.

## Code under test

- Base: PR #679 head `5f8a17c` (OLD binary).
- Follow-up branch `supervisor/issue652-ha-followup`, local commits:
  - `b13af4c` — honest 404/503 mapping + four multi-process cold-open tests.
  - `c8c6776` — procedure amendments (multi-process tests, outage probes,
    rolling upgrade, Redis operator constraint).
  - `1c3a591` — securitystore cold-open test fixture fix (session
    issuer/expiry, audit actor/timestamp). Test-only change.
- NEW binary built from `1c3a591`. Both binaries report `NexaRoute vdev`.
- Worktree: `~/workspace/ha-652-followup` (repo files only; run artifacts live
  outside the repo under `~/workspace/ha-652-followup-run/` and are not committed).

## 1. Migration/bootstrap audit verdict

All five PostgreSQL schema-creation sites are advisory-lock guarded. The
follow-up suspicion that the security store and video stores were unguarded
did NOT hold on first-hand read; they were already guarded.

| Site | Lock (first statement) | Scope |
| --- | --- | --- |
| `internal/controlplane/migrator.go` | session `pg_advisory_lock(hashtext('nexaroute:migrations'))`, held on a dedicated connection for the whole `ApplyMigrations`, acquired before the first `CREATE TABLE` (#679 fix) | per-database |
| `internal/controlplane/sqlstore.go` (`ControlPlaneSQLSchema`) | used only as migration v1 `Up`, i.e. under the migrator lock; `Put` uses per-namespace `pg_advisory_xact_lock(hashtext(ns))` | per-database |
| `internal/securitystore/postgres.go` `migrate()` | `pg_advisory_xact_lock(0x4e58534543555245)` as the first statement of a single transaction holding all 3 `CREATE TABLE`s + allocator seed | per-database |
| `internal/video/queue/pg_store.go` `migrate()` | xact lock `0x4e4558415649444a` first in transaction, then table + unique index DDL | per-database |
| `internal/video/cost/pg_ledger.go` `migrate()` | xact lock `0x4e455841434f5354` first in transaction | per-database |

Lock-scope analysis:

- PostgreSQL advisory locks are per-database: unrelated databases never
  serialize on each other.
- The four component keys are distinct int64 values, so components do not
  serialize each other. The `hashtext` key is int32-range and cannot collide
  with the `0x4e…` int64 keys.
- Redis has no DDL (hashes are implicit). Operator constraint (documented in
  the procedure, not a code defect): two deployments sharing one Redis DB
  would share state, because control-plane and security prefixes are fixed
  (`nexaroute:control:snapshot:`, `nexaroute:security:{meta,audit,sessions}`).
  Deployments must use separate Redis URLs/DB indexes.
- The gateway-level test `TestConcurrentPostgresControlPlaneBootstrapConverges`
  is sequential in-process opens, not a concurrent cold start; the
  process-level proof is the new tests below plus staging scenario S1.

## 2. Error-mapping audit verdict + three new defects fixed

Already correct on #679 head: video `get`/`getAsset` job lookup
(`storeGetError`: only `queue.ErrNotFound` → 404, else generic 503
`video_store_unavailable`); admin force-fail (ErrNotFound → 404, terminal →
409, default → 503); video `list` store error → 500 fail-closed; control-plane
stores (`ErrUnavailable` + `wrapStoreError`); eval-scorecard and admin_reveal
and client_keys 404s are genuine config/snapshot lookups, not persistence
reads. Upstream-resolve 404s are out of scope (not persistence reads).

New defects found first-hand and fixed in `b13af4c`
(new sentinel `orchestrator.ErrStoreUnavailable` + `wrapStoreErr` in
`internal/video/orchestrator/job_ops.go`, wrapped at store/ledger-originated
points in Create and Cancel; handler maps ErrNotFound → 404,
ErrStoreUnavailable → generic retryable 503 `video_store_unavailable`,
business errors unchanged 400; `getAsset`: `os.IsNotExist` → 404, else 503
`asset_unavailable`):

1. Video create: every error → 400 with `err.Error()` verbatim, so a store
   outage returned 400 and leaked backend detail.
2. Video cancel: missing job → 400 (should be 404); outage → 400 + detail.
3. `getAsset` file open: ANY open failure → 404, so disk I/O or permission
   failures masqueraded as a deleted asset.

One existing test was updated (cancel-missing pinned 400 → now expects 404).
Regression tests: orchestrator `store_error_test.go` (4 tests) and api
`store_error_test.go` extensions (create 503 with no backend detail, cancel
missing 404, cancel store-failure 503, asset storage-failure 503,
asset missing-file 404). Suites pass plain and under `-race`
(orchestrator, api).

## 3. Integration tests with real disposable services (local)

PostgreSQL: apt PostgreSQL 16, disposable database; Redis: apt redis-server,
disposable DB index 15. Go 1.27.0. All runs used the exact CI `-run`
patterns, so no workflow change is needed for CI to pick the new tests up.

| Suite (CI pattern) | Result |
| --- | --- |
| controlplane `TestPostgresIntegrationColdDatabaseConcurrentMigrationsMultiProcess` (NEW, two independent processes, fresh isolated schema, exactly 1 migration row + idempotent re-apply) | PASS (0.19s socket DSN; also PASS in the full-package run) |
| securitystore `TestPostgresSecurityStoreIntegrationConcurrentColdOpen` (NEW, two processes, session + audit round-trip) | PASS (0.40s) after the `1c3a591` fixture fix |
| video/queue `TestPostgresVideoStoreConcurrentColdOpen` (NEW, two processes, job round-trip) | PASS (0.32s) |
| video/cost `TestPostgresLedgerConcurrentColdOpen` (NEW, two processes, entries == 0) | PASS (0.31s) |
| controlplane `TestPostgresIntegration` (incl. existing cold test, two-instances-and-restart, concurrent-first-write, idempotent-checksum) | PASS except the outage test below |
| controlplane `TestPostgresIntegrationOutageRecoveryCancellationBackpressure` | PASS on a TCP DSN (0.20s). It FAILS on a socket DSN (`parse integration URL host: <nil>`) because the test builds a TCP fault proxy from the DSN host — an environment/DSN-form limitation, not a code regression. CI uses a TCP DSN, so CI is unaffected. |
| securitystore `TestPostgresSecurityStoreIntegration` | PASS (2.12s) |
| sharedstate `TestPostgresBreakerStoreIntegration` | PASS (0.22s) |
| video/queue `TestPostgresVideoStore`, video/cost `TestPostgresLedger` | PASS (1.54s, 1.22s) |
| breakersync `TestBreakerSyncPostgresTwoInstances` | PASS (0.30s) |
| cmd/gateway `TestConcurrentPostgresControlPlaneBootstrapConverges` | PASS (0.24s) |
| controlplane `TestRedisIntegration`, sharedstate `TestRedisBreakerStoreIntegration` | PASS (3.29s, 0.03s) |
| breakersync `TestBreakerSyncRedisTwoInstances`, cmd/gateway `TestControlPlaneRedisIntegration` (plain + `-race`, per `integration-redis.yml`) | PASS (0.01s, 0.05s, 1.22s race) |
| video orchestrator + api suites (plain + `-race`) | PASS |

Not run locally: full `scripts/verify.sh` (pre-merge gate; CI runs it on a
pushed branch — push needs the owner's word). No CI run exists for the new
commits because nothing was pushed.

## 4. Staging drill (same VM, three gateway processes, shared services)

Topology (honest label): TWO gateway processes on ONE VM host (plus a third
for the restore read-back), separate data dirs, separate listen ports
(A 18091/replica-a, B 18092/replica-b, C 18093/replica-c), shared TCP
PostgreSQL + shared Redis over loopback, stub upstream in `ok` mode except
where noted. NOT physical multi-host. Times are UTC; all timings are
single-run observations on this VM, not SLOs. A fresh disposable client key
and video token were generated for this drill; neither is recorded here.

### S1 — Concurrent cold start (the #679 defect scenario)

- Empty database `nexaroute_ha652_drill`. A + B (NEW binary) launched
  simultaneously at 2026-10-28T12:19:54.618Z.
- Both `/readyz` 200 at the first poll ~3s after launch. No migration race
  recurrence.
- After start: 7 tables (controlplane 2, security 3, video 2),
  `nexaroute_control_migrations` = exactly 1 row.

### S2 — Baseline

- `/healthz` 200 on A and B. Chat via A and via B both returned the stub
  completion (`stub-ok`). Stub hit counter 30 at this point (includes gateway
  on-start capability probes, so the counter is NOT a clean
  requests-forwarded metric — see S3).

### S3 — Breaker sync: INCONCLUSIVE from hit counts, one confirmed asymmetry

- Stub switched to `fail` mode. A: first request 500 (stub), then 503
  fail-fast. After an 8s wait (> 5s sync interval), B: first request 500
  (reached the stub), then 503. Hit counts moved +6/+6 across the two
  request batches, but gateway probes also increment the same counter, so
  zero-new-hits fail-fast could not be proven from counts.
- Decisive evidence came from the shared snapshot row instead:
  `nexaroute:breaker/v1` held
  `{"stub/stub-model": {"status": "cooldown",
  "cooldown_until": "2026-10-28T12:35:24Z",
  "reason_class": "provider_incident", "origin_replica": "replica-b"}}`.
  Cross-replica propagation DOES work: after restart, A (peer) enforced
  B's entry and returned 503 on chat/`/readyz`
  ("no verified healthy model deployments in ready queue") even though its
  own startup probe reported ready=1.
- Asymmetry (pre-existing breaker-sync behavior, not a #679 regression):
  after restart, B — the origin replica of the entry — served 200 while A
  still served 503 on the same shared entry. The origin replica does not
  re-apply its own shared cooldown after restart; a peer does. This needs a
  breaker-sync owner decision; it is reported here, not fixed here.
- The cooldown lasted 15 minutes (900s recovery cooldown), not the 10s
  provider cooldown. A brief stub failure therefore latched chat on peer
  replicas for 15 minutes. Operators should know this before relying on
  fast failover for chat traffic.

### S4 — Replica SIGKILL

- B was SIGKILLed at 2026-10-28T12:21:40Z while A was unhealthy from S3, so
  the first kill attempt could not assess survivor serving (A was already
  503 for the breaker reason above, not because B died).
- Clean measurement: A SIGKILLed at 2026-10-28T12:22:49.449Z; B returned
  `/readyz` 200 and chat 200 at 1s after the kill (2026-10-28T12:22:50.458Z).
  The survivor never dropped. Row counts unchanged
  (migrations 1, snapshots 2, sessions 0, audit 0, jobs 0) — no duplication
  or loss from the kill. Direct-port probes only; load-balancer detection
  time is NOT included in this RTO.

### S5 — Video job across replicas; assets are NOT shared

- Create via B with the fake provider (`fake`/`fake-video-1`): job
  `video-1793190189309663466` created (a create without a provider
  preference correctly failed 400 `no eligible video provider` — business
  error, not a store error). Job completed (progress 1), PG row count 1.
- Job metadata GET: 200 via B and 200 via A (shared PG row readable
  cross-replica). Missing job GET via B: genuine 404.
- Asset `fake-1-output`: 200 via B (bytes served from B's disk).
  Via A: 503 `asset_unavailable` (generic, retryable, no leak) — NOT 404,
  because the stored asset path lives under B's storage root and fails A's
  storage-root containment check. Video assets remain per-instance local
  disks; they are NOT shared across nodes. Do not claim otherwise.

### S6 — PostgreSQL outage and recovery WITHOUT gateway restart

- Before: jobs 1, cost reservations 1, migrations 1, audit 0.
- PG stopped (`pg_ctl stop -m fast`) at 2026-10-28T12:23:55Z. Probes with the
  NEW binary (the fixed mapping):
  - video GET (existing job) → 503 `video_store_unavailable`, generic body,
    retryable. (Before the fix, reads were already 503; creates/cancels were not.)
  - video CREATE → 503 (was 400 + backend detail before the fix).
  - video CANCEL → 503 (was 400 before the fix).
  - video ASSET → 503.
  - video GET (missing job) during the outage → 503, not a false 404:
    during an outage the gateway cannot distinguish missing from unreachable.
  - No response body contained SQL, DSN text, or dial/connectivity detail.
- PG restarted; gateways NOT restarted. First probes ~3s after PG start:
  video GET 200 via B and via A. Counts identical after recovery
  (jobs 1, reservations 1, migrations 1, audit 0).
- RPO for committed data under a clean PG stop/restart: 0. RTO to first
  successful shared-state read: a few seconds from PG start (first-probe
  bound, not a measured-to-the-second value). Reconnection without gateway
  restart is confirmed for the video store path.

### S7 — Rolling upgrade (one replica at a time)

- NEW → OLD (one replica, then the other) → NEW (one replica, then the
  other). Video reads returned 200 on both replicas at every step.
- Counts stable throughout (jobs 1, reservations 1, migrations 1): no
  duplicate migration rows across binary swaps. Rolling upgrade continuity
  holds for this OLD↔NEW pair on a shared database.

### S8 — Backup and restore

- `nexaroute backup` for instance B wrote a 9,216-byte bundle (config only
  in this topology — file stores are PG-backed here). The bundle alone is
  NOT a restore: `pg_dump` of the drill database was also taken (9,540
  bytes, exit 0).
- `pg_dump` restored into a fresh database `nexaroute_ha652_restored`
  (exit 0): jobs 1, migrations 1, reservations 1.
- Gateway C (NEW binary, restored DB, port 18093) started cleanly:
  `/healthz` 200, video GET for the drilled job 200. Restore read-back
  verified end-to-end.

### Redis outage probe (topology fact, not a failover claim)

- Redis on 6390 was stopped while B served: `/healthz` 200, video GET 200.
  Redis is NOT a configured backend in this topology (control-plane,
  security, and video all use PostgreSQL; the Redis URL env var is set but
  unused), so this proves only that a Redis outage does not affect this
  PG-backed topology. It proves nothing about Redis failover. Redis
  failover evidence is at the store-integration level (§3), not the gateway
  level.

### Not demonstrated live in this drill (honest gaps)

- Gateway-level admin session/audit persistence across restart: no admin
  login was performed in this drill, so session/audit row counts stayed 0
  throughout. Store-level evidence exists (securitystore integration suite
  and the new multi-process test, which round-trip a session + audit event
  through PostgreSQL), but gateway-level login persistence was NOT
  exercised live here.
- Genuine multi-host topology, load-balancer failover timing, long soak:
  not run (see BLOCKED_EXTERNAL).
- Session/affinity and OIDC cross-replica behavior: OIDC was not enabled in
  this topology; per-instance behavior is documented in
  `docs/HA_STATE_INVENTORY.md`, unchanged by this work.

## 5. RPO / RTO summary (observed only)

| Quantity | Observed value | Method / limit |
| --- | --- | --- |
| RPO, clean PG stop/restart | 0 committed records lost | PG row counts before/after S6 identical |
| RPO, replica SIGKILL | 0 | Row counts before/after S4 identical |
| RTO, replica kill (survivor) | Survivor never dropped (200 at 1s after kill) | Direct-port probes; LB detection NOT included |
| RTO, PG outage → first successful read | A few seconds from PG start, no gateway restart | First-probe bound (~3s sleep + probe); not sub-second measured |
| RTO, chat after stub failure (peer replicas) | 15-minute shared breaker cooldown (until 12:35:24Z) | Shared snapshot row; this is the dominant chat RTO in this topology, not the PG RTO |
| Duplicated work | None observed | Migration rows, job rows, reservation rows all count-stable across cold start, kills, outage, rolling upgrade, restore |

## 6. Remaining single-instance dependencies (unchanged)

- Video assets on per-instance local disks (S5: B 200, A 503).
- Per-instance config files (a config edit on one replica does not reach
  the other; verified by construction — configs differ only in per-instance
  fields).
- File-store backends, where used, are single-host by design.
- No load balancer was in the topology; failover timing through a real LB
  is unmeasured.

## 7. What still needs the owner's word

- Push the follow-up branch / open a PR so CI runs the new multi-process
  tests and full `verify.sh` on `1c3a591`. Nothing was pushed here.
- Breaker-sync asymmetry (§S3): origin replica not re-applying its own
  shared cooldown after restart — owner/maintainer decision, out of #679 scope.
- #652 stays OPEN until genuine multi-host acceptance exists.
