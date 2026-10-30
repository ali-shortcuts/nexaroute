# Two-host staging topology for issue #652 — design (2026-10-30)

Status: **DESIGN ONLY. No host was contacted, no resource was created,
no live run happened.** The live two-host run is BLOCKED on the
authorization inputs listed in section 6. This document is the exact
topology the authorized run must build, so the run itself becomes a
mechanical execution of the merged harness
(`scripts/ha-two-host-acceptance.py`, `--two-host` mode) rather than a
new design exercise under time pressure.

Base: main `7a2623a9` (includes PR #776's harness and PR #778). The
harness drill is S1–S9; S9 (breaker-sync convergence) and the
same-host authorization rejection were added after the 2026-10-30
container run and have never executed against a live topology —
their first execution is the authorized run below, and any S9 failure
there is a drill finding, not a gateway verdict, until triaged.

## 1. Topology

```
operator machine (runs the harness; needs go, psql, pg_dump, ssh, scp, python3 on PATH)
  │  SSH (BatchMode, key or agent; no passwords)
  ├─ host A ── gateway replica A (port 18191-range, free-port probed)
  │            └─ loopback stub A (port 19091, per-host upstream stand-in)
  ├─ host B ── gateway replica B (port 18291-range)
  │            └─ loopback stub B (port 19091)
  ├─ host A ── restore replica C (port 18391-range, reads the restored DB only)
  └─ shared PostgreSQL (operator-supplied, disposable)
       ├─ main database (NEXAROUTE_HA_PG_DSN)
       └─ restore database (NEXAROUTE_HA_RESTORE_DSN, empty, disposable)
  └─ load balancer (OPTIONAL but required for a load-balancer claim; see section 4)
```

Host A and host B must be two independent hosts: different machines,
different kernels, different disks. Two containers on one host, two
processes on one host, and two VMs on one hypervisor host that share
fate for the drilled fault are NOT this topology. The harness now
refuses two SSH targets that resolve to the same host (including a
`user@host` alias), but it cannot detect shared fate below the
hostname — the operator must attest the hosts are independent in the
authorization document's scope note.

The per-host loopback stub is deliberate, not a shortcut: the stub
stands in for an upstream provider, and each replica gets its own so
a stub failure on A cannot be mistaken for shared-state behaviour.
The stub is never the system under test; PostgreSQL is.

## 2. What is shared versus deliberately local

From `docs/HA_STATE_INVENTORY.md` and the drill configs (every backend
is PostgreSQL; the cache is disabled):

| State | Shared across A/B? | Notes |
|---|---|---|
| Control-plane snapshots, budget-alert marks | Yes (PostgreSQL) | Serialized; stale writes fail with typed conflict, never lost update |
| Security sessions, audit chain | Yes (PostgreSQL) | Audit appends transactional; conflict errors, never forks |
| Video jobs, checkpoints, cost ledger | Yes (PostgreSQL) | Atomic `TryClaim`, one lease owner per job, one settlement row per job |
| Video assets (output files) | **No — per-instance local storage** | A 200 on the producing replica and 404 on the peer is the documented boundary, not a defect. No shared asset backend exists |
| Config snapshot | **No — per-instance local file** | Distribute identical configs by deployment tooling; a replica never observes another's admin edits |
| Quota / RPM / TPM reservations | **No — per-instance** | N replicas can oversubscribe a provider quota by up to N×; size per-instance limits accordingly |
| Session affinity, request idempotency | **No — per-instance** | A retried submission landing on the other replica is a new request there |
| OIDC pending transactions | **No — per-instance** | Drill configs disable OIDC, so OIDC behaviour is NOT_RUN in this run |
| Provider breaker / cooldown / health | Per-instance by default; **shared markers only with opt-in breaker sync** | Drill configs enable sync (5 s interval, distinct replica ids). Remote retired markers apply only as bounded cooldowns |
| Redis | **Not a backend in this drill** | No gateway config points at Redis. A Redis-backed topology needs its own drill variant with Redis backends configured; nothing about Redis is proven by this run |
| Load balancer state | Depends on the LB (section 4) | Not part of the gateway; its health-check interval bounds LB failover detection |

## 3. Fault matrix: directive steps 4–6 mapped to harness scenarios

| Directive step | Harness | What is measured |
|---|---|---|
| Concurrent cold start | S1 | Both ready; exactly 1 control migration row |
| Replica termination, survivor healthy | S3 (repeated `--repetitions` times) | Kill UTC, survivor first-success UTC → survivor RTO; restart-command UTC, restarted-ready UTC → restart RTO; row counts before/after → RPO |
| PostgreSQL interrupt + restore | S5 | Stop UTC, accepting UTC, first-read UTC per replica → first-read RTO; health/ready during outage; 503 (never 404, never backend detail) for job read/create; counts before/after → RPO |
| Reconnect / restart without gateway restart | S5 | Gateways recover with no restart; sync errors stop on heal |
| Backup creation + restore | S6 | `nexaroute backup` bundle bytes + duration; `restore --verify` status (BLOCKED on external-store ownership is the honest expected result for PostgreSQL); `pg_dump` → fresh database → replica C reads the drilled job; dump-to-ready seconds |
| No duplicate video-job claim / ledger settlement | S4 | 6 concurrent jobs across replicas; one lease owner per job; one reservation + one settled row per job; duplicate-owner and duplicate-settlement queries must return 0 |
| Breaker-sync convergence | S9 (new, runs last) | Trip A via its failing stub (500 → 503 fail-fast, two consecutive zero-new-hit chats); poll B once a second until first 503 with zero new B-stub hits → convergence seconds (1 s poll resolution) |
| Bounded soak, agreed duration | S8 | `NEXAROUTE_HA_SOAK_SECONDS` set to the owner-agreed duration; rounds, chat/health/video failures, final counts |
| Split / rejoin (network partition) | **NOT in the harness — planned manual step** | Operator blocks replica B's route to PostgreSQL (firewall rule on B or on the PG host), observes: B's shared-state calls fail closed (503s), A keeps serving shared state; removes the rule, observes B reconnecting without restart and counts unchanged. No split-brain write claim exists to test: PostgreSQL is a single primary, so the partitioned side cannot commit shared state. Record the rule, the block UTC, and the rejoin UTC; do not improvise a partition method during the run |
| Rolling upgrade | **NOT in the harness — manual, procedure step 7** | Replace one replica's binary at a time (same data dir + config), record health + one shared read + counts after each replacement. If old/new binaries disagree on schema version, stop: the migration story must be written first, never hand-edited |
| Load-balancer failover | **NOT in the harness — see section 4** | Without an LB in the path, `load_balancer_failover` stays NOT_MEASURED |

Process kill (SIGKILL) is not host death. Host death additionally
covers disk loss, kernel loss, and LB detection; this drill proves
process-death failover and database-outage recovery, not host-death
recovery. The report must say exactly that.

## 4. Load balancer: required for a failover claim, optional for the run

The harness probes replicas directly; it has no LB leg. Two honest
options for the authorized run:

- **With an LB (full failover claim).** The operator provides an LB
  (third host or managed service — never co-located with a replica
  whose death it must survive) in front of A/B with health checks
  against `/healthz` (liveness) and ideally `/readyz` (readiness).
  During S3, the operator additionally polls the LB endpoint from
  the moment of the kill until its first success and records: LB
  health-check interval + timeout (the detection bound), kill UTC,
  LB first-success UTC → LB RTO. LB RTO and direct-replica RTO are
  reported as separate numbers; the LB number is the one clients
  experience.
- **Without an LB (partial claim).** The run still proves replica
  failover, database recovery, backup/restore, and RPO/RTO through
  direct replica URLs, and `load_balancer_failover` stays
  NOT_MEASURED in the evidence. The #652 close criteria then still
  need either a later LB run or an explicit owner-approved
  exclusion of the LB leg — the direct-replica numbers must never
  be relabelled as LB failover.

## 5. RPO / RTO measurement method (no invented numbers)

- **RPO** = committed rows lost across a fault, computed from
  before/after row counts per table (`nexaroute_control_*`,
  `nexaroute_security_*`, `nexaroute_video_*`) by the harness
  (`rpo_from_counts`). A table that gains rows is not negative
  loss; the reported RPO is the maximum lost count across tables,
  or NOT_MEASURED when no count scenario completed.
- **RTO** = UTC timestamp difference between a fault/recovery
  boundary and the first successful observed operation, per
  scenario: survivor RTO (kill → survivor first success), restart
  RTO (restart command → restarted ready), PG first-read RTO
  (PG accepting → first successful shared read), dump-restore
  RTO (dump start → restored replica ready), breaker-sync
  convergence (trip → first fail-fast bracket on B, 1 s poll
  resolution).
- **Repetitions.** Run the full drill 3 times (or pass
  `--repetitions 3`, which repeats S3 within a run) and report
  every run's values plus the range. A single run's numbers are
  one sample, not an SLO. S5 runs once per drill invocation, so
  repeated full runs are how S5 gets repeated samples — do not
  average S3 repetitions into S5.
- **Soak duration** is the owner-agreed value, set via
  `NEXAROUTE_HA_SOAK_SECONDS` and recorded in the evidence. A
  60 s bounded soak is not a multi-hour soak, and a multi-hour
  soak on this topology is a separate agreed run, not an
  extrapolation.
- **NOT_MEASURED** is written for every metric whose scenario did
  not run, including LB failover without an LB, Redis behaviour,
  OIDC behaviour, host death, and any scenario interrupted
  before its measurement completed. An interrupted scenario is
  reported with its cause, never silently retried into a pass.

## 6. Exact inputs that unblock the run (authorization checklist)

Nothing below exists yet. Each item must be supplied by the owner
in writing before any host is contacted:

1. Authorization JSON (schema enforced by the harness): `issue`
   652, `authorized_by`, `scope` `disposable-staging`,
   `production_access` false, `expires_utc` (RFC3339, future),
   `hosts` (both SSH targets, distinct), `cost_ceiling_usd`
   (0 for no-cost staging). The harness rejects wrong-issue,
   expired, production, out-of-scope, uncovered-host, and
   same-host-twice documents with exit 3 before contacting
   anything.
2. SSH targets host A and host B (distinct hosts, section 1),
   reachable with key/agent auth in BatchMode, Python 3 on both,
   and a writable home directory for the `.nexaroute-ha652` run
   root.
3. Shared disposable PostgreSQL DSN + a separate empty
   disposable restore database DSN. Both databases must be
   droppable/disposable; production DSNs are never accepted.
4. `NEXAROUTE_HA_PG_STOP_CMD` / `NEXAROUTE_HA_PG_START_CMD`:
   operator commands that stop/start ONLY the disposable
   PostgreSQL (for example a scoped systemctl/docker/SSH
   command). No command that can touch a production service.
5. Agreed soak duration (`NEXAROUTE_HA_SOAK_SECONDS`) and, if
   claimed, the LB endpoint + its health-check interval.
6. Operator machine with go, psql, pg_dump, ssh, scp on PATH
   (the harness refuses to start without them, exit 3).
7. Secret injection: DSNs and tokens travel via environment and
   SSH stdin into per-host `.env` files (0600 umask), never via
   command-line arguments, shell history, or evidence files.
   Evidence is redacted before writing (DSNs, tokens, URLs).

## 7. Teardown (part of the run, not an afterthought)

The harness cleanup stops the gateway/stub processes and removes
the remote `.env` files (DSN + video token). The operator must
then, in order: delete the remote `.nexaroute-ha652` run roots
once logs are no longer needed; drop the disposable main and
restore databases; revoke/rotate the staging database credentials
and video token; confirm no production system was contacted (the
authorization document's `production_access: false` plus the
evidence's redacted topology section are the audit trail). A run
whose teardown is skipped is an incomplete run.

## 8. Known limitations carried into the run (not re-litigated there)

- Video assets stay per-instance (section 2). Multi-replica video
  is coherent for jobs + ledger only; a shared asset backend does
  not exist (ADR 0004, Option B).
- Quota reservations stay per-instance with N× oversubscription
  (section 2). Size limits per instance.
- Breaker-sync origin-replica asymmetry (loopback report
  2026-10-28): an origin replica does not re-apply its own shared
  cooldown after restart. Unchanged since, needs an
  owner/maintainer decision, out of this drill's scope — S9
  measures peer convergence, not origin re-application.
- Config edits do not propagate between replicas (ADR 0001).
- The file control-plane backend is single-host only (flock +
  CAS); it is not a cross-host store and a network filesystem is
  never a coordination substitute.
- `nexaroute backup` alone is not a PostgreSQL restore; the
  bundle + `pg_dump` pair is the recovery unit (S6).

## 9. Current sandbox blocker (verified 2026-10-30)

On the audit box: no docker/podman, no psql/pg_dump, go not on
PATH, loopback-only network policy. `--two-host` with no inputs
exits 3 listing every missing prerequisite and contacts no host;
`--containers` cannot start without docker. The live run therefore
cannot happen here — it needs the section 6 inputs and a
machine that has them. #652 stays OPEN until the authorized
two-host evidence exists or a reviewed owner-approved exclusion
is recorded.
