# Issue #652 — iximiuz backend recovery and backup evidence

**Date:** 2026-11-09 08:07 UTC  
**Repository:** `ali-shortcuts/nexaroute`  
**Gateway revision:** `83a3330467bee986665d1a20c11ac4583fe621e2`  
**Image:** `registry.iximiuz.com/nexaroute:ha652-83a33304`  
**Image digest:** `sha256:48c9cf18489e71bbbb60768980a615ecf687f22c245bf84f1c21fb52948accbf`

## Topology

The run used the disposable iximiuz Labs Kubernetes Cluster Playground:

- `cplane-01` — control-plane
- `node-01` — worker
- `node-02` — worker
- Kubernetes `v1.37.1`
- containerd runtime
- namespace `nexaroute-ha652`

The NexaRoute Deployment had two replicas, with required node affinity restricting them to `node-01` and `node-02`, and required hostname anti-affinity keeping the replicas on distinct workers. At the final verification both gateway replicas were Ready, one on each worker. No current NexaRoute-related Pod remained on `cplane-01`; only cluster/system Pods were present there. Historical operator debug Pods had briefly been scheduled on `cplane-01` and were deleted before final verification. They were not gateway replicas.

PostgreSQL was a single Deployment replica constrained to `node-01`. Redis was a single Deployment replica constrained to `node-02`. Both used `emptyDir`; PostgreSQL was backed by neither a PVC nor a replicated database service, and Redis ran with `--appendonly no` and `emptyDir`.

## Evidence collected

### PostgreSQL backup

A PostgreSQL custom-format backup was created using the database credentials injected into the PostgreSQL Pod:

```text
file: postgres.dump
size: 6186 bytes
sha256: cbd2619b26d2a94608b4f23db6d113ae2f31046fd080b602b09424ca3140a5f6
```

The source database was `nexaroute` and contained these application tables:

```text
public.nexaroute_control_migrations
public.nexaroute_control_snapshots
public.nexaroute_security_audit
public.nexaroute_security_meta
public.nexaroute_security_sessions
```

### Backup restore verification

The dump was restored into a separate temporary database named `nexaroute_restore_check`, then the temporary database was dropped. Restore completed successfully:

```text
RESTORE_CHECK=PASS
nexaroute_control_migrations=1
nexaroute_control_snapshots=1
nexaroute_security_audit=0
nexaroute_security_meta=1
nexaroute_security_sessions=0
```

This proves that the captured PostgreSQL dump can be restored into a database on the same disposable PostgreSQL instance. It does not prove durable disaster recovery, cross-host restore, or production backup retention.

### Strict PostgreSQL restart recovery

The original PostgreSQL Pod UID was recorded. The Pod was deleted, and recovery was measured only after a replacement Pod with a different UID reached `Ready=True` and `pg_isready` succeeded:

```text
failure:  2026-11-09T08:07:05.051Z
recovery: 2026-11-09T08:07:13.431Z
measured Ready recovery: 8379 ms
replacement: postgres-f9d96d785-7sz9s
node: node-01
```

A SQL query after recovery succeeded:

```text
nexaroute|nexaroute|2026-11-09 08:07:13.50494+00
```

### Strict Redis restart recovery

The original Redis Pod UID was recorded. The Pod was deleted, and recovery was measured only after a replacement Pod with a different UID reached `Ready=True` and returned `PONG`:

```text
failure:  2026-11-09T08:07:17.040Z
recovery: 2026-11-09T08:07:23.406Z
measured Ready recovery: 6366 ms
replacement: redis-6ffccfb8b8-xrpm2
node: node-02
```

### Gateway health during PostgreSQL outage

While PostgreSQL was scaled to zero, the Gateway endpoints continued to return HTTP 200:

```json
{"ok":true}
{"deployments":1,"ok":true,"usable":1}
```

This is evidence that `/healthz` and `/readyz` in this build do not synchronously validate PostgreSQL availability. It is not evidence that PostgreSQL-backed control-plane operations remain usable during a database outage. Database functional-query behavior during the outage was not measured.

### Existing gateway replica recovery

A prior deletion of one gateway Pod was measured through the Kubernetes Deployment status:

```text
failure:  2026-11-09T08:02:23.971Z
recovery: 2026-11-09T08:03:36.633Z
measured ready=2 / available=2 recovery: 72662 ms
```

The Service remained configured with the surviving endpoint while Kubernetes recreated the second replica.

## Doctor result and security boundary

The Gateway doctor completed with no hard failures:

```text
PASS config: loads and validates (1 providers, 0 route profiles)
PASS master_key: no encrypted values stored
PASS listen: 0.0.0.0:8080
WARN security_store: backend "postgres" adapters land with M4-2; admin authentication fails closed in this build
PASS control_plane_postgres: reachable via NEXAROUTE_CONTROL_PLANE_PG_DSN
PASS restore_journal: no interrupted restore transactions
doctor summary: 5 pass, 2 warn, 0 fail
```

The M4-2 warning remains an explicit limitation. It is not converted to PASS by this acceptance run.

## Acceptance disposition

This run provides the following bounded evidence:

| Check | Result |
|---|---|
| Two Gateway replicas on distinct workers | **PASS** |
| Gateway Pod deletion and Deployment recovery | **PASS**, measured 72.662 s |
| PostgreSQL single-Pod restart and Ready recovery | **PASS**, measured 8.379 s |
| Redis single-Pod restart and Ready recovery | **PASS**, measured 6.366 s |
| PostgreSQL custom-format backup | **PASS** |
| Restore into temporary PostgreSQL database | **PASS** |
| Current NexaRoute placement excludes control-plane | **PASS** |
| Durable PostgreSQL persistence | **NOT PROVEN** — `emptyDir` |
| Durable Redis persistence | **NOT PROVEN** — `emptyDir`, AOF disabled |
| PostgreSQL multi-host failover | **NOT RUN** — one replica only |
| Redis multi-host failover | **NOT RUN** — one replica only |
| Independent backend failure-domain recovery | **NOT RUN** |
| Durable multi-host RPO/RTO | **NOT PROVEN** |
| Long soak | **NOT RUN** |
| M4-2 admin authentication support | **WARN / NOT PASS** |

## Issue status

Issue #652 remains **OPEN**. This report must not be interpreted as production HA certification or as evidence of PostgreSQL/Redis failover between hosts. The remaining gates require a topology with durable storage and independently replicated or failover-capable PostgreSQL and Redis services, followed by a measured cross-host failure drill and documented RPO/RTO.
