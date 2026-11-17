# Durable PostgreSQL and Redis for the two-host HA drill

This directory holds the **durable** backend topology for the
multi-host HA acceptance drill (issue tracker: HA acceptance).
It is an alternative to — not a replacement for — the disposable
profiles: the gateway Helm chart deploys no database, the
`values-killercoda-ha.yaml` profile points at external backends,
and `scripts/k8s-acceptance.sh` deploys an ephemeral PostgreSQL
with no volume at all. Those paths stay disposable; this
directory is the one to apply when the drill needs backends
that survive a Pod loss.

## Scope: disposable drill only, not production

Apply these manifests only on a disposable staging cluster
with operator authorization. Do not use them for production.

## What this proves, and what it does not

Each backend is a **single-replica StatefulSet with a
PersistentVolumeClaim**:

- It survives a Pod restart and a Pod move to another node
  **if the PVC survives** — i.e. only when the cluster's
  default StorageClass provisions durable volumes. Verify
  `kubectl get pvc` shows `Bound` before the drill, and
  record the StorageClass in the evidence report.
- It does **not** prove multi-replica PostgreSQL or Redis
  failover. There is no Patroni/repmgr, no Redis Sentinel
  or Cluster, and no second replica here. True backend
  failover across independent failure domains needs one
  of those (or a managed service) and is an owner
  decision, not something these manifests claim.
- PostgreSQL WAL archiving is **not** configured. The
  RPO bound for shared state is the backup interval
  plus the dump/restore procedure in
  `docs/HA_ACCEPTANCE_PROCEDURE.md` scenario 6/8.
- Redis runs with AOF enabled (`appendonly yes`,
  `appendfsync everysec`) plus RDB snapshots, so a
  clean restart loses at most ~1 second of writes;
  under a crash the loss bound is larger and must be
  measured by the drill, not assumed.
- Redis runs with `--protected-mode no` because the
  drill uses no password. That is acceptable only on
  a private disposable network. Production requires
  AUTH (`requirepass`) and a NetworkPolicy; do not
  copy this setting to a shared network.

## Prerequisites

- A two-node Kubernetes cluster with a durable default
  StorageClass (the disposable kind two-node profile
  plus these manifests is the drill topology).
- `kubectl` against that cluster only.
- An operator-created Secret holding the PostgreSQL
  password. Create it from the operator's secret
  manager; never commit the password:

```sh
kubectl -n nexaroute-ha create secret generic nexaroute-ha-pg \
  --from-literal=password="$(openssl rand -base64 24)"
```

## Apply

```sh
kubectl create namespace nexaroute-ha
kubectl -n nexaroute-ha apply -f deploy/ha/
kubectl -n nexaroute-ha rollout status statefulset/nexaroute-ha-pg
kubectl -n nexaroute-ha rollout status statefulset/nexaroute-ha-redis
kubectl -n nexaroute-ha get pvc
```

Verify both backends answer before the drill:

```sh
kubectl -n nexaroute-ha exec statefulset/nexaroute-ha-pg -- pg_isready -U nexaroute -d nexaroute
kubectl -n nexaroute-ha exec statefulset/nexaroute-ha-redis -- redis-cli ping
```

## Wiring the drill to these backends

Point the two-host harness at the in-cluster Services
(the harness takes DSNs from the environment; the
gateway Helm values take them from Secrets):

- Shared PostgreSQL DSN:
  `postgres://nexaroute:<password>@nexaroute-ha-pg.nexaroute-ha.svc.cluster.local:5432/nexaroute`
  as `NEXAROUTE_HA_PG_DSN` (and an empty database of
  the same shape as `NEXAROUTE_HA_RESTORE_DSN`).
- Shared Redis URL:
  `redis://nexaroute-ha-redis.nexaroute-ha.svc.cluster.local:6379/0`
  as the Redis backend URL for any gateway component
  configured to use Redis.

The current harness (`scripts/ha-two-host-acceptance.py`)
configures every gateway backend on PostgreSQL; Redis is
present in `--containers` mode but no backend points at
it, so a Redis-backend outage is still untested by the
drill. Adding a Redis-outage scenario is the next harness
step once the owner provisions Redis as a configured
backend; these manifests supply the backend it would
exercise. Do not describe a PostgreSQL-only drill as a
Redis failover result.

## Teardown

```sh
kubectl -n nexaroute-ha delete -f deploy/ha/
kubectl delete namespace nexaroute-ha
```

Deleting the namespace deletes the PVCs and all drill
data. That is intentional for a disposable drill; never
run it against a namespace holding data the owner wants
to keep.
