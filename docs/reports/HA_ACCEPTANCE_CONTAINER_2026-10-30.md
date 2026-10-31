# HA acceptance — container-isolated single host — 2026-10-30

- Issue: #652 (remains OPEN)
- Gateway source: `7daa36da962bf3031b353487f760506cd60aedb2` (unchanged)
- Tested commit: `6b89a23b7db5f00f18092b28089fd991c12fc9ac` (harness commit;
  worktree clean at build and run time)
- Harness: `scripts/ha-two-host-acceptance.py` (new in this change)
- Runtime: Go 1.25.0, static `CGO_ENABLED=0` gateway binary
- Shared services: PostgreSQL 16.15 (`postgres:16-alpine`). A Redis 7.4.12
  (`redis:7-alpine`) container was also started, but no drill config
  pointed any backend at it (control plane, security store, and video
  job store all use PostgreSQL; the cache is disabled), so it was
  present but not a configured backend. The original wording of this
  report ("sharing PostgreSQL and Redis") overstated its role and is
  corrected here: Redis was not shared state in this run.
- Gateway container image: `python:3.12-alpine` (static gateway binary plus a
  per-replica loopback stub)
- Evidence: `.artifacts/ha-two-host/container-20261030/`
  (`evidence.json`, `report.md`, dump/bundle artifacts; git-ignored)

## Multi-host gate outcome

This run used **two gateway containers on one Docker host**, with separate
network namespaces, config files, and data directories, sharing PostgreSQL and
Redis. It is **not** two independent hosts.

- Container-isolated single host: **PASS**
- Two independent hosts: **NOT_MEASURED**
- Load-balancer failover: **NOT_MEASURED**

The run therefore does not close #652 and does not support a multi-host HA,
host-failure, or load-balancer claim. Container isolation on one Docker host
remains a same-host evidence class.

## Measured values

| Measurement | Value |
| --- | --- |
| Concurrent cold start to both ready | 1.102 s |
| Survivor first success after replica SIGKILL (3 runs) | 0.152 / 0.137 / 0.144 s |
| Restarted replica ready after SIGKILL (3 runs) | 0.404 / 0.405 / 0.404 s |
| PostgreSQL accepting to first successful shared read | 0.019 s |
| Application backup duration | 0.083 s |
| Database dump to restored replica ready | 1.710 s |
| Bounded soak duration | 60.36 s |
| RPO (records lost in the completed kill/outage drills) | 0 |

These are container/process-observation timings, not multi-host RPO/RTO
values. No load balancer was in the path.

## Scenarios

1. **S1 — concurrent cold start (PASS).** Two replicas started against an
   empty shared database; exactly one control migration row was created.
2. **S2 — baseline (PASS).** `/healthz` and `/readyz` returned 200 on both
   replicas, and chat returned 200 with the disposable stub response on both.
3. **S3 — replica SIGKILL and restart (PASS, 3 repetitions).** The survivor
   served while one replica was killed; the restarted replica became ready
   again. Shared row counts were unchanged and no committed row was lost.
4. **S4 — concurrent video jobs and ledger (PASS).** Six jobs created across
   both replicas completed with one claim owner per job and one settled ledger
   entry per job; no duplicate lease owner or settlement row was observed.
5. **S5 — shared PostgreSQL outage and recovery (PASS).** Gateways stayed
   alive and `/healthz`/`/readyz` stayed 200 during the outage. Existing video
   job reads and new job creation returned retryable 503s with no backend
   detail. After PostgreSQL restarted, the gateways recovered without a
   restart. Committed row counts were unchanged (RPO 0 for this clean
   stop/start).
6. **S6 — backup, dump, and restore (PASS).** `nexaroute backup` produced a
   bundle; `restore --verify` returned `BLOCKED` with exit 1, the documented
   result when external stores are not owned by the bundle. A separate
   `pg_dump` was restored into a fresh database; row counts matched the source
   and a completed video job was readable through the restored replica.
7. **S7 — local-only boundaries (PASS).** The two generated configs differed
   only in `control_plane.breaker_sync_replica_id`,
   `admin.security_store_path`, `video.storage_root`, and `video.store_path`.
   A video asset produced on replica A returned 200 on A and 404 on B,
   confirming the documented per-instance asset boundary rather than a shared
   asset backend.
8. **S8 — bounded soak (PASS, 60.36 s).** Twelve rounds alternating replicas:
   12/12 chat successes, 0 health failures, 4/4 video job creations, no
   failures. Final counts were 10 jobs, 10 reservations, 10 settled. This is a
   short bounded soak, not a multi-hour soak.

## Not run / not measured

- Two independent physical hosts.
- Load-balancer failover and its detection interval.
- Breaker-sync convergence across replicas (procedure step 3) in this run;
  prior same-host multi-process evidence remains the only evidence for that
  behaviour. The harness gained an S9 breaker-sync scenario after this run;
  S9 was not executed here and its future result does not retroactively
  cover this run.
- Rolling upgrade between two binaries.
- Redis outage/failover; no gateway backend pointed at Redis in this
  run, so stopping it would have proved nothing. A Redis-backed
  topology needs its own drill variant with Redis backends configured
  and is NOT covered by this report.
- OIDC pending-transaction behaviour; OIDC was disabled.
- Multi-hour soak.
- Host death, network partition, disk loss, and production-like traffic.

## Reproduction

```sh
python3 scripts/ha-two-host-acceptance.py --self-test
NEXAROUTE_HA_SOAK_SECONDS=60 \
  python3 scripts/ha-two-host-acceptance.py --containers --repetitions 3 \
  --evidence-dir .artifacts/ha-two-host/container-20261030
```

The `--two-host` mode requires a written owner authorization document and
disposable staging PostgreSQL/Redis endpoints (including a separate restore
database). It refuses to run with exit 3 when those inputs are absent. The
authorization document must name the issue, scope, hosts, expiry, and cost
ceiling, and must explicitly state that production access is **not** granted.

## Conclusion

The container-isolated topology passed the scenarios above, including
shared-state failover after a replica SIGKILL, PostgreSQL outage/recovery,
concurrent video claiming and ledger settlement, and dump-based restore. The
multi-host claim for #652 remains **NOT_MEASURED**. Closing #652 still requires
a real two-host run or an explicit, reviewed owner exclusion.
