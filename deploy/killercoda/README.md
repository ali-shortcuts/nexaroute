# Killercoda two-node acceptance profile

This directory contains the disposable Kubernetes operator runbook for issue
[#652](https://github.com/ali-shortcuts/nexaroute/issues/652). It prepares a
candidate topology; it does **not** provide evidence that the topology has
been executed. The acceptance verdict remains `NOT_RUN` until an authorized
operator runs the real drill and commits sanitized evidence.

## Topology and scope

Use the official [Killercoda Kubernetes Playground](https://killercoda.com/playgrounds/scenario/kubernetes)
with two schedulable nodes. Run two NexaRoute pods with required pod
anti-affinity, a shared disposable PostgreSQL service, and a shared disposable
Redis service. PostgreSQL is the shared backend exercised by the current
NexaRoute HA harness; Redis is provisioned for a later Redis-backed variant and
must not be described as tested unless the gateway configuration points at it
and the outage/recovery leg is actually run.

The profile deliberately disables the chart's normal RWO PVC. Each gateway
keeps only disposable local state in an `emptyDir`; shared control-plane,
security, video-job, and ledger state must be configured in PostgreSQL. Video
assets, configuration files, quotas, request idempotency, sessions, and OIDC
pending transactions remain per-instance boundaries documented in
[`docs/HA_STATE_INVENTORY.md`](../../docs/HA_STATE_INVENTORY.md).

## Operator preparation

Do not put credentials in this repository, in a values file, or in shell
history. Prepare the disposable databases and create the referenced Secrets
from environment variables. The exact image tag must be pinned to the commit
under test; an empty chart tag is not an acceptance claim.

```bash
# Run only after logging into the disposable Killercoda terminal.
export IMAGE_TAG="<image-built-from-the-commit-under-test>"
export PG_DSN='postgres://<user>:<password>@<postgres-service>:5432/<db>?sslmode=disable'
export REDIS_URL='redis://:<password>@<redis-service>:6379/0'

kubectl create secret generic nexaroute-killercoda-pg \
  --from-literal=dsn="$PG_DSN"
kubectl create secret generic nexaroute-killercoda-redis \
  --from-literal=url="$REDIS_URL"
```

Create `nexaroute-killercoda-config` from an operator-owned config file. The
config must use `postgres_dsn_env: NEXAROUTE_CONTROL_PLANE_PG_DSN` (and the
corresponding PostgreSQL security-store and video-job/ledger settings), enable
health probing, and point any test provider at a loopback stub. Do not use a
cluster-internal HTTP provider URL: NexaRoute's egress policy rejects plain
HTTP to non-loopback hosts. The config Secret is intentionally not generated
here because it contains deployment-specific provider and admin settings.

## Install and verify placement

```bash
helm lint deploy/helm/nexaroute
helm template nexaroute deploy/helm/nexaroute \
  -f deploy/helm/nexaroute/values-killercoda-ha.yaml \
  --set image.tag="$IMAGE_TAG" > /tmp/nexaroute-killercoda.yaml

# Review the rendered output before applying it.
grep -E 'replicas: 2|type: RollingUpdate|emptyDir: \{\}|requiredDuringSchedulingIgnoredDuringExecution' \
  /tmp/nexaroute-killercoda.yaml

helm upgrade --install nexaroute deploy/helm/nexaroute \
  -f deploy/helm/nexaroute/values-killercoda-ha.yaml \
  --set image.tag="$IMAGE_TAG"
kubectl get pods -o wide -l app.kubernetes.io/name=nexaroute
kubectl rollout status deployment/nexaroute-nexaroute --timeout=180s
kubectl get pods -o wide
```

Both gateway pods must be `Ready` and must have different
`kubernetes.io/hostname` values. If the second pod remains `Pending`, stop and
record `INSUFFICIENT: two distinct schedulable nodes`; do not remove the
anti-affinity rule or relabel a one-node run as multi-host.

## Acceptance drill

The Helm install alone is not the #652 acceptance. Before contacting hosts or
creating anything outside the disposable playground, obtain the written owner
authorization required by
[`docs/HA_ACCEPTANCE_PROCEDURE.md`](../../docs/HA_ACCEPTANCE_PROCEDURE.md).
Then run the existing harness against two genuinely independent hosts, or
record the Killercoda run as a Kubernetes candidate with its exact topology.
Use UTC timestamps and write `NOT_MEASURED` for every scenario not executed.

At minimum, the authorized run must cover concurrent cold start, one replica
termination, PostgreSQL outage and reconnect without gateway restart, shared
job claim and ledger uniqueness, backup plus PostgreSQL dump restore, and a
bounded soak. Record RPO from committed-row counts and RTO from observed UTC
fault/recovery timestamps. `nexaroute backup` alone is not a PostgreSQL
restore. A same-cluster or same-host result must not be called two-independent-
host evidence unless the operator can attest that the nodes are independent
hosts rather than merely containers sharing one machine.

## Teardown

After saving sanitized evidence, uninstall the release, delete the disposable
Secrets and database workloads, drop the disposable databases, and revoke the
staging credentials. Never copy DSNs, tokens, kubeconfigs, or private config
into `evidence.json`, `report.md`, or an issue comment.
