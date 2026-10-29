# HA acceptance — loopback staging + netns network-block finding, 2026-10-28 (issue #652)

## Status line

- Issue #652 stays OPEN. Genuine multi-host acceptance (separate hosts, or
  containers communicating over a network) is BLOCKED_EXTERNAL in this
  sandbox: network namespaces and veth pairs can be created and configured,
  but every non-loopback TCP/ICMP connection is refused with
  `Operation not permitted`, from the host namespace and from inside a new
  namespace alike (§0 below). This run therefore proves shared-store
  behaviour across two gateway processes on one VM over loopback, plus a
  first-hand, precisely bounded negative result for the network-namespace
  path — it is NOT multi-host evidence and must not close #652.
- No merge, no tag, no release was made from this work. The follow-up
  branch (`supervisor/issue652-ha-followup`, PR #698) already existed when
  this run started; this report is a new commit on that branch, not a new PR.
- No production system was touched. All services below are disposable and
  were destroyed at the end of the run: PostgreSQL 16.2 (pgserver binaries)
  on 127.0.0.1:5432, Redis 7.0.15 (apt) on 127.0.0.1:6390 for store-level
  tests only, stub upstream on 127.0.0.1:19091, gateway processes on
  127.0.0.1:18091–18093. No credentials, DSNs with secrets, client keys,
  admin keys, or tokens appear in this report (only loopback addresses,
  ports, row counts, timestamps, and video job ids).

## Code under test

- NEW binary: built from the follow-up branch head `ad37b2f` (PR #698 head
  at run start). Both binaries print `NexaRoute vdev`, so dev builds are
  distinguished by build source, not by version string.
- OLD binary: built from the verified PR #679 head `5f8a17c` in a clean
  throwaway worktree (removed after the build) for the rolling-upgrade
  scenario only. The OLD↔NEW code difference is exactly the follow-up
  commits (`b13af4c` error mapping, `c8c6776` procedure, `1c3a591`
  fixture fix); the report commit `ad37b2f` itself is docs-only.
- Go 1.27.0. Gateway configs differ from the 2026-10-28 follow-up drill
  only in per-instance fields (listen port, breaker-sync replica id,
  security-store path, video storage root, disposable admin/client keys)
  plus OIDC disabled and a disposable admin API key, so the host could call
  the admin audit API over loopback with `bind_local_only` still `true`.

## 0. Network-namespace experiment — negative result, precisely bounded

Question: can two replicas be isolated in network namespaces on this VM
(container-like separation) and share PostgreSQL over veth?

What worked:

- `unshare -n` creates a new network namespace; a holder process inside it
  configures its own loopback (`lo` up) and its veth end (IP assigned, UP).
- `ip link add veth-X type veth peer name veth-Y` succeeds, and
  `ip link set veth-Y netns <holder-pid>` succeeds — the peer appears
  inside the holder's namespace with its IP configured.

What failed (all with `PermissionError: [Errno 1] Operation not permitted`):

- TCP from inside a new namespace to the host-side veth IP, ports 5432
  (PostgreSQL) and 19091 (stub) — both refused.
- TCP from the host namespace to its own veth IP, port 5432 — refused.
- ICMP ping to any veth IP, from either side — refused.
- The same TCP failure with `unshare -U -r -n` (user + network namespace).

What still works (control cases proving the block is destination-scoped,
not a broken socket path):

- TCP from the host namespace to 127.0.0.1:5432 and 127.0.0.1:19091 —
  CONNECTED.
- TCP from inside a new namespace to its own 127.0.0.1:5432 — normal
  `ConnectionRefused` (nothing listens there), not `EPERM`.
- ICMP ping to 127.0.0.1 from the host namespace — works.

Conclusion: this sandbox permits loopback networking only. Namespace
creation is NOT the blocker (the earlier "no netns" shorthand is refined
by this experiment); cross-namespace network communication is. Any
topology that needs a non-loopback IP — a second host, a container with
its own IP, a load balancer over the network — cannot run here. That is
why the rest of this report is a loopback run, labelled as such.

## 1. State inventory confirmation (shared vs local, first-hand)

The `/api/hello` deployment report was identical on both replicas:

- shared: control plane (postgres), security store (postgres),
  audit/security store, breaker/cooldown (opt-in sync), video jobs + ledger.
- per-instance: config snapshot, quota/RPM/TPM reservations, session
  affinity, request idempotency.
- Warnings carried verbatim: config snapshots are per-instance local
  files; breaker sync applies remote retired markers only as bounded
  cooldowns; quota reservations must never be read as global limits;
  video jobs/ledger are shared via PostgreSQL while assets stay on
  per-instance local storage and are not replicated.

This matches `docs/HA_STATE_INVENTORY.md` exactly; no inventory entry
was contradicted by this run.

## 2. Loopback staging drill (two processes, one VM, shared TCP PostgreSQL)

Topology (honest label): TWO gateway processes on ONE VM host, separate
data directories and ports (A 18091/replica-a, B 18092/replica-b, plus C
18093/replica-c for the restore read-back), shared TCP PostgreSQL +
loopback stub upstream. NOT multi-host, NOT multi-namespace (see §0).
Times are UTC; all timings are single-run observations on this VM, not
SLOs. Scenario order was chosen deliberately: breaker trip last, so its
15-minute shared cooldown latch could not contaminate the soak or the
outage-health probes.

### S1 — Concurrent cold start

- Empty database `nexaroute_ha652_loop`. A + B (NEW binary) launched
  simultaneously at 2026-10-28T15:28:03.400Z.
- Both `/readyz` 200 at the first successful poll 2026-10-28T15:28:07.855Z
  (previous poll 15:28:05.295Z was not-ready; startup-to-ready ≤ 4.5s
  from launch). `/healthz` 200 on both.
- After start: 7 tables (controlplane 2, security 3, video 2),
  `nexaroute_control_migrations` = exactly 1 row. No migration race
  recurrence (the #679 lock-first fix holds under concurrent cold start).

### S2 — Baseline

- Chat via A and via B both returned the stub completion (`stub-ok`).
  Stub hits bracketed 28 → 30: exactly +2, zero probe noise (startup
  probes were already done; `ready_mesh` does not re-probe healthy
  deployments).
- Deployment reports identical on A and B (§1).

### S4 — Replica SIGKILL, survivor serving, client reconnection, restart

- Counts query before the kill failed on a helper-script bug (an env
  prefix passed through a variable instead of as a command prefix); the
  failure is recorded here, not hidden. What is established without that
  query: the database was empty of jobs/reservations (no job had been
  created yet), migrations = 1 from S1, and no login or admin call had
  happened yet (sessions/audit 0 by construction).
- B SIGKILLed at 2026-10-28T15:29:56.797Z. Survivor A at +1.07s
  (15:29:57.867Z): `/healthz` 200, `/readyz` 200, and a chat request on a
  NEW connection returned 200 — the survivor never dropped, and client
  reconnection (a fresh connection to the survivor) works. Direct-port
  probes only; load-balancer detection time is NOT included.
- B restarted at 15:29:58.792Z; first `/readyz` 200 poll at
  15:30:01.727Z (~2.9s after the restart command; the 15:29:59.257Z poll
  was not-ready). Chat via restarted B returned 200.
- Post-restart counts: migrations 1, snapshots 1, sessions 0, audit 0,
  jobs 0, reservations 0 — no duplication or loss from the kill.

### S5 — Video job across replicas; claim, ledger settlement, assets

- Create via A at 2026-10-28T15:31:26.585Z: job
  `video-1793201486585056997` (`fake`/`fake-video-1`, estimate $0.04).
  Completed by the first poll 15:31:28.465Z (progress 1); read-back via B
  also completed/progress 1. Missing job GET via B: genuine 404.
- Single claim, first-hand: the job row's `lease_owner` is A's process
  (`htch-runtime/43024`, A's PID); B's workers never executed it.
  Job rows = 1, reservation rows = 1. The reservation row is
  `estimate_settled`, estimated $0.04, actual $0, `entry_listed` true —
  exactly one settlement for one job (primary key is `job_id`; a second
  settlement for the same job cannot insert a duplicate row).
- Asset `fake-1-output`: 200 (76 bytes) via A, 503 `asset_unavailable`
  (generic, retryable) via B. A's disk holds the asset file; B's video
  directory is empty. Video assets remain per-instance local disks; they
  are NOT shared across replicas. Do not claim otherwise.
- Lease note (observed, not a defect claim): after completion the row
  still carries `lease_until` 2026-10-28T15:36:26Z (about 5 minutes past
  completion). Terminal jobs are not re-claimed, so this is not a live
  stale-lease problem; live takeover of an EXPIRED lease for a
  NON-terminal job was NOT proven here (no job was in flight at kill
  time, and fake jobs complete in ~2s — too fast to kill mid-flight).
  Lease takeover stays covered at unit level (`TryClaim` matrix,
  single-winner, persist-across-reopen), not at staging level.

### Admin audit sharing (new vs the follow-up drill)

- Admin audit reads via A and via B (disposable `x-admin-key` over
  loopback, `bind_local_only` still `true`) both returned 200.
- Audit rows went 0 → 4: two rows per read (`admin.operation.authorized`
  + `admin.operation.completed`, actor `legacy-admin-break-glass`, path
  `/admin/api/audit`), in the SAME shared PostgreSQL audit table. This
  closes the follow-up drill's gap for audit reads at gateway level.
- Session rows stayed 0: API-key reads create no login session, so
  login-session replication across replicas is still NOT proven at
  gateway level (OIDC was disabled in staging). Store-level
  session/audit round-trips are proven by the security-store integration
  suite (§4), not by a gateway login here.

### S6 — PostgreSQL outage and recovery WITHOUT gateway restart

- Before: jobs 1, reservations 1, audit 4, migrations 1.
- PG stopped (`pg_ctl stop -m fast`) at 2026-10-28T15:34:21.640Z.
  Gateways stayed alive, `/healthz` 200 on both, and breaker-sync logged
  `breaker_sync_error ... store unavailable` every sync interval (5s)
  without clearing local state.
- Probes with the NEW binary (the follow-up mapping fix):
  video GET (existing job) → 503 `video_store_unavailable`, generic body,
  retryable; video CREATE → 503 (not 400 + backend detail); video CANCEL
  (existing job) → 503; video GET (missing job) → 503, not a false 404;
  video ASSET → 503. No response body contained SQL, DSN text, or
  driver detail.
- PG restarted; gateways NOT restarted. PG accepting at
  15:34:41.774Z; first successful shared-state reads via A and B at
  15:34:42.485Z (< 1s after PG accept). Counts identical after recovery
  (jobs 1, reservations 1, audit 4, migrations 1); reservation row
  unchanged (`estimate_settled`).
- RPO for committed data under a clean PG stop/restart: 0.

### S7 — Rolling upgrade (one replica at a time, NEW → OLD → NEW)

- OLD binary = verified #679 head `5f8a17c` build; NEW binary =
  follow-up head build. Each swap: stop one replica, start the other
  binary on the same data directory and config, wait for `/readyz` 200.
- A OLD ready 15:35:00.347Z, B OLD ready 15:35:05.945Z, A NEW ready
  15:35:11.764Z, B NEW ready 15:35:17.388Z (~5s per swap, first-200-poll
  bound). Video GET 200 at every step; counts stable throughout
  (jobs 1, reservations 1, migrations 1); chat via B(NEW) 200 after the
  final swap. No migration error, no duplicated row, no dropped row.

### S8 — Backup and restore

- `nexaroute backup` for instance B wrote a 9,216-byte bundle (config
  6,178 bytes; master key NOT included, fingerprint recorded in the
  manifest). The bundle alone is NOT a restore.
- `restore --verify` status: BLOCKED (by design) on
  `external_store_ownership` — the bundle does NOT snapshot external
  state (control-plane postgres, security store postgres, video
  assets/jobs). Other checks PASS (bundle integrity, config decryption,
  route usability, master-key pairing, isolated materialization);
  freshness UNVERIFIED (no `--max-age` bound given) and audit-chain /
  control-plane-state UNVERIFIED (no file-backend state present, because
  this topology is PG-backed). Verify took 99ms.
- `pg_dump` of the drill database (10,854 bytes, exit 0) restored into a
  fresh database (exit 0): jobs 1, reservations 1, audit 4, migrations 1.
- Gateway C (NEW binary, restored DB, port 18093): `/readyz` 200 at
  15:36:01.388Z, `/healthz` 200, video GET for the drilled job
  `completed`/progress 1. Restore read-back verified end-to-end.
  Recovery requires BOTH artifacts (bundle + database dump); neither
  alone suffices for a PG-backed installation.

### Local-only state boundaries (documented, re-verified)

- Config diff A vs B (field paths only, never values): only
  `listen`, `control_plane.breaker_sync_replica_id`,
  `admin.security_store_path`, `video.storage_root`, `admin.api_key`,
  `client_auth.key_hashes` differ. (In a real multi-host deployment the
  provider base-URL host would also differ per replica; here both use
  the same loopback stub URL, so it does not appear in the diff.)
- Assets: per-instance (S5). Config: per-instance (diff above; an edit
  on one replica does not reach the other — verified by construction
  in the inventory, not by a live admin edit here, since no admin write
  was performed in this drill). Quota reservations: per-instance
  (inventory + deployment report). OIDC pending transactions:
  per-instance by design; NOT_RUN live here (OIDC disabled).

### Soak — bounded, repeatable, recorded (not a multi-hour soak)

- Script pattern (repeatable): 45 rounds, one round every 20s, chat
  alternating A/B, `/healthz` on both every round, plus one video job
  at rounds 0, 15, 30 (t = 0, 300s, 600s).
- Window: 2026-10-28T15:36:34.321Z → 15:51:39.863Z (15 minutes 5 seconds).
- Result: rounds 45, chat 45/45 ok, chat failures 0, health failures 0,
  video jobs 3/3 completed, video failures 0. Stub hits 117 → 162:
  exactly +45 (the soak chats; zero probe noise).
- Post-soak counts: jobs 4 (the S5 job + 3 soak jobs, all `completed`),
  reservations 4 (all `estimate_settled`), audit 4, migrations 1.
  No duplicate job, no duplicate settlement, no lost row, no
  unrecovered work across the soak.
- Limit: this is a 15-minute bounded soak, not the multi-hour soak the
  issue's long-soak wording contemplates. A longer soak on a real
  multi-host topology is still open.

### S6b — Health during a PG outage (established-model chat survives)

- PG stopped at 2026-10-28T15:52:48.267Z (brief window; the main S6
  outage above lasted ~20s). During the outage: `/healthz` 200 on both,
  `/readyz` 200 on both, and a chat request via A returned 200 with
  exactly +1 stub hit (162 → 163) — the request genuinely reached the
  stub and returned `stub-ok` while PostgreSQL was down.
- PG accepting at 15:52:49.127Z; `/readyz` 200 on both at
  15:52:49.278Z.
- Interpretation (recorded, not a defect): established-model chat does
  NOT need PostgreSQL on its hot path in this topology — routing uses
  the in-memory snapshot with `config_failure: last_known_good`, so a
  PG outage degrades the video store (503) and breaker sync (loud 5s
  errors), but does not stop chat for already-healthy models. Operators
  must therefore not read "chat 200 + /readyz 200" as "shared state is
  healthy": the video-store 503s and breaker-sync errors are the
  outage signal for shared state. Scope: established models only; a
  deployment needing a fresh probe during an outage was not tested.

### S3 — Breaker sync (last scenario, by design)

- Stub switched to `fail` at 2026-10-28T15:53:03.041Z. Trip via A:
  attempt 1 → 500 (stub failure reached), attempt 2 → 503 fail-fast.
- After a 12s wait (> 2 sync intervals of 5s), ONE request via B:
  → 503 with stub hits UNCHANGED 169 → 169 — zero new stub hits, the
  decisive fail-fast proof (this run's bracketed counter is clean,
  unlike the follow-up drill's probe-noisy counts). B `/readyz` 503
  (no verified healthy deployments in the ready queue — consistent).
- Shared snapshot row `nexaroute:breaker/v1` at that point:
  `{"schema":1,"entries":{"stub/stub-model":{"status":"cooldown","cooldown_until":"2026-10-28T16:08:05.12777835Z","reason_class":"provider_incident","origin_replica":"replica-a","observed_at":"2026-10-28T20:23:05.12777823+04:30"}}}`
  (observed_at is +04:30 local; cooldown_until = observed + 900s.)
- Stub restored to `ok` at 15:53:15.438Z. The shared cooldown latch
  persists by design until `cooldown_until` (16:08:05Z); the drill
  ended here and all processes plus the disposable database were then
  destroyed, so no latch survives anywhere.
- Pre-existing breaker-sync asymmetry from the follow-up drill (origin
  replica not re-applying its own shared cooldown after restart) was
  NOT re-tested here and is unchanged: it still needs an
  owner/maintainer decision and is out of #679/#698 scope.

## 3. RPO / RTO summary (observed only, with definitions)

Definitions used in this report:

- RPO = committed PostgreSQL rows lost, measured as the before/after
  row-count difference for migrations, snapshots, sessions, audit,
  jobs, and reservations.
- RTO (replica kill, survivor) = time from SIGKILL to the first
  successful survivor probe (direct-port probes; load-balancer
  detection NOT included).
- RTO (replica restart) = time from the restart command to the first
  `/readyz` 200 poll.
- RTO (PG outage) = time from PG accepting connections to the first
  successful shared-state read, with NO gateway restart.
- RTO (chat after provider failure) = time from the trip until the
  shared cooldown expires (`cooldown_until` in the snapshot row).

| Quantity | Observed value | Method / limit |
| --- | --- | --- |
| RPO, clean PG stop/restart | 0 committed records lost | Row counts before/after S6 identical |
| RPO, replica SIGKILL | 0 | Post-restart counts; see S4 helper-bug note |
| RPO, rolling upgrade | 0 | Counts stable at every swap |
| RPO, backup restore | 0 (restored DB matches drill DB counts) | pg_dump restore counts |
| RTO, replica kill (survivor) | Survivor never dropped (200 at 1.07s after kill) | Direct-port probes; LB detection NOT included |
| RTO, replica restart | ~2.9s (S4), ~5s per swap (S7) | First-200-poll bounds, not sub-second measured |
| RTO, PG outage → first shared read | < 1s after PG accept, no gateway restart | First-probe bound (S6, S6b) |
| RTO, chat after stub failure (peer replicas) | 15-minute shared breaker cooldown (until 16:08:05Z) | Shared snapshot row; dominant chat RTO, not PG RTO |
| RTO, backup restore | No single number quoted | Bundle verify 99ms + dump/restore seconds + gateway start ~2s first-200 bound; the procedure, not one number, is the RTO |
| Duplicated work | None observed | Migration, job, reservation, audit rows all count-stable across cold start, kill, outage, rolling upgrade, restore, soak |
| Unrecovered work | None observed | All 4 jobs completed with exactly-once settlements; no queued-but-lost job |

## 4. Integration, race, and local checks (this run, follow-up HEAD)

PostgreSQL 16.2 over TCP loopback, disposable test database, exact CI
`-run` patterns from `.github/workflows/ci.yml` (DSN differs only in
user/database: CI uses a service container; here trust auth as
`postgres`). Redis 7.0.15 over loopback, disposable DB index 15, exact
CI patterns from `integration-redis.yml`.

| Suite (CI pattern) | Result |
| --- | --- |
| controlplane `TestPostgresIntegration` (incl. cold-migration + outage tests) | PASS |
| controlplane `TestPostgresIntegrationColdDatabaseConcurrentMigrationsMultiProcess` (NEW, by name) | PASS (0.27s) |
| controlplane `TestPostgresIntegrationOutageRecoveryCancellationBackpressure` (by name, TCP DSN) | PASS (0.31s) — the follow-up report's socket-DSN caveat does not apply on a TCP DSN |
| securitystore `TestPostgresSecurityStoreIntegration` | PASS |
| securitystore `TestPostgresSecurityStoreIntegrationConcurrentColdOpen` (NEW, by name) | PASS (0.67s) |
| sharedstate `TestPostgresBreakerStoreIntegration` | PASS |
| video/queue `TestPostgresVideoStore` | PASS |
| video/queue `TestPostgresVideoStoreConcurrentColdOpen` (NEW, by name) | PASS (0.22s) |
| video/cost `TestPostgresLedger` | PASS |
| video/cost `TestPostgresLedgerConcurrentColdOpen` (NEW, by name) | PASS (0.33s) |
| breakersync `TestBreakerSyncPostgresTwoInstances` | PASS |
| cmd/gateway `TestConcurrentPostgresControlPlaneBootstrapConverges` | PASS |
| controlplane `TestRedisIntegration` (`-race`) | PASS |
| sharedstate `TestRedisBreakerStoreIntegration` (`-race`) | PASS |
| cmd/gateway `TestControlPlaneRedisIntegration` (`-race`) | PASS |
| breakersync `TestBreakerSyncRedisTwoInstances` (extra, plain) | PASS |
| video/api + video/orchestrator (`-race`) | PASS |
| gofmt (changed packages) / go vet (video/api, video/orchestrator, controlplane) | clean |

Not run locally: full `scripts/verify.sh` (pre-merge gate; CI runs it on
the pushed branch — see §6). No new production code was added by this
commit, so the local gate above plus the already-green CI on the code
head (§6) is the proportionate check for a docs-only commit. Redis at
gateway level was NOT tested (Redis is not a configured backend in the
loop topology; store-level Redis suites above are the Redis evidence).

## 5. Defects found by THIS run

None new. No production code was changed by this commit, and no new
defect was observed in any scenario (all error mappings behaved as the
follow-up fix specifies, including the 503-not-404 outage mapping and
the 503-not-400 create/cancel mapping).

Unchanged pre-existing items (not re-litigated here):

- Breaker-sync origin-replica asymmetry (follow-up §S3): owner decision
  still open.
- Video assets per-instance, config per-instance (ADR 0001), quota
  reservations per-instance, OIDC pending transactions per-instance:
  by design, documented in the inventory.
- Live external OIDC, live external KMS/keyring, live K8s install, live
  Azure/Vertex/Bedrock tenants: still UNVERIFIED (unchanged standing
  boundaries; not in this run's scope).

## 6. What still needs the owner's word

- PR #698 (follow-up branch) already carries the code fixes and tests;
  its CI was fully green on the code head `ad37b2f` at last check
  (verify, container, fuzz, CodeQL, govulncheck, Fresh coverage,
  integration-postgres, integration-redis, two-process PG, real-secret
  store). This commit adds evidence only; its own CI run must be green
  before any merge proposal is acted on.
- Merge of #698, merge of #679, and closure of #652 are all owner
  decisions. Nothing in this report closes #652: physical multi-host
  acceptance is still BLOCKED_EXTERNAL (§0), and a same-host loopback
  run — however thorough — is not a substitute for it.
- Open questions for the owner (unchanged): breaker origin-asymmetry
  decision; whether a network-namespace result with real networking
  (outside this sandbox) would count toward #652, or only separate
  hosts/containers do; longer soak duration on a real topology.

## 7. BLOCKED / NOT_RUN list (complete for this run)

- Physical multi-host topology (separate hosts, or containers with
  network IPs): BLOCKED_EXTERNAL — sandbox allows loopback only (§0).
  No second host, no usable container runtime networking, no LB.
- Load-balancer failover timing: NOT_RUN (no LB in the topology).
- OIDC cross-replica login sessions at gateway level: NOT_RUN (OIDC
  disabled in staging; pending transactions are per-instance by design).
- Live stale-lease takeover for a non-terminal job: NOT_RUN (see S5
  lease note; unit-level `TryClaim` coverage only).
- Redis gateway-level failover: NOT_RUN (Redis not a configured
  backend in the loop topology; Redis store-level suites PASS, §4).
- Multi-hour soak: NOT_RUN (15-minute bounded soak only, §Soak).
- Host or disk failure: NOT_RUN (single VM, shared disk).
- Full `scripts/verify.sh` locally: NOT_RUN (CI gate; §4).
