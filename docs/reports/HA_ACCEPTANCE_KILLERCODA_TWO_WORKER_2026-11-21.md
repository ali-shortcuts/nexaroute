# HA acceptance — durable two-worker drill (Killercoda playground)

Commit under test: `1ba0a324746ab4ccb46b1403ff67e3829b3b7964` (main, after PR #884)
Date: 2026-11-21. All times UTC.
Issue: #652 (HA acceptance: multi-host PostgreSQL/Redis failover and recovery).

## Topology (verified)

- Killercoda Kubernetes playground, 2 Ready nodes: controlplane + node01,
  Kubernetes v1.37.1, containerd 2.2.1, helm v4.3.0.
- Gateway: 2 replicas via `deploy/helm/nexaroute` + `values-killercoda-ha.yaml`
  (replicaCount 2, RollingUpdate, required podAntiAffinity on
  `kubernetes.io/hostname`), image `ghcr.io/ali-shortcuts/nexaroute:ha652-1ba0a324`
  built in the playground from 1ba0a324 (sha256 d5bde8400df4, VERSION=1ba0a324),
  present on both nodes.
- Durable backends via `deploy/ha/`: PostgreSQL StatefulSet (PVC 10Gi) and
  Redis StatefulSet (PVC 5Gi), StorageClass `local-path`, both Bound.
- Config: drill config built from `scripts/ha-two-host-acceptance.py`
  `build_config` with `postgres_dsn_env = NEXAROUTE_CONTROL_PLANE_PG_DSN`,
  `control_plane.redis_url_env = NEXAROUTE_CONTROL_PLANE_REDIS_URL`,
  breaker_sync ON (replica id empty → hostname-pid default),
  `recovery_cooldown_seconds: 5` (drill value; production default is 900),
  stub provider `http://127.0.0.1:18081/v1` (model `stub-model`),
  admin api_key and video token (disposable drill-only values; the
  playground environment is destroyed).

## Honest scope (read before the results)

- The two playground nodes run on ONE playground host: they are two
  schedulable Kubernetes nodes, NOT two independent failure domains.
  Two-independent-hosts + load-balancer failover remains NOT_MEASURED.
- `local-path` volumes are node-local: a Pod's data survives a Pod restart
  on the SAME node, but the volume cannot move to the other node.
- The durable manifests do NOT prove multi-replica PostgreSQL/Redis
  failover (no Patroni/Sentinel) — that remains an owner decision.
- Redis is wired only via `control_plane.redis_url_env`; all backends in
  the drill config run on PostgreSQL, so a Redis-backend outage is limited
  evidence.
- The validated 2h soak for this commit already exists from CI
  (run 44748356175: 7424s, 10/10 stages, 28,802 samples); the soak in this
  topology is a short bounded run only (60-minute playground session).
- The single-host HA drill for this commit is already green in CI
  (run 44764989646: S1–S9 ALL PASS).

## Timeline (UTC)

- 02:05:31Z Helm release `nexaroute` installed (revision 1).
- 02:08:32Z Pods placed on distinct nodes but ImagePullBackOff.
- 02:14:15Z Gateway CrashLoopBackOff: `cannot atomically migrate
  configuration secrets`.
- 02:17:28Z After the initContainer fix: both pods 2/2 Running.
- 02:18:03Z Baseline (see below).

## Deviations and findings

1. **ImagePullBackOff (fixed in-drill).** The image was in Docker's store
   on both nodes, but kubelet reads containerd's `k8s.io` namespace.
   Fix: `ctr -n k8s.io images import /tmp/nr-ha652.tar` on both nodes,
   then recreate the pods.
2. **CrashLoopBackOff — Helm chart vs gateway startup migration
   (finding).** At startup the gateway atomically rewrites
   `/config/config.json` to encrypt plaintext secrets
   (`internal/config/config.go`, `writeAtomic`: temp file + rename).
   The only plaintext secret in the drill config is
   `admin.api_key`. The chart mounts `config.json` as a READ-ONLY
   subPath Secret file, so the rename fails and the gateway exits.
   Drill-local fix (no chart change): JSON-patch the Deployment —
   move the gateway's config mount to `/secret/config.json` and add
   initContainer `config-copy` (python:3.12-alpine) that copies
   `/secret/config.json` → `/config/config.json` (writable EmptyDir,
   fsGroup 65532) before start. The CI harness drill never hits this
   because its config file is writable. Follow-up: the chart should
   either ship this copy step or document that configs with plaintext
   secrets cannot use the read-only subPath mount.

## Results

### Baseline — 02:18:03Z — PASS

- Both replicas: `/healthz` 200, `/readyz` 200 (via stub sidecar).
- Chat `POST /v1/chat/completions` (stub-model): HTTP 200, content "OK".
- `GET /api/hello`: HTTP 200, version 1ba0a324,
  control_plane_backend postgres, video_jobs_ledger shared.
- Video job `POST /v1/video/jobs` (fake provider): HTTP 202,
  job `video-a7f889944ebf06ffa89fe10698a982d3`, state queued,
  estimate $0.01.
- PostgreSQL row counts: snapshots 1, sessions 0, audit 0,
  video_jobs 1, cost_res 1.
- `/api/hello` warnings (topology limits, recorded as-is):
  config snapshots per-instance; quota/RPM/TPM per-instance in-memory;
  video assets on per-instance local storage, not replicated.

### S8 replica kill — 02:18:42–02:18:55Z — PASS

- Deleted replica `nexaroute-nexaroute-6c6c59f94b-fhfkp` (node01).
- Replacement pod `-prjj4` landed on node01, 2/2 Running, 0 restarts.
- RTO: `kubectl wait --for=condition=ready` returned in **11.981s**;
  2/2 Ready again ≈ 13s after the delete.

### S9 PostgreSQL Pod delete — 02:19:27–02:19:34Z — PASS

- Deleted `nexaroute-ha-pg-0`; new PG pod Ready in **6.028s**.
- `pg_isready`: accepting connections.
- Row counts UNCHANGED from baseline: video_jobs 1, snapshots 1 — **RPO 0**.
- Chat after the PG kill: HTTP 200, content "OK" — the gateway
  reconnected without a restart.

### S10 Redis Pod delete — 02:20:18–02:20:27Z — PASS

- Deleted `nexaroute-ha-redis-0`; new Redis pod Ready in **8.035s**.
- `redis-cli ping`: PONG.
- Chat after the Redis kill: HTTP 200.
- Honest scope: Redis is wired only via `control_plane.redis_url_env`;
  all drill backends run on PostgreSQL.

### S11 pg_dump restore into fresh database — 02:21:03–02:21:30Z — PASS

- Dump `/tmp/nexaroute-dump.sql` created inside the PG pod.
- First attempt failed on a psql quirk (DROP + CREATE in one `-c`
  runs inside an implicit transaction block); retry with separate
  DROP and CREATE calls succeeded.
- Restored counts in `nexaroute_restored` MATCH baseline exactly:
  video_jobs 1, snapshots 1.

### S12 short bounded soak — PASS (60/60 samples, all HTTP 200)

- Healthz polled every 5s for 5 minutes through the Service port-forward;
  60 consecutive HTTP 200 responses, 0 failures.
- First attempt (foreground loop) was interrupted by a terminal disconnect
  after ~30 consecutive 200s; the re-run used a background loop logging
  each result to a file (`/tmp/soak12.log`) so the count survived the
  terminal clearing its on-screen output.
- The validated 2h soak for this commit from CI (run 44748356175:
  7424s, 10/10 stages, 28,802 samples) remains the long-soak evidence.

## NOT_RUN / NOT_MEASURED

- Two genuinely independent hosts + load-balancer failover: NOT_MEASURED
  (single playground host).
- Multi-replica PostgreSQL/Redis failover (Patroni/Sentinel): NOT_MEASURED
  (single-instance StatefulSets by design; owner decision pending).
- Long 2h soak in the two-worker topology: NOT_RUN (60-minute session);
  validated 2h soak for this commit exists from CI (run 44748356175).
- `nexaroute backup` bundle `restore --verify`: BLOCKED by design for
  PG-backed installs (dump restore verified instead, S11).
- Live external providers / OIDC / paid calls: NOT_RUN (out of scope).

## Verdict

All scenarios through S11 PASS with measured RPO/RTO on the durable
two-worker topology (commit 1ba0a324): baseline green; replica kill
RTO ≈ 13s; PostgreSQL Pod loss RTO ≈ 6s, RPO 0, gateway reconnected
without restart; Redis Pod loss RTO ≈ 8s, chat still 200; pg_dump
restore row counts match baseline exactly. S12 short soak PASS
(60/60 samples, all 200). Two-independent-hosts +
load-balancer failover remains NOT_MEASURED (single playground host);
multi-replica PostgreSQL/Redis failover remains NOT_MEASURED (owner
decision pending). Within those stated bounds, the durable two-worker
acceptance for #652 is satisfied.
