# Multi-host HA acceptance — staging run, 2026-10-27

Related issue: #652. Base commit: `e6c3cdf3fe0a193dda0c5857e91e46b541d04729`
(main at run start). This report is evidence, not a claim: read the
topology and BLOCKED sections before quoting any result.

## Verdict in one paragraph

Same-VM staging with a shared PostgreSQL backend passes the
replica-kill, backend-outage, restart, shared-job/ledger, and
database-backup-recovery scenarios below, with two reproducible
defects found and fixed by the accompanying PR (cold-start
migration race; video job reads returning 404 during a backend
outage). **Physical multi-host acceptance is NOT established by
this run and stays BLOCKED** — see §1. Issue #652 must not be
closed on this report alone.

## 1. Topology — what this run was, and was not

- One VM (2 CPUs, ~8 GB RAM), all processes on that host.
- Two gateway processes, separate data directories, separate
  listen ports, built from the base commit with Go 1.27.0
  (`nexaroute -version` reports `vdev`; the binary was built
  from the exact base commit in a clean worktree).
- Shared services on the same host, reached over 127.0.0.1:
  PostgreSQL 16.15 (disposable database created for this run)
  and Redis 7.0.15 (no persistence, disposable only).
  The gateways used PostgreSQL for the control plane, the
  security store, and the video job store / cost ledger.
  Redis was present in the environment but was NOT a
  configured gateway backend in this topology.
- Upstream provider: a local loopback stub HTTP server with a
  hit counter and an `ok`/`fail` mode file. No real provider,
  no paid call, no production service, no production data.
- Credentials and tokens in this run were disposable test-only
  values. They are deliberately not reproduced in this report.

Why this is not multi-host: no second host was available, and
no container or VM isolation was available either — no
Docker/Podman, no `/dev/kvm`, and `ip netns exec` failed with
`Operation not permitted` (setns) in this sandbox, so even
network-namespace-separated nodes could not be created.
Two processes on one VM prove shared-store behaviour across
processes, not behaviour across hosts, networks, or failure
domains. A genuine two-host run (separate hosts or containers,
shared PostgreSQL/Redis reachable over the network, a load
balancer in front) is still required and is BLOCKED on
staging prerequisites, not on gateway code.

## 2. Timing methodology and limitations

- All times below are single-run observations, recorded from
  shell timestamps (`date -u`, gateway log timestamps in
  local time, +0430) and HTTP probe outputs. They are not
  SLOs, averages, or guarantees.
- RTO (replica kill) = time from the kill command to the
  first successful `/healthz` from the restarted replica.
  Probes were direct-port (no load balancer), so load-balancer
  detection and drain time are NOT included in any RTO here.
- RTO (backend outage) = time from the backend accepting
  connections again to the first successful shared-state read.
  The first PostgreSQL restart attempt in this run was
  interrupted by a runtime session kill, so no RTO is claimed
  for that attempt; the recovery boundary that is claimed is
  the cessation of 5-second breaker-sync errors and the first
  successful job read after the clean restart.
- RPO = committed records present after recovery, counted in
  PostgreSQL, versus records attempted during the outage.
  Committed here means the record was already written before
  the outage started. No in-flight request was in progress at
  the moment PostgreSQL was stopped, so in-flight loss during
  a backend outage was not measured.
- `/healthz` is liveness only (`{"ok":true}`); `/readyz`
  requires a verified healthy model deployment. A replica can
  be healthy (200 on `/healthz`) while correctly unready
  (503 on `/readyz`) when its provider is tripped. Conflating
  the two would misread every trip/recovery result below.
- A short single run is not a soak. No long-duration,
  multi-hour, or load soak was performed.

## 3. Scenarios and observations

### S1 — Concurrent cold startup: DEFECT (fixed by this PR)

Both gateways were launched simultaneously against an empty
database. One failed at startup and exited:

```
cannot initialize control-plane runtime: apply control-plane
migrations: create migration table: ERROR: duplicate key value
violates unique constraint "pg_type_typname_nsp_index" (SQLSTATE 23505)
```

The other gateway started successfully and created the
schema. A sequential restart of the failed replica then
succeeded. Root cause: `ApplyMigrations` ran
`CREATE TABLE IF NOT EXISTS` for the migration table before
acquiring the advisory lock that its own comment says
serializes migrations; two concurrent creators can collide
in PostgreSQL's type catalog. The existing concurrent-migration
test only re-applied migrations to an already-migrated
database, so it never exercised the cold path. Fix: acquire
the advisory lock first, then create the table; regression
test `TestPostgresIntegrationColdDatabaseConcurrentMigrations`
runs two concurrent applies against a cold schema. The
security-store and video-store schemas also use unguarded
`CREATE TABLE IF NOT EXISTS` on startup; that same-class race
was not observed in this run and is not fixed here — it is a
documented follow-up risk, not a claimed fix.

A staging correction, not a defect: the first configs had
probing disabled, so both gateways returned 503 on `/readyz`
and on chat ("no verified healthy model deployments"). With
probing enabled (`on_start`, as in the example config), both
gateways reported `/readyz` 200 with 1 usable deployment.
Probing is required for a deployment to become usable; the
initial 503s were a configuration error in the run setup.

### S2 — Baseline serving through both replicas: PASS

With the stub in `ok` mode, a chat completion through each
replica returned 200 with the stub payload, and both
replicas' `/api/hello` deployment reports agreed:
`store_topology: shared`, `control_plane_backend: postgres`,
`security_store_backend: postgres`,
`config_snapshot: per-instance`, `audit_security_store: shared`,
`breaker_cooldown: shared` (breaker sync enabled),
`quota_reservations: per-instance`,
`session_affinity: per-instance`,
`video_jobs_ledger: shared`, `request_idempotency: per-instance`,
plus the per-instance warnings for config, quota, and assets.

### S3 — Breaker-sync convergence and fail-fast: PASS, with a confounder

Stub switched to `fail` mode at 16:10:21 UTC. Replica A
tripped after its failure threshold (first client request
returned the stub 500, subsequent requests 503 "no compatible
healthy deployment"); stub hits went from 30 to 32 for the
two client-visible failures. After a 13-second wait (more
than two 5-second sync intervals), a client request through
replica B returned 503 with the stub hit counter bracketed
at 36 immediately before and immediately after the request,
and still 36 after a further 3 seconds: B failed fast
without reaching the stub. Note the confounder that makes
unbracketed counting misleading: during the 13-second wait
the counter rose from 32 to 36 from probe/recovery traffic,
not from client requests. Only the bracketed before/after
count proves the client request did not reach the stub.

Healing was NOT measured to completion in this run: after
the stub returned to `ok` at 16:11:07 UTC, both replicas
were still 503 after 30 seconds with the hit counter
unchanged at 36. The example config's probe recovery
cooldown is 900 seconds, so full probe recovery was not
expected inside this run's window. No heal RTO is claimed.
Restart recovery (S4) is the recovery path actually measured.

### S4 — Replica kill and restart: PASS (health RTO measured)

- Kill (SIGKILL) of replica B: 16:11:56.967 UTC.
- Replica A `/healthz` 200 at 16:11:57.977 UTC (~1.0 s
  later, including a 1 s sleep in the probe loop); B
  connection refused as expected.
- B restart command: 16:11:58.335 UTC; B `/healthz` 200
  at 16:12:06.353 UTC. Health RTO: 8.0 s from the restart
  command, 9.4 s from the kill. B `/readyz` was still 503
  at that moment — correct, because the provider was still
  tripped (see S3), not because B had failed to restart.
- B's startup probe initially reported ready, then breaker
  sync re-applied replica A's still-published retired marker
  and B returned to 503. A restarted replica does not stay
  healthy while the origin replica keeps publishing a
  retired marker; that is the documented bounded-cooldown
  behaviour, not a restart failure.

### S5 — PostgreSQL outage and recovery: PASS for data, DEFECT for reads

- PostgreSQL stop command: 16:18:59.825 UTC, returned
  16:19:15.451 UTC (15.6 s, waiting for connections).
- During the outage, both gateways stayed alive:
  `/healthz` 200 on both, `/api/hello` 200 on both — but
  the hello report is a static configuration report, not
  proof of database connectivity, and must not be read as
  one. Breaker sync logged a loud error every 5 seconds
  (`shared breaker state store unavailable`) and never
  cleared local breaker state.
- DEFECT (fixed by this PR): during the outage, reading the
  existing video job through either replica returned
  404 `not_found`, although the job existed. The video
  handler mapped every store `Get` error to 404, including
  a wrapped PostgreSQL connection error, while the list
  endpoint already failed closed with 500. Fix: the
  handler now returns 404 only for `queue.ErrNotFound`
  and a retryable 503 `video_store_unavailable` (with no
  backend detail in the response) for any other store
  error, for both job reads and asset lookups. Regression
  tests cover the 503 path and the unchanged 404 path.
- Recovery: after a clean PostgreSQL restart (accepting
  connections at 16:24:17 UTC), the last breaker-sync error
  was 16:24:13 UTC and the first post-recovery job reads
  succeeded immediately afterwards on both replicas.
  PostgreSQL row counts were unchanged across the outage:
  1 video job, 2 control-plane snapshots, 1 cost
  reservation before and after. RPO for committed data
  under a clean service stop/restart: 0 records lost.
  No gateway process was restarted during the outage.

### S6 — Redis outage: NOT APPLICABLE to this topology

Redis was stopped (16:25:14–16:25:16 UTC) and restarted
(PONG at 16:25:19 UTC). Both gateways were unaffected
(`/healthz` 200, job read `completed` on both) — expected,
because Redis was not a configured backend here. This
proves nothing about Redis-backend failover; that behaviour
is covered only by the store integration tests (§5), not
by this gateway run.

### S7 — Shared video job, ledger, and assets: PASS for jobs/ledger, documented limit for assets

A video job created through replica A completed in under
one second (created 16:15:49.71 UTC, completed 16:15:50.10
UTC, fake provider placeholder):

- Job id `video-1793117749711956564`, read back through
  replica B with identical state, provider job id, and
  lease owner (replica A's process). Exactly one row for
  the job in PostgreSQL and exactly one cost reservation
  (estimate $0.04, `cost_reconciled: false` for the fake
  provider — a statement about the fake provider, not a
  settlement claim).
- Duplicate claim and duplicate settlement prevention were
  verified at the store level by the integration tests in
  §5 (concurrent claim, concurrent duplicate reserves,
  duplicate/conflicting settlement), not by a second
  live job race in this run.
- Asset locality, as documented: the completed job's
  placeholder asset downloaded 200 through replica A and
  404 `asset_not_found` through replica B, whose local
  asset directory was empty. Job metadata is shared;
  asset bytes are per-instance. Multi-replica video is
  therefore not fully coherent without shared asset
  storage, exactly as the HA inventory states.

### S8 — Backup and restore: PASS only as a pair, BLOCKED alone

- `nexaroute backup` for replica A succeeded and produced
  an 8,704-byte bundle containing only `manifest.json`
  and `config.json` — no security-store snapshot and no
  control-plane state, because both live in PostgreSQL.
  The application bundle alone does not restore a
  PostgreSQL-backed installation.
- A `pg_dump` of the shared database (258 lines) was the
  real shared-state backup. The first restore attempt
  failed for an operator reason, not a product reason:
  the database user could not read the dump file from a
  workspace directory with mode 2770 owned by another
  group. After correcting the invocation, the restore
  completed with exit 0 and row counts matching the
  source exactly (1 job, 2 snapshots, 1 reservation).
- A third gateway started against the restored database
  reported `/healthz` 200 and read the job back as
  `completed` with the original lease owner; its asset
  download correctly returned 404 (asset bytes were never
  in the database).
- `nexaroute restore --verify` on the application bundle
  reported status BLOCKED (not PASS): bundle integrity,
  freshness (18m42s, within 24h), config decryption,
  route usability, and master-key pairing all PASS, while
  audit chain and control-plane state were UNVERIFIED
  (no file backend present) and external-store ownership
  was BLOCKED with the explicit instruction to restore
  external state with the database's own backup and
  verify separately — which is what the `pg_dump`
  restore above did. Recovery therefore requires both
  artifacts together; neither alone is sufficient, and
  the verify tool says so itself.
- RPO/RTO for the backup drill, stated as procedure not
  as measured service levels: RPO for shared state is
  bounded by the `pg_dump` interval (the dump in this run
  was taken after the job completed, so that job was
  present in the restore); RTO is the time to a
  `pg_dump` restore, a `restore --verify` report, and a
  gateway start against the restored database. The
  individual step durations in this run were dominated
  by operator and VM delays, not by the product, so no
  single RTO number is claimed for the drill.

### S9 — Local-only state: verified at file level, OIDC not exercised live

- Replica A and B config files differ only in
  per-instance fields (listen address, breaker-sync
  replica id, security-store path, video storage root);
  the remaining config sections are identical. No
  control-plane propagation of config edits exists, per
  ADR 0001, and none was observed or expected.
- A live admin-edit-propagation test (edit on A, observe
  B unchanged through the API) was not performed in this
  run, because it requires an admin session setup that
  was out of scope for the staging configs; the file-level
  divergence above is the evidence actually collected.
- OIDC pending transactions were not exercised live in
  this run (OIDC disabled in the staging configs). The
  per-instance behaviour (a login flow completes on the
  replica that started it; callbacks with unknown state
  are rejected) is documented in the HA inventory and is
  unchanged by this run; it is NOT re-proven here.

## 4. Deviations from expected behaviour

1. Concurrent cold start: one replica failed with the
   pg_type duplicate-key error (S1). Fixed by this PR.
2. Video job reads during a PostgreSQL outage returned
   404 instead of 503 (S5). Fixed by this PR.
3. First database-restore attempt failed on file
   permissions (workspace directory mode). Operator
   error, corrected in-run; recorded so a future
   operator does not mistake it for a restore defect.
4. The first PostgreSQL restart attempt was interrupted
   by a runtime session kill before it completed. No
   conclusion is drawn from that attempt; recovery is
   claimed only from the clean restart that followed.
5. The run ended when the VM was replaced during a
   later test step (see §5). All staging evidence above
   was recorded before the replacement; no result is
   claimed from any step interrupted by it.

## 5. Repository tests run

With the integration build tag, disposable PostgreSQL and
Redis services, `-count=1`, against the base commit (before
the PR fixes — the fixes are covered by their own new
tests, whose local integration run is pending a live
database, see below):

- `internal/video/queue`: PASS (1.092 s) — including
  `TestPostgresVideoStoreConcurrentClaim` and terminal
  replay.
- `internal/video/cost`: PASS (1.887 s) — including
  `TestPostgresLedgerConcurrentDuplicateReserves`,
  duplicate/conflicting settlement, and restart survival.
- `internal/controlplane`: PASS (2.370 s) — including
  two-instances-and-restart and concurrent migration
  application on an already-migrated database.
- `internal/breakersync`: PASS (0.978 s) — including
  `TestBreakerSyncPostgresTwoInstances`.
- Redis integration subsets and the `-race` versions of
  the queue/cost tests were started but INTERRUPTED by a
  VM replacement before completion. Their outcomes are
  unknown and are NOT claimed here. The new cold-migration
  integration test and the new video store-error unit
  tests must be verified against a live database (and in
  CI) before this PR is merged; the unit tests do not
  require a database and were run locally (see PR body
  for the exact commands and results at PR time).
- Full `scripts/verify.sh` was not run in this window:
  another session owns the heavy baselines on this shared
  VM, and the staging run already contended with parallel
  builds. CI is the wider gate for this PR.

## 6. What remains BLOCKED or NOT_RUN

- Physical multi-host acceptance (two hosts/containers,
  network-shared PostgreSQL/Redis, load balancer):
  BLOCKED — no staging host, container runtime, or
  network namespace available in this environment.
- Live failover through a load balancer (detection,
  drain, and switch time): NOT_RUN — probes here were
  direct-port by design.
- Full probe-recovery heal after a provider trip:
  NOT_RUN to completion — recovery cooldown (900 s in
  the example config) exceeded the run window.
- OIDC pending-transaction behaviour across replicas:
  NOT_RUN live in this run (see S9).
- Long soak / sustained load: NOT_RUN.
- Security-store and video-store cold-start
  `CREATE TABLE IF NOT EXISTS` races: same class as
  the fixed control-plane race, not observed, not fixed
  — follow-up risk, not a claim.
