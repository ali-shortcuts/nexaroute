# Runtime state inventory and multi-instance guarantees (#434)

This inventory states, for every piece of runtime state in
NexaRoute, who owns it, how durable it is, and what is actually
guaranteed when more than one gateway process runs. It is the
reference behind the machine-readable deployment report the
gateway serves at `/api/hello` (`deployment` object, computed by
`deploymentReportForConfig` from the real startup configuration).
**No HA property is claimed here without a test or a store
contract behind it.** Where a guarantee does not exist, the entry
says so; the UI and health surfaces must repeat the per-instance
truth, never imply a global one.

Scopes: `shared` (coherent across replicas via a shared store),
`shared-host` (coherent across processes on one host only),
`per-instance` (each process holds its own state).

| State | Owner / store | Durability | Scope today | Restart | Rolling deploy / replica loss | Network partition | Ambiguous submission |
|---|---|---|---|---|---|---|---|
| Config snapshot | Local config file, read by `config.Load`/`config.LoadBase` and written atomically by `config.SaveAtomic` | Local file; durable only on the host/filesystem that holds it | **per-instance** — the control-plane store never holds config; it carries only runtime metadata and budget-alert marks | Reloaded from the local file at startup and after an admin save on that replica | Another replica keeps its own file and does not observe the edit; a lost replica loses unsaved admin edits unless its file was backed up separately | Each replica keeps its local file; no shared config write exists to split | N/A |
| Admin security store: sessions, audit chain, client keys | `internal/securitystore`: bbolt (default) / PostgreSQL / Redis | Durable except memory test backend | shared (PG/Redis), shared-host (bbolt) | Reloaded from store | PG serializes audit-chain writes transactionally; Redis uses version-checked Lua CAS; concurrent appends from two instances have real-service coverage | Backend down ⇒ fail closed | Audit append is append-only with chain verification; a conflicted append errors instead of forking the chain |
| Provider breaker / cooldown / health | `internal/health`, `internal/probe` (in-process); optional `internal/breakersync` over the control-plane store | None locally (rebuilt by probing); shared markers durable with the control-plane backend | **per-instance by default; shared (PostgreSQL/Redis) or shared-host (file) when `control_plane.breaker_sync` is enabled** — cooldown/retired markers only; a remote retired marker applies only as a bounded cooldown, never a local retirement (ADR 0003) | Local cooldowns reset and probes re-establish state; with sync enabled, a restarted replica re-applies unexpired shared markers on its first sync round | With sync disabled, replicas may disagree about a provider's cooldown during the probe window; routing stays safe (each replica probes independently) but not identical. With sync enabled, replicas converge within two sync intervals plus store latency (up to one interval for a change to be published, up to one more for a peer to observe it) | Each replica keeps its local state; a store outage never clears local circuits and sync resumes on heal | N/A |
| Quota / RPM / TPM reservations and budget counters | In-process limiters + optional control-plane budget store | In-memory (reservations); budget snapshots durable only with the control plane | **per-instance for reservations** | Reservations reset | **N replicas can collectively oversubscribe a provider quota by up to N× the per-instance limit.** The deployment report and UI must label these limits per-instance, never global | Same as replica loss, per partition side | Provider-reported usage settles reservations per instance; no cross-instance settlement exists |
| Session affinity / routing stickiness | Router in-process state | None | per-instance | Lost | A load balancer may land a session on a replica without its affinity state; routing remains correct, affinity is best-effort | Per partition side | N/A |
| Request idempotency (client-facing) | Per-instance request handling | None | per-instance | Lost | A retried submission landing on another replica is a new request to that replica | Same | **Ambiguous provider submissions are never auto-duplicated by the gateway**; retries follow the configured policy on the owning replica only |
| Video jobs, checkpoints, cost ledger | In-process video runtime; job store is a local file by default or PostgreSQL (`video.job_store_backend=postgres`, #609/#623); the cost ledger follows the same backend (in-memory, or the durable PostgreSQL ledger in the same database) | File backend: local files only, **not durable across hosts, not distributed**. PostgreSQL backend: jobs and cost reservations/settlements are durable and shared; settlement is exactly-once (one row per job, row-locked state transitions, idempotent repeats, conflicting actuals rejected). **Assets remain on per-instance local storage in both backends** (integrity-verified on read via the recorded SHA-256); no shared asset backend exists | per-instance (file) / shared (postgres, jobs+ledger only) | File: jobs on the lost replica are not resumed elsewhere. Postgres: jobs and ledger state survive restart; a restarted process re-reads durable reservations and settlements | File: a second replica does not see the first replica's jobs or ledger. Postgres: replicas share jobs and ledger; assets written by one replica are not visible to another, so multi-replica video still needs shared asset storage to be fully coherent | Per partition side; with postgres, the side that keeps the database keeps jobs+ledger | Jobs are leased per instance (`lease_owner`/`lease_until` on the job, atomic `TryClaim` on the store, renewed while polling): two orchestrators over one store execute a job exactly once (tested). The guarantee is cross-process only when the store's claim is atomic across processes — the file store's whole-file overwrite makes it single-process only; the PostgreSQL store provides the atomic conditional claim |
| OIDC pending transactions | In-process / security store session state | Session-lifetime | per-instance (login flows complete on the replica that started them) | Pending logins invalidated | Load balancers should keep auth callbacks sticky to the starting replica | Per partition side | Callbacks with unknown state are rejected, never guessed |
| Metrics / activity / audit views in the UI | Aggregated from the serving replica + shared stores where configured | n/a | Mixed: audit views are as shared as their stores; config views reflect the serving replica's local file; live activity and health views are **per-instance observations** | n/a | The UI of one replica does not show another replica's config edits or in-flight activity; health shown is the serving replica's own | Per partition side | n/a |

## Rules that follow from this inventory

1. Running multiple replicas is supported for stateless request
   serving with the PostgreSQL or Redis control-plane and security
   stores. Config snapshots remain per-instance local files:
   distribute the same config to every replica through deployment
   tooling, never by expecting control-plane propagation.
   Quota-reservation and affinity state remain per-instance, with the
   oversubscription consequence documented above — size per-instance
   limits accordingly. Breaker state is per-instance by default and
   shared only when opt-in breaker sync is enabled (ADR 0003); video
   job and cost-ledger state is shared on the PostgreSQL job-store
   backend, while video assets remain per-instance.
   [ADR 0001](adr/0001-shared-config-snapshot.md) records why shared
   configuration is not implemented: it was accepted on 2026-10-22 as
   Option 1 — local configuration stays authoritative indefinitely.
2. The file control-plane backend coordinates processes on **one
   host** (flock + CAS). It is not a cross-host store, and a
   network filesystem is never a coordination substitute.
3. Deployment mode and these scopes are exposed at `/api/hello`
   (`deployment` object) and must be surfaced by operator tooling
   and the UI instead of any hand-written HA claim. Documentation
   that contradicts this inventory is a defect.
4. Backup/restore (`nexaroute backup`, `restore --verify`,
   `doctor`) covers config, security-store snapshots and
   file-backed control-plane state. It does not snapshot external
   PostgreSQL/Redis or video assets/jobs; see
   `docs/KNOWN_GAPS.md` → “State and HA”.

## Test evidence behind this inventory

Scenario-by-scenario map for the #434 acceptance criterion —
every multi-instance scenario names its proving test(s), or
its documented non-claim. Integration tests are build-tagged
(`integration`) and run in the CI `integration-postgres` /
`integration-redis` jobs against real service containers;
everything else runs in the ordinary unit suite.

- **Concurrent updates to shared control-plane state** —
  `TestPostgresIntegrationTwoInstancesAndRestart` and
  `TestPostgresIntegrationConcurrentFirstWriteRace`
  (PostgreSQL: cross-instance visibility, stale writes fail
  with typed `ErrConflict`, never a lost update or a leaked
  database error); `TestRedisIntegrationTwoClientsAndCorruption`
  (the same semantics on Redis);
  `TestPostgresIntegrationMigrationsIdempotentChecksumAndConcurrent`
  (schema migrations under concurrency);
  `TestPostgresIntegrationStoreConformance` /
  `TestRedisIntegrationStoreConformance`; security-store
  equivalents `TestPostgresSecurityStoreIntegration` /
  `TestRedisSecurityStoreIntegration`. In-process:
  `TestTwoInstanceFileStoreCoherence` (two FileStore instances
  over one file, plus cross-process flock tests).
- **Restart** — the restart leg of
  `TestPostgresIntegrationTwoInstancesAndRestart` (a brand-new
  connection pool sees exactly the committed state);
  `TestFileStoreClaimPersistsAcrossReopen` (video leases).
- **Duplicate job claims** — the video lease suite:
  `TestTryClaimMatrix`, `TestTryClaimSingleWinnerUnderConcurrency`
  (16 claimants, exactly one winner),
  `TestFileStoreClaimPersistsAcrossReopen`,
  `TestTwoInstancesNeverExecuteSameJob` (two orchestrators,
  one store, exactly one provider submission). The lease is
  cross-process only where the store's claim is atomic across
  processes. A shared PostgreSQL job-store backend exists
  (`internal/video/queue/pg_store.go`, atomic `TryClaim`) and is
  exercised by the PostgreSQL integration suite; assets remain
  per-instance local storage in both backends, so end-to-end
  multi-node video is not a declared topology (ADR 0004,
  Accepted — Option B).
- **Config snapshot scope** — `TestConfigSnapshotReportMatchesLocalStore`
  proves every control-plane backend (file, postgres, redis, memory)
  reports `per-instance`, and that an admin save lands only in the local
  config file while a second replica's file stays unchanged.
- **Cooldown visibility** — **per-instance by default**:
  breaker/cooldown state is not shared unless opt-in breaker sync
  is enabled (ADR 0003), and
  `TestDeploymentReportSharedControlPlane` pins that even
  with a shared control-plane backend the report keeps
  breaker/cooldown (and quota reservations) labeled
  per-instance while sync is disabled; with sync enabled the
  report upgrades the breaker/cooldown scope to shared or
  shared-host according to the backend.
  `TestDeploymentReportDefaultIsPerInstance`
  pins the default topology's labels and warnings.
- **Reservation semantics** — deliberately **per-instance**:
  `TestBudgetLedgerConcurrentInvariants`,
  `TestBudgetLedgerAdmissionAndRelease` and
  `TestBudgetLedgerExpiryCommitsEstimate` prove the local
  ledger's exactly-once, bounded semantics; KNOWN_GAPS states
  reservations are advisory and never coordinated across
  processes, and the admin UI surfaces the answering
  instance's own ledger as such. A shared reservation store
  is the same owner topology decision as above (#435).
- **Deployment observability** —
  `TestHelloExposesDeploymentReport` (the report reaches
  `/api/hello`) and the Overview deployment panel, so a
  partially-shared topology is visible rather than implied
  global.
- Live multi-host behavior (real PG/Redis clusters, rolling
  deploys) remains **UNVERIFIED** in this tree; the integration
  suites are the strongest available evidence and are gated on
  operator-provided services.
- Same-VM staging run 2026-10-27
  (`docs/reports/HA_ACCEPTANCE_2026-10-27.md`, procedure in
  `docs/HA_ACCEPTANCE_PROCEDURE.md`): two gateway processes
  on one host against shared PostgreSQL — replica kill and
  restart, PostgreSQL outage and recovery, shared video
  job/ledger read-back, and `pg_dump` restore were observed
  live, with two defects fixed by that PR (concurrent cold
  start migration race; video job reads returning 404
  during a backend outage). That run is explicitly NOT
  multi-host evidence: physical multi-host acceptance
  stays BLOCKED on staging prerequisites.
- Same-VM loopback staging run 2026-10-28
  (`docs/reports/HA_ACCEPTANCE_LOOPBACK_2026-10-28.md`): two
  gateway processes on one host against shared TCP PostgreSQL —
  concurrent cold start, replica kill with survivor serving and
  client reconnection, restart, single-claim video job with
  exactly-once ledger settlement, per-instance assets, PostgreSQL
  outage and recovery without gateway restart, rolling upgrade
  NEW→OLD→NEW, backup plus `pg_dump` restore with third-replica
  read-back, shared admin-audit rows across replicas, a 15-minute
  bounded soak (45/45 chats, 3/3 video jobs, zero failures), and
  breaker-sync fail-fast with zero new stub hits. The same run
  proved a network-namespace pair cannot carry traffic in its
  sandbox (namespace + veth configuration succeed, but all
  non-loopback TCP/ICMP is `Operation not permitted` — loopback
  only), so physical multi-host acceptance stays BLOCKED. No new
  code defect was found by this run.
